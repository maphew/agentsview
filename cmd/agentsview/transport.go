// ABOUTME: detectTransport picks between the HTTP and direct-DB
// ABOUTME: SessionService backends based on whether a running
// ABOUTME: agentsview daemon is discoverable via its state file.
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/server"
	"github.com/wesm/agentsview/internal/service"
)

type transportMode int

const (
	transportDirect transportMode = iota
	transportHTTP
)

// transport captures how to reach the session-data layer from a
// CLI subcommand. Either the HTTP daemon (URL set) or the local
// DB (DirectReadOnly indicates whether the daemon was starting up
// and we should read without writes).
type transport struct {
	Mode           transportMode
	URL            string
	ReadOnly       bool // state-file ReadOnly flag (true for pg serve)
	DirectReadOnly bool // daemon is active but TCP probe failed; read DB only
}

// detectTransport picks the transport mode:
//  1. If a state file points to a live listening daemon, use HTTP.
//  2. If a startup lock exists, wait up to waitTimeout for the
//     daemon to become ready, then try again.
//  3. If a server is active but not yet listening, fall back to
//     read-only direct access (don't compete with the daemon for
//     write ownership).
//  4. Otherwise use full direct access.
func detectTransport(
	dataDir string, waitTimeout time.Duration,
) (transport, error) {
	if sf := server.FindRunningServer(dataDir); sf != nil {
		return transport{
			Mode:     transportHTTP,
			URL:      urlFromStateFile(sf),
			ReadOnly: sf.ReadOnly,
		}, nil
	}
	if server.IsStartupLocked(dataDir) {
		fmt.Fprintln(os.Stderr,
			"server is starting up, waiting...")
		if waitTimeout <= 0 {
			waitTimeout = startupWaitTimeout
		}
		server.WaitForStartup(dataDir, waitTimeout)
		if sf := server.FindRunningServer(dataDir); sf != nil {
			return transport{
				Mode:     transportHTTP,
				URL:      urlFromStateFile(sf),
				ReadOnly: sf.ReadOnly,
			}, nil
		}
	}
	if server.IsLocalServerActive(dataDir) {
		// A writable local daemon owns the SQLite archive but
		// its TCP probe transiently failed. Don't compete for
		// write ownership — read only via direct DB.
		return transport{
			Mode:           transportDirect,
			DirectReadOnly: true,
		}, nil
	}
	// IsServerActive is true but IsLocalServerActive is false —
	// i.e. only a pg serve (read-only) daemon is live and its
	// TCP probe failed. pg serve does not touch the local
	// SQLite archive, so direct reads AND writes are safe.
	return transport{Mode: transportDirect}, nil
}

// urlFromStateFile returns the HTTP URL a CLI client should use
// to reach the daemon described by sf. Bind-all addresses are
// mapped to loopback. IPv6 hosts are bracketed via
// net.JoinHostPort so the URL is well-formed.
func urlFromStateFile(sf *server.StateFile) string {
	host := sf.Host
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(sf.Port))
}

// newService builds the SessionService matching the detected
// transport. The returned cleanup function must be called when
// the caller is done with the service.
func newService(
	cfg config.Config, tr transport,
) (service.SessionService, func(), error) {
	switch tr.Mode {
	case transportHTTP:
		return service.NewHTTPBackend(tr.URL, cfg.AuthToken, tr.ReadOnly),
			func() {}, nil
	default:
		applyClassifierConfig(cfg)
		d, err := db.Open(cfg.DBPath)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"opening db: %w", err,
			)
		}
		cleanup := func() { d.Close() }
		if tr.DirectReadOnly {
			return service.NewReadOnlyBackend(d), cleanup, nil
		}
		// engine is nil — CLI reads don't need it, and Sync
		// is handled via the HTTP daemon when one is running.
		return service.NewDirectBackend(d, nil), cleanup, nil
	}
}
