//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreSearchILIKE(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "hello",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) == 0 {
		t.Error("expected at least 1 search result")
	}
	for _, r := range page.Results {
		if r.Agent == "" {
			t.Error("Agent field is empty, want populated")
		}
		if r.SessionEndedAt == "" {
			t.Error("SessionEndedAt is empty, want populated")
		}
	}
}

func TestPGSearchDeduplication(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	// store-test-001 has 2 messages; searching "hello" only matches ordinal 0.
	// With session grouping, should return exactly 1 result.
	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "hello",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 1 {
		t.Errorf("got %d results, want 1 (deduplicated to session)", len(page.Results))
	}
}

func TestPGSearchRecencySort(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	// Open a write connection to insert additional test data.
	pg, err := Open(pgURL, testSchema, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	// Insert a newer session that also matches "hello".
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			('recency-test-002', 'test-machine',
			 'test-project', 'codex',
			 'hello again',
			 '2026-04-01T10:00:00Z'::timestamptz,
			 '2026-04-01T10:30:00Z'::timestamptz,
			 1, 1)
		ON CONFLICT (id) DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting newer session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length)
		VALUES
			('recency-test-002', 0, 'user',
			 'hello again newer',
			 '2026-04-01T10:00:00Z'::timestamptz, 17)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting newer message: %v", err)
	}

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "hello",
		Limit: 10,
		Sort:  "recency",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) < 2 {
		t.Fatalf("want >= 2 results, got %d", len(page.Results))
	}
	// recency-test-002 has ended_at 2026-04-01, store-test-001 has 2026-03-12
	if page.Results[0].SessionID != "recency-test-002" {
		t.Errorf("recency sort: first result = %q, want %q",
			page.Results[0].SessionID, "recency-test-002")
	}
}

func TestPGSearchRelevanceSort(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	// Insert two sessions:
	// - relevance-early: match appears at position 1 (start of content)
	// - relevance-late: match appears after 50 chars of prefix
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			('relevance-early', 'test-machine',
			 'test-project', 'claude',
			 'needle at start',
			 '2025-01-01T10:00:00Z'::timestamptz,
			 '2025-01-01T10:30:00Z'::timestamptz,
			 1, 1),
			('relevance-late', 'test-machine',
			 'test-project', 'claude',
			 'lots of text before needle',
			 '2025-01-02T10:00:00Z'::timestamptz,
			 '2025-01-02T10:30:00Z'::timestamptz,
			 1, 1)
		ON CONFLICT (id) DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting sessions: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('relevance-early', 0, 'user',
			 'needleunique at the very beginning of content',
			 '2025-01-01T10:00:00Z'::timestamptz, 45),
			('relevance-late', 0, 'user',
			 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaneedleunique at the end',
			 '2025-01-02T10:00:00Z'::timestamptz, 73)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting messages: %v", err)
	}

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "needleunique",
		Limit: 10,
		Sort:  "relevance",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) < 2 {
		t.Fatalf("want >= 2 results, got %d", len(page.Results))
	}
	// relevance-early has match at position 1; relevance-late has it after 50 chars
	// relevance sort = match_pos ASC, so relevance-early must come first
	if page.Results[0].SessionID != "relevance-early" {
		t.Errorf("relevance sort: first result = %q, want %q",
			page.Results[0].SessionID, "relevance-early")
	}
}

func TestPGSearchNullTimestampSorting(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	// Insert a session with NULL ended_at and started_at.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 message_count, user_message_count)
		VALUES
			('null-ts-001', 'test-machine',
			 'test-project', 'claude',
			 'nullsort keyword here',
			 1, 1)
		ON CONFLICT (id) DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting null-ts session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length)
		VALUES
			('null-ts-001', 0, 'user',
			 'nullsort keyword here',
			 '2026-01-01T00:00:00Z'::timestamptz, 21)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting null-ts message: %v", err)
	}

	// Insert a session with real timestamps.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at,
			 message_count, user_message_count)
		VALUES
			('null-ts-002', 'test-machine',
			 'test-project', 'claude',
			 'nullsort keyword here',
			 '2026-03-10T10:00:00Z'::timestamptz,
			 '2026-03-10T10:30:00Z'::timestamptz,
			 1, 1)
		ON CONFLICT (id) DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting real-ts session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length)
		VALUES
			('null-ts-002', 0, 'user',
			 'nullsort keyword here',
			 '2026-03-10T10:00:00Z'::timestamptz, 21)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting real-ts message: %v", err)
	}

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Test recency sort: NULL-timestamp session must not be first.
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "nullsort",
		Limit: 10,
		Sort:  "recency",
	})
	if err != nil {
		t.Fatalf("recency search: %v", err)
	}
	if len(page.Results) < 2 {
		t.Fatalf("want >= 2 results, got %d", len(page.Results))
	}
	if page.Results[0].SessionID == "null-ts-001" {
		t.Error("recency: NULL-timestamp session appeared first, want last")
	}

	// Test relevance sort: both have same match_pos, so
	// session_ended_at DESC is the tie-breaker — NULL must not win.
	page, err = store.Search(ctx, db.SearchFilter{
		Query: "nullsort",
		Limit: 10,
		Sort:  "relevance",
	})
	if err != nil {
		t.Fatalf("relevance search: %v", err)
	}
	if len(page.Results) < 2 {
		t.Fatalf("want >= 2 results, got %d", len(page.Results))
	}
	if page.Results[0].SessionID == "null-ts-001" {
		t.Error("relevance: NULL-timestamp session appeared first, want last")
	}
}

func TestPGSearchSystemMessageExcluded(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	// Insert a session whose only matching message is a system message.
	// first_message intentionally does NOT contain the search term so the
	// name branch does not accidentally surface this session.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			('sysonly-session', 'test-machine',
			 'test-project', 'claude',
			 'hello world',
			 '2026-03-01T10:00:00Z'::timestamptz,
			 '2026-03-01T10:30:00Z'::timestamptz,
			 1, 0)
		ON CONFLICT (id) DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length, is_system)
		VALUES
			('sysonly-session', 0, 'user',
			 'sysonly unique term',
			 '2026-03-01T10:00:00Z'::timestamptz, 19, TRUE)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting system message: %v", err)
	}

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "sysonly unique",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 0 {
		t.Errorf("got %d results for system-only session, want 0",
			len(page.Results))
	}
}

// TestPGSearchNameBranchExcludesSystemOnlySessions verifies that a
// session whose display_name or first_message matches the search query
// does not appear in global search results when all its messages are
// system messages (the EXISTS guard in the name branch).
func TestPGSearchNameBranchExcludesSystemOnlySessions(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	// Session with only display_name matching, system messages only.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 display_name, started_at, ended_at,
			 message_count, user_message_count)
		VALUES
			('name-sysonly-dn', 'test-machine',
			 'test-project', 'claude',
			 'no match here',
			 'pgdnguardterm display',
			 '2026-03-10T10:00:00Z'::timestamptz,
			 '2026-03-10T10:30:00Z'::timestamptz,
			 1, 0)
		ON CONFLICT (id) DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting dn session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length, is_system)
		VALUES
			('name-sysonly-dn', 0, 'user',
			 'irrelevant content',
			 '2026-03-10T10:00:00Z'::timestamptz, 19, TRUE)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting dn system message: %v", err)
	}

	// Session with only first_message matching, system messages only.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent,
			 first_message, started_at, ended_at,
			 message_count, user_message_count)
		VALUES
			('name-sysonly-fm', 'test-machine',
			 'test-project', 'claude',
			 'pgfmguardterm first msg',
			 '2026-03-11T10:00:00Z'::timestamptz,
			 '2026-03-11T10:30:00Z'::timestamptz,
			 1, 0)
		ON CONFLICT (id) DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting fm session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length, is_system)
		VALUES
			('name-sysonly-fm', 0, 'user',
			 'irrelevant content',
			 '2026-03-11T10:00:00Z'::timestamptz, 19, TRUE)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting fm system message: %v", err)
	}

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	t.Run("display_name path", func(t *testing.T) {
		page, err := store.Search(ctx, db.SearchFilter{
			Query: "pgdnguardterm",
			Limit: 10,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(page.Results) != 0 {
			t.Errorf("got %d results for system-only session via display_name, want 0",
				len(page.Results))
		}
	})

	t.Run("first_message path", func(t *testing.T) {
		page, err := store.Search(ctx, db.SearchFilter{
			Query: "pgfmguardterm",
			Limit: 10,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(page.Results) != 0 {
			t.Errorf("got %d results for system-only session via first_message, want 0",
				len(page.Results))
		}
	})
}

// TestPGSearchSessionExcludesSystemMessages verifies that SearchSession
// (the in-session Cmd+F find-bar) excludes system messages since the
// frontend hides them and matching would produce phantom highlights.
func TestPGSearchSessionExcludesSystemMessages(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	// Insert a session with one regular and one system message, both
	// containing the search term "syssearch".
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('sess-syssearch', 'test-machine',
			 'test-project', 'claude',
			 'syssearch regular',
			 NOW(), 2, 1)
		ON CONFLICT (id) DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length, is_system)
		VALUES
			('sess-syssearch', 0, 'user',
			 'syssearch regular content',
			 NOW(), 25, FALSE),
			('sess-syssearch', 1, 'assistant',
			 'syssearch system-only content',
			 NOW(), 29, TRUE)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		t.Fatalf("inserting messages: %v", err)
	}

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	ordinals, err := store.SearchSession(ctx, "sess-syssearch", "syssearch")
	if err != nil {
		t.Fatalf("SearchSession: %v", err)
	}
	if len(ordinals) != 1 {
		t.Fatalf("got %d ordinals, want 1 (session search excludes system messages): %v",
			len(ordinals), ordinals)
	}
	if ordinals[0] != 0 {
		t.Errorf("got ordinals %v, want [0]", ordinals)
	}
}

func TestPGSearchSessionNameMatch(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	// Insert session with display_name containing unique search term,
	// no messages match.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message, display_name,
			 started_at, message_count, user_message_count)
		VALUES
			('name-match-001', 'test-machine', 'test-project', 'claude',
			 'first msg text', 'uniquedisplayterm session',
			 '2026-03-15T10:00:00Z'::timestamptz, 1, 1)
		ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('name-match-001', 0, 'user', 'no match here',
			 '2026-03-15T10:00:00Z'::timestamptz, 13)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	page, err := store.Search(context.Background(), db.SearchFilter{
		Query: "uniquedisplayterm", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(page.Results))
	}
	r := page.Results[0]
	if r.SessionID != "name-match-001" {
		t.Errorf("got session %q, want name-match-001", r.SessionID)
	}
	if r.Ordinal != -1 {
		t.Errorf("ordinal = %d, want -1 (name-only match)", r.Ordinal)
	}
	if r.Name == "" {
		t.Error("Name field is empty")
	}
}

func TestPGSearchRecencyNameOnlyBeatsOlderContent(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	// older-content-001: message matches "recencytestterm", older timestamp
	// newer-name-001: display_name matches "recencytestterm", newer timestamp
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('older-content-recency', 'test-machine', 'test-project', 'claude',
			 'first msg', '2026-01-01T10:00:00Z'::timestamptz, 1, 1),
			('newer-name-recency', 'test-machine', 'test-project', 'claude',
			 'first msg', '2026-01-02T10:00:00Z'::timestamptz, 1, 1)
		ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		t.Fatalf("insert sessions: %v", err)
	}
	_, err = pg.Exec(`
		UPDATE sessions SET display_name = 'recencytestterm session'
		WHERE id = 'newer-name-recency'`)
	if err != nil {
		t.Fatalf("set display_name: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('older-content-recency', 0, 'user', 'recencytestterm content',
			 '2026-01-01T10:00:00Z'::timestamptz, 22),
			('newer-name-recency', 0, 'user', 'no match here',
			 '2026-01-02T10:00:00Z'::timestamptz, 13)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	page, err := store.Search(context.Background(), db.SearchFilter{
		Query: "recencytestterm", Limit: 10, Sort: "recency",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) < 2 {
		t.Fatalf("got %d results, want >= 2", len(page.Results))
	}
	// Recency mode: newer session (name-only) must appear before older content match.
	if page.Results[0].SessionID != "newer-name-recency" {
		t.Errorf("recency sort: first result = %q, want newer-name-recency (name-only but newer)",
			page.Results[0].SessionID)
	}
}

// TestPGSearchSnippetMatchingField verifies that when a session has a
// display_name set but the search term only matches first_message, the
// snippet returned is the first_message (the matching field), not the
// display_name.
func TestPGSearchSnippetMatchingField(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	// Session: display_name is set to something unrelated; only first_message
	// contains the search term.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message, display_name,
			 started_at, message_count, user_message_count)
		VALUES
			('snippet-field-001', 'test-machine', 'test-project', 'claude',
			 'snippetfieldterm in first message', 'unrelated display name',
			 '2026-03-16T10:00:00Z'::timestamptz, 1, 1)
		ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	// Message that does NOT contain the search term.
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('snippet-field-001', 0, 'user', 'no match here',
			 '2026-03-16T10:00:00Z'::timestamptz, 13)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	page, err := store.Search(context.Background(), db.SearchFilter{
		Query: "snippetfieldterm", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(page.Results))
	}
	r := page.Results[0]
	if r.SessionID != "snippet-field-001" {
		t.Errorf("got session %q, want snippet-field-001", r.SessionID)
	}
	if r.Ordinal != -1 {
		t.Errorf("ordinal = %d, want -1 (name-only match)", r.Ordinal)
	}
	// Snippet must be first_message (the matching field), not display_name.
	if r.Snippet != "snippetfieldterm in first message" {
		t.Errorf("snippet = %q, want first_message text", r.Snippet)
	}
}

// TestGetMessagesIsSystemField verifies that GetMessages and GetAllMessages
// correctly populate db.Message.IsSystem from the is_system column.
func TestGetMessagesIsSystemField(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_is_system_test"
	pg, err := Open(pgURL, schema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	ctx := context.Background()
	if _, err := pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if err := EnsureSchema(ctx, pg, schema); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('is-system-001', 'test-machine', 'test-project', 'claude',
			 'hello', '2026-03-16T10:00:00Z'::timestamptz, 2, 1)
		ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length, is_system)
		VALUES
			('is-system-001', 0, 'user', 'normal message',
			 '2026-03-16T10:00:00Z'::timestamptz, 14, FALSE),
			('is-system-001', 1, 'user', 'system message',
			 '2026-03-16T10:00:01Z'::timestamptz, 14, TRUE)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	store, err := NewStore(pgURL, schema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	// GetMessages
	msgs, err := store.GetMessages(ctx, "is-system-001", 0, 10, true)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("GetMessages: got %d messages, want 2", len(msgs))
	}
	if msgs[0].IsSystem {
		t.Errorf("GetMessages: msgs[0].IsSystem = true, want false")
	}
	if !msgs[1].IsSystem {
		t.Errorf("GetMessages: msgs[1].IsSystem = false, want true")
	}

	// GetAllMessages
	all, err := store.GetAllMessages(ctx, "is-system-001")
	if err != nil {
		t.Fatalf("GetAllMessages: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("GetAllMessages: got %d messages, want 2", len(all))
	}
	if all[0].IsSystem {
		t.Errorf("GetAllMessages: all[0].IsSystem = true, want false")
	}
	if !all[1].IsSystem {
		t.Errorf("GetAllMessages: all[1].IsSystem = false, want true")
	}
}

// TestGetMessagesIDPopulated regresses #439: scanPGMessages must
// populate db.Message.ID with a unique-within-session value that
// matches int64(ordinal). The frontend keys {#each messages
// (message.id)} on it, and joins with TurnRow.MessageID via
// turnByMessage.get(message.id) — so collisions or zero-fill
// crash the message panel with each_key_duplicate. PG has no id
// column (composite PK on session_id, ordinal), so the synthetic
// ID matches the existing convention used by TurnRow.MessageID
// and CallRow.MessageID in session_timing.go.
func TestGetMessagesIDPopulated(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_msg_id_test"
	pg, err := Open(pgURL, schema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	ctx := context.Background()
	if _, err := pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if err := EnsureSchema(ctx, pg, schema); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	if _, err := pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('msg-id-001', 'test-machine', 'test-project', 'claude',
			 'hello', '2026-03-16T10:00:00Z'::timestamptz, 4, 2)`,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length, has_tool_use)
		VALUES
			('msg-id-001', 0, 'user', 'first',
			 '2026-03-16T10:00:00Z'::timestamptz, 5, FALSE),
			('msg-id-001', 1, 'assistant', 'second',
			 '2026-03-16T10:00:01Z'::timestamptz, 6, TRUE),
			('msg-id-001', 2, 'user', 'third',
			 '2026-03-16T10:00:02Z'::timestamptz, 5, FALSE),
			('msg-id-001', 3, 'assistant', 'fourth',
			 '2026-03-16T10:00:03Z'::timestamptz, 6, TRUE)`,
	); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	store, err := NewStore(pgURL, schema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	check := func(t *testing.T, label string, msgs []db.Message) {
		t.Helper()
		if len(msgs) != 4 {
			t.Fatalf("%s: got %d messages, want 4", label, len(msgs))
		}
		seen := map[int64]int{}
		for i, m := range msgs {
			if prev, ok := seen[m.ID]; ok {
				t.Errorf("%s: msgs[%d].ID == msgs[%d].ID == %d (duplicate keys would crash Svelte each, regression of #439)",
					label, prev, i, m.ID)
			}
			seen[m.ID] = i
			if m.ID != int64(m.Ordinal) {
				t.Errorf("%s: msgs[%d].ID = %d, want int64(ordinal=%d) for parity with TurnRow.MessageID",
					label, i, m.ID, m.Ordinal)
			}
		}
	}

	msgs, err := store.GetMessages(ctx, "msg-id-001", 0, 100, true)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	check(t, "GetMessages", msgs)

	all, err := store.GetAllMessages(ctx, "msg-id-001")
	if err != nil {
		t.Fatalf("GetAllMessages: %v", err)
	}
	check(t, "GetAllMessages", all)

	// Cross-check: every TurnRow.MessageID must correspond to a message
	// returned by GetMessages with the same ID. The frontend looks up
	// turns via turnByMessage.get(message.id); messages without a turn
	// are allowed (lookup returns undefined), but a turn whose
	// MessageID has no matching message means the join will silently
	// drop timing data.
	timing, err := store.GetSessionTiming(ctx, "msg-id-001")
	if err != nil {
		t.Fatalf("GetSessionTiming: %v", err)
	}
	if timing == nil {
		t.Fatal("GetSessionTiming returned nil")
	}
	if len(timing.Turns) == 0 {
		t.Fatal("GetSessionTiming.Turns empty; cannot verify cross-check")
	}
	msgIDs := map[int64]bool{}
	for _, m := range msgs {
		msgIDs[m.ID] = true
	}
	for _, turn := range timing.Turns {
		if !msgIDs[turn.MessageID] {
			t.Errorf("turn.MessageID=%d has no matching message.ID; "+
				"frontend turnByMessage join will drop this turn",
				turn.MessageID)
		}
	}
}
