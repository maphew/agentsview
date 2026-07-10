package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestGarbageCollectDeletesSupersededArtifactsAfterGrace(t *testing.T) {
	root, oldPaths, livePaths := supersededArtifactFixture(t)
	now := time.Unix(1_800_000_000, 0)
	touchPaths(t, now.Add(-2*time.Hour), oldPaths...)

	res, err := GarbageCollect(context.Background(), GCOptions{
		Root:  root,
		Grace: time.Hour,
		Now:   now,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, res.Origins)
	assert.Equal(t, 3, res.Candidates)
	assert.Equal(t, 3, res.Eligible)
	assert.Equal(t, 3, res.Deleted)
	assert.Zero(t, res.KeptByGrace)
	for _, path := range oldPaths {
		assertNoFile(t, path)
	}
	for _, path := range livePaths {
		assertFileExists(t, path)
	}
}

func TestGarbageCollectDryRunLogsAndKeepsArtifacts(t *testing.T) {
	root, oldPaths, _ := supersededArtifactFixture(t)
	now := time.Unix(1_800_000_000, 0)
	touchPaths(t, now.Add(-2*time.Hour), oldPaths...)

	var logs []string
	res, err := GarbageCollect(context.Background(), GCOptions{
		Root:   root,
		Grace:  time.Hour,
		Now:    now,
		DryRun: true,
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	})
	require.NoError(t, err)

	assert.Equal(t, 3, res.Candidates)
	assert.Equal(t, 3, res.Eligible)
	assert.Zero(t, res.Deleted)
	require.NotEmpty(t, logs)
	assert.Contains(t, logs[0], "would delete")
	for _, path := range oldPaths {
		assertFileExists(t, path)
	}
}

func TestGarbageCollectKeepsUnreferencedArtifactsWithinGrace(t *testing.T) {
	root, oldPaths, _ := supersededArtifactFixture(t)
	now := time.Unix(1_800_000_000, 0)
	touchPaths(t, now.Add(-30*time.Minute), oldPaths...)

	res, err := GarbageCollect(context.Background(), GCOptions{
		Root:  root,
		Grace: time.Hour,
		Now:   now,
	})
	require.NoError(t, err)

	assert.Equal(t, 3, res.Candidates)
	assert.Zero(t, res.Eligible)
	assert.Equal(t, 3, res.KeptByGrace)
	assert.Zero(t, res.Deleted)
	for _, path := range oldPaths {
		assertFileExists(t, path)
	}
}

func TestGarbageCollectRejectsSymlinkedArtifactKindWithoutDeletingTarget(t *testing.T) {
	fixture := newGCClosureFixture(t)
	external := t.TempDir()
	liveSegment := filepath.Base(fixture.latest.segment)
	liveData, err := os.ReadFile(fixture.latest.segment)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(external, liveSegment), liveData, 0o644))
	staleName := strings.Repeat("a", 64) + segmentExtension
	stalePath := filepath.Join(external, staleName)
	require.NoError(t, os.WriteFile(stalePath, []byte("outside"), 0o644))
	require.NoError(t, os.RemoveAll(filepath.Join(fixture.originRoot, KindSegments)))
	require.NoError(t, os.Symlink(external, filepath.Join(fixture.originRoot, KindSegments)))

	_, err = GarbageCollect(context.Background(), GCOptions{
		Root: fixture.root,
		Now:  time.Now().Add(time.Hour),
	})
	require.Error(t, err)
	assert.FileExists(t, stalePath)
}

func TestGarbageCollectRejectsSymlinkedOrigin(t *testing.T) {
	external := newGCClosureFixture(t)
	root := t.TempDir()
	require.NoError(t, os.Symlink(
		external.originRoot, filepath.Join(root, external.origin),
	))

	_, err := GarbageCollect(context.Background(), GCOptions{Root: root})
	require.Error(t, err)
	for _, path := range external.oldPaths {
		assert.FileExists(t, path)
	}
}

func TestGarbageCollectSkipsOriginsWithoutCheckpoints(t *testing.T) {
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	path := filepath.Join(root, origin, KindManifests, hash+manifestExtension)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("orphan"), 0o644))

	res, err := GarbageCollect(context.Background(), GCOptions{
		Root:  root,
		Grace: 0,
		Now:   time.Unix(1_800_000_000, 0),
	})
	require.NoError(t, err)

	assert.Equal(t, 1, res.Origins)
	assert.Equal(t, 1, res.SkippedOrigins)
	assert.Zero(t, res.Candidates)
	assert.Zero(t, res.Deleted)
	assertFileExists(t, path)
}

func TestGarbageCollectSkipsOriginWithCorruptLiveManifest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	_, err := Export(ctx, database, root, origin)
	require.NoError(t, err)

	manifests := globArtifacts(t, root, origin, "manifests", "*"+manifestExtension)
	require.Len(t, manifests, 1)
	require.NoError(t, os.Remove(manifests[0]))
	require.NoError(t, writeCompressed(manifests[0], []byte("tampered")))
	segments := globArtifacts(t, root, origin, "segments", "*"+segmentExtension)
	require.Len(t, segments, 1)

	res, err := GarbageCollect(ctx, GCOptions{Root: root, Grace: 0})
	require.NoError(t, err)
	assert.Equal(t, 1, res.SkippedOrigins)
	assert.Zero(t, res.Deleted)
	assertFileExists(t, segments[0])
}

func TestGarbageCollectKeepsRawReferencedByLiveManifest(t *testing.T) {
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	originRoot := filepath.Join(root, origin)
	for _, dir := range []string{KindCheckpoints, KindManifests, KindSegments, KindRaw} {
		require.NoError(t, os.MkdirAll(filepath.Join(originRoot, dir), 0o755))
	}

	segmentData := []byte("{\"content\":\"hello\",\"ordinal\":0,\"role\":\"user\",\"v\":1}\n")
	segmentHash := hashHex(segmentData)
	require.NoError(t, writeCompressed(
		filepath.Join(originRoot, KindSegments, segmentHash+segmentExtension),
		segmentData,
	))

	liveRaw := []byte("live raw")
	liveRawHash := hashHex(liveRaw)
	require.NoError(t, os.WriteFile(
		filepath.Join(originRoot, KindRaw, liveRawHash),
		liveRaw,
		0o644,
	))
	staleRaw := []byte("stale raw")
	staleRawHash := hashHex(staleRaw)
	staleRawPath := filepath.Join(originRoot, KindRaw, staleRawHash)
	require.NoError(t, os.WriteFile(staleRawPath, staleRaw, 0o644))

	gid := origin + "~sess-1"
	m := manifest{
		Version:         formatVersion,
		Origin:          origin,
		NativeSessionID: "sess-1",
		Session:         manifestSession{ID: "sess-1", Machine: origin},
		Segments:        []string{segmentHash},
		RawSource: &rawSourceRef{
			Hash: liveRawHash,
			Size: int64(len(liveRaw)),
		},
		DataVersion: 1,
		Generation:  1,
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

	res, err := GarbageCollect(context.Background(), GCOptions{
		Root:  root,
		Grace: 0,
		Now:   time.Unix(1_800_000_000, 0),
	})
	require.NoError(t, err)

	assert.Equal(t, 1, res.Candidates)
	assert.Equal(t, 1, res.Deleted)
	assertNoFile(t, staleRawPath)
	assertFileExists(t, filepath.Join(originRoot, KindRaw, liveRawHash))
}

func TestGarbageCollectSkipsOriginWhenLatestClosureIsIncompleteOrCorrupt(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *gcClosureFixture)
	}{
		{
			name: "missing segment",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				require.NoError(t, os.Remove(fixture.latest.segment))
			},
		},
		{
			name: "corrupt segment",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				require.NoError(t, os.Remove(fixture.latest.segment))
				require.NoError(t, writeCompressed(fixture.latest.segment, []byte("corrupt\n")))
			},
		},
		{
			name: "future segment",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				data, err := canonicalJSON(segmentMessage{
					Version: formatVersion + 1,
					Ordinal: 0,
					Role:    "user",
					Content: "future",
				})
				require.NoError(t, err)
				hash := hashHex(data)
				require.NoError(t, writeCompressed(
					filepath.Join(fixture.originRoot, KindSegments, hash+segmentExtension),
					data,
				))
				m := fixture.latest.manifest
				m.Segments = []string{hash}
				manifestData, err := canonicalJSON(m)
				require.NoError(t, err)
				manifestHash := hashHex(manifestData)
				require.NoError(t, writeCompressed(
					filepath.Join(fixture.originRoot, KindManifests, manifestHash+manifestExtension),
					manifestData,
				))
				writeCheckpoint(t, fixture.originRoot, checkpoint{
					Version:  formatVersion,
					Origin:   fixture.origin,
					Sequence: 2,
					Sessions: map[string]string{fixture.gid: manifestHash},
				})
			},
		},
		{
			name: "invalid manifest reference",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				m := fixture.latest.manifest
				m.Segments = []string{"not-a-segment-hash"}
				manifestData, err := canonicalJSON(m)
				require.NoError(t, err)
				manifestHash := hashHex(manifestData)
				require.NoError(t, writeCompressed(
					filepath.Join(fixture.originRoot, KindManifests, manifestHash+manifestExtension),
					manifestData,
				))
				writeCheckpoint(t, fixture.originRoot, checkpoint{
					Version:  formatVersion,
					Origin:   fixture.origin,
					Sequence: 2,
					Sessions: map[string]string{fixture.gid: manifestHash},
				})
			},
		},
		{
			name: "tool call amplification",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				data := nestedSegmentData(t, segmentMessage{
					ToolCalls: make([]segmentToolCall, 257),
				})
				hash := hashHex(data)
				require.NoError(t, writeCompressed(
					filepath.Join(fixture.originRoot, KindSegments, hash+segmentExtension),
					data,
				))
				m := fixture.latest.manifest
				m.Segments = []string{hash}
				replaceLatestGCManifest(t, fixture, m)
			},
		},
		{
			name: "result event amplification",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				data := nestedSegmentData(t, segmentMessage{
					ToolCalls: []segmentToolCall{{
						ResultEvents: make([]segmentResultEvent, 1_025),
					}},
				})
				hash := hashHex(data)
				require.NoError(t, writeCompressed(
					filepath.Join(fixture.originRoot, KindSegments, hash+segmentExtension),
					data,
				))
				m := fixture.latest.manifest
				m.Segments = []string{hash}
				replaceLatestGCManifest(t, fixture, m)
			},
		},
		{
			name: "session message amplification",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				m := fixture.latest.manifest
				m.Segments = nil
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
						filepath.Join(fixture.originRoot, KindSegments, hash+segmentExtension),
						data,
					))
					m.Segments = append(m.Segments, hash)
				}
				replaceLatestGCManifest(t, fixture, m)
			},
		},
		{
			name: "checkpoint sequence mismatch",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				manifestData, err := canonicalJSON(fixture.latest.manifest)
				require.NoError(t, err)
				data, err := canonicalJSON(checkpoint{
					Version:  formatVersion,
					Origin:   fixture.origin,
					Sequence: 1,
					Sessions: map[string]string{
						fixture.gid: hashHex(manifestData),
					},
				})
				require.NoError(t, err)
				path := filepath.Join(
					fixture.originRoot, KindCheckpoints, "cp-0000000002.json",
				)
				require.NoError(t, os.Remove(path))
				require.NoError(t, writeFileAtomic(path, data, 0o644))
			},
		},
		{
			name: "missing raw",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				require.NoError(t, os.Remove(fixture.latest.raw))
			},
		},
		{
			name: "raw size mismatch",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				require.NoError(t, os.WriteFile(fixture.latest.raw, []byte("wrong size"), 0o644))
			},
		},
		{
			name: "raw hash mismatch",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				require.NoError(t, os.WriteFile(fixture.latest.raw, []byte("bad raw bytes"), 0o644))
			},
		},
		{
			name: "raw is not regular",
			mutate: func(t *testing.T, fixture *gcClosureFixture) {
				t.Helper()
				require.NoError(t, os.Remove(fixture.latest.raw))
				require.NoError(t, os.Mkdir(fixture.latest.raw, 0o755))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newGCClosureFixture(t)
			now := time.Unix(1_800_000_000, 0)
			touchPaths(t, now.Add(-2*time.Hour), fixture.oldPaths...)
			tt.mutate(t, fixture)

			res, err := GarbageCollect(context.Background(), GCOptions{
				Root: fixture.root, Grace: 0, Now: now,
			})
			require.NoError(t, err)
			assert.Equal(t, 1, res.Origins)
			assert.Equal(t, 1, res.SkippedOrigins)
			assert.Zero(t, res.Candidates)
			assert.Zero(t, res.Deleted)
			for _, path := range fixture.oldPaths {
				assertFileExists(t, path)
			}
		})
	}
}

type gcClosureFixture struct {
	root       string
	origin     string
	originRoot string
	gid        string
	oldPaths   []string
	latest     gcGeneration
}

type gcGeneration struct {
	manifest manifest
	segment  string
	raw      string
}

func newGCClosureFixture(t *testing.T) *gcClosureFixture {
	t.Helper()
	fixture := &gcClosureFixture{
		root:   t.TempDir(),
		origin: "laptop-a1b2c3",
	}
	fixture.originRoot = filepath.Join(fixture.root, fixture.origin)
	fixture.gid = fixture.origin + "~sess-1"
	for _, dir := range []string{KindCheckpoints, KindManifests, KindSegments, KindRaw} {
		require.NoError(t, os.MkdirAll(filepath.Join(fixture.originRoot, dir), 0o755))
	}

	old := writeGCGeneration(t, fixture, 1, "old message", []byte("old raw bytes"))
	fixture.latest = writeGCGeneration(t, fixture, 2, "new message", []byte("new raw bytes"))
	oldManifestData, err := canonicalJSON(old.manifest)
	require.NoError(t, err)
	fixture.oldPaths = []string{
		filepath.Join(fixture.originRoot, KindCheckpoints, "cp-0000000001.json"),
		filepath.Join(fixture.originRoot, KindManifests, hashHex(oldManifestData)+manifestExtension),
		old.segment,
		old.raw,
	}
	return fixture
}

func writeGCGeneration(
	t *testing.T,
	fixture *gcClosureFixture,
	sequence int,
	content string,
	raw []byte,
) gcGeneration {
	t.Helper()
	segmentData, err := canonicalJSON(segmentMessage{
		Version:       formatVersion,
		Ordinal:       0,
		Role:          "user",
		Content:       content,
		ContentLength: len(content),
	})
	require.NoError(t, err)
	segmentHash := hashHex(segmentData)
	segmentPath := filepath.Join(
		fixture.originRoot, KindSegments, segmentHash+segmentExtension,
	)
	require.NoError(t, writeCompressed(segmentPath, segmentData))

	rawHash := hashHex(raw)
	rawPath := filepath.Join(fixture.originRoot, KindRaw, rawHash)
	require.NoError(t, os.WriteFile(rawPath, raw, 0o644))
	m := manifest{
		Version:         formatVersion,
		Origin:          fixture.origin,
		NativeSessionID: "sess-1",
		Session: manifestSession{
			ID:      "sess-1",
			Machine: fixture.origin,
		},
		Segments: []string{segmentHash},
		RawSource: &rawSourceRef{
			Hash: rawHash,
			Size: int64(len(raw)),
		},
		DataVersion: sequence,
		Generation:  sequence,
	}
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	require.NoError(t, writeCompressed(
		filepath.Join(fixture.originRoot, KindManifests, manifestHash+manifestExtension),
		manifestData,
	))
	writeCheckpoint(t, fixture.originRoot, checkpoint{
		Version:  formatVersion,
		Origin:   fixture.origin,
		Sequence: sequence,
		Sessions: map[string]string{fixture.gid: manifestHash},
	})
	return gcGeneration{manifest: m, segment: segmentPath, raw: rawPath}
}

func replaceLatestGCManifest(t *testing.T, fixture *gcClosureFixture, m manifest) {
	t.Helper()
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	require.NoError(t, writeCompressed(
		filepath.Join(fixture.originRoot, KindManifests, manifestHash+manifestExtension),
		manifestData,
	))
	writeCheckpoint(t, fixture.originRoot, checkpoint{
		Version:  formatVersion,
		Origin:   fixture.origin,
		Sequence: 2,
		Sessions: map[string]string{fixture.gid: manifestHash},
	})
}

func supersededArtifactFixture(t *testing.T) (string, []string, []string) {
	t.Helper()
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	gid := origin + "~sess-1"
	originRoot := filepath.Join(root, origin)

	seedSession(t, database, "sess-1", "alpha")
	_, err := Export(ctx, database, root, origin)
	require.NoError(t, err)
	firstCP, err := readLatestCheckpoint(originRoot)
	require.NoError(t, err)
	require.NotNil(t, firstCP)
	firstManifestHash := firstCP.Sessions[gid]
	require.NotEmpty(t, firstManifestHash)
	firstManifest, err := readManifest(originRoot, firstManifestHash)
	require.NoError(t, err)
	require.Len(t, firstManifest.Segments, 1)

	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "planet", ContentLength: 6},
	}))
	_, err = Export(ctx, database, root, origin)
	require.NoError(t, err)
	latestCP, err := readLatestCheckpoint(originRoot)
	require.NoError(t, err)
	require.NotNil(t, latestCP)
	latestManifestHash := latestCP.Sessions[gid]
	require.NotEmpty(t, latestManifestHash)
	require.NotEqual(t, firstManifestHash, latestManifestHash)
	latestManifest, err := readManifest(originRoot, latestManifestHash)
	require.NoError(t, err)
	require.Len(t, latestManifest.Segments, 1)
	require.NotEqual(t, firstManifest.Segments[0], latestManifest.Segments[0])

	oldPaths := []string{
		filepath.Join(originRoot, KindCheckpoints, "cp-0000000001.json"),
		filepath.Join(originRoot, KindManifests, firstManifestHash+manifestExtension),
		filepath.Join(originRoot, KindSegments, firstManifest.Segments[0]+segmentExtension),
	}
	livePaths := []string{
		filepath.Join(originRoot, KindCheckpoints, "cp-0000000002.json"),
		filepath.Join(originRoot, KindManifests, latestManifestHash+manifestExtension),
		filepath.Join(originRoot, KindSegments, latestManifest.Segments[0]+segmentExtension),
	}
	return root, oldPaths, livePaths
}

func touchPaths(t *testing.T, ts time.Time, paths ...string) {
	t.Helper()
	for _, path := range paths {
		require.NoError(t, os.Chtimes(path, ts, ts))
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
