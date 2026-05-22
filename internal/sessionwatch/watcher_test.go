package sessionwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// testWatcher creates a Watcher backed by a fresh SQLite database
// and a minimal sync engine for tests that need checkDBForChanges
// access.
func testWatcher(t *testing.T) *Watcher {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {dir},
		},
		Machine: "test",
	})
	return New(database, engine)
}

func TestStatMtime_NonexistentFile(t *testing.T) {
	t.Parallel()
	got := StatMtime(
		filepath.Join(t.TempDir(), "no-such-file"),
	)
	if got != 0 {
		t.Errorf("StatMtime(nonexistent) = %d, want 0", got)
	}
}

func TestStatMtime_ExistingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(
		path, []byte("data"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	got := StatMtime(path)
	if got == 0 {
		t.Error("StatMtime(existing) = 0, want nonzero")
	}
}

func TestCheckDBForChanges_FileDisappears(t *testing.T) {
	t.Parallel()
	w := testWatcher(t)

	path := filepath.Join(t.TempDir(), "gone.jsonl")
	var lastMtime int64 = 12345
	var mchanged time.Time
	var lastCount int
	var lastDBMtime int64

	changed := w.checkDBForChanges(
		"test-session",
		&lastCount,
		&lastDBMtime,
		&path,
		&lastMtime,
		&mchanged,
	)
	if changed {
		t.Error("expected no change signal")
	}
	if path != "" {
		t.Errorf("sourcePath = %q, want empty", path)
	}
	if lastMtime != 0 {
		t.Errorf("lastMtime = %d, want 0", lastMtime)
	}
}
