//go:build sshtest

package ssh

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

func TestSSHSyncEndToEnd(t *testing.T) {
	host := testSSHHost(t)
	port := testSSHPort(t)
	user := testSSHUser(t)
	opts := testSSHOpts(t)
	database := testDB(t)

	rs := &RemoteSync{
		Host:    host,
		User:    user,
		Port:    port,
		Full:    true,
		DB:      database,
		SSHOpts: opts,
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()

	stats, err := rs.Run(ctx)
	if err != nil {
		t.Fatalf("remote sync: %v", err)
	}

	if stats.SessionsSynced == 0 {
		t.Fatal("expected at least 1 session synced")
	}

	// Verify session landed in DB.
	page, err := database.ListSessions(
		context.Background(), db.SessionFilter{Limit: 100},
	)
	if err != nil {
		t.Fatalf("listing sessions: %v", err)
	}
	if len(page.Sessions) == 0 {
		t.Fatal("no sessions in database")
	}

	// Session ID should carry the host prefix.
	found := false
	for _, s := range page.Sessions {
		if s.Machine == host {
			found = true
			if !strings.HasPrefix(s.ID, host+"~") {
				t.Errorf(
					"session ID %q missing host prefix",
					s.ID,
				)
			}
			break
		}
	}
	if !found {
		t.Errorf("no session with machine=%q", host)
	}
}

func TestSSHSyncIncremental(t *testing.T) {
	host := testSSHHost(t)
	port := testSSHPort(t)
	user := testSSHUser(t)
	opts := testSSHOpts(t)
	database := testDB(t)

	rs := &RemoteSync{
		Host:    host,
		User:    user,
		Port:    port,
		Full:    false,
		DB:      database,
		SSHOpts: opts,
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()

	// First sync: should pull sessions.
	stats1, err := rs.Run(ctx)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if stats1.SessionsSynced == 0 {
		t.Fatal("first sync: expected sessions")
	}

	// Second sync: nothing changed, should skip all.
	stats2, err := rs.Run(ctx)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if stats2.SessionsSynced != 0 {
		t.Errorf(
			"second sync: expected 0 synced, got %d",
			stats2.SessionsSynced,
		)
	}
	if stats2.Skipped == 0 {
		t.Error("second sync: expected skipped > 0")
	}
}

func TestSSHSyncFull(t *testing.T) {
	host := testSSHHost(t)
	port := testSSHPort(t)
	user := testSSHUser(t)
	opts := testSSHOpts(t)
	database := testDB(t)

	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()

	// Incremental first.
	rs := &RemoteSync{
		Host:    host,
		User:    user,
		Port:    port,
		Full:    false,
		DB:      database,
		SSHOpts: opts,
	}
	_, err := rs.Run(ctx)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Full flag clears the remote skip cache but the engine
	// still skips unchanged sessions via DB lookup. Verify
	// it completes without error.
	rs.Full = true
	stats, err := rs.Run(ctx)
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	// Session was already synced and unchanged, so it may
	// be skipped by the engine's own DB-based detection.
	if stats.SessionsSynced+stats.Skipped == 0 {
		t.Fatal("full sync: expected sessions processed")
	}
}
