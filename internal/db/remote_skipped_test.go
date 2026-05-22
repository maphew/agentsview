package db_test

import (
	"maps"
	"testing"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestRemoteSkippedFiles_InitiallyEmpty(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	loaded, err := d.LoadRemoteSkippedFiles("devbox1")
	if err != nil {
		t.Fatalf("LoadRemoteSkippedFiles: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected empty, got %d entries", len(loaded))
	}
}

func TestRemoteSkippedFiles_RoundTrip(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	entries := map[string]int64{
		"/home/user/.claude/sessions/a.jsonl": 1000,
		"/home/user/.claude/sessions/b.jsonl": 2000,
		"/home/user/.claude/sessions/c.jsonl": 3000,
	}
	if err := d.ReplaceRemoteSkippedFiles(
		"devbox1", entries,
	); err != nil {
		t.Fatalf("ReplaceRemoteSkippedFiles: %v", err)
	}

	loaded, err := d.LoadRemoteSkippedFiles("devbox1")
	if err != nil {
		t.Fatalf("LoadRemoteSkippedFiles: %v", err)
	}
	if !maps.Equal(loaded, entries) {
		t.Errorf("loaded %v, want %v", loaded, entries)
	}
}

func TestRemoteSkippedFiles_HostIsolation(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	entries := map[string]int64{
		"/a.jsonl": 100,
		"/b.jsonl": 200,
	}
	if err := d.ReplaceRemoteSkippedFiles(
		"devbox1", entries,
	); err != nil {
		t.Fatalf("ReplaceRemoteSkippedFiles: %v", err)
	}

	// Different host should return empty.
	loaded, err := d.LoadRemoteSkippedFiles("devbox2")
	if err != nil {
		t.Fatalf("LoadRemoteSkippedFiles devbox2: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf(
			"devbox2: expected empty, got %d entries",
			len(loaded),
		)
	}

	// Original host still has its entries.
	loaded, err = d.LoadRemoteSkippedFiles("devbox1")
	if err != nil {
		t.Fatalf("LoadRemoteSkippedFiles devbox1: %v", err)
	}
	if !maps.Equal(loaded, entries) {
		t.Errorf("devbox1: loaded %v, want %v", loaded, entries)
	}
}

func TestRemoteSkippedFiles_ReplaceOverwrites(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	first := map[string]int64{
		"/a.jsonl": 100,
		"/b.jsonl": 200,
	}
	if err := d.ReplaceRemoteSkippedFiles(
		"devbox1", first,
	); err != nil {
		t.Fatalf("ReplaceRemoteSkippedFiles: %v", err)
	}

	// Replace with different entries.
	second := map[string]int64{
		"/c.jsonl": 300,
	}
	if err := d.ReplaceRemoteSkippedFiles(
		"devbox1", second,
	); err != nil {
		t.Fatalf("ReplaceRemoteSkippedFiles: %v", err)
	}

	loaded, err := d.LoadRemoteSkippedFiles("devbox1")
	if err != nil {
		t.Fatalf("LoadRemoteSkippedFiles: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("got %d entries, want 1", len(loaded))
	}
	if loaded["/c.jsonl"] != 300 {
		t.Errorf(
			"loaded[/c.jsonl] = %d, want 300",
			loaded["/c.jsonl"],
		)
	}
}

func TestRemoteSkippedFiles_ReplaceDoesNotAffectOtherHosts(
	t *testing.T,
) {
	d := dbtest.OpenTestDB(t)

	host1 := map[string]int64{"/a.jsonl": 100}
	host2 := map[string]int64{"/b.jsonl": 200}

	if err := d.ReplaceRemoteSkippedFiles(
		"devbox1", host1,
	); err != nil {
		t.Fatalf("ReplaceRemoteSkippedFiles devbox1: %v", err)
	}
	if err := d.ReplaceRemoteSkippedFiles(
		"devbox2", host2,
	); err != nil {
		t.Fatalf("ReplaceRemoteSkippedFiles devbox2: %v", err)
	}

	// Replace devbox1 with empty — devbox2 unaffected.
	if err := d.ReplaceRemoteSkippedFiles(
		"devbox1", map[string]int64{},
	); err != nil {
		t.Fatalf("ReplaceRemoteSkippedFiles empty: %v", err)
	}

	loaded1, err := d.LoadRemoteSkippedFiles("devbox1")
	if err != nil {
		t.Fatalf("LoadRemoteSkippedFiles devbox1: %v", err)
	}
	if len(loaded1) != 0 {
		t.Fatalf(
			"devbox1: got %d entries, want 0", len(loaded1),
		)
	}

	loaded2, err := d.LoadRemoteSkippedFiles("devbox2")
	if err != nil {
		t.Fatalf("LoadRemoteSkippedFiles devbox2: %v", err)
	}
	if !maps.Equal(loaded2, host2) {
		t.Errorf(
			"devbox2: loaded %v, want %v", loaded2, host2,
		)
	}
}
