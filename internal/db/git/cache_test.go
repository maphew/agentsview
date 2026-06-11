package git

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cacheSchema matches the `git_cache` DDL in internal/db/schema.sql. We keep
// it inline so these tests don't depend on loading the full server schema.
const cacheSchema = `
CREATE TABLE IF NOT EXISTS git_cache (
    cache_key   TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    payload     TEXT NOT NULL,
    computed_at TEXT NOT NULL
);
`

// newCacheDB returns a file-backed SQLite DB seeded with the git_cache
// table. A file (rather than `:memory:`) keeps the pool stable across
// multiple connection acquisitions by the *sql.DB.
func newCacheDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "sql.Open")
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(cacheSchema)
	require.NoError(t, err, "init git_cache schema")
	return db
}

func TestCache_GetOrCompute_FirstCallInvokesCompute(t *testing.T) {
	db := newCacheDB(t)
	cache := NewCache(db)

	var calls int
	got, err := cache.GetOrCompute(
		context.Background(), "k1", "log", time.Hour,
		func() ([]byte, error) {
			calls++
			return []byte(`{"commits":3}`), nil
		},
	)
	require.NoError(t, err, "GetOrCompute")
	assert.Equal(t, 1, calls, "compute call count")
	assert.Equal(t, `{"commits":3}`, string(got), "payload")

	// Verify the row landed in git_cache with the expected kind.
	var kind, payload string
	err = db.QueryRow(
		`SELECT kind, payload FROM git_cache WHERE cache_key = ?`, "k1",
	).Scan(&kind, &payload)
	require.NoError(t, err, "row not persisted")
	assert.Equal(t, "log", kind, "row kind")
	assert.Equal(t, `{"commits":3}`, payload, "row payload")
}

func TestCache_GetOrCompute_WithinTTLReturnsCached(t *testing.T) {
	db := newCacheDB(t)
	cache := NewCache(db)

	var calls int
	compute := func() ([]byte, error) {
		calls++
		return []byte(`{"n":1}`), nil
	}

	_, err := cache.GetOrCompute(
		context.Background(), "k1", "log", time.Hour, compute,
	)
	require.NoError(t, err, "first GetOrCompute")
	require.Equal(t, 1, calls, "after first call")

	got, err := cache.GetOrCompute(
		context.Background(), "k1", "log", time.Hour, compute,
	)
	require.NoError(t, err, "second GetOrCompute")
	assert.Equal(t, 1, calls, "compute called again within TTL")
	assert.Equal(t, `{"n":1}`, string(got), "cached payload")
}

func TestCache_GetOrCompute_PastTTLRecomputes(t *testing.T) {
	db := newCacheDB(t)
	cache := NewCache(db)

	var calls int
	compute := func() ([]byte, error) {
		calls++
		return []byte(`{"call":` + strconv.Itoa(calls) + `}`), nil
	}

	// Seed the cache with a timestamp well in the past so the second call
	// sees an expired row.
	_, err := cache.GetOrCompute(
		context.Background(), "k1", "log", time.Hour, compute,
	)
	require.NoError(t, err, "first GetOrCompute")
	oldTime := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	_, err = db.Exec(
		`UPDATE git_cache SET computed_at = ? WHERE cache_key = ?`,
		oldTime, "k1",
	)
	require.NoError(t, err, "backdating row")

	got, err := cache.GetOrCompute(
		context.Background(), "k1", "log", time.Hour, compute,
	)
	require.NoError(t, err, "second GetOrCompute")
	assert.Equal(t, 2, calls, "compute invocations (past TTL)")
	assert.Equal(t, `{"call":2}`, string(got), "recomputed payload")
}

func TestCache_GetOrCompute_ErrorDoesNotWriteRow(t *testing.T) {
	db := newCacheDB(t)
	cache := NewCache(db)

	boom := errors.New("compute blew up")
	_, err := cache.GetOrCompute(
		context.Background(), "k1", "log", time.Hour,
		func() ([]byte, error) { return nil, boom },
	)
	require.ErrorIs(t, err, boom)

	var n int
	err = db.QueryRow(
		`SELECT count(*) FROM git_cache WHERE cache_key = ?`, "k1",
	).Scan(&n)
	require.NoError(t, err, "count")
	assert.Zero(t, n, "row count after error")
}

func TestCacheKey_DeterministicAndSensitiveToEachField(t *testing.T) {
	base := CacheKey("log", "/r", "a@x", "2026-01-01", "2026-02-01")
	require.NotEmpty(t, base, "CacheKey returned empty string")
	assert.Equal(t, base, CacheKey("log", "/r", "a@x", "2026-01-01", "2026-02-01"),
		"CacheKey non-deterministic")

	cases := []struct {
		name                     string
		kind, repo, author, s, u string
	}{
		{"kind", "pr", "/r", "a@x", "2026-01-01", "2026-02-01"},
		{"repo", "log", "/r2", "a@x", "2026-01-01", "2026-02-01"},
		{"author", "log", "/r", "b@x", "2026-01-01", "2026-02-01"},
		{"since", "log", "/r", "a@x", "2026-01-02", "2026-02-01"},
		{"until", "log", "/r", "a@x", "2026-01-01", "2026-02-02"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CacheKey(c.kind, c.repo, c.author, c.s, c.u)
			assert.NotEqual(t, base, got,
				"CacheKey did not change when %s differed", c.name)
		})
	}
}
