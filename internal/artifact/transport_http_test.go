package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// fakeArtifactPeer is an in-memory peer implementing the artifact API surface
// the HTTP transport exchanges against: origin listing, per-origin index, and
// artifact get/post.
type fakeArtifactPeer struct {
	mu    sync.Mutex
	arts  map[string][]byte // "origin/kind/name" -> bytes
	posts []string
}

func newFakeArtifactPeer() *fakeArtifactPeer {
	return &fakeArtifactPeer{arts: map[string][]byte{}}
}

func (p *fakeArtifactPeer) put(origin, kind, name string, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.arts[origin+"/"+kind+"/"+name] = append([]byte(nil), data...)
}

func (p *fakeArtifactPeer) has(origin, kind, name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.arts[origin+"/"+kind+"/"+name]
	return ok
}

func (p *fakeArtifactPeer) postedKinds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	kinds := make([]string, 0, len(p.posts))
	for _, key := range p.posts {
		parts := strings.SplitN(key, "/", 3)
		if len(parts) == 3 {
			kinds = append(kinds, parts[1])
		}
	}
	return kinds
}

func (p *fakeArtifactPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, artifactAPIPath+"/")
	p.mu.Lock()
	defer p.mu.Unlock()
	if rest == "origins" {
		seen := map[string]bool{}
		for key := range p.arts {
			seen[strings.SplitN(key, "/", 2)[0]] = true
		}
		origins := make([]string, 0, len(seen))
		for origin := range seen {
			origins = append(origins, origin)
		}
		sort.Strings(origins)
		_ = json.NewEncoder(w).Encode(map[string]any{"origins": origins})
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "index" {
		idx := OriginArtifactIndex{Origin: parts[0]}
		for key := range p.arts {
			kp := strings.SplitN(key, "/", 3)
			if kp[0] != parts[0] {
				continue
			}
			switch kp[1] {
			case KindCheckpoints:
				idx.Checkpoints = append(idx.Checkpoints, kp[2])
			case KindManifests:
				idx.Manifests = append(idx.Manifests, kp[2])
			case KindSegments:
				idx.Segments = append(idx.Segments, kp[2])
			case KindMeta:
				idx.Meta = append(idx.Meta, kp[2])
			case KindRaw:
				idx.Raw = append(idx.Raw, kp[2])
			}
		}
		_ = json.NewEncoder(w).Encode(idx)
		return
	}
	if len(parts) == 3 {
		switch r.Method {
		case http.MethodGet:
			data, ok := p.arts[rest]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		case http.MethodPost:
			data, _ := io.ReadAll(r.Body)
			p.arts[rest] = data
			p.posts = append(p.posts, rest)
			w.WriteHeader(http.StatusCreated)
		}
		return
	}
	http.NotFound(w, r)
}

func TestHTTPTransportRequiresTLSForNonLoopbackPeers(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "public HTTP", target: "http://203.0.113.10:8080", wantErr: true},
		{name: "hostname HTTP", target: "http://peer.example.test:8080", wantErr: true},
		{name: "public HTTPS", target: "https://peer.example.test:8443"},
		{name: "localhost HTTP", target: "http://localhost:8080"},
		{name: "IPv4 loopback HTTP", target: "http://127.0.0.1:8080"},
		{name: "IPv6 loopback HTTP", target: "http://[::1]:8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, err := newHTTPTransport(tt.target, "", false)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "requires HTTPS")
				assert.Nil(t, tr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, tr)
		})
	}
}

func TestHTTPTransportAllowsExplicitRemotePlaintextOptIn(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	tr, err := newHTTPTransport("http://peer.example.test:8080", "", true)

	require.NoError(t, err)
	require.NotNil(t, tr)
	assert.Equal(t, "http://peer.example.test:8080"+artifactAPIPath, tr.base)
	assert.Contains(t, logs.String(), "warning")
	assert.Contains(t, logs.String(), "plaintext HTTP")
}

func TestHTTPTransportPrepareHonorsCanceledSync(t *testing.T) {
	var requests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"origins": []string{}})
	}))
	t.Cleanup(peer.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Sync(ctx, testDB(t), SyncOptions{
		DataDir: t.TempDir(),
		Target:  peer.URL,
		Origin:  "laptop-a1b2c3",
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, requests.Load(), "canceled preparation must not contact the peer")
}

func TestHTTPTransportPullSkipsCorruptRemoteArtifact(t *testing.T) {
	origin := "desktop-d4e5f6"
	remoteRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-7", "beta")
	})
	remoteIdx, err := ListArtifacts(remoteRoot, origin)
	require.NoError(t, err)
	peer := newFakeArtifactPeer()
	for _, item := range indexItems(remoteIdx) {
		art, err := ReadArtifact(remoteRoot, origin, item.kind, item.name)
		require.NoError(t, err)
		peer.put(origin, item.kind, item.name, art.Data)
	}
	corruptName := hashHex([]byte("corrupt")) + segmentExtension
	peer.put(origin, KindSegments, corruptName, []byte("garbage"))

	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	tr, err := newHTTPTransport(srv.URL, "", false)
	require.NoError(t, err)
	localRoot := filepath.Join(t.TempDir(), "artifacts")
	require.NoError(t, tr.Exchange(context.Background(), localRoot))

	gotIdx, err := ListArtifacts(localRoot, origin)
	require.NoError(t, err)
	assert.ElementsMatch(t, indexItems(remoteIdx), indexItems(gotIdx))
	assert.NoFileExists(t, filepath.Join(localRoot, origin, KindSegments, corruptName))
}

func TestHTTPTransportPushSkipsAndQuarantinesCorruptLocalArtifact(t *testing.T) {
	origin := "laptop-a1b2c3"
	localRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	validIdx, err := ListArtifacts(localRoot, origin)
	require.NoError(t, err)
	corruptName := hashHex([]byte("junk")) + segmentExtension
	corruptPath := filepath.Join(localRoot, origin, KindSegments, corruptName)
	require.NoError(t, os.WriteFile(corruptPath, []byte("garbage"), 0o644))

	peer := newFakeArtifactPeer()
	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	tr, err := newHTTPTransport(srv.URL, "", false)
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localRoot))

	for _, item := range indexItems(validIdx) {
		assert.True(t, peer.has(origin, item.kind, item.name),
			"expected %s/%s on the peer", item.kind, item.name)
	}
	assert.False(t, peer.has(origin, KindSegments, corruptName))
	assert.NoFileExists(t, corruptPath)
	assert.FileExists(t, corruptPath+quarantineSuffix)
}

func TestHTTPTransportPushPublishesDependenciesBeforeCheckpoint(t *testing.T) {
	localRoot := exportStore(t, "laptop-a1b2c3", func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	peer := newFakeArtifactPeer()
	server := httptest.NewServer(peer)
	t.Cleanup(server.Close)
	transport, err := newHTTPTransport(server.URL, "", false)
	require.NoError(t, err)

	require.NoError(t, transport.Exchange(context.Background(), localRoot))

	assert.Equal(t,
		[]string{KindSegments, KindManifests, KindCheckpoints},
		peer.postedKinds(),
	)
}

func TestHTTPTransportExchangeDetectsDivergentCheckpoint(t *testing.T) {
	origin := "laptop-a1b2c3"
	localRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	checkpoints := globArtifacts(t, localRoot, origin, KindCheckpoints, "cp-*.json")
	require.Len(t, checkpoints, 1)
	name := filepath.Base(checkpoints[0])

	// The peer holds a different, equally valid checkpoint under the same
	// sequence name, as a rebuilt store under a reused origin id would.
	divergent, err := canonicalJSON(checkpoint{
		Version: formatVersion, Origin: origin, Sequence: 1,
		Sessions: map[string]string{origin + "~other": hashHex([]byte("other"))},
	})
	require.NoError(t, err)
	peer := newFakeArtifactPeer()
	peer.put(origin, KindCheckpoints, name, divergent)

	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	tr, err := newHTTPTransport(srv.URL, "", false)
	require.NoError(t, err)

	err = tr.Exchange(context.Background(), localRoot)
	require.Error(t, err)
	assert.ErrorIs(t, err, errArtifactPathConflict)
}

func TestHTTPTransportExchangeRepairsCorruptLocalCheckpoint(t *testing.T) {
	origin := "laptop-a1b2c3"
	localRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	checkpoints := globArtifacts(t, localRoot, origin, KindCheckpoints, "cp-*.json")
	require.Len(t, checkpoints, 1)
	name := filepath.Base(checkpoints[0])
	valid, err := os.ReadFile(checkpoints[0])
	require.NoError(t, err)

	peer := newFakeArtifactPeer()
	peer.put(origin, KindCheckpoints, name, valid)
	require.NoError(t, os.WriteFile(checkpoints[0], []byte("not json"), 0o644))

	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	tr, err := newHTTPTransport(srv.URL, "", false)
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localRoot))

	got, err := os.ReadFile(checkpoints[0])
	require.NoError(t, err)
	assert.Equal(t, valid, got, "corrupt local checkpoint should be re-fetched from the peer")
}

func TestHTTPTransportPostArtifactSetsPeerOrigin(t *testing.T) {
	var gotOrigin string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrigin = r.Header.Get("Origin")
		w.WriteHeader(http.StatusCreated)
	}))
	defer peer.Close()

	tr, err := newHTTPTransport(peer.URL+"/api/v1/artifacts", "", false)
	require.NoError(t, err)

	err = tr.postArtifact(context.Background(), "peer-a1b2c3", KindSegments, strings64("a"), []byte("artifact"))
	require.NoError(t, err)

	assert.Equal(t, peer.URL, gotOrigin)
}

func TestHTTPTransportRejectsRedirectedArtifactPost(t *testing.T) {
	type requestCapture struct {
		authorization string
		body          []byte
		readErr       error
	}

	var destinationReached atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationReached.Store(true)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(destination.Close)

	captured := make(chan requestCapture, 1)
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		captured <- requestCapture{
			authorization: r.Header.Get("Authorization"),
			body:          body,
			readErr:       err,
		}
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	tr, err := newHTTPTransport(source.URL, "peer-secret", false)
	require.NoError(t, err)
	tr.client.Transport = source.Client().Transport

	err = tr.postArtifact(context.Background(), "peer-a1b2c3", KindSegments, strings64("a"), []byte("artifact-secret"))
	require.Error(t, err)
	assert.ErrorIs(t, err, errHTTPPeer)
	var got requestCapture
	select {
	case got = <-captured:
	case <-time.After(time.Second):
		require.FailNow(t, "redirect source was not reached", "timed out waiting for the artifact POST")
	}
	require.NoError(t, got.readErr)
	assert.Equal(t, "Bearer peer-secret", got.authorization)
	assert.Equal(t, []byte("artifact-secret"), got.body)
	assert.False(t, destinationReached.Load())
}
