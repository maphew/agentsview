package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// seedSearchSession inserts one session with the given messages
// (role/content pairs) for content-search tests.
func seedSearchSession(t *testing.T, d *DB, id, project string, msgs [][2]string) {
	t.Helper()
	// UserMessageCount > 1 so the session is not treated as one-shot and
	// excluded by the default session-list-parity filter.
	insertSession(t, d, id, project, func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	var out []Message
	for i, rc := range msgs {
		out = append(out, Message{
			SessionID: id, Ordinal: i, Role: rc[0],
			Content: rc[1], Timestamp: "2026-05-20T12:00:0" + itoa(i) + "Z",
		})
	}
	if err := d.ReplaceSessionMessages(id, out); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
}

func TestSearchContentSubstringMessages(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "please find the DATABASE_URL value"},
		{"assistant", "sure, here is the answer"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "database_url", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}
	if len(got.Matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(got.Matches), got.Matches)
	}
	m := got.Matches[0]
	if m.SessionID != "s1" || m.Location != "message" || m.Ordinal != 0 {
		t.Errorf("unexpected match: %+v", m)
	}
	if m.Role != "user" {
		t.Errorf("Role = %q, want user", m.Role)
	}
	if !contains(m.Snippet, "DATABASE_URL") {
		t.Errorf("snippet missing matched text: %q", m.Snippet)
	}
}

// TestSearchContentRedactsStraddlingSecret pins the default (non-reveal)
// content-search guarantee: a secret adjacent to the match that extends past
// the snippet window must not leak. A snippet-only redaction would cut the PEM
// block short and ship raw key bytes.
func TestSearchContentRedactsStraddlingSecret(t *testing.T) {
	d := testDB(t)
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 5) +
		"-----END RSA PRIVATE KEY-----"
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "deploy with this attached key " + pem + " ok"},
		{"assistant", "done"},
	})
	base := ContentSearchFilter{
		Pattern: "attached key", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	}
	got, err := d.SearchContent(context.Background(), base)
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}
	if len(got.Matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(got.Matches))
	}
	if strings.Contains(got.Matches[0].Snippet, "SECRETKEYMATERIAL") {
		t.Errorf("default snippet leaked key material: %q", got.Matches[0].Snippet)
	}
	if !contains(got.Matches[0].Snippet, "attached key") {
		t.Errorf("snippet lost the matched context: %q", got.Matches[0].Snippet)
	}

	// Reveal opts out of redaction (localhost-gated upstream): raw bytes show.
	base.RevealSecrets = true
	rev, err := d.SearchContent(context.Background(), base)
	if err != nil {
		t.Fatalf("SearchContent reveal: %v", err)
	}
	if !strings.Contains(rev.Matches[0].Snippet, "SECRETKEYMATERIAL") {
		t.Errorf("reveal snippet should show raw bytes: %q", rev.Matches[0].Snippet)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestCaseInsensitiveIndexUnicodeOffset pins that the returned offset indexes
// the original string, not strings.ToLower(s). The Kelvin sign U+212A is three
// bytes but lowercases to one ('k'), so a ToLower-based index would report a
// byte offset shifted left of the real match position.
func TestCaseInsensitiveIndexUnicodeOffset(t *testing.T) {
	body := strings.Repeat("K", 5) + "match here"
	got := CaseInsensitiveIndex(body, "MATCH")
	want := strings.Index(body, "match") // real offset into the original string
	if got != want {
		t.Errorf("CaseInsensitiveIndex = %d, want %d (offset into original body)", got, want)
	}
}

// TestSubstringSnippetUnicodeOffset guards against the snippet panic and
// mis-centering when lowercasing changes byte length. U+023A lowercases to the
// 3-byte U+2C65, so a ToLower-derived offset runs past the original bounds and
// slicing panics; the offset-preserving search must center on the real match.
func TestSubstringSnippetUnicodeOffset(t *testing.T) {
	pat := "MATCH"
	body := strings.Repeat("Ⱥ", 100) + pat + " trailing context here"
	f := ContentSearchFilter{Pattern: pat, Mode: "substring"}
	got := f.substringSnippet(body) // must not panic
	if !strings.Contains(got, pat) {
		t.Errorf("snippet did not center on the match: %q", got)
	}
}

func TestSearchContentToolIO(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s2", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	msgs := []Message{{
		SessionID: "s2", Ordinal: 0, Role: "assistant", Content: "running it",
		Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
			InputJSON:     `{"command":"printenv"}`,
			ResultContent: "AWS_SECRET=topsecretvalue123",
		}},
	}}
	if err := d.ReplaceSessionMessages("s2", msgs); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
	// match in tool input
	in, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "printenv", Mode: "substring",
		Sources: []string{"tool_input"}, Limit: 50,
	})
	if err != nil || len(in.Matches) != 1 || in.Matches[0].Location != "tool_input" {
		t.Fatalf("tool_input search: %+v err=%v", in.Matches, err)
	}
	if in.Matches[0].ToolName != "Bash" {
		t.Errorf("ToolName = %q, want Bash", in.Matches[0].ToolName)
	}
	// match in tool result
	res, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "topsecretvalue", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	if err != nil || len(res.Matches) != 1 || res.Matches[0].Location != "tool_result" {
		t.Fatalf("tool_result search: %+v err=%v", res.Matches, err)
	}
}

// TestSearchContentEmptyToolUseIDNotSuppressed guards the tool-result dedup:
// when one empty-tool_use_id call has a result event, it must not suppress the
// result_content of a different empty-tool_use_id call. The dedup is keyed on
// tool_use_id, so one empty ID matching another would hide the second result.
func TestSearchContentEmptyToolUseIDNotSuppressed(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "empti", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	msgs := []Message{{
		SessionID: "empti", Ordinal: 0, Role: "assistant",
		Content: "running tools", Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{
			{ // empty tool_use_id, result only in result_content, no events
				ToolName: "Bash", Category: "Bash", ToolUseID: "",
				InputJSON: `{"command":"a"}`, ResultContent: "FINDA in result",
			},
			{ // empty tool_use_id, result delivered as an event
				ToolName: "Bash", Category: "Bash", ToolUseID: "",
				InputJSON: `{"command":"b"}`,
				ResultEvents: []ToolResultEvent{
					{Source: "stdout", Status: "ok", Content: "FINDB event"},
				},
			},
		},
	}}
	if err := d.ReplaceSessionMessages("empti", msgs); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
	// ReplaceSessionMessages routes empty ToolUseID through nilIfEmpty so
	// it lands as NULL. NULL = NULL is false in SQL, so the dedup bug we
	// want to pin (an empty string matching another empty string) only
	// triggers when the column actually holds ''. Force both rows to the
	// literal empty-string form here so the test fails on the old buggy
	// query.
	for _, sql := range []string{
		"UPDATE tool_calls SET tool_use_id = '' WHERE session_id = 'empti'",
		"UPDATE tool_result_events SET tool_use_id = '' WHERE session_id = 'empti'",
	} {
		if _, err := d.getWriter().Exec(sql); err != nil {
			t.Fatalf("force empty tool_use_id: %v", err)
		}
	}
	for _, mode := range []string{"substring", "regex"} {
		got, err := d.SearchContent(context.Background(), ContentSearchFilter{
			Pattern: "FINDA", Mode: mode,
			Sources: []string{"tool_result"}, Limit: 50,
		})
		if err != nil {
			t.Fatalf("SearchContent %s: %v", mode, err)
		}
		if len(got.Matches) != 1 || got.Matches[0].Location != "tool_result" {
			t.Fatalf("%s: empty-ID result_content suppressed: got %+v, want 1 tool_result",
				mode, got.Matches)
		}
	}
	// The event-delivered result is still searchable via the events branch.
	ev, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "FINDB", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent events: %v", err)
	}
	if len(ev.Matches) != 1 {
		t.Errorf("event content not found: got %+v", ev.Matches)
	}
}

// TestSearchContentPaginationStableAcrossTies seeds one message ordinal that
// produces three hits tying on (session, ordinal) — the message body, the tool
// input, and the tool result. Without a stable tie-break, OFFSET paging over
// the UNION could duplicate or skip these rows; the src/row_id keys make
// page-by-page retrieval reproduce the single-page order exactly.
func TestSearchContentPaginationStableAcrossTies(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "tie", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	msgs := []Message{{
		SessionID: "tie", Ordinal: 0, Role: "assistant",
		Content:   "FINDME in message body",
		Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
			InputJSON:     `{"command":"FINDME"}`,
			ResultContent: "FINDME in result",
		}},
	}}
	if err := d.ReplaceSessionMessages("tie", msgs); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
	base := ContentSearchFilter{
		Pattern: "FINDME", Mode: "substring",
		Sources: []string{"messages", "tool_input", "tool_result"},
	}
	full := base
	full.Limit = 50
	all, err := d.SearchContent(context.Background(), full)
	if err != nil {
		t.Fatalf("SearchContent full: %v", err)
	}
	if len(all.Matches) != 3 {
		t.Fatalf("want 3 tied matches, got %d: %+v", len(all.Matches), all.Matches)
	}
	// The tie-break orders the three sources deterministically by source rank.
	wantOrder := []string{"message", "tool_input", "tool_result"}
	for i, loc := range wantOrder {
		if all.Matches[i].Location != loc {
			t.Errorf("match %d Location = %q, want %q", i, all.Matches[i].Location, loc)
		}
	}
	// Page one row at a time; the sequence must equal the single-page order.
	var paged []ContentMatch
	for cursor := 0; ; {
		p := base
		p.Limit = 1
		p.Cursor = cursor
		page, err := d.SearchContent(context.Background(), p)
		if err != nil {
			t.Fatalf("SearchContent page at cursor %d: %v", cursor, err)
		}
		paged = append(paged, page.Matches...)
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	if len(paged) != len(all.Matches) {
		t.Fatalf("paged %d rows, want %d (duplicates or gaps)", len(paged), len(all.Matches))
	}
	for i := range all.Matches {
		if paged[i].Location != all.Matches[i].Location {
			t.Errorf("row %d: paged Location %q != single-page %q",
				i, paged[i].Location, all.Matches[i].Location)
		}
	}
}

func TestSearchContentRegex(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "r1", "proj", [][2]string{
		{"user", "key AKIA7QHWN2DKR4FYPLJM here"},
		{"assistant", "no secrets in this line"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: `AKIA[0-9A-Z]{16}`, Mode: "regex",
		Sources: []string{"messages"}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent regex: %v", err)
	}
	if len(got.Matches) != 1 || got.Matches[0].Ordinal != 0 {
		t.Fatalf("regex match = %+v, want 1 at ordinal 0", got.Matches)
	}
}

func TestSearchContentUnknownSource(t *testing.T) {
	d := testDB(t)
	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "x", Mode: "substring", Sources: []string{"messages", "bogus"},
	})
	if err == nil {
		t.Fatal("expected error for unknown source name")
	}
}

func TestSearchContentRegexInvalid(t *testing.T) {
	d := testDB(t)
	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: `(unclosed`, Mode: "regex", Sources: []string{"messages"},
	})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestSearchContentFTS(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "f1", "proj", [][2]string{
		{"user", "optimize the database query performance"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "optimize", Mode: "fts",
		Sources: []string{"messages"}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent fts: %v", err)
	}
	if len(got.Matches) != 1 || got.Matches[0].Location != "message" {
		t.Fatalf("fts match = %+v, want 1 message", got.Matches)
	}
}

func TestSearchContentFTSInvalidQuery(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "f2", "proj", [][2]string{
		{"user", "hello world"},
	})
	// A lone double quote is an unbalanced FTS phrase, so SQLite raises a
	// generic syntax error that must be classified as user input, not a 500.
	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: `"`, Mode: "fts",
		Sources: []string{"messages"}, Limit: 50,
	})
	var inputErr *SearchInputError
	if !errors.As(err, &inputErr) {
		t.Fatalf("malformed FTS query error = %v, want *SearchInputError", err)
	}
}

func TestSearchContentFTSUnavailable(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	// Drop the FTS table so HasFTS reports unavailable; the FTS search must
	// then fail with an internal (non-input) error rather than being
	// misclassified as an invalid user query (HTTP 400).
	if _, err := d.getWriter().Exec("DROP TABLE IF EXISTS messages_fts"); err != nil {
		t.Fatalf("drop messages_fts: %v", err)
	}
	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "x", Mode: "fts",
		Sources: []string{"messages"}, Limit: 50,
	})
	if err == nil {
		t.Fatal("expected error when FTS is unavailable")
	}
	var inputErr *SearchInputError
	if errors.As(err, &inputErr) {
		t.Errorf("FTS-unavailable misclassified as input error: %v", err)
	}
}

func TestSearchContentExcludeSystem(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s3", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	// Plain content (no legacy system-prefix string) so the exclusion is
	// driven solely by the persisted is_system flag, not SystemPrefixSQL.
	msgs := []Message{
		{SessionID: "s3", Ordinal: 0, Role: "user",
			Content: "ordinary message holding NEEDLE", IsSystem: true,
			Timestamp: "2026-05-20T12:00:00Z"},
	}
	if err := d.ReplaceSessionMessages("s3", msgs); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
	withSys, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent with system: %v", err)
	}
	if len(withSys.Matches) != 1 {
		t.Errorf("default should include system messages: got %d", len(withSys.Matches))
	}
	noSys, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"},
		ExcludeSystem: true, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent exclude system: %v", err)
	}
	if len(noSys.Matches) != 0 {
		t.Errorf("ExcludeSystem should drop system messages: got %d", len(noSys.Matches))
	}
}

func TestSearchContentExcludesAutomatedByDefault(t *testing.T) {
	d := testDB(t)
	// Automated sessions are single-turn by definition (UserMessageCount <= 1
	// plus a recognized first message, per sessionIsAutomated), so this one is
	// excluded by default. IncludeAutomated must re-include it via the one-shot
	// automated exemption — which only works if the automated flag is wired.
	insertSession(t, d, "auto", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 1
		fm := "Warmup"
		s.FirstMessage = &fm
	})
	msgs := []Message{{
		SessionID: "auto", Ordinal: 0, Role: "user",
		Content: "automated NEEDLE run", Timestamp: "2026-05-20T12:00:00Z",
	}}
	if err := d.ReplaceSessionMessages("auto", msgs); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
	def, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}
	if len(def.Matches) != 0 {
		t.Errorf("automated session should be excluded by default: got %d", len(def.Matches))
	}
	inc, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"},
		IncludeAutomated: true, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent include: %v", err)
	}
	if len(inc.Matches) != 1 {
		t.Errorf("IncludeAutomated should include the session: got %d", len(inc.Matches))
	}
}

func TestSearchContentExcludesOneShotByDefault(t *testing.T) {
	d := testDB(t)
	// A one-shot session: user_message_count <= 1.
	insertSession(t, d, "one", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 1
	})
	msgs := []Message{{
		SessionID: "one", Ordinal: 0, Role: "user",
		Content: "leaked NEEDLE token", Timestamp: "2026-05-20T12:00:00Z",
	}}
	if err := d.ReplaceSessionMessages("one", msgs); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
	def, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}
	if len(def.Matches) != 0 {
		t.Errorf("one-shot session should be excluded by default: got %d", len(def.Matches))
	}
	inc, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"},
		IncludeOneShot: true, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent include: %v", err)
	}
	if len(inc.Matches) != 1 {
		t.Errorf("IncludeOneShot should include the session: got %d", len(inc.Matches))
	}
}

func TestSearchContentToolResultDedup(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "dup", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	// The pattern appears in BOTH result_content and a result event. The
	// canonical rule must report it once, from the event branch.
	msgs := []Message{{
		SessionID: "dup", Ordinal: 0, Role: "assistant", Content: "run",
		Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
			InputJSON:     `{"command":"echo"}`,
			ResultContent: "DUPNEEDLE in result_content",
			ResultEvents: []ToolResultEvent{{
				ToolUseID: "tu1", Status: "success",
				Content: "DUPNEEDLE in event", EventIndex: 0,
			}},
		}},
	}}
	if err := d.ReplaceSessionMessages("dup", msgs); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "DUPNEEDLE", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}
	if len(got.Matches) != 1 {
		t.Fatalf("canonical dedup: got %d matches, want 1: %+v", len(got.Matches), got.Matches)
	}
	if !contains(got.Matches[0].Snippet, "event") {
		t.Errorf("expected the event content to win, got snippet %q", got.Matches[0].Snippet)
	}
}

func TestSearchContentCursorPagination(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "pg", "proj", [][2]string{
		{"user", "alpha NEEDLE one"},
		{"user", "beta NEEDLE two"},
		{"user", "gamma NEEDLE three"},
	})
	first, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"}, Limit: 2,
	})
	if err != nil {
		t.Fatalf("SearchContent page1: %v", err)
	}
	if len(first.Matches) != 2 || first.NextCursor != 2 {
		t.Fatalf("page1: got %d matches, cursor %d; want 2 and 2",
			len(first.Matches), first.NextCursor)
	}
	second, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"},
		Limit: 2, Cursor: first.NextCursor,
	})
	if err != nil {
		t.Fatalf("SearchContent page2: %v", err)
	}
	if len(second.Matches) != 1 || second.NextCursor != 0 {
		t.Fatalf("page2: got %d matches, cursor %d; want 1 and 0",
			len(second.Matches), second.NextCursor)
	}
}

func TestSearchContentMultiSourceWithProjectFilter(t *testing.T) {
	d := testDB(t)
	for _, p := range [][2]string{{"a", "alpha"}, {"b", "beta"}} {
		id, project := p[0], p[1]
		insertSession(t, d, id, project, func(s *Session) {
			s.Agent = "claude"
			s.UserMessageCount = 2
		})
		msgs := []Message{{
			SessionID: id, Ordinal: 0, Role: "assistant", Content: "FINDME here",
			Timestamp: "2026-05-20T12:00:00Z",
			ToolCalls: []ToolCall{{
				ToolName: "Bash", Category: "Bash", ToolUseID: id + "-tu",
				InputJSON: `{"command":"FINDME"}`, ResultContent: "out FINDME",
			}},
		}}
		if err := d.ReplaceSessionMessages(id, msgs); err != nil {
			t.Fatalf("ReplaceSessionMessages %s: %v", id, err)
		}
	}
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "FINDME", Mode: "substring",
		Sources: []string{"messages", "tool_input", "tool_result"},
		Project: "alpha", Limit: 50,
	})
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}
	if len(got.Matches) == 0 {
		t.Fatal("expected matches in project alpha")
	}
	for _, m := range got.Matches {
		if m.SessionID != "a" || m.Project != "alpha" {
			t.Errorf("project filter leaked match from %q (project %q)", m.SessionID, m.Project)
		}
	}
}

func TestSnippetWindowRuneBoundaries(t *testing.T) {
	// "é" and "ü" are two bytes each, so a byte-radius that lands inside them
	// would slice mid-rune. The match itself is ASCII; only the padding edges
	// are at risk.
	text := strings.Repeat("é", 5) + "MATCH" + strings.Repeat("ü", 5)
	start := strings.Index(text, "MATCH")
	end := start + len("MATCH")
	window := func(radius int) string {
		lo, hi := snippetBounds(text, start, end, radius)
		return text[lo:hi]
	}
	// radius 3 lands mid-rune on both sides, so the partial padding runes are
	// trimmed back to a boundary, leaving one whole rune of padding each side.
	got := window(3)
	if got != "éMATCHü" {
		t.Errorf("mid-rune radius = %q, want éMATCHü", got)
	}
	// radius 4 lands exactly on rune boundaries, so aligned padding is kept.
	if aligned := window(4); aligned != "ééMATCHüü" {
		t.Errorf("boundary-aligned radius = %q, want ééMATCHüü", aligned)
	}
	if !utf8.ValidString(got) {
		t.Errorf("snippet not valid UTF-8: %q", got)
	}
}

func TestFTSSnippetCentersOnPhrase(t *testing.T) {
	// A stray first token ("error") sits at the start; the real phrase ("error
	// handler") is far past the snippet radius. Centering on the first token
	// alone would window the stray match and drop the phrase.
	prefix := "error in the early part " + strings.Repeat("x ", 80)
	body := prefix + "the real error handler lives here"

	t.Run("phrase present centers on phrase", func(t *testing.T) {
		f := ContentSearchFilter{Pattern: `"error handler"`, Mode: "fts"}
		if snip := f.ftsSnippet(body); !strings.Contains(snip, "error handler") {
			t.Errorf("snippet did not center on the phrase: %q", snip)
		}
	})
	t.Run("phrase absent falls back to first token", func(t *testing.T) {
		// No contiguous "error handler" substring, so centering falls back to
		// the first token "error", windowing its early occurrence.
		f := ContentSearchFilter{Pattern: `"error nonexistent"`, Mode: "fts"}
		if snip := f.ftsSnippet(body); !strings.Contains(snip, "error in the early") {
			t.Errorf("fallback snippet not centered on first token: %q", snip)
		}
	})
}
