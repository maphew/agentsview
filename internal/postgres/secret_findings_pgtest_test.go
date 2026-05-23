//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"go.kenn.io/agentsview/internal/db"
)

// seedSecretFindingsSession inserts a session, optional messages, and findings.
func seedSecretFindingsSession(
	t *testing.T, store *Store,
	sid, project, agent string,
	startedAt string,
	msgs []struct {
		ordinal int
		role    string
		content string
	},
	findings []db.SecretFinding,
) {
	t.Helper()
	pg := store.DB()

	if _, err := pg.Exec(
		`DELETE FROM secret_findings WHERE session_id = $1`, sid,
	); err != nil {
		t.Fatalf("delete secret_findings: %v", err)
	}
	if _, err := pg.Exec(
		`DELETE FROM messages WHERE session_id = $1`, sid,
	); err != nil {
		t.Fatalf("delete messages: %v", err)
	}
	if _, err := pg.Exec(
		`DELETE FROM sessions WHERE id = $1`, sid,
	); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	var saVal interface{} = nil
	if startedAt != "" {
		saVal = startedAt
	}
	if _, err := pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count,
			 secret_leak_count)
		VALUES ($1, 'test-machine', $2, $3, 'test',
			$4::timestamptz, $5, 0, $6)`,
		sid, project, agent, saVal,
		len(msgs)+1, len(findings),
	); err != nil {
		t.Fatalf("insert session %s: %v", sid, err)
	}

	for _, m := range msgs {
		if _, err := pg.Exec(`
			INSERT INTO messages
				(session_id, ordinal, role, content, content_length)
			VALUES ($1, $2, $3, $4, $5)`,
			sid, m.ordinal, m.role, m.content, len(m.content),
		); err != nil {
			t.Fatalf("insert message ord=%d: %v", m.ordinal, err)
		}
	}

	for _, f := range findings {
		if _, err := pg.Exec(`
			INSERT INTO secret_findings
				(session_id, rule_name, confidence, location_kind,
				 message_ordinal, call_index, event_index,
				 match_start, match_end, match_index,
				 redacted_match, rules_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			f.SessionID, f.RuleName, f.Confidence, f.LocationKind,
			f.MessageOrdinal, f.CallIndex, f.EventIndex,
			f.MatchStart, f.MatchEnd, f.MatchIndex,
			f.RedactedMatch, f.RulesVersion,
		); err != nil {
			t.Fatalf("insert finding: %v", err)
		}
	}
}

func TestPGListSecretFindings(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	seedSecretFindingsSession(t, store,
		"sf-s1", "alpha-proj", "claude-code",
		"2026-03-12T10:00:00Z",
		nil,
		[]db.SecretFinding{
			{SessionID: "sf-s1", RuleName: "aws-access-key",
				Confidence: "definite", LocationKind: "message",
				MessageOrdinal: 0, MatchStart: 0, MatchEnd: 20,
				MatchIndex: 0, RedactedMatch: "AKIA…", RulesVersion: "v1"},
		},
	)
	seedSecretFindingsSession(t, store,
		"sf-s2", "beta-proj", "codex",
		"2026-03-13T10:00:00Z",
		nil,
		[]db.SecretFinding{
			{SessionID: "sf-s2", RuleName: "jwt",
				Confidence: "candidate", LocationKind: "message",
				MessageOrdinal: 0, MatchStart: 0, MatchEnd: 10,
				MatchIndex: 0, RedactedMatch: "eyJ…", RulesVersion: "v1"},
		},
	)

	// All findings.
	all, err := store.ListSecretFindings(ctx, db.SecretFindingFilter{Limit: 50})
	if err != nil {
		t.Fatalf("ListSecretFindings all: %v", err)
	}
	// At least 2 findings (may be more from other tests).
	foundS1, foundS2 := false, false
	for _, f := range all.Findings {
		if f.SessionID == "sf-s1" {
			foundS1 = true
			if f.Project != "alpha-proj" {
				t.Errorf("sf-s1 Project = %q, want alpha-proj", f.Project)
			}
			if f.Agent != "claude-code" {
				t.Errorf("sf-s1 Agent = %q, want claude-code", f.Agent)
			}
		}
		if f.SessionID == "sf-s2" {
			foundS2 = true
			if f.Project != "beta-proj" {
				t.Errorf("sf-s2 Project = %q, want beta-proj", f.Project)
			}
			if f.Agent != "codex" {
				t.Errorf("sf-s2 Agent = %q, want codex", f.Agent)
			}
		}
	}
	if !foundS1 || !foundS2 {
		t.Errorf("foundS1=%v foundS2=%v; both must be present", foundS1, foundS2)
	}

	// Project filter.
	alpha, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{Project: "alpha-proj", Limit: 50})
	if err != nil {
		t.Fatalf("ListSecretFindings project filter: %v", err)
	}
	for _, f := range alpha.Findings {
		if f.Project != "alpha-proj" {
			t.Errorf("project filter leak: got project %q", f.Project)
		}
	}
	found := false
	for _, f := range alpha.Findings {
		if f.SessionID == "sf-s1" {
			found = true
		}
	}
	if !found {
		t.Errorf("sf-s1 not found in project filter results")
	}

	// Agent filter.
	codex, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{Agent: "codex", Limit: 50})
	if err != nil {
		t.Fatalf("ListSecretFindings agent filter: %v", err)
	}
	for _, f := range codex.Findings {
		if f.Agent != "codex" {
			t.Errorf("agent filter leak: got agent %q", f.Agent)
		}
	}

	// Confidence filter: definite.
	def, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{Confidence: "definite", Limit: 50})
	if err != nil {
		t.Fatalf("ListSecretFindings confidence filter: %v", err)
	}
	for _, f := range def.Findings {
		if f.Confidence != "definite" {
			t.Errorf("confidence filter leak: got %q", f.Confidence)
		}
	}

	// Rule filter.
	jwt, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{Rule: "jwt", Limit: 50})
	if err != nil {
		t.Fatalf("ListSecretFindings rule filter: %v", err)
	}
	for _, f := range jwt.Findings {
		if f.RuleName != "jwt" {
			t.Errorf("rule filter leak: got %q", f.RuleName)
		}
	}
}

func TestPGListSecretFindingsPagination(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Seed 5 findings on a single session with a distinctive rule name.
	findings := make([]db.SecretFinding, 5)
	for i := range findings {
		findings[i] = db.SecretFinding{
			SessionID: "sf-page-s1", RuleName: "pg-pagination-test",
			Confidence: "definite", LocationKind: "message",
			MessageOrdinal: 0, MatchStart: i * 10, MatchEnd: i*10 + 5,
			MatchIndex: i, RedactedMatch: "X…", RulesVersion: "v1",
		}
	}
	seedSecretFindingsSession(t, store,
		"sf-page-s1", "page-proj", "claude-code",
		"2026-04-01T10:00:00Z",
		nil, findings,
	)

	seen := map[int]int{}
	cursor, pages := 0, 0
	for {
		page, err := store.ListSecretFindings(ctx,
			db.SecretFindingFilter{
				Rule:  "pg-pagination-test",
				Limit: 2, Cursor: cursor,
			})
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

func TestPGListSecretFindingsDateFilter(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	seedSecretFindingsSession(t, store,
		"sf-date-mar", "df-proj", "claude-code",
		"2026-03-15T10:00:00Z",
		nil,
		[]db.SecretFinding{{
			SessionID: "sf-date-mar", RuleName: "df-rule",
			Confidence: "definite", LocationKind: "message",
			MessageOrdinal: 0, MatchStart: 0, MatchEnd: 5,
			MatchIndex: 0, RedactedMatch: "X", RulesVersion: "v1",
		}},
	)
	seedSecretFindingsSession(t, store,
		"sf-date-may", "df-proj", "claude-code",
		"2026-05-10T10:00:00Z",
		nil,
		[]db.SecretFinding{{
			SessionID: "sf-date-may", RuleName: "df-rule",
			Confidence: "definite", LocationKind: "message",
			MessageOrdinal: 0, MatchStart: 0, MatchEnd: 5,
			MatchIndex: 0, RedactedMatch: "Y", RulesVersion: "v1",
		}},
	)

	// Wide range includes both.
	wide, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{
			Rule: "df-rule", DateFrom: "2000-01-01", Limit: 50,
		})
	if err != nil {
		t.Fatalf("wide: %v", err)
	}
	if len(wide.Findings) != 2 {
		t.Fatalf("wide = %d findings, want 2", len(wide.Findings))
	}

	// March-only range.
	mar, err := store.ListSecretFindings(ctx,
		db.SecretFindingFilter{
			Rule:     "df-rule",
			DateFrom: "2026-03-01", DateTo: "2026-03-31",
			Limit: 50,
		})
	if err != nil {
		t.Fatalf("march: %v", err)
	}
	if len(mar.Findings) != 1 || mar.Findings[0].SessionID != "sf-date-mar" {
		t.Fatalf("march filter = %+v, want only sf-date-mar", mar.Findings)
	}
}

func TestPGSecretFindingSource(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	pg := store.DB()
	ctx := context.Background()

	// Clean up prior runs. The sessions table uses "id" not "session_id".
	sid := "sf-src-s1"
	for _, tbl := range []string{
		"secret_findings", "tool_result_events",
		"tool_calls", "messages",
	} {
		if _, err := pg.Exec(
			"DELETE FROM "+tbl+" WHERE session_id = $1", sid,
		); err != nil {
			t.Fatalf("cleanup %s: %v", tbl, err)
		}
	}
	if _, err := pg.Exec(
		"DELETE FROM sessions WHERE id = $1", sid,
	); err != nil {
		t.Fatalf("cleanup sessions: %v", err)
	}

	if _, err := pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES ($1, 'test-machine', 'src-proj', 'claude-code',
			'test', '2026-05-01T00:00:00Z'::timestamptz, 1, 0)`,
		sid,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, content_length)
		VALUES ($1, 0, 'assistant',
			'key AKIA7QHWN2DKR4FYPLJM here', 28)`,
		sid,
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	// Insert a tool_call at call_index=0 with result_content.
	if _, err := pg.Exec(`
		INSERT INTO tool_calls
			(session_id, message_ordinal, call_index, tool_name, category,
			 tool_use_id, input_json, result_content)
		VALUES ($1, 0, 0, 'Bash', 'Bash', 'tu0',
			'{"command":"printenv"}', 'AWS_SECRET=topsecretvalue123')`,
		sid,
	); err != nil {
		t.Fatalf("insert tool_call: %v", err)
	}
	// Insert a tool_call at call_index=1 with a result event.
	if _, err := pg.Exec(`
		INSERT INTO tool_calls
			(session_id, message_ordinal, call_index, tool_name, category,
			 tool_use_id)
		VALUES ($1, 0, 1, 'Bash', 'Bash', 'tu1')`,
		sid,
	); err != nil {
		t.Fatalf("insert tool_call 2: %v", err)
	}
	if _, err := pg.Exec(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, source, status, content, event_index)
		VALUES ($1, 0, 1, 'tu1', 'stdout', 'ok',
			'event-secret-value', 0)`,
		sid,
	); err != nil {
		t.Fatalf("insert tool_result_event: %v", err)
	}

	cases := []struct {
		name string
		f    db.SecretFinding
		want string
		ok   bool
	}{
		{"message",
			db.SecretFinding{SessionID: sid, LocationKind: "message",
				MessageOrdinal: 0},
			"key AKIA7QHWN2DKR4FYPLJM here", true},
		{"tool_input",
			db.SecretFinding{SessionID: sid, LocationKind: "tool_input",
				MessageOrdinal: 0, CallIndex: ptr(0)},
			`{"command":"printenv"}`, true},
		{"tool_result",
			db.SecretFinding{SessionID: sid, LocationKind: "tool_result",
				MessageOrdinal: 0, CallIndex: ptr(0)},
			"AWS_SECRET=topsecretvalue123", true},
		{"tool_result_event",
			db.SecretFinding{SessionID: sid, LocationKind: "tool_result_event",
				MessageOrdinal: 0, CallIndex: ptr(1), EventIndex: ptr(0)},
			"event-secret-value", true},
		{"missing ordinal",
			db.SecretFinding{SessionID: sid, LocationKind: "message",
				MessageOrdinal: 99},
			"", false},
		{"call index out of range",
			db.SecretFinding{SessionID: sid, LocationKind: "tool_input",
				MessageOrdinal: 0, CallIndex: ptr(9)},
			"", false},
		{"tool_result skipped when events present",
			db.SecretFinding{SessionID: sid, LocationKind: "tool_result",
				MessageOrdinal: 0, CallIndex: ptr(1)},
			"", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := store.SecretFindingSource(ctx, tc.f)
			if err != nil {
				t.Fatalf("SecretFindingSource: %v", err)
			}
			if ok != tc.ok || got != tc.want {
				t.Errorf("got (%q, %v), want (%q, %v)",
					got, ok, tc.want, tc.ok)
			}
		})
	}
}

func ptr(i int) *int { return &i }
