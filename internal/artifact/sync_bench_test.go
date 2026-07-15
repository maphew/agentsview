package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// Artifact archive benchmarks cover the local-first sync costs that are not
// represented by the parser/SQLite ingest benchmarks:
//
//   - BenchmarkArtifactInitialExport: serialize a populated local archive into
//     a fresh immutable artifact store.
//   - BenchmarkArtifactInitialImport: hydrate a fresh peer database from that
//     store.
//   - BenchmarkArtifactSyncWarmNoop: scan and exchange a fully converged local
//     archive without writing another generation.
//   - BenchmarkArtifactSyncSingleSessionIncremental: export and import one
//     changed session while the rest of the archive remains unchanged.
//
// Fixture sizes scale through AGENTSVIEW_BENCH_ARTIFACT_SESSIONS,
// AGENTSVIEW_BENCH_ARTIFACT_MESSAGES, and
// AGENTSVIEW_BENCH_ARTIFACT_CONTENT_BYTES. Fixture construction, database
// creation, source mutation, cleanup, and correctness checks are outside timed
// regions. The incremental fixture deliberately gains one message and one
// immutable artifact generation per iteration, so compare it only at the same
// fixed iteration count (as the benchmark gate does). MB/s reports the initial
// archive's uncompressed message content traversed per operation, not artifact
// bytes copied on the wire; warm and incremental exports still canonicalize
// every owned session while detecting the unchanged majority.

const (
	defaultArtifactBenchSessions     = 75
	defaultArtifactBenchMessages     = 40
	defaultArtifactBenchContentBytes = 256
	artifactBenchOrigin              = "laptop-a1b2c3"
	artifactBenchPeerOrigin          = "desktop-d4e5f6"
)

type artifactBenchArchive struct {
	sessions          int
	messages          int
	contentBytes      int
	uncompressedBytes int64
}

func newArtifactBenchArchive(b *testing.B) artifactBenchArchive {
	b.Helper()
	return artifactBenchArchive{
		sessions: artifactBenchIntFromEnv(
			b,
			"AGENTSVIEW_BENCH_ARTIFACT_SESSIONS",
			defaultArtifactBenchSessions,
		),
		messages: artifactBenchIntFromEnv(
			b,
			"AGENTSVIEW_BENCH_ARTIFACT_MESSAGES",
			defaultArtifactBenchMessages,
		),
		contentBytes: artifactBenchIntFromEnv(
			b,
			"AGENTSVIEW_BENCH_ARTIFACT_CONTENT_BYTES",
			defaultArtifactBenchContentBytes,
		),
	}
}

func artifactBenchIntFromEnv(b *testing.B, name string, fallback int) int {
	b.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		b.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return value
}

func (a artifactBenchArchive) reportScale(b *testing.B) {
	b.Helper()
	b.ReportMetric(float64(a.sessions), "sessions")
	b.ReportMetric(float64(a.messages), "messages/session")
	b.ReportMetric(float64(a.contentBytes), "content-bytes/message")
}

func (a *artifactBenchArchive) seed(b *testing.B, database *db.DB) {
	b.Helper()
	writes := make([]db.SessionBatchWrite, 0, a.sessions)
	for sessionIndex := range a.sessions {
		sessionID := artifactBenchSessionID(sessionIndex)
		messages := a.sessionMessages(sessionID, sessionIndex)
		startedAt := fmt.Sprintf("2026-06-%02dT10:00:00Z", 1+sessionIndex%28)
		endedAt := fmt.Sprintf("2026-06-%02dT11:00:00Z", 1+sessionIndex%28)
		firstMessage := messages[0].Content
		writes = append(writes, db.SessionBatchWrite{
			Session: db.Session{
				ID:                   sessionID,
				Project:              fmt.Sprintf("project-%02d", sessionIndex%12),
				Machine:              "local",
				Agent:                "claude",
				FirstMessage:         &firstMessage,
				StartedAt:            &startedAt,
				EndedAt:              &endedAt,
				MessageCount:         len(messages),
				UserMessageCount:     (len(messages) + 1) / 2,
				TotalOutputTokens:    (len(messages) / 2) * 48,
				PeakContextTokens:    2048 + len(messages),
				HasTotalOutputTokens: len(messages) > 1,
				HasPeakContextTokens: len(messages) > 1,
				HasToolCalls:         len(messages) >= 8,
				HasContextData:       len(messages) > 1,
				Cwd:                  fmt.Sprintf("/work/project-%02d", sessionIndex%12),
				GitBranch:            fmt.Sprintf("feature/archive-%04d", sessionIndex),
				CreatedAt:            startedAt,
			},
			Messages:        messages,
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}

	result, err := database.WriteSessionBatchAtomic(writes)
	require.NoError(b, err)
	require.Equal(b, a.sessions, result.WrittenSessions)
	require.Equal(b, a.sessions*a.messages, result.WrittenMessages)
}

func (a *artifactBenchArchive) sessionMessages(
	sessionID string,
	sessionIndex int,
) []db.Message {
	messages := make([]db.Message, 0, a.messages)
	for ordinal := range a.messages {
		message := artifactBenchMessage(
			sessionID, sessionIndex, ordinal, a.contentBytes,
		)
		a.uncompressedBytes += int64(len(message.Content))
		messages = append(messages, message)
	}
	return messages
}

func artifactBenchMessage(
	sessionID string,
	sessionIndex, ordinal, contentBytes int,
) db.Message {
	role := "user"
	if ordinal%2 == 1 {
		role = "assistant"
	}
	prefix := fmt.Sprintf(
		"%s message %04d for session %04d: ", role, ordinal, sessionIndex,
	)
	content := artifactBenchSizedContent(prefix, contentBytes)
	message := db.Message{
		SessionID:     sessionID,
		Ordinal:       ordinal,
		Role:          role,
		Content:       content,
		ContentLength: len(content),
		Timestamp: fmt.Sprintf(
			"2026-06-%02dT10:%02d:%02dZ",
			1+sessionIndex%28, (ordinal/60)%60, ordinal%60,
		),
		SourceUUID: fmt.Sprintf("source-%04d-%04d", sessionIndex, ordinal),
	}
	if role == "assistant" {
		message.Model = "claude-sonnet-bench"
		message.TokenUsage = json.RawMessage(
			`{"input_tokens":2048,"output_tokens":48,"cache_read_input_tokens":1024}`,
		)
		message.ContextTokens = 2048 + ordinal
		message.OutputTokens = 48
		message.HasContextTokens = true
		message.HasOutputTokens = true
	}
	if ordinal > 0 && ordinal%8 == 7 {
		message.HasToolUse = true
		message.ToolCalls = []db.ToolCall{{
			ToolName:      "Read",
			Category:      "read",
			ToolUseID:     fmt.Sprintf("tool-%04d-%04d", sessionIndex, ordinal),
			InputJSON:     fmt.Sprintf(`{"file_path":"internal/archive/%04d.go"}`, sessionIndex),
			FilePath:      fmt.Sprintf("internal/archive/%04d.go", sessionIndex),
			ResultContent: "package archive\n\nfunc syncArtifact() {}",
			ResultEvents: []db.ToolResultEvent{{
				Source:        "tool_result",
				Status:        "completed",
				Content:       "read completed",
				ContentLength: len("read completed"),
				EventIndex:    0,
			}},
		}}
	}
	return message
}

func artifactBenchSizedContent(prefix string, size int) string {
	if len(prefix) >= size {
		return prefix
	}
	const body = "inspect the checkpoint, compare the manifest, and preserve the session history. "
	needed := size - len(prefix)
	return prefix + strings.Repeat(body, (needed+len(body)-1)/len(body))[:needed]
}

func artifactBenchSessionID(index int) string {
	return fmt.Sprintf("bench-session-%04d", index)
}

func openArtifactBenchDB(b *testing.B, path string) *db.DB {
	b.Helper()
	database, err := db.Open(path)
	require.NoError(b, err)
	return database
}

func silenceArtifactBenchLogs(b *testing.B) {
	b.Helper()
	previous := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(previous) })
}

func verifyArtifactBenchImport(
	b *testing.B,
	database *db.DB,
	archive artifactBenchArchive,
) {
	b.Helper()
	ctx := context.Background()
	gid := artifactBenchOrigin + "~" + artifactBenchSessionID(0)
	session, err := database.GetSessionFull(ctx, gid)
	require.NoError(b, err)
	require.NotNil(b, session)
	assert.Equal(b, "project-00", session.Project)
	assert.Equal(b, artifactBenchOrigin, session.Machine)
	assert.Equal(b, archive.messages, session.MessageCount)

	messages, err := database.GetAllMessages(ctx, gid)
	require.NoError(b, err)
	require.Len(b, messages, archive.messages)
	assert.Equal(
		b,
		artifactBenchSizedContent("user message 0000 for session 0000: ", archive.contentBytes),
		messages[0].Content,
	)
	if archive.messages >= 8 {
		require.Len(b, messages[7].ToolCalls, 1)
		assert.Equal(b, "Read", messages[7].ToolCalls[0].ToolName)
		require.Len(b, messages[7].ToolCalls[0].ResultEvents, 1)
		assert.Equal(b, "read completed", messages[7].ToolCalls[0].ResultEvents[0].Content)
	}
}

// BenchmarkArtifactInitialExport measures canonical serialization,
// compression, and immutable-file writes for an already-populated archive. A
// fresh artifact root is used for every iteration; opening and seeding SQLite
// are deliberately excluded.
func BenchmarkArtifactInitialExport(b *testing.B) {
	silenceArtifactBenchLogs(b)
	archive := newArtifactBenchArchive(b)
	database := openArtifactBenchDB(b, filepath.Join(b.TempDir(), "source.db"))
	b.Cleanup(func() { require.NoError(b, database.Close()) })
	archive.seed(b, database)
	ctx := context.Background()

	// Preflight the exact public export path and its consumer-visible import
	// contract before measuring fresh-root exports.
	preflightRoot := b.TempDir()
	exported, err := Export(ctx, database, preflightRoot, artifactBenchOrigin)
	require.NoError(b, err)
	require.Equal(b, archive.sessions, exported)
	peer := openArtifactBenchDB(b, filepath.Join(b.TempDir(), "peer.db"))
	imported, err := ImportDetailed(ctx, peer, preflightRoot, artifactBenchPeerOrigin)
	require.NoError(b, err)
	require.Equal(b, archive.sessions, imported.Sessions)
	require.Equal(b, archive.sessions*archive.messages, imported.Messages)
	verifyArtifactBenchImport(b, peer, archive)
	require.NoError(b, peer.Close())

	iterationParent := b.TempDir()
	b.SetBytes(archive.uncompressedBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		root := filepath.Join(iterationParent, fmt.Sprintf("export-%06d", i))
		b.StartTimer()

		got, exportErr := Export(ctx, database, root, artifactBenchOrigin)

		b.StopTimer()
		require.NoError(b, exportErr)
		require.Equal(b, archive.sessions, got)
		checkpoint, checkpointErr := readLatestCheckpoint(
			filepath.Join(root, artifactBenchOrigin),
		)
		require.NoError(b, checkpointErr)
		require.NotNil(b, checkpoint)
		assert.Len(b, checkpoint.Sessions, archive.sessions)
		require.NoError(b, os.RemoveAll(root))
	}
	archive.reportScale(b)
}

// BenchmarkArtifactInitialImport measures decoding and writing a complete
// artifact archive into a fresh peer database. Artifact generation and database
// open/migrations are outside the timed region.
func BenchmarkArtifactInitialImport(b *testing.B) {
	silenceArtifactBenchLogs(b)
	archive := newArtifactBenchArchive(b)
	source := openArtifactBenchDB(b, filepath.Join(b.TempDir(), "source.db"))
	archive.seed(b, source)
	root := b.TempDir()
	exported, err := Export(context.Background(), source, root, artifactBenchOrigin)
	require.NoError(b, err)
	require.Equal(b, archive.sessions, exported)
	require.NoError(b, source.Close())

	ctx := context.Background()
	dbParent := b.TempDir()
	b.SetBytes(archive.uncompressedBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		dbPath := filepath.Join(dbParent, fmt.Sprintf("import-%06d.db", i))
		peer := openArtifactBenchDB(b, dbPath)
		b.StartTimer()

		result, importErr := ImportDetailed(ctx, peer, root, artifactBenchPeerOrigin)

		b.StopTimer()
		require.NoError(b, importErr)
		require.Equal(b, archive.sessions, result.Sessions)
		require.Equal(b, archive.sessions*archive.messages, result.Messages)
		assert.Zero(b, result.Deferred)
		verifyArtifactBenchImport(b, peer, archive)
		require.NoError(b, peer.Close())
		stale, globErr := filepath.Glob(dbPath + "*")
		require.NoError(b, globErr)
		for _, path := range stale {
			require.NoError(b, os.Remove(path))
		}
	}
	archive.reportScale(b)
}

// BenchmarkArtifactSyncWarmNoop measures a complete folder sync after export
// state, local artifacts, and the target have converged. Results are retained
// during the timed region and checked afterward so result assertions do not
// dilute the cost under test.
func BenchmarkArtifactSyncWarmNoop(b *testing.B) {
	silenceArtifactBenchLogs(b)
	archive := newArtifactBenchArchive(b)
	database := openArtifactBenchDB(b, filepath.Join(b.TempDir(), "source.db"))
	b.Cleanup(func() { require.NoError(b, database.Close()) })
	archive.seed(b, database)
	opts := SyncOptions{
		DataDir: b.TempDir(),
		Target:  b.TempDir(),
		Origin:  artifactBenchOrigin,
	}
	ctx := context.Background()

	initial, err := SyncFolder(ctx, database, opts)
	require.NoError(b, err)
	require.Equal(b, archive.sessions, initial.ExportedSessions)
	preflight, err := SyncFolder(ctx, database, opts)
	require.NoError(b, err)
	assert.Equal(b, SyncResult{Origin: artifactBenchOrigin}, preflight)

	results := make([]SyncResult, b.N)
	errors := make([]error, b.N)
	b.SetBytes(archive.uncompressedBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		results[i], errors[i] = SyncFolder(ctx, database, opts)
	}
	b.StopTimer()

	for i := range b.N {
		require.NoError(b, errors[i])
		assert.Equal(b, SyncResult{Origin: artifactBenchOrigin}, results[i])
	}
	checkpoint, err := readLatestCheckpoint(
		filepath.Join(opts.DataDir, "artifacts", artifactBenchOrigin),
	)
	require.NoError(b, err)
	require.NotNil(b, checkpoint)
	assert.Equal(b, 1, checkpoint.Sequence,
		"a no-op sync must not publish another checkpoint generation")
	archive.reportScale(b)
}

// BenchmarkArtifactSyncSingleSessionIncremental measures end-to-end folder
// sync after one session changes: the source exports and publishes the new
// generation, then the peer fetches and imports it. Updating the source DB and
// validating the peer's stored tail are both excluded from the timed region.
func BenchmarkArtifactSyncSingleSessionIncremental(b *testing.B) {
	silenceArtifactBenchLogs(b)
	archive := newArtifactBenchArchive(b)
	source := openArtifactBenchDB(b, filepath.Join(b.TempDir(), "source.db"))
	peer := openArtifactBenchDB(b, filepath.Join(b.TempDir(), "peer.db"))
	b.Cleanup(func() {
		require.NoError(b, source.Close())
		require.NoError(b, peer.Close())
	})
	archive.seed(b, source)
	share := b.TempDir()
	sourceOpts := SyncOptions{
		DataDir: b.TempDir(),
		Target:  share,
		Origin:  artifactBenchOrigin,
	}
	peerOpts := SyncOptions{
		DataDir: b.TempDir(),
		Target:  share,
		Origin:  artifactBenchPeerOrigin,
	}
	ctx := context.Background()

	initial, err := SyncFolder(ctx, source, sourceOpts)
	require.NoError(b, err)
	require.Equal(b, archive.sessions, initial.ExportedSessions)
	initialImport, err := SyncFolder(ctx, peer, peerOpts)
	require.NoError(b, err)
	require.Equal(b, archive.sessions, initialImport.ImportedSessions)
	verifyArtifactBenchImport(b, peer, archive)

	sessionID := artifactBenchSessionID(0)
	peerSessionID := artifactBenchOrigin + "~" + sessionID
	b.SetBytes(archive.uncompressedBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		tailContent := fmt.Sprintf("incremental artifact message %06d", i)
		appendArtifactBenchMessage(b, source, sessionID, tailContent)
		b.StartTimer()

		sourceResult, sourceErr := SyncFolder(ctx, source, sourceOpts)
		peerResult, peerErr := SyncFolder(ctx, peer, peerOpts)

		b.StopTimer()
		require.NoError(b, sourceErr)
		require.NoError(b, peerErr)
		assert.Equal(b, 1, sourceResult.ExportedSessions)
		assert.Zero(b, sourceResult.ImportedSessions)
		assert.Equal(b, 1, peerResult.ImportedSessions)
		assert.Equal(b, archive.messages+i+1, peerResult.ImportedMessages)

		messages, messagesErr := peer.GetAllMessages(ctx, peerSessionID)
		require.NoError(b, messagesErr)
		require.Len(b, messages, archive.messages+i+1)
		assert.Equal(b, tailContent, messages[len(messages)-1].Content)
	}
	archive.reportScale(b)
}

func appendArtifactBenchMessage(
	b *testing.B,
	database *db.DB,
	sessionID, content string,
) {
	b.Helper()
	ctx := context.Background()
	session, err := database.GetSessionFull(ctx, sessionID)
	require.NoError(b, err)
	require.NotNil(b, session)
	messages, err := database.GetAllMessages(ctx, sessionID)
	require.NoError(b, err)
	ordinal := len(messages)
	messages = append(messages, db.Message{
		SessionID:     sessionID,
		Ordinal:       ordinal,
		Role:          "user",
		Content:       content,
		ContentLength: len(content),
		Timestamp:     fmt.Sprintf("2026-07-15T12:%02d:%02dZ", (ordinal/60)%60, ordinal%60),
		SourceUUID:    fmt.Sprintf("incremental-%06d", ordinal),
	})
	session.MessageCount = len(messages)
	session.UserMessageCount++
	endedAt := messages[len(messages)-1].Timestamp
	session.EndedAt = &endedAt
	require.NoError(b, database.UpsertSession(*session))
	require.NoError(b, database.ReplaceSessionMessages(sessionID, messages))
}
