package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
)

type artifactOriginsBody struct {
	Origins []string `json:"origins"`
}

type artifactPostBody struct {
	Origin    string `json:"origin"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Hash      string `json:"hash,omitempty"`
	Size      int64  `json:"size"`
	Duplicate bool   `json:"duplicate"`
}

type artifactRemoteStore struct {
	db.Store
}

func (artifactRemoteStore) ReadOnly() bool { return true }

func (artifactRemoteStore) MachineSessionCounts(
	context.Context,
) (map[string]int, error) {
	return map[string]int{}, nil
}

func (artifactRemoteStore) CountMetadataConflicts(context.Context) (int, error) {
	return 0, nil
}

func TestArtifactPeerRoutesRequireBearerAuthWhenConfigured(t *testing.T) {
	te := setup(t, withAuth("secret"))

	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/origins", nil, "")
	assertStatus(t, w, http.StatusUnauthorized)

	w = artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/origins", nil, "secret")
	assertStatus(t, w, http.StatusOK)
}

func TestArtifactPeerRoutesPostDuplicateAndFetch(t *testing.T) {
	te := setup(t, withAuth("secret"))
	origin := "peer-a1b2c3"
	metadataBody, metadataName := peerMetadataArtifact(
		origin,
		"2026-06-14T010203.000000001Z-00000000000000000000",
	)

	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		metadataBody, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	posted := decode[artifactPostBody](t, w)
	assert.False(t, posted.Duplicate)
	assert.Equal(t, "meta", posted.Kind)
	assert.Equal(t, metadataName, posted.Name)

	w = artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		metadataBody, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	posted = decode[artifactPostBody](t, w)
	assert.True(t, posted.Duplicate)

	w = artifactPeerRequest(
		t, te, http.MethodGet,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		nil, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, metadataBody, w.Body.Bytes())

	checkpoint := []byte(`{"origin":"peer-a1b2c3","seq":1,"sessions":{},"v":1}` + "\n")
	w = artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/checkpoints/cp-0000000001",
		checkpoint, "secret",
	)
	assertStatus(t, w, http.StatusOK)

	w = artifactPeerRequest(
		t, te, http.MethodGet,
		"/api/v1/artifacts/"+origin+"/checkpoint",
		nil, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, checkpoint, w.Body.Bytes())

	w = artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/origins", nil, "secret")
	assertStatus(t, w, http.StatusOK)
	origins := decode[artifactOriginsBody](t, w)
	assert.Contains(t, origins.Origins, origin)
}

func TestArtifactPeerPostRejectsRemoteStoreBeforeWrite(t *testing.T) {
	dir := tempDirWithRetryCleanup(t)
	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		WriteTimeout: 30 * time.Second,
	}
	srv := server.New(cfg, artifactRemoteStore{}, nil)
	te := &testEnv{
		srv:     srv,
		handler: wrapTestHandler(cfg, srv.Handler()),
		dataDir: dir,
	}
	origin := "peer-a1b2c3"
	metadataBody, metadataName := peerMetadataArtifact(
		origin,
		"2026-06-14T010203.000000001Z-00000000000000000000",
	)

	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		metadataBody, "",
	)

	assertStatus(t, w, http.StatusNotImplemented)
	assertArtifactMissing(t, dir, origin, "meta", metadataName)
}

func TestArtifactPeerReadRoutesRejectRemoteStoreBeforeReadingFiles(t *testing.T) {
	dir := tempDirWithRetryCleanup(t)
	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		WriteTimeout: 30 * time.Second,
	}
	origin := "peer-a1b2c3"
	checkpoint := []byte(`{"origin":"peer-a1b2c3","seq":1,"sessions":{},"v":1}` + "\n")
	_, err := artifact.WriteArtifact(
		filepath.Join(dir, "artifacts"),
		origin,
		"checkpoints",
		"cp-0000000001",
		checkpoint,
	)
	require.NoError(t, err)

	srv := server.New(cfg, artifactRemoteStore{}, nil)
	te := &testEnv{
		srv:     srv,
		handler: wrapTestHandler(cfg, srv.Handler()),
		dataDir: dir,
	}

	for _, path := range []string{
		"/api/v1/artifacts/origins",
		"/api/v1/artifacts/peers",
		"/api/v1/artifacts/" + origin + "/index",
		"/api/v1/artifacts/" + origin + "/checkpoint",
		"/api/v1/artifacts/" + origin + "/checkpoints/cp-0000000001",
	} {
		w := artifactPeerRequest(t, te, http.MethodGet, path, nil, "")
		assertStatus(t, w, http.StatusNotImplemented)
	}
}

func TestArtifactPeerPostRejectsReadOnlySQLiteBeforeWrite(t *testing.T) {
	dir := tempDirWithRetryCleanup(t)
	dbPath := filepath.Join(dir, "test.db")
	writable, err := db.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, writable.Close())
	readonly, err := db.OpenReadOnly(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { readonly.Close() })

	cfg := config.Config{
		Host:             "127.0.0.1",
		Port:             0,
		DataDir:          dir,
		DBPath:           dbPath,
		ArtifactOriginID: "desktop-d4e5f6",
		WriteTimeout:     30 * time.Second,
	}
	srv := server.New(cfg, readonly, nil)
	te := &testEnv{
		srv:     srv,
		handler: wrapTestHandler(cfg, srv.Handler()),
		db:      readonly,
		dataDir: dir,
	}
	origin := "peer-a1b2c3"
	metadataBody, metadataName := peerMetadataArtifact(
		origin,
		"2026-06-14T010203.000000001Z-00000000000000000000",
	)

	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		metadataBody, "",
	)

	assertStatus(t, w, http.StatusNotImplemented)
	assertArtifactMissing(t, dir, origin, "meta", metadataName)
}

type artifactPeerBody struct {
	Origin            string `json:"origin"`
	IsLocal           bool   `json:"is_local"`
	CheckpointSeq     int    `json:"checkpoint_seq"`
	PublishedSessions int    `json:"published_sessions"`
	LocalSessions     int    `json:"local_sessions"`
	LastPublished     string `json:"last_published"`
}

type artifactPeersBody struct {
	LocalOrigin   string             `json:"local_origin"`
	Peers         []artifactPeerBody `json:"peers"`
	ConflictCount int                `json:"conflict_count"`
}

func TestArtifactPeersStatus(t *testing.T) {
	local := "desktop-d4e5f6"
	te := setup(t, withArtifactOrigin(local))
	ctx := context.Background()
	artifactRoot := filepath.Join(te.dataDir, "artifacts")
	first := "hi"

	// Two owned sessions, exported so the local origin gets a checkpoint.
	dbtest.SeedSession(t, te.db, "local-1", "proj", func(s *db.Session) { s.FirstMessage = &first })
	dbtest.SeedSession(t, te.db, "local-2", "proj", func(s *db.Session) { s.FirstMessage = &first })
	exported, err := artifact.Export(ctx, te.db, artifactRoot, local)
	require.NoError(t, err)
	require.Equal(t, 2, exported)

	// A foreign peer publishes one session that the server imports.
	origin := "peer-a1b2c3"
	peerRoot := t.TempDir()
	peerDB, err := db.Open(filepath.Join(t.TempDir(), "peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { peerDB.Close() })
	dbtest.SeedSession(t, peerDB, "sess-1", "alpha", func(s *db.Session) { s.FirstMessage = &first })
	require.NoError(t, peerDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
	}))
	_, err = artifact.Export(ctx, peerDB, peerRoot, origin)
	require.NoError(t, err)
	postArtifactFile(t, te, origin, "segments", oneArtifactPath(t, peerRoot, origin, "segments", "*"))
	postArtifactFile(t, te, origin, "manifests", oneArtifactPath(t, peerRoot, origin, "manifests", "*"))
	postArtifactFile(t, te, origin, "checkpoints", oneArtifactPath(t, peerRoot, origin, "checkpoints", "*"))

	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/peers", nil, "")
	assertStatus(t, w, http.StatusOK)
	body := decode[artifactPeersBody](t, w)

	assert.Equal(t, local, body.LocalOrigin)
	assert.Equal(t, 0, body.ConflictCount)
	require.Len(t, body.Peers, 2)

	byOrigin := map[string]artifactPeerBody{}
	for _, p := range body.Peers {
		byOrigin[p.Origin] = p
	}

	localPeer, ok := byOrigin[local]
	require.True(t, ok, "local origin present in peers")
	assert.True(t, localPeer.IsLocal)
	assert.Equal(t, 2, localPeer.PublishedSessions)
	assert.Equal(t, 2, localPeer.LocalSessions)
	assert.NotEmpty(t, localPeer.LastPublished)

	peer, ok := byOrigin[origin]
	require.True(t, ok, "foreign origin present in peers")
	assert.False(t, peer.IsLocal)
	assert.Equal(t, 1, peer.PublishedSessions)
	assert.Equal(t, 1, peer.LocalSessions)
	assert.Equal(t, 1, peer.CheckpointSeq)
}

func TestArtifactPeersStatusUsesLatestCheckpointImportProvenance(t *testing.T) {
	te := setup(t, withArtifactOrigin("desktop-d4e5f6"))
	ctx := context.Background()
	firstMessage := "hello"

	// This peer is fully imported, then its local row is trashed. The import
	// provenance still proves the published manifest landed successfully.
	trashedOrigin := "trashed-a1b2c3"
	trashedRoot := t.TempDir()
	trashedDB, err := db.Open(filepath.Join(t.TempDir(), "trashed-peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { trashedDB.Close() })
	dbtest.SeedSession(t, trashedDB, "sess-1", "alpha", func(s *db.Session) {
		s.FirstMessage = &firstMessage
	})
	require.NoError(t, trashedDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
	}))
	_, err = artifact.Export(ctx, trashedDB, trashedRoot, trashedOrigin)
	require.NoError(t, err)
	postArtifactFile(t, te, trashedOrigin, "segments",
		oneArtifactPath(t, trashedRoot, trashedOrigin, "segments", "*"))
	postArtifactFile(t, te, trashedOrigin, "manifests",
		oneArtifactPath(t, trashedRoot, trashedOrigin, "manifests", "*"))
	postArtifactFile(t, te, trashedOrigin, "checkpoints",
		oneArtifactPath(t, trashedRoot, trashedOrigin, "checkpoints", "*"))
	require.NoError(t, te.db.SoftDeleteSession(trashedOrigin+"~sess-1"))

	// This peer's first manifest landed, but its latest checkpoint references a
	// newer manifest that has not arrived. The active stale row is not current.
	staleOrigin := "stale-d4e5f6"
	staleRoot := t.TempDir()
	staleDB, err := db.Open(filepath.Join(t.TempDir(), "stale-peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { staleDB.Close() })
	dbtest.SeedSession(t, staleDB, "sess-1", "before", func(s *db.Session) {
		s.FirstMessage = &firstMessage
	})
	require.NoError(t, staleDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
	}))
	_, err = artifact.Export(ctx, staleDB, staleRoot, staleOrigin)
	require.NoError(t, err)
	postArtifactFile(t, te, staleOrigin, "segments",
		oneArtifactPath(t, staleRoot, staleOrigin, "segments", "*"))
	postArtifactFile(t, te, staleOrigin, "manifests",
		oneArtifactPath(t, staleRoot, staleOrigin, "manifests", "*"))
	postArtifactFile(t, te, staleOrigin, "checkpoints",
		filepath.Join(staleRoot, staleOrigin, "checkpoints", "cp-0000000001.json"))

	dbtest.SeedSession(t, staleDB, "sess-1", "after", func(s *db.Session) {
		s.FirstMessage = &firstMessage
	})
	exported, err := artifact.Export(ctx, staleDB, staleRoot, staleOrigin)
	require.NoError(t, err)
	require.Equal(t, 1, exported)
	postArtifactFile(t, te, staleOrigin, "checkpoints",
		filepath.Join(staleRoot, staleOrigin, "checkpoints", "cp-0000000002.json"))
	require.NoError(t, te.db.UpsertSession(db.Session{
		ID:               staleOrigin + "~unrelated",
		Project:          "unrelated",
		Machine:          staleOrigin,
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-07-15T12:00:00Z",
	}), "seed an active same-origin row absent from the checkpoint")

	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/peers", nil, "")
	assertStatus(t, w, http.StatusOK)
	body := decode[artifactPeersBody](t, w)
	byOrigin := make(map[string]artifactPeerBody, len(body.Peers))
	for _, peer := range body.Peers {
		byOrigin[peer.Origin] = peer
	}

	trashedPeer, ok := byOrigin[trashedOrigin]
	require.True(t, ok)
	assert.Equal(t, 1, trashedPeer.PublishedSessions)
	assert.Equal(t, 1, trashedPeer.LocalSessions,
		"a locally trashed row remains landed when its manifest provenance matches")
	stalePeer, ok := byOrigin[staleOrigin]
	require.True(t, ok)
	assert.Equal(t, 1, stalePeer.PublishedSessions)
	assert.Equal(t, 0, stalePeer.LocalSessions,
		"stale and checkpoint-unrelated active rows must not count as landed")
}

func TestArtifactPeersStatusPublishesEmptyLocalOrigin(t *testing.T) {
	te := setup(t, withArtifactOrigin("desktop-d4e5f6"))
	// Discovery publishes an explicit empty checkpoint for a configured origin.
	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/peers", nil, "")
	assertStatus(t, w, http.StatusOK)
	body := decode[artifactPeersBody](t, w)
	assert.Equal(t, "desktop-d4e5f6", body.LocalOrigin)
	require.Len(t, body.Peers, 1)
	assert.True(t, body.Peers[0].IsLocal)
	assert.Equal(t, 0, body.Peers[0].PublishedSessions)
	assert.Equal(t, 1, body.Peers[0].CheckpointSeq)
	assert.NotEmpty(t, body.Peers[0].LastPublished)
}

func TestArtifactPeerPostRejectsHashMismatch(t *testing.T) {
	te := setup(t, withAuth("secret"))
	origin := "peer-a1b2c3"
	metadataBody, _ := peerMetadataArtifact(
		origin,
		"2026-06-14T010203.000000001Z-00000000000000000000",
	)
	badName := "2026-06-14T010203.000000001Z-peer-a1b2c3-" + strings.Repeat("0", 64)

	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(badName),
		metadataBody, "secret",
	)
	assertStatus(t, w, http.StatusBadRequest)
}

func TestArtifactPeerPostImportsAndEmitsDataChanged(t *testing.T) {
	te := setup(t, withArtifactOrigin("desktop-d4e5f6"))
	origin := "peer-a1b2c3"
	artifactRoot := t.TempDir()
	peerDB, err := db.Open(filepath.Join(t.TempDir(), "peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { peerDB.Close() })

	first := "hello"
	started := "2026-06-14T01:02:03Z"
	ended := "2026-06-14T01:03:03Z"
	dbtest.SeedSession(t, peerDB, "sess-1", "alpha", func(s *db.Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.FirstMessage = &first
		s.StartedAt = &started
		s.EndedAt = &ended
	})
	require.NoError(t, peerDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5},
	}))
	_, err = artifact.Export(context.Background(), peerDB, artifactRoot, origin)
	require.NoError(t, err)

	postArtifactFile(t, te, origin, "segments",
		oneArtifactPath(t, artifactRoot, origin, "segments", "*"))
	postArtifactFile(t, te, origin, "manifests",
		oneArtifactPath(t, artifactRoot, origin, "manifests", "*"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	stream := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		te.handler.ServeHTTP(stream, req)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)

	postArtifactFile(t, te, origin, "checkpoints",
		oneArtifactPath(t, artifactRoot, origin, "checkpoints", "*"))
	te.waitForSSEEvent(t, stream, "data_changed", 3*time.Second)

	// Live clients only refresh the session index on the "sessions"
	// scope and only invalidate hydrated session details on the
	// "messages" scope; an import needs both.
	assert.Eventually(t, func() bool {
		scopes := dataChangedScopes(stream)
		return scopes["messages"] && scopes["sessions"]
	}, 3*time.Second, 10*time.Millisecond,
		"import must emit data_changed with both messages and sessions scopes")

	got, err := te.db.GetSession(context.Background(), origin+"~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, origin, got.Machine)
	assert.Equal(t, "alpha", got.Project)

	cancel()
	<-done
}

// dataChangedScopes collects the scope payloads of every data_changed
// event written to the SSE stream so far.
func dataChangedScopes(w *flushRecorder) map[string]bool {
	scopes := make(map[string]bool)
	for _, e := range parseSSE(w.BodyString()) {
		if e.Event != "data_changed" {
			continue
		}
		var payload struct {
			Scope string `json:"scope"`
		}
		if json.Unmarshal([]byte(e.Data), &payload) == nil {
			scopes[payload.Scope] = true
		}
	}
	return scopes
}

func artifactPeerRequest(
	t *testing.T,
	te *testEnv,
	method string,
	path string,
	body []byte,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	return w
}

func postArtifactFile(
	t *testing.T,
	te *testEnv,
	origin string,
	kind string,
	path string,
) {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/"+kind+"/"+url.PathEscape(filepath.Base(path)),
		body, "",
	)
	assertStatus(t, w, http.StatusOK)
}

func assertArtifactMissing(
	t *testing.T,
	dataDir string,
	origin string,
	kind string,
	name string,
) {
	t.Helper()
	path := filepath.Join(dataDir, "artifacts", origin, kind, name)
	_, err := os.Stat(path)
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"artifact should not have been written at %s", path)
}

func oneArtifactPath(
	t *testing.T,
	root string,
	origin string,
	kind string,
	pattern string,
) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, origin, kind, pattern))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	return paths[0]
}

func peerMetadataArtifact(origin, hlc string) ([]byte, string) {
	body := []byte(`{"hlc":"` + hlc + `","op":"rename","origin":"` + origin + `","session_gid":"` + origin + `~sess-1","v":1,"value":{"display_name":"Remote"}}` + "\n")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	return body, hlc + "-" + hash + ".json"
}

func TestArtifactPeersStatusToleratesCorruptLatestCheckpoint(t *testing.T) {
	local := "desktop-d4e5f6"
	te := setup(t, withArtifactOrigin(local))
	ctx := context.Background()
	artifactRoot := filepath.Join(te.dataDir, "artifacts")
	first := "hi"

	dbtest.SeedSession(t, te.db, "local-1", "proj", func(s *db.Session) { s.FirstMessage = &first })
	_, err := artifact.Export(ctx, te.db, artifactRoot, local)
	require.NoError(t, err)
	dbtest.SeedSession(t, te.db, "local-2", "proj", func(s *db.Session) { s.FirstMessage = &first })
	_, err = artifact.Export(ctx, te.db, artifactRoot, local)
	require.NoError(t, err)

	latest := filepath.Join(artifactRoot, local, "checkpoints", "cp-0000000002.json")
	require.NoError(t, os.WriteFile(latest, []byte("not json"), 0o644))

	// One corrupt newest checkpoint must not make the peers page unusable:
	// discovery quarantines it and publishes the current two-session state at
	// the next unused sequence.
	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/peers", nil, "")
	assertStatus(t, w, http.StatusOK)
	body := decode[artifactPeersBody](t, w)
	require.Len(t, body.Peers, 1)
	assert.Equal(t, 3, body.Peers[0].CheckpointSeq)
	assert.Equal(t, 2, body.Peers[0].PublishedSessions)
	assert.NoFileExists(t, latest)
	assert.FileExists(t, latest+".corrupt")
}
