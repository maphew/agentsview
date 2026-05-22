package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/server"
)

// freeTCPListener binds to a free loopback port and returns the
// listener (caller closes) and the port number. The listener
// stays alive so detectTransport's TCP probe succeeds.
func freeTCPListener(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	port := l.Addr().(*net.TCPAddr).Port
	return l, port
}

func TestDetectTransport_NoDaemon_ReturnsDirect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tr, err := detectTransport(dir, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
	assert.False(t, tr.ReadOnly)
	assert.False(t, tr.DirectReadOnly)
	assert.Empty(t, tr.URL)
}

func TestDetectTransport_LocalServe_ReturnsHTTPWriteCapable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, port := freeTCPListener(t)
	_, err := server.WriteStateFile(dir, "127.0.0.1", port, "test", false)
	require.NoError(t, err)
	t.Cleanup(func() { server.RemoveStateFile(dir, port) })

	tr, err := detectTransport(dir, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.False(t, tr.ReadOnly)
	assert.Contains(t, tr.URL, "http://127.0.0.1:"+strconv.Itoa(port))
}

func TestDetectTransport_PGServe_ReturnsReadOnlyHTTP(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, port := freeTCPListener(t)
	_, err := server.WriteStateFile(dir, "127.0.0.1", port, "test", true)
	require.NoError(t, err)
	t.Cleanup(func() { server.RemoveStateFile(dir, port) })

	tr, err := detectTransport(dir, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.True(t, tr.ReadOnly)
	assert.Contains(t, tr.URL, "http://127.0.0.1:"+strconv.Itoa(port))
}

// TestDetectTransport_PrefersWritableOverPGServe verifies that when
// both a writable local daemon and a read-only pg serve daemon
// advertise the same data dir, the CLI picks the writable one so
// sync/write operations don't silently land on a read-only target.
func TestDetectTransport_PrefersWritableOverPGServe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, writablePort := freeTCPListener(t)
	_, readOnlyPort := freeTCPListener(t)

	// Write state files out of order so the winner isn't just
	// whatever the directory scan happens to see first.
	_, err := server.WriteStateFile(
		dir, "127.0.0.1", readOnlyPort, "test", true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { server.RemoveStateFile(dir, readOnlyPort) })

	_, err = server.WriteStateFile(
		dir, "127.0.0.1", writablePort, "test", false,
	)
	require.NoError(t, err)
	t.Cleanup(func() { server.RemoveStateFile(dir, writablePort) })

	tr, err := detectTransport(dir, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.False(t, tr.ReadOnly,
		"expected writable daemon to win over pg serve")
	assert.Contains(t, tr.URL,
		"http://127.0.0.1:"+strconv.Itoa(writablePort),
		"expected URL to point at the writable daemon")
}

// TestDetectTransport_PGServeUnreachable_AllowsDirectWrite verifies
// that a live-but-unreachable pg serve state file does NOT force
// the CLI into DirectReadOnly. pg serve never touches the local
// SQLite archive, so direct writes (session sync) are safe even
// when its TCP probe fails. Gating on IsLocalServerActive ensures
// only a writable local daemon triggers the read-only fallback.
func TestDetectTransport_PGServeUnreachable_AllowsDirectWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Pick a free port and immediately release it so the TCP
	// probe fails — the state file still has a live PID.
	ln, port := freeTCPListener(t)
	ln.Close()
	_, err := server.WriteStateFile(
		dir, "127.0.0.1", port, "test", true, // readOnly = pg serve
	)
	require.NoError(t, err)
	t.Cleanup(func() { server.RemoveStateFile(dir, port) })

	tr, err := detectTransport(dir, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
	assert.False(t, tr.DirectReadOnly,
		"unreachable pg serve must not gate direct writes")
}

// TestDetectTransport_LocalDaemonUnreachable_SetsDirectReadOnly
// verifies that a live-but-unreachable *writable* local daemon
// still forces DirectReadOnly so the CLI doesn't race the daemon
// for SQLite write ownership.
func TestDetectTransport_LocalDaemonUnreachable_SetsDirectReadOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ln, port := freeTCPListener(t)
	ln.Close()
	_, err := server.WriteStateFile(
		dir, "127.0.0.1", port, "test", false, // writable local
	)
	require.NoError(t, err)
	t.Cleanup(func() { server.RemoveStateFile(dir, port) })

	tr, err := detectTransport(dir, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
	assert.True(t, tr.DirectReadOnly,
		"unreachable writable daemon must gate direct writes")
}

// TestDetectTransport_StartupLocked simulates a server that's
// starting up (lock file present, no state file, no listener).
// Our current PID is alive, so isServerStarting returns true.
// The helper waits out the timeout then falls back to direct.
func TestDetectTransport_StartupLocked_FallsBackToDirect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create a lock file referencing our own PID.
	pid := os.Getpid()
	lockPath := filepath.Join(dir,
		"server.starting."+strconv.Itoa(pid))
	require.NoError(t, os.WriteFile(
		lockPath, []byte(strconv.Itoa(pid)), 0o644,
	))
	t.Cleanup(func() { os.Remove(lockPath) })

	tr, err := detectTransport(dir, 100*time.Millisecond)
	require.NoError(t, err)
	// Still no state file after wait, so IsServerActive sees
	// only the startup lock and returns direct (writable) since
	// no state file means no daemon claim.
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

// TestNewService_DirectReadOnly verifies that the DirectReadOnly
// branch opens the DB and returns a read-only service.
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

func TestUrlFromStateFile_BindAllMapsToLoopback(t *testing.T) {
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
			got := urlFromStateFile(&server.StateFile{
				Host: tc.host,
				Port: 8080,
			})
			assert.Equal(t, tc.want, got)
		})
	}
}
