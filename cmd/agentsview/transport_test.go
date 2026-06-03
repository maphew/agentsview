package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/daemon"
)

func daemonRuntimeDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "runtime")
}

// freeTCPListener binds to a free loopback port and returns the
// listener (caller closes) and the port number. Tests that need
// an unreachable daemon close the listener after reserving the port.
func freeTCPListener(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	port := l.Addr().(*net.TCPAddr).Port
	return l, port
}

func freeHTTPDaemon(t *testing.T) (host string, port int) {
	t.Helper()
	ts := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: "agentsview",
		Version: "test",
	}))
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err = strconv.Atoi(portText)
	require.NoError(t, err)
	return host, port
}

func freeAuthenticatedHTTPDaemon(
	t *testing.T, token string,
) (host string, port int) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(daemon.PingInfo{
			OK:      true,
			Service: "agentsview",
			PID:     os.Getpid(),
		}))
	}))
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err = strconv.Atoi(portText)
	require.NoError(t, err)
	return host, port
}

func TestDetectTransport_NoDaemon_ReturnsDirect(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
	assert.False(t, tr.ReadOnly)
	assert.Empty(t, tr.URL)
}

func TestDetectTransport_LocalServe_ReturnsHTTPWriteCapable(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	host, port := freeHTTPDaemon(t)
	_, err := WriteDaemonRuntime(dir, host, port, "test", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.False(t, tr.ReadOnly)
	assert.Contains(t, tr.URL, "http://127.0.0.1:"+strconv.Itoa(port))
}

func TestDetectTransport_PGServe_ReturnsReadOnlyHTTP(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	host, port := freeHTTPDaemon(t)
	_, err := WriteDaemonRuntime(dir, host, port, "test", true)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.True(t, tr.ReadOnly)
	assert.Contains(t, tr.URL, "http://127.0.0.1:"+strconv.Itoa(port))
}

func TestDetectTransport_AuthenticatedDaemonUsesBearerToken(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	host, port := freeAuthenticatedHTTPDaemon(t, "secret")
	_, err := WriteDaemonRuntime(dir, host, port, "test", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	tr, err := detectTransport(dir, "secret", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.False(t, tr.ReadOnly)
	assert.Contains(t, tr.URL, "http://127.0.0.1:"+strconv.Itoa(port))
}

// TestDetectTransport_LocalServeWritableRecordWins verifies that a
// writable kit runtime record is exposed as a write-capable HTTP transport.
func TestDetectTransport_LocalServeWritableRecordWins(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	host, writablePort := freeHTTPDaemon(t)
	_, err := WriteDaemonRuntime(
		dir, host, writablePort, "test", false,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.False(t, tr.ReadOnly)
	assert.Contains(t, tr.URL,
		"http://127.0.0.1:"+strconv.Itoa(writablePort),
		"expected URL to point at the writable daemon")
}

// TestDetectTransport_PGServeUnreachable_AllowsDirectWrite verifies
// that an unprobeable pg serve runtime record does not prove daemon
// ownership, so direct access remains available.
func TestDetectTransport_PGServeUnreachable_AllowsDirectWrite(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	// Pick a free port and immediately release it so the TCP
	// probe fails — the runtime record still has a live PID.
	ln, port := freeTCPListener(t)
	ln.Close()
	_, err := WriteDaemonRuntime(
		dir, "127.0.0.1", port, "test", true, // readOnly = pg serve
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
}

// TestDetectTransport_LocalDaemonUnreachable_SetsDirectReadOnly verifies
// that a writable runtime record suppresses direct writes even when
// the daemon ping is temporarily unavailable.
func TestDetectTransport_LocalDaemonUnreachable_SetsDirectReadOnly(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	ln, port := freeTCPListener(t)
	ln.Close()
	_, err := WriteDaemonRuntime(
		dir, "127.0.0.1", port, "test", false, // writable local
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
	assert.True(t, tr.DirectReadOnly)
}

// TestDetectTransport_DaemonStarting simulates a server that's
// starting up (start lock held, no runtime record, no listener).
// The held kit lock makes IsDaemonStarting return true.
// The helper waits out the timeout then falls back to direct.
func TestDetectTransport_DaemonStarting_FallsBackToDirect(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	// Still no runtime record after wait, so IsDaemonActive sees
	// only the start lock and returns direct (writable) since
	// no runtime record means no daemon claim.
	assert.Equal(t, transportDirect, tr.Mode)
}

// TestNewService_HTTPMode verifies that newService returns a
// working HTTP-backed service and a cleanup function when the
// transport is HTTP mode. No DB is opened in this path.
func TestNewService_HTTPMode(t *testing.T) {
	t.Parallel()
	tr := transport{
		Mode: transportHTTP,
		URL:  "http://127.0.0.1:8080",
	}
	svc, cleanup, err := newService(config.Config{}, tr)
	require.NoError(t, err)
	require.NotNil(t, svc)
	require.NotNil(t, cleanup)
	cleanup()
}

// TestNewService_DirectMode verifies that newService opens the
// local SQLite DB and returns a direct-backed service when the
// transport is direct mode. The cleanup function must close the
// DB.
func TestNewService_DirectMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.Config{DBPath: filepath.Join(dir, "sessions.db")}

	svc, cleanup, err := newService(cfg, transport{Mode: transportDirect})
	require.NoError(t, err)
	require.NotNil(t, svc)
	require.NotNil(t, cleanup)
	cleanup()
}

// TestNewService_DirectReadOnly verifies that the DirectReadOnly branch
// opens the DB and returns a read-only service.
func TestNewService_DirectReadOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.Config{DBPath: filepath.Join(dir, "sessions.db")}

	svc, cleanup, err := newService(cfg, transport{
		Mode:           transportDirect,
		DirectReadOnly: true,
	})
	require.NoError(t, err)
	require.NotNil(t, svc)
	require.NotNil(t, cleanup)
	cleanup()
}

func TestUrlFromDaemonRuntime_BindAllMapsToLoopback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		host string
		want string
	}{
		{"", "http://127.0.0.1:8080"},
		{"0.0.0.0", "http://127.0.0.1:8080"},
		{"::", "http://[::1]:8080"},
		{"192.168.1.10", "http://192.168.1.10:8080"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			got := urlFromDaemonRuntime(&DaemonRuntime{
				Host: tc.host,
				Port: 8080,
			})
			assert.Equal(t, tc.want, got)
		})
	}
}
