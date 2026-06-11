//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestListSessions_HasSecret verifies that the HasSecret filter
// returns only sessions where secret_leak_count > 0.
func TestListSessions_HasSecret(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	pg := store.DB()

	// Seed a session with leaks and one without.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count, secret_leak_count)
		VALUES
			('has-secret-leaky', 'test-machine', 'test-project',
			 'claude-code', 'secret session',
			 '2026-03-12T09:00:00Z'::timestamptz,
			 '2026-03-12T09:30:00Z'::timestamptz,
			 2, 1, 3),
			('has-secret-clean', 'test-machine', 'test-project',
			 'claude-code', 'clean session',
			 '2026-03-12T08:00:00Z'::timestamptz,
			 '2026-03-12T08:30:00Z'::timestamptz,
			 2, 1, 0)
	`)
	require.NoError(t, err, "inserting test sessions")

	ctx := context.Background()
	page, err := store.ListSessions(ctx, db.SessionFilter{
		HasSecret: true,
		Limit:     50,
	})
	require.NoError(t, err, "ListSessions")

	// Only the leaky session should appear.
	for _, s := range page.Sessions {
		assert.NotEqual(t, "has-secret-clean", s.ID,
			"clean session (secret_leak_count=0) included in HasSecret results")
	}

	var found *db.Session
	for i := range page.Sessions {
		if page.Sessions[i].ID == "has-secret-leaky" {
			found = &page.Sessions[i]
			break
		}
	}
	require.NotNil(t, found, "leaky session not found in HasSecret results")
	assert.Equal(t, 3, found.SecretLeakCount)

	_, err = pg.Exec(`
		UPDATE sessions
		SET secrets_rules_version = 'v-current'
		WHERE id = 'has-secret-leaky';
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count, secret_leak_count, secrets_rules_version)
		VALUES
			('has-secret-stale', 'test-machine', 'test-project',
			 'claude-code', 'stale secret session',
			 '2026-03-12T07:00:00Z'::timestamptz,
			 '2026-03-12T07:30:00Z'::timestamptz,
			 2, 1, 2, 'old-rules')
	`)
	require.NoError(t, err, "seeding stale secret session")
	current, err := store.ListSessions(ctx, db.SessionFilter{
		HasSecret:            true,
		SecretsRulesVersions: []string{"v-current"},
		Limit:                50,
	})
	require.NoError(t, err, "ListSessions current rules")
	for _, s := range current.Sessions {
		require.NotEqual(t, "has-secret-stale", s.ID,
			"stale secret session included in versioned HasSecret results")
	}
}
