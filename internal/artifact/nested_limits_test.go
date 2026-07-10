package artifact

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestDecodeSegmentRejectsAggregateNestedLimitsWithSmallLimits(t *testing.T) {
	tests := []struct {
		name      string
		records   []segmentMessage
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "tool calls per segment",
			records: []segmentMessage{
				{ToolCalls: []segmentToolCall{{}}},
				{ToolCalls: []segmentToolCall{{}}},
			},
			configure: func(limits *artifactLimits) {
				limits.segmentToolCalls = 1
			},
			wantError: "segment tool call limit",
		},
		{
			name: "result events per segment",
			records: []segmentMessage{
				{ToolCalls: []segmentToolCall{{ResultEvents: []segmentResultEvent{{}}}}},
				{ToolCalls: []segmentToolCall{{ResultEvents: []segmentResultEvent{{}}}}},
			},
			configure: func(limits *artifactLimits) {
				limits.segmentResultEvents = 1
			},
			wantError: "segment result event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := productionArtifactLimits()
			tt.configure(&limits)
			data := nestedSegmentData(t, tt.records...)

			_, err := decodeSegmentWithLimits(data, limits)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
		})
	}
}

func TestReadManifestMessagesRejectsSessionNestedLimitsWithSmallLimits(t *testing.T) {
	tests := []struct {
		name      string
		record    segmentMessage
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name:   "tool calls per session",
			record: segmentMessage{ToolCalls: []segmentToolCall{{}}},
			configure: func(limits *artifactLimits) {
				limits.sessionToolCalls = 1
			},
			wantError: "session tool call limit",
		},
		{
			name: "result events per session",
			record: segmentMessage{ToolCalls: []segmentToolCall{{
				ResultEvents: []segmentResultEvent{{}},
			}}},
			configure: func(limits *artifactLimits) {
				limits.sessionResultEvents = 1
			},
			wantError: "session result event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originRoot := t.TempDir()
			m := manifest{}
			for ordinal := range 2 {
				record := tt.record
				record.Ordinal = ordinal
				record.Content = string(rune('a' + ordinal))
				data := nestedSegmentData(t, record)
				hash := hashHex(data)
				require.NoError(t, writeCompressed(
					filepath.Join(originRoot, KindSegments, hash+segmentExtension), data,
				))
				m.Segments = append(m.Segments, hash)
			}
			limits := productionArtifactLimits()
			tt.configure(&limits)

			_, err := readManifestMessagesWithLimits(originRoot, m, limits)
			require.Error(t, err)
			assert.ErrorIs(t, err, errCorruptArtifact)
			assert.Contains(t, err.Error(), tt.wantError)
		})
	}
}

func TestPeerArtifactDefersFutureNestedSchema(t *testing.T) {
	root := t.TempDir()
	origin := "peer-a1b2c3"
	data := []byte(`{"v":2,"ordinal":{"future_shape":true},"tool_calls":{"future_shape":true}}` + "\n")
	hash := hashHex(data)
	compressed := compressPeerTestData(t, data)

	res, err := WriteArtifact(root, origin, KindSegments, hash, compressed)
	require.NoError(t, err)
	assert.Equal(t, hash+segmentExtension, res.Name)
	stored, err := ReadArtifact(root, origin, KindSegments, hash)
	require.NoError(t, err)
	assert.Equal(t, compressed, stored.Data)
}

func TestDecodeSegmentAcceptsCanonicalTrailingNewlineAndEmptySession(t *testing.T) {
	record := nestedSegmentData(t, segmentMessage{})
	tests := []struct {
		name string
		data []byte
		want int
	}{
		{name: "canonical trailing newline", data: record, want: 1},
		{name: "zero byte empty segment", data: nil, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, err := decodeSegment(tt.data)
			require.NoError(t, err)
			assert.Len(t, msgs, tt.want)
		})
	}
}

func TestImportQuarantinesNestedAmplificationWithoutAdvancingState(t *testing.T) {
	tests := []struct {
		name   string
		record segmentMessage
	}{
		{
			name: "too many tool calls",
			record: segmentMessage{
				ToolCalls: make([]segmentToolCall, 257),
			},
		},
		{
			name: "too many result events",
			record: segmentMessage{
				ToolCalls: []segmentToolCall{{
					ResultEvents: make([]segmentResultEvent, 1_025),
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			origin := "laptop-a1b2c3"
			localOrigin := "desktop-d4e5f6"
			importDB := testDB(t)
			gid, segmentPath := writeNestedImportFixture(
				t, root, origin, tt.record,
			)

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
		})
	}
}

func TestExportRejectsNestedAmplificationBeforePublication(t *testing.T) {
	tests := []struct {
		name      string
		message   db.Message
		wantError string
	}{
		{
			name: "too many tool calls in one message",
			message: db.Message{
				ToolCalls: make([]db.ToolCall, 257),
			},
			wantError: "tool call limit exceeded for message ordinal 0",
		},
		{
			name: "too many result events in one tool call",
			message: db.Message{
				ToolCalls: []db.ToolCall{{
					ResultEvents: make([]db.ToolResultEvent, 1_025),
				}},
			},
			wantError: "result event limit exceeded for tool call 0 in message ordinal 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testDB(t)
			root := t.TempDir()
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			message := tt.message
			message.SessionID = "sess-1"
			message.Ordinal = 0
			message.Role = "assistant"
			require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{message}))

			_, err := Export(ctx, database, root, origin)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
			assertNoPublishedArtifactFiles(t, root, origin)
			state, err := database.GetSyncState(exportStateKey(origin, "sess-1"))
			require.NoError(t, err)
			assert.Empty(t, state)
		})
	}
}

func TestExportChunksOnAggregateNestedLimitsWithSmallLimits(t *testing.T) {
	tests := []struct {
		name         string
		resultEvents []db.ToolResultEvent
		configure    func(*artifactLimits)
	}{
		{
			name: "tool calls per segment",
			configure: func(limits *artifactLimits) {
				limits.segmentToolCalls = 2
			},
		},
		{
			name:         "result events per segment",
			resultEvents: []db.ToolResultEvent{{EventIndex: 0}},
			configure: func(limits *artifactLimits) {
				limits.segmentResultEvents = 2
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testDB(t)
			root := t.TempDir()
			origin := "laptop-a1b2c3"
			originRoot := filepath.Join(root, origin)
			for _, kind := range []string{KindManifests, KindSegments} {
				require.NoError(t, os.MkdirAll(filepath.Join(originRoot, kind), 0o755))
			}
			seedSession(t, database, "sess-1", "alpha")
			msgs := make([]db.Message, 3)
			for ordinal := range msgs {
				msgs[ordinal] = db.Message{
					SessionID: "sess-1",
					Ordinal:   ordinal,
					Role:      "assistant",
					ToolCalls: []db.ToolCall{{
						ResultEvents: tt.resultEvents,
					}},
				}
			}
			require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))
			limits := productionArtifactLimits()
			tt.configure(&limits)

			manifestHash, changed, err := exportSessionWithLimits(
				ctx, database, originRoot, origin, "sess-1", "", limits,
			)
			require.NoError(t, err)
			assert.True(t, changed)
			m, err := readManifest(originRoot, manifestHash)
			require.NoError(t, err)
			require.Len(t, m.Segments, 2)
			got, err := readManifestMessages(originRoot, m)
			require.NoError(t, err)
			require.Len(t, got, 3)
			for ordinal := range got {
				assert.Equal(t, ordinal, got[ordinal].Ordinal)
				require.Len(t, got[ordinal].ToolCalls, 1)
				assert.Len(t, got[ordinal].ToolCalls[0].ResultEvents, len(tt.resultEvents))
			}
		})
	}
}

func TestExportRejectsMessageThatCannotFitNestedSegmentLimits(t *testing.T) {
	tests := []struct {
		name      string
		message   db.Message
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "tool calls cannot split across segments",
			message: db.Message{
				ToolCalls: []db.ToolCall{{}, {}},
			},
			configure: func(limits *artifactLimits) {
				limits.segmentToolCalls = 1
			},
			wantError: "2 tool calls",
		},
		{
			name: "one tool result history cannot split across segments",
			message: db.Message{
				ToolCalls: []db.ToolCall{{
					ResultEvents: []db.ToolResultEvent{{EventIndex: 0}, {EventIndex: 1}},
				}},
			},
			configure: func(limits *artifactLimits) {
				limits.segmentResultEvents = 1
			},
			wantError: "2 result events",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testDB(t)
			root := t.TempDir()
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			message := tt.message
			message.SessionID = "sess-1"
			message.Ordinal = 0
			message.Role = "assistant"
			require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{message}))
			limits := productionArtifactLimits()
			tt.configure(&limits)

			_, _, err := exportSessionWithLimits(
				ctx, database, filepath.Join(root, origin), origin, "sess-1", "", limits,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot fit in one segment")
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

func TestExportRejectsSessionNestedLimitsBeforeWritingWithSmallLimits(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "tool calls per session",
			configure: func(limits *artifactLimits) {
				limits.sessionToolCalls = 1
			},
			wantError: "session tool call limit",
		},
		{
			name: "result events per session",
			configure: func(limits *artifactLimits) {
				limits.sessionResultEvents = 1
			},
			wantError: "session result event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testDB(t)
			root := t.TempDir()
			origin := "laptop-a1b2c3"
			originRoot := filepath.Join(root, origin)
			seedSession(t, database, "sess-1", "alpha")
			msgs := make([]db.Message, 2)
			for ordinal := range msgs {
				msgs[ordinal] = db.Message{
					SessionID: "sess-1",
					Ordinal:   ordinal,
					Role:      "assistant",
					ToolCalls: []db.ToolCall{{
						ResultEvents: []db.ToolResultEvent{{EventIndex: 0}},
					}},
				}
			}
			require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))
			limits := productionArtifactLimits()
			tt.configure(&limits)

			_, _, err := exportSessionWithLimits(
				ctx, database, originRoot, origin, "sess-1", "", limits,
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

func nestedSegmentData(t *testing.T, records ...segmentMessage) []byte {
	t.Helper()
	var data bytes.Buffer
	for ordinal := range records {
		records[ordinal].Version = formatVersion
		records[ordinal].Ordinal = ordinal
		records[ordinal].Role = "assistant"
		encoded, err := canonicalJSON(records[ordinal])
		require.NoError(t, err)
		_, err = data.Write(encoded)
		require.NoError(t, err)
	}
	return data.Bytes()
}

func writeNestedImportFixture(
	t *testing.T,
	root, origin string,
	record segmentMessage,
) (string, string) {
	t.Helper()
	originRoot := filepath.Join(root, origin)
	data := nestedSegmentData(t, record)
	segmentHash := hashHex(data)
	segmentPath := filepath.Join(originRoot, KindSegments, segmentHash+segmentExtension)
	require.NoError(t, writeCompressed(segmentPath, data))

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
	return gid, segmentPath
}
