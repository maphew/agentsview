//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"
)

const schemaTestSchema = "agentsview_schema_test"

func cleanSchemaTestPG(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	if err != nil {
		t.Fatalf("connecting to pg: %v", err)
	}
	defer pg.Close()
	_, _ = pg.Exec(
		"DROP SCHEMA IF EXISTS " + schemaTestSchema + " CASCADE",
	)
}

// TestSecretFindingsSchema verifies that EnsureSchema creates the
// secret_findings table with all required columns, and that the
// sessions table has the secret_leak_count and
// secrets_rules_version columns. Also asserts idempotency.
func TestSecretFindingsSchema(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, schemaTestSchema, true)
	if err != nil {
		t.Fatalf("connecting to pg: %v", err)
	}
	defer pg.Close()

	ctx := context.Background()

	// Run EnsureSchema twice to verify idempotency.
	if err := EnsureSchema(ctx, pg, schemaTestSchema); err != nil {
		t.Fatalf("EnsureSchema (first): %v", err)
	}
	if err := EnsureSchema(ctx, pg, schemaTestSchema); err != nil {
		t.Fatalf("EnsureSchema (second, idempotency check): %v", err)
	}

	// Verify secret_findings table exists.
	var tableExists bool
	err = pg.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1
			  AND table_name = 'secret_findings'
		)`, schemaTestSchema).Scan(&tableExists)
	if err != nil {
		t.Fatalf("checking secret_findings table: %v", err)
	}
	if !tableExists {
		t.Fatal("secret_findings table does not exist")
	}

	// Verify all required columns on secret_findings.
	requiredFindingsCols := []string{
		"id", "session_id", "rule_name", "confidence",
		"location_kind", "message_ordinal", "call_index",
		"event_index", "match_start", "match_end",
		"match_index", "redacted_match", "rules_version",
		"created_at",
	}
	for _, col := range requiredFindingsCols {
		var exists bool
		err = pg.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1
				  AND table_name = 'secret_findings'
				  AND column_name = $2
			)`, schemaTestSchema, col).Scan(&exists)
		if err != nil {
			t.Fatalf(
				"checking secret_findings.%s: %v", col, err,
			)
		}
		if !exists {
			t.Errorf(
				"secret_findings.%s column missing", col,
			)
		}
	}

	// Verify sessions has both secret-scan state columns.
	requiredSessionCols := []string{
		"secret_leak_count",
		"secrets_rules_version",
	}
	for _, col := range requiredSessionCols {
		var exists bool
		err = pg.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1
				  AND table_name = 'sessions'
				  AND column_name = $2
			)`, schemaTestSchema, col).Scan(&exists)
		if err != nil {
			t.Fatalf(
				"checking sessions.%s: %v", col, err,
			)
		}
		if !exists {
			t.Errorf("sessions.%s column missing", col)
		}
	}
}
