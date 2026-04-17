package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/parser"
)

// newTestDB opens a fresh SQLite DB in a temp dir for a single test.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// upsertSession inserts a session with minimal required fields.
func upsertSession(
	t *testing.T, d *db.DB, id, agent, startedAt string,
) {
	t.Helper()
	s := db.Session{
		ID:           id,
		Project:      "test-project",
		Machine:      "local",
		Agent:        agent,
		MessageCount: 1,
	}
	if startedAt != "" {
		s.StartedAt = &startedAt
	}
	if err := d.UpsertSession(s); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

func TestResolveSessionID_PrefixedInput_NoEvidence_UnchangedNotKnown(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// A prefixed input with no DB row and no disk evidence is
	// returned unchanged so downstream lookup/error messages
	// use what the caller typed, but known=false so the caller
	// skips the on-demand sync that would only warn about a
	// missing source file.
	input := "codex:019d5490-fe31-7e62-838c-8ba4193f245d"
	got, known := resolveRawSessionID(ctx, d, nil, input)
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
	if known {
		t.Errorf("known = true, want false (no evidence)")
	}
}

func TestResolveSessionID_HostPrefixedInput_ReturnedUnchanged(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Host-prefixed IDs are unambiguously canonical remote IDs;
	// resolution short-circuits without touching DB or disk.
	input := "other-host~codex:abc-123"
	got, known := resolveRawSessionID(ctx, d, nil, input)
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
	if !known {
		t.Errorf("known = false, want true (host-prefixed)")
	}
}

func TestResolveSessionID_BareClaudeUUID_ExactMatch(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Claude sessions have no prefix; the bare UUID is the
	// canonical ID stored in sessions.id.
	id := "11111111-1111-1111-1111-111111111111"
	upsertSession(t, d, id, "claude", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, id)
	if got != id {
		t.Errorf("got %q, want %q", got, id)
	}
	if !known {
		t.Errorf("known = false, want true (DB match)")
	}
}

func TestResolveSessionID_BareCodexUUID_ResolvesToPrefixed(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	bare := "019d5490-fe31-7e62-838c-8ba4193f245d"
	stored := "codex:" + bare
	upsertSession(t, d, stored, "codex", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, bare)
	if got != stored {
		t.Errorf("got %q, want %q", got, stored)
	}
	if !known {
		t.Errorf("known = false, want true (DB match)")
	}
}

func TestResolveSessionID_Ambiguous_MostRecentWins(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	bare := "22222222-2222-2222-2222-222222222222"
	// Older codex session.
	upsertSession(t, d, "codex:"+bare, "codex", "2026-04-16T10:00:00Z")
	// Newer amp session with same raw UUID.
	upsertSession(t, d, "amp:"+bare, "amp", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, bare)
	if got != "amp:"+bare {
		t.Errorf("got %q, want amp:%s (most recent)", got, bare)
	}
	if !known {
		t.Errorf("known = false, want true")
	}
}

func TestResolveSessionID_NotInDB_FoundOnDisk(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Create a codex session file on disk: the probe path
	// should resolve a bare raw UUID to the prefixed form.
	codexDir := filepath.Join(t.TempDir(), "codex-sessions")
	bare := "33333333-3333-3333-3333-333333333333"
	dayDir := filepath.Join(codexDir, "2026", "04", "17")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fname := "rollout-2026-04-17T10-00-00-" + bare + ".jsonl"
	fpath := filepath.Join(dayDir, fname)
	if err := os.WriteFile(fpath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	agentDirs := map[parser.AgentType][]string{
		parser.AgentCodex: {codexDir},
	}
	got, known := resolveRawSessionID(ctx, d, agentDirs, bare)
	if got != "codex:"+bare {
		t.Errorf("got %q, want codex:%s (disk probe)", got, bare)
	}
	if !known {
		t.Errorf("known = false, want true (disk probe found match)")
	}
}

func TestResolveSessionID_NotFoundAnywhere_PassThrough(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	bare := "44444444-4444-4444-4444-444444444444"
	got, known := resolveRawSessionID(ctx, d, nil, bare)
	if got != bare {
		t.Errorf("got %q, want %q (pass-through)", got, bare)
	}
	if known {
		t.Errorf("known = true, want false (nothing found)")
	}
}

func TestResolveSessionID_BareClaudeAndPrefixedSameUUID_ClaudeExactWins(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Edge: a bare Claude UUID that ALSO exists as a prefixed
	// session (e.g. codex:<same-uuid>). The Claude row is an
	// exact match and should win over the suffix match.
	bare := "55555555-5555-5555-5555-555555555555"
	upsertSession(t, d, bare, "claude", "2026-04-16T10:00:00Z")
	upsertSession(t, d, "codex:"+bare, "codex", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, bare)
	if got != bare {
		t.Errorf("got %q, want %q (exact claude match)", got, bare)
	}
	if !known {
		t.Errorf("known = false, want true")
	}
}

func TestResolveSessionID_ExactMatchWinsOverNewerCollisions(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Bare Claude session is the exact match but older than
	// multiple prefixed sessions sharing the same suffix. The
	// exact row must always win, even if a LIMIT on the suffix
	// query would exclude it by recency.
	bare := "88888888-8888-8888-8888-888888888888"
	upsertSession(t, d, bare, "claude", "2026-04-10T10:00:00Z")
	upsertSession(t, d, "codex:"+bare, "codex",
		"2026-04-15T10:00:00Z")
	upsertSession(t, d, "amp:"+bare, "amp",
		"2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, bare)
	if got != bare {
		t.Errorf("got %q, want %q (exact match must beat "+
			"newer suffix collisions)", got, bare)
	}
	if !known {
		t.Errorf("known = false, want true")
	}
}

func TestResolveSessionID_KimiRawID_ResolvesToPrefixed(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Kimi raw IDs have the shape "<project-hash>:<session-uuid>".
	// The stored canonical form prepends "kimi:".
	raw := "proj-hash-abc:66666666-6666-6666-6666-666666666666"
	stored := "kimi:" + raw
	upsertSession(t, d, stored, "kimi", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, raw)
	if got != stored {
		t.Errorf("got %q, want %q (kimi raw ID resolves)", got, stored)
	}
	if !known {
		t.Errorf("known = false, want true")
	}
}

func TestResolveSessionID_OpenClawRawID_ResolvesToPrefixed(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// OpenClaw raw IDs have the shape "<agentId>:<sessionId>".
	raw := "main:abc-123"
	stored := "openclaw:" + raw
	upsertSession(t, d, stored, "openclaw", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, raw)
	if got != stored {
		t.Errorf("got %q, want %q (openclaw raw ID resolves)",
			got, stored)
	}
	if !known {
		t.Errorf("known = false, want true")
	}
}

func TestResolveSessionID_CanonicalKimiID_ResolvesWhenInDB(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// A canonical Kimi ID already in the DB resolves via the
	// exact-match branch. A canonical ID with no DB row and no
	// disk evidence falls through to known=false so no
	// misleading sync warning is emitted.
	input := "kimi:proj-abc:77777777-7777-7777-7777-777777777777"
	upsertSession(t, d, input, "kimi", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, input)
	if got != input {
		t.Errorf("got %q, want %q (exact DB match)", got, input)
	}
	if !known {
		t.Errorf("known = false, want true (exact DB match)")
	}
}

func TestResolveSessionID_CanonicalCodexID_OnDiskNotInDB(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Canonical "codex:<uuid>" not yet synced but present on
	// disk must resolve via the canonical disk probe — which
	// strips the prefix before calling FindSourceFunc (the
	// underlying finder rejects colon-bearing IDs).
	codexDir := filepath.Join(t.TempDir(), "codex-sessions")
	uuid := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	dayDir := filepath.Join(codexDir, "2026", "04", "17")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fname := "rollout-2026-04-17T10-00-00-" + uuid + ".jsonl"
	if err := os.WriteFile(
		filepath.Join(dayDir, fname), []byte("{}\n"), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	agentDirs := map[parser.AgentType][]string{
		parser.AgentCodex: {codexDir},
	}
	input := "codex:" + uuid
	got, known := resolveRawSessionID(ctx, d, agentDirs, input)
	if got != input {
		t.Errorf("got %q, want %q (canonical on disk)", got, input)
	}
	if !known {
		t.Errorf("known = false, want true (canonical disk probe)")
	}
}

func TestResolveSessionID_RawOpenClawCollidesWithCodexPrefix(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// OpenClaw permits arbitrary alphanumeric-dash-underscore
	// agent IDs, so a user may have one literally named "codex".
	// The raw OpenClaw ID "codex:abc-123" is stored as
	// "openclaw:codex:abc-123". Passing the raw form must not
	// be short-circuited as a canonical Codex ID — DB suffix
	// resolution must take precedence.
	raw := "codex:abc-123"
	stored := "openclaw:" + raw
	upsertSession(t, d, stored, "openclaw",
		"2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, raw)
	if got != stored {
		t.Errorf("got %q, want %q (raw openclaw must beat "+
			"canonical-prefix short-circuit)", got, stored)
	}
	if !known {
		t.Errorf("known = false, want true")
	}
}

func TestResolveSessionID_UnderscoreID_NoFalseMatch(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Underscore is a LIKE wildcard in SQLite. If the query
	// uses LIKE naively, a raw id "20260403_aaa" would match
	// rows whose id ends with ":20260403Xaaa" (X = any char).
	// Insert a decoy that would only match under naive LIKE
	// semantics, plus a true match, and assert the true match
	// wins.
	raw := "20260403_aaa"
	decoy := "codex:20260403Xaaa"
	real := "codex:" + raw
	upsertSession(t, d, decoy, "codex", "2026-04-16T10:00:00Z")
	upsertSession(t, d, real, "codex", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, raw)
	if got != real {
		t.Errorf("got %q, want %q (underscore is literal)",
			got, real)
	}
	if !known {
		t.Errorf("known = false, want true")
	}
}

func TestTokenUseExitCode_Found(t *testing.T) {
	sess := &db.Session{
		ID:                   "codex:xxx",
		HasTotalOutputTokens: true,
		TotalOutputTokens:    100,
	}
	if got := tokenUseExitCode(sess); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestTokenUseExitCode_NoData(t *testing.T) {
	sess := &db.Session{ID: "codex:xxx"}
	if got := tokenUseExitCode(sess); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestTokenUseExitCode_NotFound(t *testing.T) {
	if got := tokenUseExitCode(nil); got != 2 {
		t.Errorf("got %d, want 2", got)
	}
}

func TestTokenUseExitCode_PeakContextOnly(t *testing.T) {
	// Having only peak_context token data is still "has data".
	sess := &db.Session{
		ID:                   "claude:xxx",
		HasPeakContextTokens: true,
		PeakContextTokens:    50000,
	}
	if got := tokenUseExitCode(sess); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}
