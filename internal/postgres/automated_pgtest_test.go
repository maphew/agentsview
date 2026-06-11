//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPushSessionTrustsLocalIsAutomated verifies that
// pushSession copies sess.IsAutomated verbatim instead of
// re-running db.IsAutomatedSession on the first_message.
// Achieved by setting a user prefix locally, upserting a
// matching session (so IsAutomated=1), then clearing the
// user prefix BEFORE push and confirming the PG row stays
// IsAutomated=1.
func TestPushSessionTrustsLocalIsAutomated(t *testing.T) {
	t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)

	// Set a user prefix BEFORE inserting so UpsertSession
	// sets is_automated=1 on the SQLite row.
	db.SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	fm := "You are analyzing an essay about epistemology."
	require.NoError(t, local.UpsertSession(db.Session{
		ID:               "essay-1",
		Project:          "proj",
		Machine:          "local",
		Agent:            "claude",
		FirstMessage:     &fm,
		MessageCount:     2,
		UserMessageCount: 1,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}), "upsert")

	// Clear the user prefix so a recompute in pushSession
	// would now classify this row as is_automated=0. If
	// pushSession trusts the local value, PG sees =1 anyway.
	db.SetUserAutomationPrefixes(nil)

	ps, err := New(
		pgURL, "agentsview", local,
		"trust-test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()

	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")

	var got bool
	require.NoError(t, ps.DB().QueryRowContext(ctx,
		`SELECT is_automated FROM sessions WHERE id = $1`,
		"essay-1",
	).Scan(&got), "query pg")
	assert.True(t, got,
		"pushSession recomputed is_automated; expected to trust local value")
}

// TestBackfillIsAutomatedPGRerunsOnHashChange exercises the
// PG-side hash-driven backfill: after a classifier change
// (here, adding a user prefix), EnsureSchema re-runs the
// backfill and flips matching rows to is_automated=true.
func TestBackfillIsAutomatedPGRerunsOnHashChange(t *testing.T) {
	t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	fm := "You are analyzing an essay about epistemology."
	require.NoError(t, local.UpsertSession(db.Session{
		ID:               "essay-pg",
		Project:          "proj",
		Machine:          "local",
		Agent:            "claude",
		FirstMessage:     &fm,
		MessageCount:     2,
		UserMessageCount: 1,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}), "upsert")

	ps, err := New(
		pgURL, "agentsview", local,
		"backfill-test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()

	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")

	// Confirm the PG row starts as is_automated=false.
	var pre bool
	require.NoError(t, ps.DB().QueryRowContext(ctx,
		`SELECT is_automated FROM sessions WHERE id = $1`,
		"essay-pg",
	).Scan(&pre), "query pre")
	require.False(t, pre, "precondition: PG row should be is_automated=false")

	// Add a user prefix so the classifier hash changes,
	// then re-run the PG backfill directly (bypassing
	// Sync.EnsureSchema's memo so the second pass actually
	// executes). The matching row should flip to true.
	db.SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	require.NoError(t, backfillIsAutomatedPG(ctx, ps.DB()),
		"backfill after prefix add")

	var got bool
	require.NoError(t, ps.DB().QueryRowContext(ctx,
		`SELECT is_automated FROM sessions WHERE id = $1`,
		"essay-pg",
	).Scan(&got), "query post")
	assert.True(t, got,
		"PG row should be is_automated=true after backfill on hash change")
}
