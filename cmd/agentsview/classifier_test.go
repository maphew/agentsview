package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
)

// classifierTestEnv prepares a temp data dir and writes a
// minimal config.toml with the given user prefixes.
func classifierTestEnv(t *testing.T, prefixes []string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_VIEWER_DATA_DIR", dir)

	tomlBuf := &bytes.Buffer{}
	tomlBuf.WriteString("[automated]\nprefixes = [")
	for i, p := range prefixes {
		if i > 0 {
			tomlBuf.WriteString(", ")
		}
		tomlBuf.WriteString("\"" + p + "\"")
	}
	tomlBuf.WriteString("]\n")
	if err := os.WriteFile(
		filepath.Join(dir, "config.toml"),
		tomlBuf.Bytes(), 0o600,
	); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
	return dir
}

// seedHash opens the DB at cfg.DBPath, runs the backfill so
// a hash gets stored, then closes.
func seedHash(t *testing.T, cfg config.Config) {
	t.Helper()
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	// Opening already runs backfill; the hash is now stored.
	_ = d
}

// readStoredHash returns the stored classifier hash from the
// stats table via a raw SQLite connection. Bypasses db.Open
// because db.Open runs the backfill, which would re-write
// the hash that this helper exists to observe (e.g. after
// runClassifierRebuild deletes it).
func readStoredHash(t *testing.T, dbPath string) string {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer conn.Close()
	var v string
	err = conn.QueryRow(
		`SELECT value FROM stats WHERE key = ?`,
		"is_automated_classifier_hash",
	).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("query stats: %v", err)
	}
	return v
}

func TestClassifierRebuildClearsSQLiteHash(t *testing.T) {
	dir := classifierTestEnv(t, []string{"You are analyzing an essay"})
	cfg, err := config.LoadMinimal()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	applyClassifierConfig(cfg)
	seedHash(t, cfg)
	if got := readStoredHash(t, cfg.DBPath); got == "" {
		t.Fatalf("precondition: expected stored hash, got empty")
	}

	if err := runClassifierRebuild(
		context.Background(), cfg, &bytes.Buffer{},
	); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if got := readStoredHash(t, cfg.DBPath); got != "" {
		t.Errorf("expected hash cleared, got %q", got)
	}
}

func TestClassifierRebuildPrintsLoadedPrefixes(t *testing.T) {
	prefixes := []string{
		"You are analyzing an essay",
		"You are grading quotes",
	}
	dir := classifierTestEnv(t, prefixes)
	cfg, err := config.LoadMinimal()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	applyClassifierConfig(cfg)
	seedHash(t, cfg)

	out := &bytes.Buffer{}
	if err := runClassifierRebuild(
		context.Background(), cfg, out,
	); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	got := out.String()
	for _, p := range prefixes {
		if !strings.Contains(got, p) {
			t.Errorf("output missing %q:\n%s", p, got)
		}
	}
	if !strings.Contains(got, "loaded 2 user automation prefix") {
		t.Errorf("output missing count line:\n%s", got)
	}
	if !strings.Contains(got, "restart") {
		t.Errorf("output missing restart reminder:\n%s", got)
	}
}

func TestClassifierRebuildRefusesOnHTTPTransport(t *testing.T) {
	dir := classifierTestEnv(t, nil)
	cfg, err := config.LoadMinimal()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DBPath = filepath.Join(dir, "sessions.db")

	tr := transport{Mode: transportHTTP, URL: "http://127.0.0.1:8080"}
	err = guardClassifierRebuild(tr)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Errorf("error should mention daemon, got: %v", err)
	}
}

func TestClassifierRebuildRefusesOnDirectReadOnly(t *testing.T) {
	tr := transport{Mode: transportDirect, DirectReadOnly: true}
	err := guardClassifierRebuild(tr)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Errorf("error should mention daemon, got: %v", err)
	}
}

func TestClassifierRebuildAllowsDirectWritable(t *testing.T) {
	tr := transport{Mode: transportDirect, DirectReadOnly: false}
	if err := guardClassifierRebuild(tr); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestClassifierRebuildHardFailsOnPGUnreachable confirms
// that when PG is configured (pg.url non-empty) and the
// connection fails, runClassifierRebuild returns an error
// instead of silently skipping the PG delete.
func TestClassifierRebuildHardFailsOnPGUnreachable(t *testing.T) {
	dir := classifierTestEnv(t, nil)
	cfg, err := config.LoadMinimal()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	// Point at a deliberately-unreachable PG URL. Use port 1
	// (commonly closed) so Open returns quickly without
	// blocking the test.
	cfg.PG.URL = "postgres://nobody:nobody@127.0.0.1:1/nonexistent?sslmode=disable&connect_timeout=2"
	cfg.PG.AllowInsecure = true
	applyClassifierConfig(cfg)
	seedHash(t, cfg)

	err = runClassifierRebuild(
		context.Background(), cfg, &bytes.Buffer{},
	)
	if err == nil {
		t.Fatal("expected error for unreachable PG, got nil")
	}
	if !strings.Contains(err.Error(), "PG") &&
		!strings.Contains(err.Error(), "pg") {
		t.Errorf("error should mention PG, got: %v", err)
	}
	// Lock the spec contract: the error must surface the
	// 'pg push --full' remediation hint so a future refactor
	// can't silently drop it.
	if !strings.Contains(err.Error(), "pg push --full") {
		t.Errorf(
			"error should mention 'pg push --full' "+
				"remediation, got: %v",
			err,
		)
	}
}

// TestClassifierRebuildSkipsPGWhenNotConfigured verifies the
// silent-skip path: when pg.url is empty, the command does
// NOT attempt PG cleanup and returns nil even if PG would
// otherwise be unreachable.
func TestClassifierRebuildSkipsPGWhenNotConfigured(t *testing.T) {
	dir := classifierTestEnv(t, nil)
	cfg, err := config.LoadMinimal()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.DBPath = filepath.Join(dir, "sessions.db")
	cfg.PG.URL = ""
	applyClassifierConfig(cfg)
	seedHash(t, cfg)

	if err := runClassifierRebuild(
		context.Background(), cfg, &bytes.Buffer{},
	); err != nil {
		t.Fatalf("unexpected error when PG unconfigured: %v", err)
	}
}
