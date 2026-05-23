package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSecretFindingsSchemaExists(t *testing.T) {
	d := testDB(t)
	r := d.getReader()
	var n int
	if err := r.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='table' AND name='secret_findings'`).Scan(&n); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if n != 1 {
		t.Fatalf("secret_findings table missing")
	}
	for _, col := range []string{"secret_leak_count", "secrets_rules_version"} {
		var cnt int
		if err := r.QueryRow(
			`SELECT count(*) FROM pragma_table_info('sessions') WHERE name=?`,
			col).Scan(&cnt); err != nil {
			t.Fatalf("probe col %s: %v", col, err)
		}
		if cnt != 1 {
			t.Errorf("sessions.%s missing", col)
		}
	}
}

// TestHasSecretPartialIndexExists pins the partial index that backs the
// session --has-secret filter (secret_leak_count > 0), so a large sessions
// table does not scan to find the sparse set of leaky sessions.
func TestHasSecretPartialIndexExists(t *testing.T) {
	d := testDB(t)
	var n int
	if err := d.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='index' AND name='idx_sessions_has_secret'`).Scan(&n); err != nil {
		t.Fatalf("probe index: %v", err)
	}
	if n != 1 {
		t.Fatal("idx_sessions_has_secret partial index missing")
	}
}

func TestReplaceSessionSecretFindings(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	findings := []SecretFinding{
		{SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0, MatchStart: 5, MatchEnd: 25,
			MatchIndex: 0, RedactedMatch: "AKIA…MPLE"},
		{SessionID: "s1", RuleName: "high-entropy-assignment", Confidence: "candidate",
			LocationKind: "tool_result", MessageOrdinal: 1, CallIndex: Ptr(0),
			MatchStart: 0, MatchEnd: 24, MatchIndex: 0, RedactedMatch: "…abcd"},
	}
	if err := d.ReplaceSessionSecretFindings(
		"s1", findings, 1, "rulesv1"); err != nil {
		t.Fatalf("ReplaceSessionSecretFindings: %v", err)
	}
	got, err := d.SessionSecretFindings(context.Background(), "s1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2", len(got))
	}
	// A message finding has no call/event index (NULL round-trips to nil); a
	// tool finding with CallIndex 0 must read back as a non-nil pointer to 0,
	// not collapse to nil (the NULL-vs-zero trap for nullable ints).
	if got[0].CallIndex != nil || got[0].EventIndex != nil {
		t.Errorf("message finding indices = %v/%v, want nil/nil",
			got[0].CallIndex, got[0].EventIndex)
	}
	if got[1].CallIndex == nil || *got[1].CallIndex != 0 {
		t.Errorf("tool finding CallIndex = %v, want non-nil 0", got[1].CallIndex)
	}
	if err := d.ReplaceSessionSecretFindings("s1", findings[:1], 1, "rulesv1"); err != nil {
		t.Fatalf("re-replace: %v", err)
	}
	got, _ = d.SessionSecretFindings(context.Background(), "s1")
	if len(got) != 1 {
		t.Fatalf("replace not idempotent: got %d, want 1", len(got))
	}
}

// TestReplaceSessionMessagesResetsSecretState verifies that replacing a
// session's messages clears stale secret findings and resets the scan-state
// columns. A session re-imported with different content (the importer calls
// ReplaceSessionMessages directly) must not keep an obsolete leak count, and
// must be re-scanned by `secrets scan --backfill`, which skips sessions already
// at the current rules version.
func TestReplaceSessionMessagesResetsSecretState(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "proj")
	if err := d.ReplaceSessionMessages("s1", []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "key AKIA7QHWN2DKR4FYPLJM"},
	}); err != nil {
		t.Fatalf("seed messages: %v", err)
	}
	findings := []SecretFinding{{
		SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 4, MatchEnd: 24,
		MatchIndex: 0, RedactedMatch: "AKIA…MPLE",
	}}
	if err := d.ReplaceSessionSecretFindings("s1", findings, 1, "rulesvX"); err != nil {
		t.Fatalf("seed findings: %v", err)
	}

	if err := d.ReplaceSessionMessages("s1", []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "nothing secret here"},
	}); err != nil {
		t.Fatalf("replace messages: %v", err)
	}

	got, err := d.SessionSecretFindings(ctx, "s1")
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("stale findings survived message replace: %+v", got)
	}
	var leak int
	var ver string
	if err := d.getReader().QueryRow(
		"SELECT secret_leak_count, secrets_rules_version FROM sessions WHERE id = 's1'",
	).Scan(&leak, &ver); err != nil {
		t.Fatalf("read scan state: %v", err)
	}
	if leak != 0 {
		t.Errorf("secret_leak_count = %d, want 0 after content replace", leak)
	}
	if ver != "" {
		t.Errorf("secrets_rules_version = %q, want empty (forces backfill rescan)", ver)
	}
}

func TestOrphanCopyPreservesSecretFindings(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	insertMessages(t, srcDB,
		userMsg("s1", 0, "hello"),
		asstMsg("s1", 1, "reply"),
	)
	err = srcDB.ReplaceSessionSecretFindings("s1", []SecretFinding{
		{
			SessionID:      "s1",
			RuleName:       "aws-access-key",
			Confidence:     "definite",
			LocationKind:   "message",
			MessageOrdinal: 0,
			MatchStart:     0,
			MatchEnd:       20,
			MatchIndex:     0,
			RedactedMatch:  "AKIA…MPLE",
		},
	}, 1, "rulesv1")
	requireNoError(t, err, "ReplaceSessionSecretFindings src")
	// Stamp a distinctive past created_at so the copy can be shown to preserve
	// it rather than regenerating from the destination default.
	_, err = srcDB.getWriter().Exec(
		"UPDATE secret_findings SET created_at = ? WHERE session_id = 's1'",
		"2020-01-01T00:00:00.000Z")
	requireNoError(t, err, "stamp created_at")
	srcDB.Close()

	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	requireNoError(t, err, "CopyOrphanedDataFrom")
	if count != 1 {
		t.Fatalf("expected 1 orphaned session, got %d", count)
	}

	s, err := dstDB.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession")
	if s == nil {
		t.Fatal("session s1 not found in dst")
	}
	if s.SecretLeakCount != 1 {
		t.Errorf("SecretLeakCount = %d, want 1", s.SecretLeakCount)
	}

	findings, err := dstDB.SessionSecretFindings(ctx, "s1")
	requireNoError(t, err, "SessionSecretFindings")
	if len(findings) != 1 {
		t.Fatalf("findings count = %d, want 1", len(findings))
	}
	f := findings[0]
	if f.RuleName != "aws-access-key" {
		t.Errorf("RuleName = %q, want %q", f.RuleName, "aws-access-key")
	}
	if f.LocationKind != "message" {
		t.Errorf("LocationKind = %q, want %q", f.LocationKind, "message")
	}
	if f.RedactedMatch != "AKIA…MPLE" {
		t.Errorf("RedactedMatch = %q, want %q", f.RedactedMatch, "AKIA…MPLE")
	}

	var createdAt string
	requireNoError(t, dstDB.getReader().QueryRow(
		"SELECT created_at FROM secret_findings WHERE session_id = 's1'",
	).Scan(&createdAt), "query copied created_at")
	if createdAt != "2020-01-01T00:00:00.000Z" {
		t.Errorf("created_at = %q, want preserved 2020-01-01T00:00:00.000Z", createdAt)
	}
}

func TestListSecretFindings(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "alpha", func(s *Session) { s.Agent = "claude" })
	insertSession(t, d, "s2", "beta", func(s *Session) { s.Agent = "codex" })
	_ = d.ReplaceSessionSecretFindings("s1", []SecretFinding{
		{SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 20,
			MatchIndex: 0, RedactedMatch: "AKIA…MPLE"},
	}, 1, "v1")
	_ = d.ReplaceSessionSecretFindings("s2", []SecretFinding{
		{SessionID: "s2", RuleName: "jwt", Confidence: "candidate",
			LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 10,
			MatchIndex: 0, RedactedMatch: "eyJ…"},
	}, 0, "v1")

	all, err := d.ListSecretFindings(context.Background(), SecretFindingFilter{Limit: 50})
	if err != nil {
		t.Fatalf("ListSecretFindings: %v", err)
	}
	if len(all.Findings) != 2 {
		t.Fatalf("got %d, want 2", len(all.Findings))
	}
	// project filter
	alpha, _ := d.ListSecretFindings(context.Background(),
		SecretFindingFilter{Project: "alpha", Limit: 50})
	if len(alpha.Findings) != 1 || alpha.Findings[0].SessionID != "s1" {
		t.Fatalf("project filter = %+v", alpha.Findings)
	}
	// confidence filter
	def, _ := d.ListSecretFindings(context.Background(),
		SecretFindingFilter{Confidence: "definite", Limit: 50})
	if len(def.Findings) != 1 || def.Findings[0].RuleName != "aws-access-key" {
		t.Fatalf("confidence filter = %+v", def.Findings)
	}
}

// TestListSecretFindingsPagination walks pages with a small limit and asserts
// every finding is seen exactly once (no OFFSET drop/duplicate at the page
// boundary) and NextCursor advances then stops.
func TestListSecretFindingsPagination(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj", func(s *Session) { s.Agent = "claude" })
	var findings []SecretFinding
	for i := range 5 {
		findings = append(findings, SecretFinding{
			SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0,
			MatchStart: i * 10, MatchEnd: i*10 + 5, MatchIndex: i,
			RedactedMatch: "AKIA…",
		})
	}
	if err := d.ReplaceSessionSecretFindings("s1", findings, 5, "v1"); err != nil {
		t.Fatalf("ReplaceSessionSecretFindings: %v", err)
	}
	seen := map[int]int{}
	cursor, pages := 0, 0
	for {
		page, err := d.ListSecretFindings(context.Background(),
			SecretFindingFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("page at cursor %d: %v", cursor, err)
		}
		for _, f := range page.Findings {
			seen[f.MatchStart]++
		}
		if pages++; pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d distinct findings across pages, want 5", len(seen))
	}
	for start, n := range seen {
		if n != 1 {
			t.Errorf("finding at MatchStart=%d seen %d times, want 1", start, n)
		}
	}
}

// TestListSecretFindingsDateFilter covers the COALESCE(NULLIF(...)) fallback:
// a session with an empty-string started_at must filter on created_at rather
// than being silently dropped (date(”) is NULL).
func TestListSecretFindingsDateFilter(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "dated", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-03-15T10:00:00Z")
	})
	insertSession(t, d, "empty", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("")
	})
	// Pin the empty session's created_at so the assertions are deterministic.
	if _, err := d.getWriter().Exec(
		"UPDATE sessions SET created_at = ? WHERE id = 'empty'",
		"2026-05-10T00:00:00Z"); err != nil {
		t.Fatalf("stamp created_at: %v", err)
	}
	for _, id := range []string{"dated", "empty"} {
		if err := d.ReplaceSessionSecretFindings(id, []SecretFinding{{
			SessionID: id, RuleName: "aws-access-key", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0, MatchStart: 0,
			MatchEnd: 20, MatchIndex: 0, RedactedMatch: "AKIA…MPLE",
		}}, 1, "v1"); err != nil {
			t.Fatalf("ReplaceSessionSecretFindings %s: %v", id, err)
		}
	}
	// Wide range includes both; the empty started_at falls back to created_at.
	wide, err := d.ListSecretFindings(context.Background(),
		SecretFindingFilter{DateFrom: "2000-01-01", Limit: 50})
	if err != nil {
		t.Fatalf("wide: %v", err)
	}
	if len(wide.Findings) != 2 {
		t.Fatalf("DateFrom 2000-01-01 = %d findings, want 2 (empty must fall "+
			"back to created_at)", len(wide.Findings))
	}
	// Range covering only the dated session (empty's created_at is in May).
	mar, _ := d.ListSecretFindings(context.Background(),
		SecretFindingFilter{DateFrom: "2026-03-01", DateTo: "2026-03-31", Limit: 50})
	if len(mar.Findings) != 1 || mar.Findings[0].SessionID != "dated" {
		t.Fatalf("March range = %+v, want only dated", mar.Findings)
	}
}

// TestUpdateSessionSignalsPreservesSecretColumns verifies a signals-only
// recompute (whose SessionSignalUpdate carries zero secret fields) does not
// reset secret_leak_count/secrets_rules_version or drop findings: the
// secret columns are owned solely by the findings replacement path.
func TestUpdateSessionSignalsPreservesSecretColumns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "proj")
	if err := d.ReplaceSessionSecretFindings("s1", []SecretFinding{{
		SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 20,
		MatchIndex: 0, RedactedMatch: "AKIA…MPLE",
	}}, 1, "rulesv1"); err != nil {
		t.Fatalf("ReplaceSessionSecretFindings: %v", err)
	}
	// A signals-only recompute carries zero secret fields; it must not reset
	// the secret summary while findings still exist.
	if err := d.UpdateSessionSignals(
		"s1", SessionSignalUpdate{Outcome: "success"},
	); err != nil {
		t.Fatalf("UpdateSessionSignals: %v", err)
	}
	s, err := d.GetSession(ctx, "s1")
	if err != nil || s == nil {
		t.Fatalf("GetSession: %v", err)
	}
	if s.SecretLeakCount != 1 {
		t.Errorf("SecretLeakCount = %d, want preserved 1", s.SecretLeakCount)
	}
	findings, err := d.SessionSecretFindings(ctx, "s1")
	if err != nil {
		t.Fatalf("SessionSecretFindings: %v", err)
	}
	if len(findings) != 1 {
		t.Errorf("findings = %d, want preserved 1", len(findings))
	}
	var rv string
	if err := d.getReader().QueryRow(
		"SELECT secrets_rules_version FROM sessions WHERE id = 's1'",
	).Scan(&rv); err != nil {
		t.Fatalf("query secrets_rules_version: %v", err)
	}
	if rv != "rulesv1" {
		t.Errorf("secrets_rules_version = %q, want preserved rulesv1", rv)
	}
}

func TestSecretScanCandidates(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "scanned", "proj", func(s *Session) { s.MessageCount = 1 })
	insertSession(t, d, "stale", "proj", func(s *Session) { s.MessageCount = 1 })
	insertSession(t, d, "empty", "proj", func(s *Session) { s.MessageCount = 0 })
	// Mark "scanned" at the current version; the others stay at "".
	if err := d.ReplaceSessionSecretFindings("scanned", nil, 0, "vCur"); err != nil {
		t.Fatalf("mark scanned: %v", err)
	}
	ctx := context.Background()
	stale, err := d.SecretScanCandidates(ctx, SecretScanCandidateFilter{
		CurrentVersion: "vCur", OnlyStale: true,
	})
	if err != nil {
		t.Fatalf("SecretScanCandidates stale: %v", err)
	}
	// Only "stale" qualifies: "scanned" is current, "empty" has no messages.
	if len(stale) != 1 || stale[0] != "stale" {
		t.Fatalf("OnlyStale candidates = %v, want [stale]", stale)
	}
	all, err := d.SecretScanCandidates(ctx, SecretScanCandidateFilter{
		CurrentVersion: "vCur", OnlyStale: false,
	})
	if err != nil {
		t.Fatalf("SecretScanCandidates forced: %v", err)
	}
	// Forced rescan ignores version but still requires messages.
	if len(all) != 2 {
		t.Fatalf("forced candidates = %v, want 2 (scanned+stale)", all)
	}
}

func TestSecretFindingSource(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj", func(s *Session) { s.Agent = "claude" })
	msgs := []Message{{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		Content:   "key AKIA7QHWN2DKR4FYPLJM here",
		Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{
			{
				ToolName: "Bash", Category: "Bash", ToolUseID: "tu0",
				InputJSON:     `{"command":"printenv"}`,
				ResultContent: "AWS_SECRET=topsecretvalue123",
			},
			{
				ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
				ResultEvents: []ToolResultEvent{{
					Source: "stdout", Status: "ok",
					Content: "event-secret-value", EventIndex: 0,
				}},
			},
		},
	}}
	if err := d.ReplaceSessionMessages("s1", msgs); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
	ctx := context.Background()
	cases := []struct {
		name string
		f    SecretFinding
		want string
		ok   bool
	}{
		{"message", SecretFinding{SessionID: "s1", LocationKind: "message",
			MessageOrdinal: 0}, "key AKIA7QHWN2DKR4FYPLJM here", true},
		{"tool_input", SecretFinding{SessionID: "s1", LocationKind: "tool_input",
			MessageOrdinal: 0, CallIndex: Ptr(0)}, `{"command":"printenv"}`, true},
		{"tool_result", SecretFinding{SessionID: "s1", LocationKind: "tool_result",
			MessageOrdinal: 0, CallIndex: Ptr(0)}, "AWS_SECRET=topsecretvalue123", true},
		{"tool_result_event", SecretFinding{SessionID: "s1",
			LocationKind: "tool_result_event", MessageOrdinal: 0,
			CallIndex: Ptr(1), EventIndex: Ptr(0)}, "event-secret-value", true},
		{"missing ordinal", SecretFinding{SessionID: "s1", LocationKind: "message",
			MessageOrdinal: 99}, "", false},
		{"call index out of range", SecretFinding{SessionID: "s1",
			LocationKind: "tool_input", MessageOrdinal: 0, CallIndex: Ptr(9)}, "", false},
		{"event index not found", SecretFinding{SessionID: "s1",
			LocationKind: "tool_result_event", MessageOrdinal: 0,
			CallIndex: Ptr(1), EventIndex: Ptr(99)}, "", false},
		// Call 1 has result events, so the scanner used those, not
		// result_content; a stale tool_result finding there must not resolve.
		{"tool_result skipped when events present", SecretFinding{SessionID: "s1",
			LocationKind: "tool_result", MessageOrdinal: 0, CallIndex: Ptr(1)}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := d.SecretFindingSource(ctx, tc.f)
			if err != nil {
				t.Fatalf("SecretFindingSource: %v", err)
			}
			if ok != tc.ok || got != tc.want {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestOrphanCopyPreservesToolCallIndex verifies that a secret finding whose
// call_index points at a NON-zero (second) tool call resolves to the correct
// content after CopyOrphanedDataFrom. Without ORDER BY otc.id in the orphan
// tool_call INSERT, SQLite may assign new ids in an arbitrary order, causing
// call_index=1 to resolve to the wrong tool call.
func TestOrphanCopyPreservesToolCallIndex(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// --- Source DB: one session, one assistant message, two tool calls ---
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")

	insertSession(t, srcDB, "s1", "proj")
	msgs := []Message{{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		Content:   "ran two tools",
		Timestamp: "2026-01-01T00:00:00Z",
		ToolCalls: []ToolCall{
			{
				ToolName:      "Bash",
				Category:      "Bash",
				ToolUseID:     "tu0",
				InputJSON:     `{"command":"echo ZERO-AKIA"}`,
				ResultContent: "ZERO-AKIA",
			},
			{
				ToolName:      "Bash",
				Category:      "Bash",
				ToolUseID:     "tu1",
				InputJSON:     `{"command":"echo secret"}`,
				ResultContent: "AKIA7QHWN2DKR4FYPLJM",
			},
		},
	}}
	if err := srcDB.ReplaceSessionMessages("s1", msgs); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}

	// Finding at call_index=1 (second tool call), tool_result location.
	finding := SecretFinding{
		SessionID:      "s1",
		RuleName:       "aws-access-key",
		Confidence:     "definite",
		LocationKind:   "tool_result",
		MessageOrdinal: 0,
		CallIndex:      Ptr(1),
		MatchStart:     0,
		MatchEnd:       20,
		MatchIndex:     0,
		RedactedMatch:  "AKIA…MPLE",
	}
	if err := srcDB.ReplaceSessionSecretFindings(
		"s1", []SecretFinding{finding}, 1, "rulesv1",
	); err != nil {
		t.Fatalf("ReplaceSessionSecretFindings: %v", err)
	}
	srcDB.Close()

	// --- Destination DB: copy orphaned data ---
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	requireNoError(t, err, "CopyOrphanedDataFrom")
	if count != 1 {
		t.Fatalf("expected 1 orphaned session, got %d", count)
	}

	// --- Verify: call_index=1 must resolve to the SECOND tool call ---
	findings, err := dstDB.SessionSecretFindings(ctx, "s1")
	requireNoError(t, err, "SessionSecretFindings")
	if len(findings) != 1 {
		t.Fatalf("findings count = %d, want 1", len(findings))
	}
	f := findings[0]
	if f.CallIndex == nil || *f.CallIndex != 1 {
		t.Fatalf("CallIndex = %v, want non-nil 1", f.CallIndex)
	}

	got, ok, err := dstDB.SecretFindingSource(ctx, f)
	requireNoError(t, err, "SecretFindingSource")
	if !ok {
		t.Fatal("SecretFindingSource returned ok=false; " +
			"tool call order corrupted during orphan copy")
	}
	const wantContent = "AKIA7QHWN2DKR4FYPLJM"
	if got != wantContent {
		t.Errorf("SecretFindingSource = %q, want %q\n"+
			"(call_index=1 resolved to wrong tool call; "+
			"ORDER BY otc.id may be missing from orphan copy)",
			got, wantContent)
	}
}
