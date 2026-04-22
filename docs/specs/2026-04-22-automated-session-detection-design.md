# Automated Session Detection: Claude Code internals + roborev review combiner

## Background

agentsview already classifies "automated" sessions and offers a sidebar toggle
to include or exclude them. Today the patterns in `internal/db/automated.go`
cover roborev review/fix prompts only. Two gaps:

1. **Claude Code spawns its own internal sessions** that pollute the sidebar —
   most prominently a "conversation title generator" that runs after every user
   turn, and a "Warmup" pre-warm ping. These are not user-initiated work and
   should not appear in the default view.
1. **Insights generation** (`internal/insight/prompt.go`) reads sessions without
   filtering automated ones, so daily summaries can be skewed by automation
   noise.

A new roborev pattern (the "review combiner" prompt) is also not covered and
should be added in the same pass.

## Scope

In scope:

- Add three new patterns to `IsAutomatedSession`:
  - `"You are combining multiple code review outputs into a single GitHub PR comment."`
    (prefix) — roborev combiner.
  - `"You are a conversation title generator"` (substring) — Claude Code title
    generator. Substring because the message body is wrapped with a leading
    `-\n`.
  - `"Warmup"` (exact match, after trimming surrounding whitespace) — Claude
    Code model warmup ping.
- Introduce an exact-match category (`automatedExactMatches`) in the classifier;
  today only prefix and substring are supported.
- Filter automated sessions out of insights generation.
- Bump the backfill stats marker so existing databases re-classify all sessions
  on next open.
- Rename the sidebar toggle label from "Include automated reviews" to "Include
  automated sessions" to reflect the broader category.

Out of scope:

- Detection patterns for other agents (Codex, Cursor, iFlow, etc.).
- Splitting automation into categories or kinds (enum / new column).
- Hiding `relationship_type="subagent"` sessions wholesale — those are
  legitimate Agent-tool work and remain visible.
- Schema changes; `is_automated` already exists in both SQLite and PostgreSQL
  schemas.

## Detection details

The classifier in `internal/db/automated.go` becomes:

```go
var automatedPrefixes = []string{
    // ...existing patterns...
    "You are combining multiple code review outputs into a single GitHub PR comment.",
}

var automatedSubstrings = []string{
    // ...existing patterns...
    "You are a conversation title generator",
}

var automatedExactMatches = []string{
    "Warmup",
}

func IsAutomatedSession(firstMessage string) bool {
    for _, prefix := range automatedPrefixes {
        if strings.HasPrefix(firstMessage, prefix) {
            return true
        }
    }
    for _, sub := range automatedSubstrings {
        if strings.Contains(firstMessage, sub) {
            return true
        }
    }
    trimmed := strings.TrimSpace(firstMessage)
    for _, exact := range automatedExactMatches {
        if trimmed == exact {
            return true
        }
    }
    return false
}
```

Existing single-turn gate (`user_message_count <= 1`) is unchanged and continues
to wrap this function at the call sites that flip the `is_automated` column.
Multi-turn sessions never get classified as automated, even if their first
message matches a pattern.

Rationale for `"Warmup"` as exact-match: the word is too generic for prefix or
substring matching (a real prompt could begin "Warmup the test database
before…"). Trim before comparing so files with a trailing newline still match.
Substring match would risk false positives; exact-match plus the single-turn
gate keeps it tight.

## Insights filter

In `internal/insight/prompt.go`, `BuildPrompt` constructs a `db.SessionFilter`
and calls `database.ListSessions(ctx, filter)`. Set `ExcludeAutomated: true` on
that filter. There is no UI flag and no override — insights should never include
automation. The existing sidebar toggle remains the only way to surface
automated sessions.

## Backfill

There are two parallel backfills, one for SQLite and one for PostgreSQL, each
gated by its own marker. Both must be bumped, and the SQLite path must also
dirty `local_modified_at` so incremental `pg push` actually re-emits the changed
rows.

### Marker bumps

- `internal/db/db.go:575` — `const marker = "is_automated_backfill_v2"` →
  `"is_automated_backfill_v3"`. Hoist the literal out of
  `backfillIsAutomatedLocked` into an exported package-level constant (e.g.,
  `IsAutomatedBackfillMarker`) so tests can reference it symbolically and never
  drift again.
- `internal/postgres/schema.go:561` —
  `const isAutomatedBackfillMetadataKey = "is_automated_backfill_v2"` →
  `"is_automated_backfill_v3"`. This re-runs `backfillIsAutomatedPG` on the next
  `EnsureSchema` call so `pg serve` / direct PG read stores converge without
  requiring a local push.

On next open, every session row whose `is_automated` flag should change is
updated. Rows that were already correct are not touched.

### Push visibility (HIGH-priority fix)

`batchUpdateAutomated` (`internal/db/db.go:666`) currently sets only
`is_automated`, leaving `local_modified_at` stale. Incremental PG push selects
rows via `ListSessionsModifiedBetween` against `local_modified_at`
(`internal/db/sessions.go:1668`, `internal/postgres/push.go:133`), so backfilled
rows would be skipped by the next push.

Fix: extend the UPDATE in `batchUpdateAutomated` to also bump
`local_modified_at`:

```sql
UPDATE sessions
SET    is_automated = ?,
       local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE  id IN (...)
```

This matches the existing convention used at
`sessions.go:947, 1489, 1504, 1523`. Only rows whose flag actually changed get
touched (`setIDs`/`clearIDs` are computed from a diff against the current value
at `db.go:614-618`), so the timestamp bump cost is bounded to the migration
delta. Avoids requiring users to run `pg push --full`.

## UI label

Update the sidebar filter toggle label at
`frontend/src/lib/components/sidebar/SessionList.svelte:485`.

- Old label: `Include automated reviews`
- New label: `Include automated sessions`

URL parameter (`include_automated`), API field, store names, and DB columns are
unchanged. This is purely a copy update.

## PostgreSQL sync

`is_automated` already exists in `internal/postgres/schema.go` (`coreDDL`,
`BOOLEAN`). No DDL migration required. Two convergence paths cover the two PG
deployment modes:

- **Local SQLite + push to PG** (`pg push`): the SQLite backfill bumps
  `local_modified_at` on changed rows (see Backfill section), so incremental
  push picks them up automatically. No `--full` required.
- **Direct PG store** (`pg serve` against a PG that may have been written by an
  older agentsview version): the PG-side `backfillIsAutomatedPG` re-runs because
  its marker constant is bumped to `_v3` alongside the SQLite one.

## Testing

### `internal/db/automated_test.go`

Table-driven cases:

- One positive case per **new** pattern:
  - Title generator (with the `-\n` prefix wrapping)
  - Warmup (exact, plus a variant with trailing whitespace)
  - Roborev review combiner (with its full prompt body)
- Spot-check existing roborev patterns still classify (regression guard for at
  least: `"You are a code reviewer."`,
  `"invoked by roborev to perform this review"`).
- Negative cases:
  - `"Warmup fans for the show"` — must NOT classify as automated.
  - `"I need to generate a conversation about titles"` — must NOT classify.
  - Empty string — must NOT classify.

### `internal/db/automated_backfill_test.go` (existing)

Currently deletes the SQLite marker by literal string at lines 40 and 80:
`"DELETE FROM stats WHERE key = 'is_automated_backfill_v2'"`. After the v3 bump
these literals would no longer clear the active marker, leaving the test
exercising stale state. Replace both literal strings with a reference to the new
exported constant `IsAutomatedBackfillMarker` introduced in
`internal/db/automated.go`. Also add a new positive case that verifies a
backfilled row gets `local_modified_at` bumped (read it before/after the run and
assert it changed). Mirror the millisecond-precision guard from
`internal/db/signals_test.go:164` — sleep ~5 ms between the snapshot and the
backfill call so SQLite's `strftime('now')` produces a strictly later value,
then compare with `>` against the prior string.

### Insight integration test

Extend or add a test in `internal/insight/` that builds a prompt against a
fixture database containing one user session and one automated session; assert
the automated session does not appear in the rendered prompt.

### Frontend

No new frontend tests required — the change is a label string. If a Playwright
test asserts on the old label text, update the assertion.

## Migration risks

- **Existing user databases**: backfill runs once on next open after upgrade.
  For a database with many thousands of sessions this is a bounded one-time
  cost; the existing v2 backfill already proved this is acceptable.
- **PostgreSQL drift**: avoided. The PG-side backfill marker bump causes
  `EnsureSchema` to re-classify rows on the next PG connection, so `pg serve`
  deployments converge without depending on a local push. Local-push deployments
  converge via the bumped `local_modified_at` on changed rows.
- **False positives on Warmup**: the exact-match + single-turn gate combination
  makes accidental matches extremely unlikely. If a user reports one, they can
  untoggle the sidebar filter and the session is visible.

## Files touched

- `internal/db/automated.go` — add patterns, add exact-match branch, add
  exported `IsAutomatedBackfillMarker` constant.
- `internal/db/automated_test.go` — extend table-driven tests.
- `internal/db/db.go` — replace inline `marker` literal in
  `backfillIsAutomatedLocked` with reference to the new constant; bump marker
  value to `_v3`; extend `batchUpdateAutomated` UPDATE to also bump
  `local_modified_at`.
- `internal/db/automated_backfill_test.go` — replace `_v2` string literals with
  the new constant; add a `local_modified_at`-bump assertion case.
- `internal/postgres/schema.go` — bump `isAutomatedBackfillMetadataKey` to `_v3`
  so PG-side backfill re-runs on `EnsureSchema`.
- `internal/insight/prompt.go` — set `ExcludeAutomated: true`.
- `internal/insight/*_test.go` — verify automated sessions excluded.
- `frontend/src/lib/components/sidebar/SessionList.svelte:485` — relabel toggle.
- `frontend/e2e/*.spec.ts` — update label assertion if any exists.
