//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// TestStoreGetAnalyticsSessionShape_AutonomyExcludesSystemUsers
// seeds a session with one real user message and several promoted
// Claude system messages (role='user' + is_system=true), plus four
// assistant tool-use messages. Autonomy = assistant-tool / real-user
// = 4/1 = 4.0, which must land in the "2-5" bucket. If the PG query
// counted promoted system rows as user turns, the ratio would be
// 4/(1+3) = 1.0 and fall in "1-2".
func TestStoreGetAnalyticsSessionShape_AutonomyExcludesSystemUsers(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"autonomy-test-machine", true,
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

	started := time.Now().UTC().Add(-1 * time.Hour).
		Format(time.RFC3339)
	first := "real user turn"
	sess := db.Session{
		ID:           "autonomy-1",
		Project:      "proj",
		Machine:      "local",
		Agent:        "claude",
		FirstMessage: &first,
		StartedAt:    &started,
		MessageCount: 8,
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	msgs := []db.Message{
		{
			SessionID: "autonomy-1", Ordinal: 0,
			Role: "user", Content: first,
		},
	}
	// 3 promoted system messages: role=user + is_system=true.
	for i, sub := range []string{
		"continuation", "interrupted", "task_notification",
	} {
		msgs = append(msgs, db.Message{
			SessionID:     "autonomy-1",
			Ordinal:       i + 1,
			Role:          "user",
			IsSystem:      true,
			SourceType:    "system",
			SourceSubtype: sub,
			Content:       "sys",
		})
	}
	// 4 assistant tool-use messages.
	for i := 0; i < 4; i++ {
		msgs = append(msgs, db.Message{
			SessionID:  "autonomy-1",
			Ordinal:    4 + i,
			Role:       "assistant",
			HasToolUse: true,
			Content:    "tool call",
		})
	}
	if err := local.InsertMessages(msgs); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	if _, err := ps.Push(ctx, false, nil); err != nil {
		t.Fatalf("push: %v", err)
	}

	store, err := NewStore(pgURL, "agentsview", true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	resp, err := store.GetAnalyticsSessionShape(
		ctx, db.AnalyticsFilter{},
	)
	if err != nil {
		t.Fatalf("GetAnalyticsSessionShape: %v", err)
	}

	// Promoted system rows must be excluded: ratio = 4/1 = 4.0 → "2-5".
	// Regression case (counts all role='user'): 4/4 = 1.0 → "1-2".
	want := map[string]int{"2-5": 1}
	for label, got := range mapAutonomy(resp.AutonomyDistribution) {
		if got != want[label] {
			t.Errorf(
				"AutonomyDistribution[%q] = %d, want %d (full: %+v)",
				label, got, want[label], resp.AutonomyDistribution,
			)
		}
	}
	if resp.AutonomyDistribution == nil ||
		bucketCount(resp.AutonomyDistribution, "1-2") != 0 {
		t.Errorf(
			"expected zero sessions in '1-2' bucket; got %+v",
			resp.AutonomyDistribution,
		)
	}
	if bucketCount(resp.AutonomyDistribution, "2-5") != 1 {
		t.Errorf(
			"expected 1 session in '2-5' bucket; got %+v",
			resp.AutonomyDistribution,
		)
	}
}

// mapAutonomy and bucketCount flatten the bucket slice so the
// assertions above read naturally regardless of bucket order.
func mapAutonomy(buckets []db.DistributionBucket) map[string]int {
	m := make(map[string]int, len(buckets))
	for _, b := range buckets {
		m[b.Label] = b.Count
	}
	return m
}

func bucketCount(buckets []db.DistributionBucket, label string) int {
	for _, b := range buckets {
		if b.Label == label {
			return b.Count
		}
	}
	return 0
}
