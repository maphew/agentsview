package main

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/server"
)

func TestServeRuntimeListenerBoundBeforePostListenHookReturns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Open DB: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := config.Config{
		Host:    "127.0.0.1",
		Port:    0,
		DataDir: dir,
		DBPath:  dbPath,
	}
	var prepErr error
	cfg, prepErr = prepareServeRuntimeConfig(cfg, serveRuntimeOptions{
		Mode:          "serve",
		RequestedPort: 0,
	})
	if prepErr != nil {
		t.Fatalf("prepareServeRuntimeConfig: %v", prepErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	srv := server.New(cfg, database, nil, server.WithBaseContext(ctx))

	hookStarted := make(chan struct{})
	releaseHook := make(chan struct{})
	resultCh := make(chan struct {
		rt  *serveRuntime
		err error
	}, 1)

	go func() {
		rt, err := startServerWithOptionalCaddy(ctx, cfg, srv, serveRuntimeOptions{
			Mode:          "serve",
			RequestedPort: 0,
			PostListen: func() {
				close(hookStarted)
				<-releaseHook
			},
		})
		resultCh <- struct {
			rt  *serveRuntime
			err error
		}{rt: rt, err: err}
	}()

	select {
	case <-hookStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("post-listen hook did not start")
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port)), 2*time.Second)
	if err != nil {
		t.Fatalf("listener was not reachable while hook was blocked: %v", err)
	}
	_ = conn.Close()

	close(releaseHook)
	var rt *serveRuntime
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("startServerWithOptionalCaddy: %v", result.err)
		}
		rt = result.rt
	case <-time.After(2 * time.Second):
		t.Fatal("startServerWithOptionalCaddy did not return after hook release")
	}

	cancel()
	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		t.Fatalf("waitForServerRuntime: %v", err)
	}
}
