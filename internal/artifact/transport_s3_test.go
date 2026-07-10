package artifact

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// mockS3 is an in-memory, path-style S3-compatible server backing a single
// bucket. It implements just enough of ListObjectsV2, GetObject, PutObject, and
// DeleteObject to exercise the object-store transport, and verifies that
// requests arrive signed (Authorization plus x-amz-date) without re-validating
// the signature.
type mockS3 struct {
	t        *testing.T
	bucket   string
	pageSize int

	mu      sync.Mutex
	objects map[string][]byte
	deletes int
}

func newMockS3(t *testing.T, bucket string, pageSize int) *mockS3 {
	return &mockS3{
		t:        t,
		bucket:   bucket,
		pageSize: pageSize,
		objects:  map[string][]byte{},
	}
}

func (m *mockS3) put(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), data...)
}

func (m *mockS3) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

func (m *mockS3) deleteCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deletes
}

func (m *mockS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	assert.True(m.t, strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256"),
		"request must carry a SigV4 Authorization header")
	assert.NotEmpty(m.t, r.Header.Get("X-Amz-Date"), "request must carry an x-amz-date header")

	bucketPath := "/" + m.bucket
	if r.Method == http.MethodGet && r.URL.Path == bucketPath && r.URL.Query().Get("list-type") == "2" {
		m.list(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, bucketPath+"/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, bucketPath+"/")
	switch r.Method {
	case http.MethodGet:
		m.mu.Lock()
		data, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			http.Error(w, "no such key", http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	case http.MethodPut:
		body := make([]byte, 0)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		// Honor the write-once conditional: reject when the key already exists.
		if r.Header.Get("If-None-Match") == "*" && m.has(key) {
			http.Error(w, "precondition failed", http.StatusPreconditionFailed)
			return
		}
		m.put(key, body)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		m.mu.Lock()
		m.deletes++
		delete(m.objects, key)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (m *mockS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	token := r.URL.Query().Get("continuation-token")

	m.mu.Lock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()
	sort.Strings(keys)

	start := 0
	if token != "" {
		start, _ = strconv.Atoi(token)
	}
	pageSize := m.pageSize
	if pageSize <= 0 {
		pageSize = 1000
	}
	end := start + pageSize
	truncated := end < len(keys)
	if end > len(keys) {
		end = len(keys)
	}

	type contentsXML struct {
		Key string `xml:"Key"`
	}
	type resultXML struct {
		XMLName               xml.Name      `xml:"ListBucketResult"`
		IsTruncated           bool          `xml:"IsTruncated"`
		Contents              []contentsXML `xml:"Contents"`
		NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
	}
	out := resultXML{IsTruncated: truncated}
	for _, k := range keys[start:end] {
		out.Contents = append(out.Contents, contentsXML{Key: k})
	}
	if truncated {
		out.NextContinuationToken = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/xml")
	require.NoError(m.t, xml.NewEncoder(w).Encode(out))
}

func testObjectOptions(endpoint string) ObjectStoreOptions {
	return ObjectStoreOptions{
		Endpoint:        endpoint,
		Region:          "us-east-1",
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		PathStyle:       true,
	}
}

func TestS3TransportPrepareHonorsCanceledSync(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("<ListBucketResult></ListBucketResult>"))
	}))
	t.Cleanup(server.Close)
	transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = syncWithTransport(ctx, testDB(t), SyncOptions{
		DataDir: t.TempDir(),
		Target:  "s3://bucket/arts",
		Origin:  "laptop-a1b2c3",
	}, transport)

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, requests.Load(), "canceled preparation must not contact the object store")
}

// exportStore exports one origin's sessions into a fresh artifact store and
// returns the artifact root.
func exportStore(t *testing.T, origin string, seed func(*db.DB)) string {
	t.Helper()
	database := testDB(t)
	seed(database)
	root := filepath.Join(t.TempDir(), "artifacts")
	_, err := Export(context.Background(), database, root, origin)
	require.NoError(t, err)
	return root
}

func TestS3TransportPushRoundTrip(t *testing.T) {
	origin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	dataDir := t.TempDir()
	localRoot := filepath.Join(dataDir, "artifacts")
	_, err := Export(context.Background(), database, localRoot, origin)
	require.NoError(t, err)

	// Append a metadata event so a meta artifact is part of the push.
	rec := NewMetadataRecorder(database, MetadataRecorderOptions{
		DataDir: dataDir,
		Origin:  origin,
		Now:     func() time.Time { return fixedHLCTime() },
	})
	_, err = database.StarSession("sess-1")
	require.NoError(t, err)
	_, err = rec.Append(context.Background(), MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpStar,
	})
	require.NoError(t, err)

	mock := newMockS3(t, "bucket", 0)
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Prepare(context.Background(), localRoot))
	require.NoError(t, tr.Exchange(context.Background(), localRoot))

	idx, err := ListArtifacts(localRoot, origin)
	require.NoError(t, err)
	items := indexItems(idx)
	require.NotEmpty(t, items)
	assert.NotEmpty(t, idx.Meta, "the star event should have produced a meta artifact")
	for _, item := range items {
		key := "arts/" + origin + "/" + item.kind + "/" + item.name
		assert.True(t, mock.has(key), "expected object %q in bucket", key)
	}
}

func TestS3TransportPullRoundTrip(t *testing.T) {
	origin := "desktop-d4e5f6"

	// Produce a populated store for the origin and upload it into the bucket so
	// the transport must pull it down into an empty local store.
	remoteRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-7", "beta")
		seedSession(t, database, "sess-8", "beta")
	})
	remoteIdx, err := ListArtifacts(remoteRoot, origin)
	require.NoError(t, err)
	uploaded := indexItems(remoteIdx)
	require.NotEmpty(t, uploaded)

	// Use a small page size and more than one object to exercise the
	// continuation-token pagination path.
	mock := newMockS3(t, "bucket", 2)
	for _, item := range uploaded {
		art, err := ReadArtifact(remoteRoot, origin, item.kind, item.name)
		require.NoError(t, err)
		mock.put("arts/"+origin+"/"+item.kind+"/"+item.name, art.Data)
	}
	require.Greater(t, len(uploaded), 2, "need multiple pages to test pagination")

	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	localRoot := filepath.Join(t.TempDir(), "artifacts")
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localRoot))

	gotIdx, err := ListArtifacts(localRoot, origin)
	require.NoError(t, err)
	assert.ElementsMatch(t, indexItems(remoteIdx), indexItems(gotIdx))
}

func TestS3TransportPullRetainsCorruptRemoteArtifactOverHTTP(t *testing.T) {
	origin := "desktop-d4e5f6"
	remoteRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-7", "beta")
	})
	remoteIdx, err := ListArtifacts(remoteRoot, origin)
	require.NoError(t, err)
	uploaded := indexItems(remoteIdx)
	mock := newMockS3(t, "bucket", 0)
	for _, item := range uploaded {
		art, err := ReadArtifact(remoteRoot, origin, item.kind, item.name)
		require.NoError(t, err)
		mock.put("arts/"+origin+"/"+item.kind+"/"+item.name, art.Data)
	}
	corruptName := hashHex([]byte("corrupt")) + segmentExtension
	mock.put("arts/"+origin+"/segments/"+corruptName, []byte("garbage"))

	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	localRoot := filepath.Join(t.TempDir(), "artifacts")
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localRoot))

	gotIdx, err := ListArtifacts(localRoot, origin)
	require.NoError(t, err)
	assert.ElementsMatch(t, uploaded, indexItems(gotIdx))
	assert.NoFileExists(t, filepath.Join(localRoot, origin, KindSegments, corruptName))
	assert.True(t, mock.has("arts/"+origin+"/segments/"+corruptName))
	assert.Zero(t, mock.deleteCount())
}

func TestS3TransportPullDeletesCorruptRemoteObjectSoPushHeals(t *testing.T) {
	origin := "desktop-d4e5f6"
	ownerRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-7", "beta")
	})
	ownerIdx, err := ListArtifacts(ownerRoot, origin)
	require.NoError(t, err)
	require.Len(t, ownerIdx.Segments, 1)
	segKey := "arts/" + origin + "/segments/" + ownerIdx.Segments[0]

	mock := newMockS3(t, "bucket", 0)
	for _, item := range indexItems(ownerIdx) {
		art, err := ReadArtifact(ownerRoot, origin, item.kind, item.name)
		require.NoError(t, err)
		mock.put("arts/"+origin+"/"+item.kind+"/"+item.name, art.Data)
	}
	// The bucket copy is corrupted in place: its name still lists, so pushes
	// from valid holders would otherwise skip it forever.
	mock.put(segKey, []byte("garbage"))

	srv := httptest.NewTLSServer(mock)
	t.Cleanup(srv.Close)

	// An empty peer's pull fails to validate the object and deletes it, so
	// the name stops masking the valid copy.
	emptyRoot := filepath.Join(t.TempDir(), "artifacts")
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	tr.client.Transport = srv.Client().Transport
	require.NoError(t, tr.Exchange(context.Background(), emptyRoot))
	assert.False(t, mock.has(segKey), "corrupt object deleted from the bucket")
	assert.Equal(t, 1, mock.deleteCount(), "expected one DELETE for the corrupt object")

	// The owner's next exchange re-uploads its valid copy, and the empty
	// peer's next pull completes its store.
	ownerTr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	ownerTr.client.Transport = srv.Client().Transport
	require.NoError(t, ownerTr.Exchange(context.Background(), ownerRoot))
	assert.True(t, mock.has(segKey), "valid copy re-uploaded")

	require.NoError(t, tr.Exchange(context.Background(), emptyRoot))
	gotIdx, err := ListArtifacts(emptyRoot, origin)
	require.NoError(t, err)
	assert.ElementsMatch(t, indexItems(ownerIdx), indexItems(gotIdx))
}

func TestS3TransportRejectsRedirect(t *testing.T) {
	var sourceReached atomic.Bool
	var destinationReached atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationReached.Store(true)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>"))
	}))
	t.Cleanup(destination.Close)

	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceReached.Store(true)
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(source.URL))
	require.NoError(t, err)
	tr.client.Transport = source.Client().Transport

	_, err = tr.listPage(context.Background(), "", 0)
	require.Error(t, err)
	assert.True(t, sourceReached.Load())
	assert.False(t, destinationReached.Load())
}

func TestS3TransportPushSkipsAndQuarantinesCorruptLocalArtifact(t *testing.T) {
	origin := "laptop-a1b2c3"
	localRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	validIdx, err := ListArtifacts(localRoot, origin)
	require.NoError(t, err)
	corruptName := hashHex([]byte("junk")) + segmentExtension
	corruptPath := filepath.Join(localRoot, origin, KindSegments, corruptName)
	require.NoError(t, os.WriteFile(corruptPath, []byte("garbage"), 0o644))

	mock := newMockS3(t, "bucket", 0)
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localRoot))

	for _, item := range indexItems(validIdx) {
		assert.True(t, mock.has("arts/"+origin+"/"+item.kind+"/"+item.name),
			"expected object %s/%s in bucket", item.kind, item.name)
	}
	assert.False(t, mock.has("arts/"+origin+"/segments/"+corruptName))
	assert.NoFileExists(t, corruptPath)
	assert.FileExists(t, corruptPath+quarantineSuffix)
}

func TestS3TransportExchangeDetectsDivergentCheckpoint(t *testing.T) {
	origin := "laptop-a1b2c3"
	localRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	checkpoints := globArtifacts(t, localRoot, origin, KindCheckpoints, "cp-*.json")
	require.Len(t, checkpoints, 1)
	name := filepath.Base(checkpoints[0])

	divergent, err := canonicalJSON(checkpoint{
		Version: formatVersion, Origin: origin, Sequence: 1,
		Sessions: map[string]string{origin + "~other": hashHex([]byte("other"))},
	})
	require.NoError(t, err)
	mock := newMockS3(t, "bucket", 0)
	mock.put("arts/"+origin+"/"+KindCheckpoints+"/"+name, divergent)

	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)

	err = tr.Exchange(context.Background(), localRoot)
	require.Error(t, err)
	assert.ErrorIs(t, err, errArtifactPathConflict)
}

func TestS3TransportExchangeRepairsCorruptLocalCheckpoint(t *testing.T) {
	origin := "laptop-a1b2c3"
	localRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	checkpoints := globArtifacts(t, localRoot, origin, KindCheckpoints, "cp-*.json")
	require.Len(t, checkpoints, 1)
	name := filepath.Base(checkpoints[0])
	valid, err := os.ReadFile(checkpoints[0])
	require.NoError(t, err)

	mock := newMockS3(t, "bucket", 0)
	mock.put("arts/"+origin+"/"+KindCheckpoints+"/"+name, valid)
	require.NoError(t, os.WriteFile(checkpoints[0], []byte("not json"), 0o644))

	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localRoot))

	got, err := os.ReadFile(checkpoints[0])
	require.NoError(t, err)
	assert.Equal(t, valid, got, "corrupt local checkpoint should be re-fetched from the bucket")
}

func TestS3TransportWriteOnceRejectsDivergentContent(t *testing.T) {
	mock := newMockS3(t, "bucket", 0)
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	ctx := context.Background()
	key := "arts/laptop-a1b2c3/raw/deadbeef"

	// First write creates the object.
	require.NoError(t, tr.putObject(ctx, key, []byte("one")))
	// An identical re-write is an accepted duplicate, not an error.
	require.NoError(t, tr.putObject(ctx, key, []byte("one")))
	// Divergent content at the same key is a conflict, never a silent overwrite.
	err = tr.putObject(ctx, key, []byte("two"))
	require.Error(t, err)
	assert.ErrorIs(t, err, errObjectStore)
	// The original content is preserved.
	got, err := tr.getObject(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("one"), got)
}

func TestIsObjectTarget(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   bool
	}{
		{"s3 url", "s3://bucket/prefix", true},
		{"s3 bucket only", "s3://bucket", true},
		{"http peer", "http://example.com", false},
		{"https peer", "https://example.com", false},
		{"folder path", "/var/data/share", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsObjectTarget(tt.target))
		})
	}
}

func TestNewObjectTransport(t *testing.T) {
	creds := ObjectStoreOptions{
		Region:          "us-east-1",
		AccessKeyID:     "AK",
		SecretAccessKey: "SK",
	}

	t.Run("missing bucket", func(t *testing.T) {
		_, err := newObjectTransport("s3://", creds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bucket")
	})

	t.Run("missing credentials", func(t *testing.T) {
		_, err := newObjectTransport("s3://bucket/prefix", ObjectStoreOptions{Region: "us-east-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "AWS_ACCESS_KEY_ID")
	})

	t.Run("not an object target", func(t *testing.T) {
		_, err := newObjectTransport("https://example.com", creds)
		require.Error(t, err)
	})

	t.Run("parses bucket and prefix", func(t *testing.T) {
		tr, err := newObjectTransport("s3://bucket/some/prefix/", creds)
		require.NoError(t, err)
		assert.Equal(t, "bucket", tr.bucket)
		assert.Equal(t, "some/prefix", tr.prefix)
		assert.Equal(t, "s3.us-east-1.amazonaws.com", tr.endpoint.Host)
		assert.False(t, tr.pathStyle, "real AWS defaults to virtual-host addressing")
	})

	t.Run("custom endpoint forces path style", func(t *testing.T) {
		tr, err := newObjectTransport("s3://bucket", ObjectStoreOptions{
			Endpoint:        "http://localhost:9000",
			Region:          "us-east-1",
			AccessKeyID:     "AK",
			SecretAccessKey: "SK",
		})
		require.NoError(t, err)
		assert.True(t, tr.pathStyle)
		assert.Equal(t, "localhost:9000", tr.endpoint.Host)
		assert.Empty(t, tr.prefix)
	})

	t.Run("rejects insecure remote endpoint by default", func(t *testing.T) {
		options := creds
		options.Endpoint = "http://minio.lan:9000"

		_, err := newObjectTransport("s3://bucket", options)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "insecure S3 endpoint")
		assert.Contains(t, err.Error(), "AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT")
	})

	t.Run("allows opted-in insecure remote endpoint", func(t *testing.T) {
		options := creds
		options.Endpoint = "http://minio.lan:9000"
		options.AllowInsecureEndpoint = true

		tr, err := newObjectTransport("s3://bucket", options)
		require.NoError(t, err)
		assert.Equal(t, "http", tr.endpoint.Scheme)
	})

	for _, endpoint := range []string{
		"http://localhost:9000",
		"http://LOCALHOST:9000",
		"http://127.0.0.1:9000",
		"http://[::1]:9000",
	} {
		t.Run("allows loopback endpoint "+endpoint, func(t *testing.T) {
			options := creds
			options.Endpoint = endpoint

			tr, err := newObjectTransport("s3://bucket", options)
			require.NoError(t, err)
			assert.Equal(t, "http", tr.endpoint.Scheme)
		})
	}

	t.Run("bare host defaults to HTTPS", func(t *testing.T) {
		options := creds
		options.Endpoint = "minio.lan:9000"

		tr, err := newObjectTransport("s3://bucket", options)
		require.NoError(t, err)
		assert.Equal(t, "https", tr.endpoint.Scheme)
	})

	t.Run("rejects unsupported endpoint scheme", func(t *testing.T) {
		options := creds
		options.Endpoint = "ftp://minio.lan"

		_, err := newObjectTransport("s3://bucket", options)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ftp")
	})
}

func TestObjectStoreOptionsFromEnvAllowsInsecureEndpoint(t *testing.T) {
	for _, value := range []string{"1", "true", "yes", "YES"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", "AK")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "SK")
			t.Setenv("AGENTSVIEW_S3_ENDPOINT", "http://minio.lan:9000")
			t.Setenv("AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT", value)

			tr, err := newObjectTransport("s3://bucket", ObjectStoreOptionsFromEnv())
			require.NoError(t, err)
			assert.Equal(t, "http", tr.endpoint.Scheme)
		})
	}
}

func TestObjectStoreOptionsFromEnvRejectsInvalidInsecureEndpointOverride(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "zero", value: "0"},
		{name: "false", value: "false"},
		{name: "no", value: "no"},
		{name: "typo", value: "treu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", "AK")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "SK")
			t.Setenv("AGENTSVIEW_S3_ENDPOINT", "http://minio.lan:9000")
			t.Setenv("AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT", tt.value)

			_, err := newObjectTransport("s3://bucket", ObjectStoreOptionsFromEnv())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "insecure S3 endpoint")
			assert.Contains(t, err.Error(), "AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT")
		})
	}
}
