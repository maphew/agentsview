//go:build pgtest

package postgres

import (
	"context"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/db"
)

func prepareUsageSchema(
	t *testing.T, schema string,
) (string, *Store) {
	t.Helper()

	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = pg.Close() })

	ctx := context.Background()
	if _, err := pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if err := EnsureSchema(ctx, pg, schema); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	store, err := NewStore(pgURL, schema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return pgURL, store
}

func TestStoreGetDailyUsageUsesFallbackPricing(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_fallback_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'usage-fallback-001', 'test-machine', 'proj', 'claude',
			'2026-03-12T10:00:00Z'::timestamptz, 1, 1
		)`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage
		) VALUES (
			'usage-fallback-001', 0, 'assistant', 'hi',
			'2026-03-12T10:00:00Z'::timestamptz, 2,
			'claude-sonnet-4-20250514',
			'{"input_tokens":1000000}'
		)`)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:     "2026-03-12",
		To:       "2026-03-12",
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("GetDailyUsage: %v", err)
	}
	if got := result.Totals.TotalCost; got != 3.0 {
		t.Fatalf("TotalCost = %.2f, want 3.0", got)
	}
	if len(result.Daily) != 1 {
		t.Fatalf("daily len = %d, want 1", len(result.Daily))
	}
}

func TestStoreGetDailyUsageWithBreakdowns(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_breakdown_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES
			('test-model-a', 1, 2, 3, 0.5, 'seed'),
			('test-model-b', 2, 4, 0, 0, 'seed')`)
	if err != nil {
		t.Fatalf("insert pricing: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('usage-breakdown-001', 'test-machine', 'proj-a', 'claude',
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('usage-breakdown-002', 'test-machine', 'proj-b', 'codex',
			 '2026-03-12T11:00:00Z'::timestamptz, 1, 1)`)
	if err != nil {
		t.Fatalf("insert sessions: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES
			('usage-breakdown-001', 0, 'assistant', 'one',
			 '2026-03-12T10:00:00Z'::timestamptz, 3,
			 'test-model-a',
			 '{"input_tokens":1000000,"output_tokens":500000,"cache_creation_input_tokens":250000,"cache_read_input_tokens":250000}'),
			('usage-breakdown-002', 0, 'assistant', 'two',
			 '2026-03-12T11:00:00Z'::timestamptz, 3,
			 'test-model-b',
			 '{"input_tokens":500000,"output_tokens":250000}')`)
	if err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:       "2026-03-12",
		To:         "2026-03-12",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	if err != nil {
		t.Fatalf("GetDailyUsage: %v", err)
	}
	if len(result.Daily) != 1 {
		t.Fatalf("daily len = %d, want 1", len(result.Daily))
	}
	day := result.Daily[0]
	if got, want := day.InputTokens, 1500000; got != want {
		t.Fatalf("InputTokens = %d, want %d", got, want)
	}
	if got, want := day.OutputTokens, 750000; got != want {
		t.Fatalf("OutputTokens = %d, want %d", got, want)
	}
	if got, want := len(day.ProjectBreakdowns), 2; got != want {
		t.Fatalf("ProjectBreakdowns len = %d, want %d", got, want)
	}
	if got, want := len(day.AgentBreakdowns), 2; got != want {
		t.Fatalf("AgentBreakdowns len = %d, want %d", got, want)
	}
	if got, want := len(day.ModelBreakdowns), 2; got != want {
		t.Fatalf("ModelBreakdowns len = %d, want %d", got, want)
	}
	if day.TotalCost <= 0 {
		t.Fatalf("TotalCost = %.4f, want > 0", day.TotalCost)
	}
}

func TestStoreGetTopSessionsByCostDedupesClaudeKeys(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_top_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('test-model-top', 1, 0, 0, 0, 'seed')`)
	if err != nil {
		t.Fatalf("insert pricing: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('usage-top-001', 'test-machine', 'proj-a', 'claude',
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('usage-top-002', 'test-machine', 'proj-b', 'claude',
			 '2026-03-12T10:01:00Z'::timestamptz, 1, 1)`)
	if err != nil {
		t.Fatalf("insert sessions: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('usage-top-001', 0, 'assistant', 'one',
			 '2026-03-12T10:00:00Z'::timestamptz, 3,
			 'test-model-top', '{"input_tokens":1000000}', 'msg-1', 'req-1'),
			('usage-top-002', 0, 'assistant', 'two',
			 '2026-03-12T10:01:00Z'::timestamptz, 3,
			 'test-model-top', '{"input_tokens":1000000}', 'msg-1', 'req-1')`)
	if err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	top, err := store.GetTopSessionsByCost(ctx, db.UsageFilter{
		From:     "2026-03-12",
		To:       "2026-03-12",
		Timezone: "UTC",
	}, 20)
	if err != nil {
		t.Fatalf("GetTopSessionsByCost: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("top len = %d, want 1", len(top))
	}
	if top[0].SessionID != "usage-top-001" {
		t.Fatalf("top[0].SessionID = %q, want usage-top-001", top[0].SessionID)
	}
}

func TestStoreGetUsageSessionCountsDedupesClaudeKeys(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_counts_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('usage-counts-001', 'test-machine', 'proj-a', 'claude',
			 '2026-03-12T10:00:00Z'::timestamptz, 1, 1),
			('usage-counts-002', 'test-machine', 'proj-b', 'claude',
			 '2026-03-12T10:01:00Z'::timestamptz, 1, 1)`)
	if err != nil {
		t.Fatalf("insert sessions: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id
		) VALUES
			('usage-counts-001', 0, 'assistant', 'one',
			 '2026-03-12T10:00:00Z'::timestamptz, 3,
			 'test-model-counts', '{"input_tokens":1}', 'msg-1', 'req-1'),
			('usage-counts-002', 0, 'assistant', 'two',
			 '2026-03-12T10:01:00Z'::timestamptz, 3,
			 'test-model-counts', '{"input_tokens":1}', 'msg-1', 'req-1')`)
	if err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	counts, err := store.GetUsageSessionCounts(ctx, db.UsageFilter{
		From:     "2026-03-12",
		To:       "2026-03-12",
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("GetUsageSessionCounts: %v", err)
	}
	if counts.Total != 1 {
		t.Fatalf("Total = %d, want 1", counts.Total)
	}
	if counts.ByProject["proj-a"] != 1 {
		t.Fatalf("ByProject[proj-a] = %d, want 1", counts.ByProject["proj-a"])
	}
	if _, ok := counts.ByProject["proj-b"]; ok {
		t.Fatalf("proj-b should have been deduped out: %#v", counts.ByProject)
	}
}

func TestPostgresUsageQueriesUnionUsageEvents(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_usage_events_union_test")

	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES
			('claude-sonnet-4-20250514', 1, 1, 1, 1, 'seed'),
			('gpt-5.4', 1, 1, 1, 1, 'seed')`)
	if err != nil {
		t.Fatalf("insert pricing: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES
			('claude-msg', 'test-machine', 'proj-a', 'claude',
			 '2026-05-14T09:00:00Z'::timestamptz, 1, 1),
			('hermes-event', 'test-machine', 'proj-b', 'hermes',
			 '2026-05-14T10:00:00Z'::timestamptz, 1, 1),
			('hermes-event-2', 'test-machine', 'proj-b', 'hermes',
			 '2026-05-14T10:10:00Z'::timestamptz, 1, 1)`)
	if err != nil {
		t.Fatalf("insert sessions: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage
		) VALUES
			('claude-msg', 0, 'assistant', 'one',
			 '2026-05-14T09:05:00Z'::timestamptz, 3,
			 'claude-sonnet-4-20250514',
			 '{"input_tokens":100,"output_tokens":40}')`)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO usage_events (
			session_id, source, model, input_tokens, output_tokens,
			cache_read_input_tokens, occurred_at, dedup_key
		) VALUES
			('hermes-event', 'session', 'gpt-5.4', 300, 70, 20,
			 '2026-05-14T10:05:00Z'::timestamptz, 'shared-key'),
			('hermes-event-2', 'session', 'gpt-5.4', 50, 5, 0,
			 '2026-05-14T10:10:00Z'::timestamptz, 'shared-key')`)
	if err != nil {
		t.Fatalf("insert usage event: %v", err)
	}

	filter := db.UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Timezone:   "UTC",
		Breakdowns: true,
	}
	result, err := store.GetDailyUsage(ctx, filter)
	if err != nil {
		t.Fatalf("GetDailyUsage: %v", err)
	}
	if got, want := result.Totals.InputTokens, 450; got != want {
		t.Fatalf("InputTokens = %d, want %d", got, want)
	}
	if got, want := result.Totals.OutputTokens, 115; got != want {
		t.Fatalf("OutputTokens = %d, want %d", got, want)
	}
	if got, want := result.Totals.CacheReadTokens, 20; got != want {
		t.Fatalf("CacheReadTokens = %d, want %d", got, want)
	}
	if got, want := len(result.Daily[0].AgentBreakdowns), 2; got != want {
		t.Fatalf("AgentBreakdowns len = %d, want %d", got, want)
	}

	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	if err != nil {
		t.Fatalf("GetTopSessionsByCost: %v", err)
	}
	if got, want := len(top), 3; got != want {
		t.Fatalf("top len = %d, want %d", got, want)
	}
	if got, want := top[0].SessionID, "hermes-event"; got != want {
		t.Fatalf("top[0].SessionID = %q, want %q", got, want)
	}
	if got, want := top[0].TotalTokens, 390; got != want {
		t.Fatalf("top[0].TotalTokens = %d, want %d", got, want)
	}

	counts, err := store.GetUsageSessionCounts(ctx, filter)
	if err != nil {
		t.Fatalf("GetUsageSessionCounts: %v", err)
	}
	if got, want := counts.Total, 3; got != want {
		t.Fatalf("Total = %d, want %d", got, want)
	}
	if got, want := counts.ByAgent["hermes"], 2; got != want {
		t.Fatalf("ByAgent[hermes] = %d, want %d", got, want)
	}
	if got, want := counts.ByProject["proj-b"], 2; got != want {
		t.Fatalf("ByProject[proj-b] = %d, want %d", got, want)
	}
}

func TestPushSyncsModelPricingToPostgres(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	if err := local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "test-model-sync",
		InputPerMTok:         1.5,
		OutputPerMTok:        2.5,
		CacheCreationPerMTok: 3.5,
		CacheReadPerMTok:     0.5,
	}}); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}

	ps, err := New(pgURL, "agentsview", local, "test-machine", true, SyncOptions{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ps.Close()

	if _, err := ps.Push(context.Background(), false, nil); err != nil {
		t.Fatalf("Push: %v", err)
	}

	store, err := NewStore(pgURL, "agentsview", true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	rows, err := store.DB().QueryContext(context.Background(), `
		SELECT model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok
		FROM model_pricing
		WHERE model_pattern = 'test-model-sync'`)
	if err != nil {
		t.Fatalf("query pricing: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatal("expected synced pricing row")
	}
	var (
		model                                   string
		input, output, cacheCreation, cacheRead float64
	)
	if err := rows.Scan(
		&model, &input, &output, &cacheCreation, &cacheRead,
	); err != nil {
		t.Fatalf("scan pricing: %v", err)
	}
	if model != "test-model-sync" {
		t.Fatalf("model = %q, want test-model-sync", model)
	}
	if input != 1.5 || output != 2.5 || cacheCreation != 3.5 || cacheRead != 0.5 {
		t.Fatalf(
			"pricing row = (%v,%v,%v,%v), want (1.5,2.5,3.5,0.5)",
			input, output, cacheCreation, cacheRead,
		)
	}
}

func TestPushFallsBackToBuiltinPricingWhenLocalTableEmpty(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, "agentsview", local, "test-machine", true, SyncOptions{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ps.Close()

	if _, err := ps.Push(context.Background(), false, nil); err != nil {
		t.Fatalf("Push: %v", err)
	}

	store, err := NewStore(pgURL, "agentsview", true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	rows, err := store.DB().QueryContext(context.Background(), `
		SELECT model_pattern
		FROM model_pricing
		ORDER BY model_pattern`)
	if err != nil {
		t.Fatalf("query pricing: %v", err)
	}
	defer rows.Close()

	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			t.Fatalf("scan model: %v", err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	joined := strings.Join(models, ",")
	if !strings.Contains(joined, "claude-sonnet-4-20250514") {
		t.Fatalf("fallback pricing not synced: %s", joined)
	}
}
