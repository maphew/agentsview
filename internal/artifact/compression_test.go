package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestReadCompressedRejectsDecodedOutputAboveKindLimit(t *testing.T) {
	tests := []struct {
		name      string
		extension string
		total     int64
	}{
		{name: "manifest", extension: manifestExtension, total: 16<<20 + 1},
		{name: "segment", extension: segmentExtension, total: 64<<20 + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compressed, _ := compressedArtifactWithPadding(t, []byte("{}\n"), tt.total)
			path := filepath.Join(t.TempDir(), "artifact"+tt.extension)
			require.NoError(t, os.WriteFile(path, compressed, 0o644))

			_, err := readCompressed(path)
			require.Error(t, err)
			assert.ErrorIs(t, err, errCorruptArtifact)
			assert.Contains(t, err.Error(), "decoded output exceeds")
		})
	}
}

func TestReadCompressedLimitsTotalAcrossConcatenatedFrames(t *testing.T) {
	first, _ := compressedArtifactWithPadding(t, []byte("{}\n"), 9<<20)
	second, _ := compressedArtifactWithPadding(t, nil, 9<<20)
	path := filepath.Join(t.TempDir(), "artifact"+manifestExtension)
	require.NoError(t, os.WriteFile(path, append(first, second...), 0o644))

	_, err := readCompressed(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, errCorruptArtifact)
	assert.Contains(t, err.Error(), "decoded output exceeds")
}

func TestReadCompressedRejectsFrameAboveWriterWindow(t *testing.T) {
	compressed, _ := compressedArtifactWithPadding(
		t, []byte("{}\n"), 9<<20,
		zstd.WithWindowSize(16<<20),
		zstd.WithEncoderConcurrency(1),
	)
	path := filepath.Join(t.TempDir(), "artifact"+segmentExtension)
	require.NoError(t, os.WriteFile(path, compressed, 0o644))

	_, err := readCompressed(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, errCorruptArtifact)
	assert.Contains(t, err.Error(), "window size exceeded")
}

func TestWriteArtifactRejectsOversizedDecodedOutputWithoutWriting(t *testing.T) {
	root := t.TempDir()
	origin := "peer-a1b2c3"
	segmentData, err := canonicalJSON(segmentMessage{
		Version: formatVersion,
		Ordinal: 0,
		Role:    "user",
		Content: "hello",
	})
	require.NoError(t, err)
	manifestData, err := canonicalJSON(manifest{
		Version:         formatVersion,
		Origin:          origin,
		NativeSessionID: "sess-1",
		Session: manifestSession{
			ID:      "sess-1",
			Machine: origin,
		},
		Segments: []string{strings64("a")},
	})
	require.NoError(t, err)

	tests := []struct {
		name      string
		kind      string
		extension string
		prefix    []byte
		total     int64
	}{
		{
			name: "manifest", kind: KindManifests, extension: manifestExtension,
			prefix: manifestData, total: 16<<20 + 1,
		},
		{
			name: "segment", kind: KindSegments, extension: segmentExtension,
			prefix: segmentData, total: 64<<20 + 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compressed, hash := compressedArtifactWithPadding(t, tt.prefix, tt.total)

			_, err := WriteArtifact(root, origin, tt.kind, hash, compressed)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
			assert.Contains(t, err.Error(), "decoded output exceeds")
			assert.NoFileExists(t, filepath.Join(root, origin, tt.kind, hash+tt.extension))
		})
	}
}

func TestWriteArtifactRejectsFrameAboveWriterWindowWithoutWriting(t *testing.T) {
	root := t.TempDir()
	origin := "peer-a1b2c3"
	prefix, err := canonicalJSON(segmentMessage{
		Version: formatVersion,
		Ordinal: 0,
		Role:    "user",
		Content: "hello",
	})
	require.NoError(t, err)
	compressed, hash := compressedArtifactWithPadding(
		t, prefix, 9<<20,
		zstd.WithWindowSize(16<<20),
		zstd.WithEncoderConcurrency(1),
	)

	_, err = WriteArtifact(root, origin, KindSegments, hash, compressed)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	assert.Contains(t, err.Error(), "window size exceeded")
	assert.NoFileExists(t, filepath.Join(root, origin, KindSegments, hash+segmentExtension))
}

func TestWriteArtifactRejectsManifestStructuralAmplification(t *testing.T) {
	origin := "peer-a1b2c3"
	validHash := strings64("a")
	tests := []struct {
		name      string
		manifest  manifest
		wantError string
	}{
		{
			name: "duplicate segment references",
			manifest: manifest{
				Segments: []string{validHash, validHash},
			},
			wantError: "duplicate segment reference",
		},
		{
			name: "too many segment references",
			manifest: manifest{
				Segments: syntheticSegmentHashes(17),
			},
			wantError: "segment reference limit",
		},
		{
			name: "too many usage events",
			manifest: manifest{
				Segments:    []string{validHash},
				UsageEvents: make([]artifactUsageEvent, 32_769),
			},
			wantError: "usage event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			m := tt.manifest
			m.Version = formatVersion
			m.Origin = origin
			m.NativeSessionID = "sess-1"
			m.Session = manifestSession{ID: "sess-1", Machine: origin}
			data, err := canonicalJSON(m)
			require.NoError(t, err)
			hash := hashHex(data)

			_, err = WriteArtifact(
				root, origin, KindManifests, hash, compressPeerTestData(t, data),
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
			assert.Contains(t, err.Error(), tt.wantError)
			assert.NoFileExists(t, filepath.Join(
				root, origin, KindManifests, hash+manifestExtension,
			))
		})
	}
}

func TestWriteArtifactRejectsSegmentRecordAmplification(t *testing.T) {
	root := t.TempDir()
	origin := "peer-a1b2c3"
	data := syntheticSegmentRecords(t, 4_097)
	hash := hashHex(data)

	_, err := WriteArtifact(
		root, origin, KindSegments, hash, compressPeerTestData(t, data),
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	assert.Contains(t, err.Error(), "message record limit")
	assert.NoFileExists(t, filepath.Join(
		root, origin, KindSegments, hash+segmentExtension,
	))
}

func TestWriteArtifactRejectsSegmentNestedAmplificationWithoutWriting(t *testing.T) {
	origin := "peer-a1b2c3"
	tests := []struct {
		name      string
		record    segmentMessage
		wantError string
	}{
		{
			name: "too many tool calls in one message",
			record: segmentMessage{
				ToolCalls: make([]segmentToolCall, 257),
			},
			wantError: "tool call limit",
		},
		{
			name: "too many result events in one tool call",
			record: segmentMessage{
				ToolCalls: []segmentToolCall{{
					ResultEvents: make([]segmentResultEvent, 1_025),
				}},
			},
			wantError: "result event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			record := tt.record
			record.Version = formatVersion
			record.Ordinal = 7
			record.Role = "assistant"
			data, err := canonicalJSON(record)
			require.NoError(t, err)
			hash := hashHex(data)

			_, err = WriteArtifact(
				root, origin, KindSegments, hash, compressPeerTestData(t, data),
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
			assert.Contains(t, err.Error(), tt.wantError)
			assert.NoFileExists(t, filepath.Join(
				root, origin, KindSegments, hash+segmentExtension,
			))
		})
	}
}

func TestWriteArtifactRejectsBlankSegmentRecordsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	origin := "peer-a1b2c3"
	data := bytes.Repeat([]byte("\n"), 4_097)
	hash := hashHex(data)

	_, err := WriteArtifact(
		root, origin, KindSegments, hash, compressPeerTestData(t, data),
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	assert.Contains(t, err.Error(), "blank message record")
	assert.NoFileExists(t, filepath.Join(
		root, origin, KindSegments, hash+segmentExtension,
	))
}

func TestReadManifestRejectsDuplicateSegmentReferences(t *testing.T) {
	originRoot := t.TempDir()
	hash := strings64("a")
	data, err := canonicalJSON(manifest{
		Version:         formatVersion,
		Origin:          "laptop-a1b2c3",
		NativeSessionID: "sess-1",
		Session: manifestSession{
			ID:      "sess-1",
			Machine: "laptop-a1b2c3",
		},
		Segments: []string{hash, hash},
	})
	require.NoError(t, err)
	manifestHash := hashHex(data)
	path := filepath.Join(originRoot, KindManifests, manifestHash+manifestExtension)
	require.NoError(t, writeCompressed(path, data))

	_, err = readManifest(originRoot, manifestHash)
	require.Error(t, err)
	assert.ErrorIs(t, err, errCorruptArtifact)
	assert.Contains(t, err.Error(), "duplicate segment reference")
	assert.NoFileExists(t, path)
	assert.FileExists(t, path+quarantineSuffix)
}

func TestReadSegmentRejectsRecordAmplification(t *testing.T) {
	originRoot := t.TempDir()
	data := syntheticSegmentRecords(t, 4_097)
	hash := hashHex(data)
	path := filepath.Join(originRoot, KindSegments, hash+segmentExtension)
	require.NoError(t, writeCompressed(path, data))

	_, err := readSegmentMessages(originRoot, hash)
	require.Error(t, err)
	assert.ErrorIs(t, err, errCorruptArtifact)
	assert.Contains(t, err.Error(), "message record limit")
	assert.NoFileExists(t, path)
	assert.FileExists(t, path+quarantineSuffix)
}

func TestReadSegmentQuarantinesNestedAmplification(t *testing.T) {
	tests := []struct {
		name      string
		record    segmentMessage
		wantError string
	}{
		{
			name: "too many tool calls in one message",
			record: segmentMessage{
				ToolCalls: make([]segmentToolCall, 257),
			},
			wantError: "tool call limit",
		},
		{
			name: "too many result events in one tool call",
			record: segmentMessage{
				ToolCalls: []segmentToolCall{{
					ResultEvents: make([]segmentResultEvent, 1_025),
				}},
			},
			wantError: "result event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originRoot := t.TempDir()
			record := tt.record
			record.Version = formatVersion
			record.Ordinal = 7
			record.Role = "assistant"
			data, err := canonicalJSON(record)
			require.NoError(t, err)
			hash := hashHex(data)
			path := filepath.Join(originRoot, KindSegments, hash+segmentExtension)
			require.NoError(t, writeCompressed(path, data))

			_, err = readSegmentMessages(originRoot, hash)
			require.Error(t, err)
			assert.ErrorIs(t, err, errCorruptArtifact)
			assert.Contains(t, err.Error(), tt.wantError)
			assert.NoFileExists(t, path)
			assert.FileExists(t, path+quarantineSuffix)
		})
	}
}

func TestReadSegmentQuarantinesBlankRecords(t *testing.T) {
	originRoot := t.TempDir()
	data := bytes.Repeat([]byte("\n"), 4_097)
	hash := hashHex(data)
	path := filepath.Join(originRoot, KindSegments, hash+segmentExtension)
	require.NoError(t, writeCompressed(path, data))

	_, err := readSegmentMessages(originRoot, hash)
	require.Error(t, err)
	assert.ErrorIs(t, err, errCorruptArtifact)
	assert.Contains(t, err.Error(), "blank message record")
	assert.NoFileExists(t, path)
	assert.FileExists(t, path+quarantineSuffix)
}

func TestReadManifestMessagesRejectsSessionMessageAmplification(t *testing.T) {
	originRoot := t.TempDir()
	m := manifest{Segments: make([]string, 0, 9)}
	ordinal := 0
	for segment := range 9 {
		count := 4_096
		if segment == 8 {
			count = 1
		}
		data := syntheticSegmentRecordsFrom(t, ordinal, count)
		ordinal += count
		hash := hashHex(data)
		require.NoError(t, writeCompressed(
			filepath.Join(originRoot, KindSegments, hash+segmentExtension), data,
		))
		m.Segments = append(m.Segments, hash)
	}

	_, err := readManifestMessages(originRoot, m)
	require.Error(t, err)
	assert.ErrorIs(t, err, errCorruptArtifact)
	assert.Contains(t, err.Error(), "session message limit")
}

func TestReadManifestMessagesRejectsSessionDecodedByteBudgetWithSmallLimit(t *testing.T) {
	originRoot := t.TempDir()
	m := manifest{}
	totalBytes := 0
	for ordinal := range 2 {
		data := syntheticSegmentRecordsFrom(t, ordinal, 1)
		totalBytes += len(data)
		hash := hashHex(data)
		require.NoError(t, writeCompressed(
			filepath.Join(originRoot, KindSegments, hash+segmentExtension), data,
		))
		m.Segments = append(m.Segments, hash)
	}
	limits := productionArtifactLimits()
	limits.sessionDecodedBytes = int64(totalBytes - 1)

	_, err := readManifestMessagesWithLimits(originRoot, m, limits)
	require.Error(t, err)
	assert.ErrorIs(t, err, errCorruptArtifact)
	assert.Contains(t, err.Error(), "session decoded byte limit")
}

func TestExportChunksOnMessageRecordLimit(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	msgs := make([]db.Message, 4_097)
	for i := range msgs {
		msgs[i] = db.Message{SessionID: "sess-1", Ordinal: i, Role: "user"}
	}
	require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))

	_, err := Export(ctx, database, root, origin)
	require.NoError(t, err)
	cp, err := readLatestCheckpoint(filepath.Join(root, origin))
	require.NoError(t, err)
	require.NotNil(t, cp)
	m, err := readManifest(filepath.Join(root, origin), cp.Sessions[origin+"~sess-1"])
	require.NoError(t, err)
	require.Len(t, m.Segments, 2)
	got, err := readManifestMessages(filepath.Join(root, origin), m)
	require.NoError(t, err)
	assert.Len(t, got, 4_097)
}

func TestExportRejectsOversizedGeneratedManifestBeforePublication(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha", func(sess *db.Session) {
		first := strings.Repeat("x", int(manifestDecodedLimit))
		sess.FirstMessage = &first
	})

	_, err := Export(ctx, database, root, origin)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generated manifest exceeds")
	assertNoPublishedArtifactFiles(t, root, origin)
	state, err := database.GetSyncState(exportStateKey(origin, "sess-1"))
	require.NoError(t, err)
	assert.Empty(t, state)
}

func TestExportRejectsSessionMessageAmplificationBeforePublication(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	msgs := make([]db.Message, 32_769)
	for i := range msgs {
		msgs[i] = db.Message{SessionID: "sess-1", Ordinal: i, Role: "user"}
	}
	require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))

	_, err := Export(ctx, database, root, origin)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session message limit")
	assertNoPublishedArtifactFiles(t, root, origin)
	state, err := database.GetSyncState(exportStateKey(origin, "sess-1"))
	require.NoError(t, err)
	assert.Empty(t, state)
}

func TestExportRejectsUsageEventAmplificationBeforePublication(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	events := make([]db.UsageEvent, 32_769)
	for i := range events {
		events[i] = db.UsageEvent{
			SessionID: "sess-1",
			Source:    "fixture",
			DedupKey:  fmt.Sprintf("usage-%d", i),
		}
	}
	require.NoError(t, database.ReplaceSessionUsageEvents("sess-1", events))

	_, err := Export(ctx, database, root, origin)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage event limit")
	assertNoPublishedArtifactFiles(t, root, origin)
	state, err := database.GetSyncState(exportStateKey(origin, "sess-1"))
	require.NoError(t, err)
	assert.Empty(t, state)
}

func TestExportSessionRejectsAggregateLimitsBeforeWritingWithSmallLimits(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "decoded bytes",
			configure: func(limits *artifactLimits) {
				limits.sessionDecodedBytes = 1
			},
			wantError: "session decoded byte limit",
		},
		{
			name: "segment references",
			configure: func(limits *artifactLimits) {
				limits.segmentMessages = 1
				limits.manifestSegments = 1
			},
			wantError: "segment reference limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testDB(t)
			root := t.TempDir()
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			limits := productionArtifactLimits()
			tt.configure(&limits)

			_, _, err := exportSessionWithLimits(
				ctx, database, filepath.Join(root, origin), origin, "sess-1", "", limits,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
			assert.Empty(t, globArtifacts(
				t, root, origin, KindSegments, "*"+segmentExtension,
			))
			assert.Empty(t, globArtifacts(
				t, root, origin, KindManifests, "*"+manifestExtension,
			))
		})
	}
}

func TestImportQuarantinesOversizedSegmentWithoutAdvancingState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	localOrigin := "desktop-d4e5f6"
	importDB := testDB(t)
	originRoot := filepath.Join(root, origin)

	prefix, err := canonicalJSON(segmentMessage{
		Version:       formatVersion,
		Ordinal:       0,
		Role:          "user",
		Content:       "hello",
		ContentLength: 5,
	})
	require.NoError(t, err)
	compressed, segmentHash := compressedArtifactWithPadding(t, prefix, 64<<20+1)
	segmentPath := filepath.Join(originRoot, KindSegments, segmentHash+segmentExtension)
	require.NoError(t, os.MkdirAll(filepath.Dir(segmentPath), 0o755))
	require.NoError(t, os.WriteFile(segmentPath, compressed, 0o644))

	gid := origin + "~sess-1"
	m := manifest{
		Version:         formatVersion,
		Origin:          origin,
		NativeSessionID: "sess-1",
		Session: manifestSession{
			ID:      "sess-1",
			Machine: origin,
		},
		Segments: []string{segmentHash},
	}
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	require.NoError(t, writeCompressed(
		filepath.Join(originRoot, KindManifests, manifestHash+manifestExtension),
		manifestData,
	))
	writeCheckpoint(t, originRoot, checkpoint{
		Version:  formatVersion,
		Origin:   origin,
		Sequence: 1,
		Sessions: map[string]string{gid: manifestHash},
	})

	res, err := ImportDetailed(ctx, importDB, root, localOrigin)
	require.NoError(t, err)
	assert.False(t, res.Changed())
	state, err := importDB.GetSyncState(importStateKey(origin, gid))
	require.NoError(t, err)
	assert.Empty(t, state)
	got, err := importDB.GetSessionFull(ctx, gid)
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.NoFileExists(t, segmentPath)
	assert.FileExists(t, segmentPath+quarantineSuffix)
}

func TestExportChunksLargeMultiMessageSessionInOrder(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	content := strings.Repeat("x", 9<<20)
	msgs := make([]db.Message, 4)
	for i := range msgs {
		msgs[i] = db.Message{
			SessionID:     "sess-1",
			Ordinal:       i,
			Role:          "user",
			Content:       content,
			ContentLength: len(content),
		}
	}
	require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))

	count, err := Export(ctx, database, root, origin)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	cp, err := readLatestCheckpoint(filepath.Join(root, origin))
	require.NoError(t, err)
	require.NotNil(t, cp)
	m, err := readManifest(filepath.Join(root, origin), cp.Sessions[origin+"~sess-1"])
	require.NoError(t, err)
	require.Len(t, m.Segments, 2)

	got, err := readManifestMessages(filepath.Join(root, origin), m)
	require.NoError(t, err)
	require.Len(t, got, 4)
	for i := range got {
		assert.Equal(t, i, got[i].Ordinal)
		assert.Equal(t, content, got[i].Content)
	}
}

func TestExportRejectsSingleEncodedRecordAboveReadableLimit(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	content := strings.Repeat("x", 64<<20)
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{{
		SessionID:     "sess-1",
		Ordinal:       0,
		Role:          "user",
		Content:       content,
		ContentLength: len(content),
	}}))

	_, err := Export(ctx, database, root, origin)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encoded message record")
	assert.Contains(t, err.Error(), "67108864-byte readable limit")
	assert.Empty(t, globArtifacts(t, root, origin, KindCheckpoints, "cp-*.json"))
}

func TestExportPreservesSmallSingleSegmentHash(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	msgs, err := database.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	segmentData, err := encodeSegment(canonicalMessages(msgs))
	require.NoError(t, err)
	wantHash := hashHex(segmentData)

	_, err = Export(ctx, database, root, origin)
	require.NoError(t, err)
	cp, err := readLatestCheckpoint(filepath.Join(root, origin))
	require.NoError(t, err)
	require.NotNil(t, cp)
	m, err := readManifest(filepath.Join(root, origin), cp.Sessions[origin+"~sess-1"])
	require.NoError(t, err)
	assert.Equal(t, []string{wantHash}, m.Segments)
}

type repeatedByteReader byte

func (r repeatedByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func compressedArtifactWithPadding(
	t *testing.T,
	prefix []byte,
	total int64,
	opts ...zstd.EOption,
) ([]byte, string) {
	t.Helper()
	require.LessOrEqual(t, int64(len(prefix)), total)
	var compressed bytes.Buffer
	enc, err := zstd.NewWriter(&compressed, opts...)
	require.NoError(t, err)
	h := sha256.New()
	w := io.MultiWriter(enc, h)
	_, err = w.Write(prefix)
	require.NoError(t, err)
	_, err = io.CopyN(w, repeatedByteReader('\n'), total-int64(len(prefix)))
	require.NoError(t, err)
	require.NoError(t, enc.Close())
	return compressed.Bytes(), fmt.Sprintf("%x", h.Sum(nil))
}

func syntheticSegmentHashes(count int) []string {
	hashes := make([]string, count)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("%064x", i+1)
	}
	return hashes
}

func syntheticSegmentRecords(t *testing.T, count int) []byte {
	return syntheticSegmentRecordsFrom(t, 0, count)
}

func syntheticSegmentRecordsFrom(t *testing.T, start, count int) []byte {
	t.Helper()
	var data bytes.Buffer
	for i := range count {
		record, err := canonicalJSON(segmentMessage{
			Version: formatVersion,
			Ordinal: start + i,
			Role:    "user",
		})
		require.NoError(t, err)
		_, err = data.Write(record)
		require.NoError(t, err)
	}
	return data.Bytes()
}

func assertNoPublishedArtifactFiles(t *testing.T, root, origin string) {
	t.Helper()
	for _, kind := range []string{
		KindCheckpoints, KindManifests, KindSegments, KindMeta, KindRaw,
	} {
		entries, err := os.ReadDir(filepath.Join(root, origin, kind))
		require.NoError(t, err)
		assert.Empty(t, entries, "unexpected published %s artifact", kind)
	}
}
