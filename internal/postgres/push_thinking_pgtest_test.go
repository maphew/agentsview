//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

// TestPushThinkingText_SanitizesNullAndInvalidUTF8 verifies that
// bulkInsertMessages runs ThinkingText through sanitizePG before
// sending it to PostgreSQL. Without the sanitize call, a NUL byte
// or invalid UTF-8 in a thinking block would make the PG INSERT
// reject the entire batch and stall the push.
func TestPushThinkingText_SanitizesNullAndInvalidUTF8(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"thinking-test-machine", true,
		SyncOptions{},
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	started := time.Now().UTC().Format(time.RFC3339)
	first := "hello"
	sess := db.Session{
		ID:           "think-1",
		Project:      "proj",
		Machine:      "local",
		Agent:        "claude",
		FirstMessage: &first,
		StartedAt:    &started,
		MessageCount: 1,
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Message whose thinking_text contains a NUL byte and a
	// truncated multi-byte UTF-8 sequence. Before the fix the
	// insert would fail with "invalid byte sequence".
	thinking := "plan\x00step\xe2"
	if err := local.InsertMessages([]db.Message{{
		SessionID:    "think-1",
		Ordinal:      0,
		Role:         "assistant",
		Content:      "ok",
		ThinkingText: thinking,
		HasThinking:  true,
	}}); err != nil {
		t.Fatalf("insert local message: %v", err)
	}

	if _, err := ps.Push(ctx, false, nil); err != nil {
		t.Fatalf("push: %v", err)
	}

	store, err := NewStore(pgURL, "agentsview", true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	msgs, err := store.GetMessages(ctx, "think-1", 0, 10, true)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	// NUL bytes and invalid UTF-8 must be stripped; the
	// remaining text stays intact and in order.
	if got, want := msgs[0].ThinkingText, "planstep"; got != want {
		t.Errorf(
			"ThinkingText = %q, want %q (sanitize skipped?)",
			got, want,
		)
	}
}
