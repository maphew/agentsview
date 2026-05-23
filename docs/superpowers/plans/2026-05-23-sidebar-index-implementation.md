# Sidebar Index Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the sidebar's eager full-session pagination with a skinny
session index plus visible-row hydration.

**Architecture:** Add a SQLite and PostgreSQL sidebar-index read path that
returns every filtered row the client grouper needs, but only as skinny
metadata. The frontend fetches this index once per filter state, groups and
sorts globally on the client, hydrates the first visible rows before rendering,
and then hydrates additional visible rows on demand.

**Tech Stack:** Go HTTP server and stores (`internal/db`, `internal/postgres`,
`internal/server`), Svelte 5 frontend store/components, Vitest, Go tests with
`fts5` and optional `pgtest`.

______________________________________________________________________

## File Structure

- `internal/db/sessions.go`: Add `SidebarSessionIndexRow`,
  `SidebarSessionIndex`, `GetSidebarSessionIndex`, and SQLite query logic using
  existing `SessionFilter` filtering.
- `internal/db/store.go`: Add `GetSidebarSessionIndex` to `Store`.
- `internal/db/filter_test.go` or `internal/db/sessions_test.go`: Add SQLite
  sidebar-index tests.
- `internal/postgres/sessions.go`: Add matching PG `GetSidebarSessionIndex`
  query using PG filter helpers and `position(...)`.
- `internal/postgres/store_test.go`: Add PG sidebar-index tests and
  parity-focused assertions.
- `internal/server/sessions.go`: Add `handleSidebarSessionIndex`, using the same
  sidebar filter params as the spec and intentionally excluding
  outcome/health/min_tool_failures.
- `internal/server/server.go`: Register `GET /api/v1/sessions/sidebar-index`.
- `internal/server/server_test.go`: Add HTTP route tests for include automated,
  display name, and teammate flag.
- `frontend/src/lib/api/types/core.ts`: Add `SidebarSessionIndexRow` and
  `SidebarSessionIndexResponse`.
- `frontend/src/lib/api/client.ts`: Add `getSidebarSessionIndex`.
- `frontend/src/lib/stores/sessions.svelte.ts`: Add index loading, a narrow
  `SessionGroupInput` model, index-versioned hydration cache, visible hydration
  method, and index-to-display merging that preserves `is_teammate`.
- `frontend/src/lib/stores/sessions.test.ts`: Add store tests for skinny-row
  grouping invariants, index loading, first-viewport hydration discipline, stale
  hydration dropping, delete/restore behavior, live refresh behavior, and no
  eager full-list pagination.
- `frontend/src/lib/components/sidebar/SessionList.svelte`: Trigger hydration
  for visible rows, gate first viewport rendering, preserve client-side starred
  filtering, and provide viewport sizing to the store.
- `frontend/src/lib/components/sidebar/SessionItem.svelte`: Use
  `session.is_teammate` as the teammate signal when present, falling back to
  `first_message` for hydrated/full rows.
- `frontend/src/lib/components/sidebar/session-list-utils.ts`: Keep existing
  constants and grouping helpers usable by the store/component.

## Task 1: Backend Sidebar Index For SQLite And HTTP

**Files:**

- Modify: `internal/db/store.go`

- Modify: `internal/db/sessions.go`

- Test: `internal/db/filter_test.go` or `internal/db/sessions_test.go`

- Modify: `internal/server/sessions.go`

- Modify: `internal/server/server.go`

- Test: `internal/server/server_test.go`

- [ ] **Step 1: Write failing SQLite store tests**

Add tests that seed normal, automated, display-named, teammate, child, and
one-shot sessions. Verify `GetSidebarSessionIndex(ctx, filter)`:

- excludes automated rows by default via `SessionFilter{ExcludeAutomated: true}`
- includes automated rows when `ExcludeAutomated: false`
- honors one-shot filtering, including automated one-shot inclusion when
  automated sessions are included
- returns child/subagent/fork rows needed for root walking when a root matches
  filters
- returns `DisplayName`
- computes `IsTeammate` from `first_message`

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db -run 'Test.*SidebarSessionIndex' -count=1
```

Expected: FAIL because `GetSidebarSessionIndex` does not exist.

- [ ] **Step 2: Implement SQLite index types and query**

Add:

```go
type SidebarSessionIndexRow struct {
    ID              string  `json:"id"`
    ParentSessionID *string `json:"parent_session_id,omitempty"`
    RelationshipType string `json:"relationship_type,omitempty"`
    Project         string  `json:"project"`
    Machine         string  `json:"machine"`
    Agent           string  `json:"agent"`
    DisplayName     *string `json:"display_name,omitempty"`
    StartedAt       *string `json:"started_at"`
    EndedAt         *string `json:"ended_at"`
    CreatedAt       string  `json:"created_at"`
    TerminationStatus *string `json:"termination_status,omitempty"`
    MessageCount    int     `json:"message_count"`
    UserMessageCount int    `json:"user_message_count"`
    IsAutomated     bool    `json:"is_automated"`
    IsTeammate      bool    `json:"is_teammate"`
}

type SidebarSessionIndex struct {
    Sessions []SidebarSessionIndexRow `json:"sessions"`
    Total    int                      `json:"total"`
}
```

Use `buildSessionFilter` with `IncludeChildren: true`, no cursor, no limit.
Select only skinny columns plus:

```sql
INSTR(COALESCE(first_message, ''), '<teammate-message') > 0
```

Keep the same deterministic result order as `ListSessions`: effective recency
descending, then `id DESC`. Client sorting still owns the visible ordering, but
deterministic input order prevents tie-order flakes.

- [ ] **Step 3: Run SQLite tests green**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db -run 'Test.*SidebarSessionIndex|TestExcludeOneShotWithIncludeAutomated' -count=1
```

Expected: PASS.

- [ ] **Step 4: Write failing HTTP route tests**

Add server tests for `GET /api/v1/sessions/sidebar-index` proving JSON includes
`display_name`, `is_teammate`, and automated rows only when
`include_automated=true`.

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server -run 'Test.*SidebarIndex' -count=1
```

Expected: FAIL because route does not exist.

- [ ] **Step 5: Implement HTTP handler and route**

Register `GET /api/v1/sessions/sidebar-index`. Parse the same sidebar filter
params as `handleListSessions`, excluding `outcome`, `health_grade`, and
`min_tool_failures`. Convert `include_one_shot` / `include_automated` into
`SessionFilter{ExcludeOneShot: !includeOneShot, ExcludeAutomated: !includeAutomated, IncludeChildren: true}`
and return `db.SidebarSessionIndex`.

- [ ] **Step 6: Run HTTP tests green**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server -run 'Test.*SidebarIndex' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/db/store.go internal/db/sessions.go internal/db/*test.go internal/server/sessions.go internal/server/server.go internal/server/*test.go
git commit -m "feat: add sidebar index endpoint"
```

If this task and Task 2 are not completed in one uninterrupted implementation
pass, defer this commit until the PostgreSQL store method exists so the shared
`db.Store` interface is never left broken at a commit boundary.

## Task 2: PostgreSQL Sidebar Index And Parity

**Files:**

- Modify: `internal/postgres/sessions.go`

- Test: `internal/postgres/store_test.go`

- [ ] **Step 1: Write failing PG index tests**

Add tests for `Store.GetSidebarSessionIndex` mirroring the SQLite behavior:

- `position('<teammate-message' in COALESCE(first_message, '')) > 0` sets
  `IsTeammate`
- display name is returned
- automated and one-shot filters match SQLite semantics
- child rows for matching roots are included

Run early:

```bash
make test-postgres
```

Expected before implementation: FAIL because PG store does not implement the new
`db.Store` method.

- [ ] **Step 2: Implement PG index query**

In `internal/postgres/sessions.go`, reuse the existing PG session filter
assembly. Force `IncludeChildren: true`; no cursor or limit. Select the same
JSON-shape fields as SQLite and compute teammate with:

```sql
position('<teammate-message' in COALESCE(first_message, '')) > 0
```

Use the PostgreSQL equivalent of the SQLite deterministic ordering: effective
recency descending, then `id DESC`.

- [ ] **Step 3: Run PG parity tests**

Run:

```bash
make test-postgres
```

Expected: PASS. If Docker/Postgres is unavailable, run the pgtest-tagged package
test to prove whether the blocker is environment setup:

```bash
CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/postgres -run 'Test.*SidebarSessionIndex' -count=1
```

and report the environment blocker.

- [ ] **Step 4: Commit**

```bash
git add internal/postgres/sessions.go internal/postgres/store_test.go
git commit -m "feat: add postgres sidebar index"
```

## Task 3: Frontend API And Store Index Loading

**Files:**

- Modify: `frontend/src/lib/api/types/core.ts`

- Modify: `frontend/src/lib/api/client.ts`

- Modify: `frontend/src/lib/stores/sessions.svelte.ts`

- Modify: `frontend/src/lib/components/sidebar/SessionItem.svelte`

- Test: `frontend/src/lib/stores/sessions.test.ts`

- Test: `frontend/src/lib/components/sidebar/session-list-utils.test.ts`

- [ ] **Step 1: Write failing frontend API/store tests**

Tests should verify:

- `sessions.load()` calls `getSidebarSessionIndex` instead of paginating
  `listSessions`
- `buildSessionGroups` accepts skinny `SessionGroupInput` rows, not only full
  `Session` rows
- teammate orphan adoption uses `is_teammate`, not `first_message`
- status-tier ordering remains working, waiting, idle, stale, quiet, unclean
- freshness rollup uses all group members from the skinny index
- skinny index rows merge with hydrated rows without changing group order
- `display_name` is available without hydration
- hydration results are dropped when their index version is stale
- delete removes an index row locally and invalidates metadata
- `restoreSession` reloads the sidebar index

Run:

```bash
cd frontend && npm test -- --run src/lib/stores/sessions.test.ts -t 'sidebar index'
```

Expected: FAIL because the frontend still uses `listSessions`.

- [ ] **Step 2: Add API types and client**

Add `SidebarSessionIndexRow`, `SidebarSessionIndexResponse`, and
`getSidebarSessionIndex(params)` mapping to `/sessions/sidebar-index`.

- [ ] **Step 3: Implement store index loading**

Replace the eager all-pages `load()` path with one sidebar index request. Do not
fake teammate state through `first_message`; instead:

- define a narrow `SessionGroupInput` interface in `sessions.svelte.ts`
- make `buildSessionGroups`, `getSessionStatus`, freshness helpers, teammate
  helpers, and group types work with that interface
- add optional `is_teammate?: boolean` to the frontend `Session` type so
  hydrated/full rows and skinny rows share the same teammate signal
- use
  `session.is_teammate ?? session.first_message?.includes("<teammate-message")`
  in grouping and in `SessionItem`
- keep `display_name` from index rows and merge hydrated full sessions by ID
  when available

Keep:

- `sidebarIndexVersion`
- `hydratedSessionsByVersion`
- `hydrateVisibleSessions(ids, version?)`
- stale-version result dropping

Preserve existing filter serialization, active-session behavior, deletion,
restore, rename, and metadata cache invalidation.

- [ ] **Step 4: Run store tests green**

Run:

```bash
cd frontend && npm test -- --run src/lib/stores/sessions.test.ts
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/api/types/core.ts frontend/src/lib/api/client.ts frontend/src/lib/stores/sessions.svelte.ts frontend/src/lib/stores/sessions.test.ts
git commit -m "feat: load sidebar from skinny index"
```

## Task 4: Sidebar Visible Hydration And Live Refresh Discipline

**Files:**

- Modify: `frontend/src/lib/components/sidebar/SessionList.svelte`

- Modify: `frontend/src/lib/stores/sessions.svelte.ts`

- Modify: `frontend/src/lib/stores/events.svelte.ts` only if needed for
  testability

- Test: `frontend/src/lib/components/sidebar/SessionList.test.ts`

- Test: `frontend/src/lib/stores/sessions.test.ts`

- [ ] **Step 1: Write failing visible hydration tests**

Add tests that mount `SessionList`, provide index rows with null
`first_message`, and verify:

- the initial hydration target is
  `Math.ceil(viewportHeight / ITEM_HEIGHT) + OVERSCAN`
- first visible paint is gated until that initial hydration batch resolves
- renamed rows with `display_name` render without waiting for hydration
- newly visible rows trigger hydration after scroll/visible item changes
- hydration requests are concurrency bounded during fast visibility changes
- starred-only filtering still applies client-side after grouping
- per-session/message-scope events do not trigger a full sidebar index reload
- `sessions` and `sync` scope events trigger one debounced sidebar index reload

Run:

```bash
cd frontend && npm test -- --run src/lib/components/sidebar/SessionList.test.ts src/lib/stores/sessions.test.ts -t 'hydrate'
```

Expected: FAIL before component wiring.

- [ ] **Step 2: Wire visible hydration**

Use `ITEM_HEIGHT` and `OVERSCAN` to compute the first viewport hydration target
as:

```ts
Math.ceil(viewportHeight / ITEM_HEIGHT) + OVERSCAN
```

Implementation contract:

- `SessionList` reports viewport height to the store as soon as the
  `ResizeObserver` has a value.

- `sessions.load()` fetches the index and publishes a new index version only
  after the fetch succeeds. `SessionList` derives the actual first visible IDs
  from grouped display items, starts hydration for those rows, and keeps the
  previous painted list visible until the initial visible hydration resolves.

- If a filter change happens while initial hydration is in flight, stale-version
  results are dropped and the old sidebar remains visible until the new version
  is ready.

- After initial paint, when visible items change, call the store hydration
  method for visible primary and child session IDs.

- Do not let stale hydration mutate a newer index version.

- Limit concurrent detail hydration requests to a small constant such as
  `SIDEBAR_HYDRATION_CONCURRENCY = 6`; queue or skip duplicate in-flight IDs.

- [ ] **Step 3: Adjust live refresh**

Separate index reload from detail invalidation using the current global event
scopes:

- filter changes and restore reload the index immediately
- `messages` scope clears hydrated detail cache only and does not reload the
  full index
- `sessions` and `sync` scopes trigger one debounced index reload
- safety net remains no more frequent than five minutes

The current `/events` payload is only `{ scope }`, so slice 1 does not attempt
per-session detail invalidation by ID. If a task changes the backend event
shape, it must also update `DataChangedEvent`, event tests, and keep PG serve's
503 behavior unchanged.

- [ ] **Step 4: Run frontend tests and build**

Run:

```bash
cd frontend && npm test -- --run
cd frontend && npm run build
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/components/sidebar/SessionList.svelte frontend/src/lib/stores/sessions.svelte.ts frontend/src/lib/components/sidebar/SessionList.test.ts frontend/src/lib/stores/sessions.test.ts frontend/src/lib/stores/events.svelte.ts
git commit -m "feat: hydrate sidebar rows on visibility"
```

## Final Verification

- [ ] Run Go unit tests for touched packages:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/server -count=1
```

- [ ] Run PG parity/integration tests early and again before final handoff:

```bash
make test-postgres
```

- [ ] Run frontend tests and build:

```bash
cd frontend && npm test -- --run
cd frontend && npm run build
```

- [ ] Manual verification:

Start the frontend dev server against the local backend, enable Include
automated sessions with Hide single-turn disabled, and verify recent
roborev/code-review sessions appear in the sidebar without hundreds of
`/sessions` requests.

- [ ] Commit any final integration fixes.
