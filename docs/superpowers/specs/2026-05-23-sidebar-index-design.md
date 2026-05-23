# Sidebar Index Design

## Problem

The sidebar currently calls the full session list endpoint with
`include_children=true` and then loads every page into memory before the list is
complete. Including automated sessions can expand the result set to roughly
80,000 rows. In the observed local archive this required 159 full-row requests
and took about 42 seconds.

The first patch for this bug publishes page 1 early, but the architecture is
still wrong: the sidebar is a globally sorted, grouped navigation index, not an
analytics table and not a transcript detail view. It should not hydrate every
full session row before the user asks to see it.

## Constraints

- Preserve the existing global sidebar ordering: working, waiting, idle, stale,
  quiet, unclean, then freshness within each tier.
- Preserve freshness rollup across all members of a group.
- Preserve `findRoot` grouping across continuations, forks, and subagents.
- Preserve orphan teammate adoption, which needs to compare teammate rows
  against possible target groups in the same project.
- Preserve filter semantics for include automated sessions, show/hide
  single-turn sessions, project, agent, machine, date, activity, and status.
- Avoid moving status-tier sorting and teammate adoption into SQL in slice 1.
- Avoid a new batch hydrate endpoint in slice 1.

## Chosen Approach

Add a lightweight sidebar index endpoint that returns all filtered rows needed
by the current client-side grouper, but with only the fields needed for
grouping, sorting, filter display, and visible-row hydration decisions.

The sidebar will fetch this index once per filter state, run the existing
grouping and sort logic over the index rows, and hydrate only visible rows with
existing detail endpoints.

This keeps correctness local to the existing client grouping algorithm while
replacing "full rows times all pages" with "skinny index rows once plus visible
detail fetches".

## API

### `GET /api/v1/sessions/sidebar-index`

Query parameters mirror the current sidebar-facing subset of
`GET /api/v1/sessions`:

- `project`
- `exclude_project`
- `machine`
- `agent`
- `termination`
- `date`
- `date_from`
- `date_to`
- `active_since`
- `min_messages`
- `max_messages`
- `min_user_messages`
- `include_one_shot`
- `include_automated`

These match `Filters` in `frontend/src/lib/stores/sessions.svelte.ts`.
`health_grade`, `outcome`, and `min_tool_failures` are intentionally excluded
because the sidebar does not currently filter by them. The starred-only filter
also stays client-side: `SessionList.svelte` layers it on top of grouped sidebar
output.

The endpoint returns no cursor. It returns every filtered row that the sidebar
grouper can walk. `total` is the filtered skinny-row count, not the visible
sidebar group count; the sidebar header still derives its displayed count from
the grouped client output. `total` exists for monitoring and debugging.

Response shape:

```json
{
  "sessions": [
    {
      "id": "codex:...",
      "parent_session_id": "codex:...",
      "relationship_type": "subagent",
      "project": "agentsview",
      "machine": "local",
      "agent": "codex",
      "display_name": "Review sidebar loading fix",
      "started_at": "2026-05-23T18:13:58.627Z",
      "ended_at": "2026-05-23T18:16:18.309Z",
      "created_at": "2026-05-23T18:16:18.934Z",
      "termination_status": "clean",
      "message_count": 3,
      "user_message_count": 1,
      "is_automated": true,
      "is_teammate": false
    }
  ],
  "total": 79261
}
```

Expected size is roughly 250-300 bytes per row before compression. That is
acceptable for current localhost and LAN deployments, including PG serve. If the
archive grows another order of magnitude, the next lever is a more compact wire
format such as binary or columnar encoding, not SQL pagination.

## Backend Details

The endpoint uses the same filter semantics as `ListSessions`, but projects to a
new skinny row type instead of `db.Session`.

Both read backends must implement the endpoint:

- SQLite in `internal/db/`.
- PostgreSQL in `internal/postgres/sessions.go`, for `pg serve` deployments that
  read through the PG store directly.

The implementations should reuse the existing filter helper logic in each
backend so sidebar-index filters stay aligned with current session-list filters.

`is_teammate` is computed in the index query for slice 1. SQLite uses:

```sql
INSTR(COALESCE(first_message, ''), '<teammate-message') > 0
```

PostgreSQL uses:

```sql
position('<teammate-message' in COALESCE(first_message, '')) > 0
```

This avoids a schema migration until the signal has another consumer. If other
code needs teammate classification later, add a non-destructive migration with
`ALTER TABLE sessions ADD COLUMN is_teammate ...` and backfill from
`first_message`.

`include_children` is not part of the new endpoint. The index endpoint is
sidebar-specific and always returns the filtered set of rows needed for
grouping.

## Frontend Data Model

Add a `SidebarSessionIndexRow` type with the fields from the endpoint.

Refactor `buildSessionGroups` and helper functions to depend on a narrow
`SessionGroupInput` interface instead of full `Session`. That interface includes
only the fields the grouper reads:

- IDs and parent links
- relationship type
- project, agent, machine
- timestamps
- termination status
- message counts
- automation flag
- teammate flag
- display name

The current teammate check:

```ts
s.first_message?.includes("<teammate-message")
```

becomes:

```ts
s.is_teammate === true
```

for index rows. Full `Session` rows can keep working through an adapter or an
optional helper that computes `is_teammate` from `first_message`.

## Visible Row Hydration

The sidebar title still comes from `display_name` or a first-message preview.
The index carries `display_name` because renamed sessions already have stable
user-authored titles and should not show placeholders while hydration is in
flight. The index intentionally does not carry full `first_message`, so visible
rows without `display_name` must hydrate before being displayed as final row
content.

Slice 1 uses existing endpoints:

- `GET /api/v1/sessions/{id}` for visible primary rows that need preview text or
  full detail.
- Existing child-session loading for expanded groups where needed.

The virtualized sidebar should compute the initial visible item IDs immediately
after grouping the index. The initial hydration target is:

```ts
Math.ceil(viewportHeight / ITEM_HEIGHT) + OVERSCAN
```

using the existing constants from `session-list-utils.ts`. It then starts
parallel hydration for those IDs and gates the first visible paint on that
initial hydration batch. This prevents a reload from showing empty placeholder
titles in the normal viewport.

After first paint, hydration is demand-driven:

- Hydrate newly visible rows as the virtual window changes.
- Keep a `Map<sessionID, Session>` cache in the store.
- Key the hydration cache by the current sidebar index version. If filters
  change while hydration is in flight, drop stale-version hydration results
  instead of merging them into the new index.
- Merge hydrated rows into display items without changing the index ordering.
- Use bounded parallelism so fast scrolling does not create unbounded request
  fanout.

Delete and restore keep their current local behavior in the new model: delete
removes the row locally and invalidates metadata; restore clears the undo entry
and refetches the sidebar index.

## Live Refresh

The current `startLiveRefresh` debounces events and calls `load()`, which would
refetch the whole sidebar index too often.

Slice 1 ships a deliberately conservative refresh policy:

- Index refreshes are debounced separately from detail refreshes.
- Session file/message events invalidate hydrated detail for the affected
  session, but do not immediately refetch the full index.
- Sync-complete or coalesced bulk-change events trigger an index refresh.
- Manual filter changes trigger an immediate index refresh.
- Restore triggers an immediate index refresh.
- A safety-net interval may refresh the index, but no more often than every five
  minutes.

If the event stream does not currently expose enough event kinds to distinguish
bulk sync completion from per-session updates, slice 1 should add the minimum
event type needed for that distinction. The long-term shape is per-session index
patch events for upsert/delete, but slice 1 does not need to implement patches.

## Loading States

Initial load:

- Show sidebar loading state while fetching the index and hydrating the first
  viewport.
- Once the first viewport is hydrated, render the sidebar.

Filter change:

- Keep old sidebar content visible while fetching the new index.
- Replace it once the new index is grouped and the first viewport is hydrated.
- Show the existing loading indicator during the transition.

Hydration failure:

- Keep the row in the list.
- Fall back to a stable title such as the session ID suffix or project/agent
  label.
- Retry when the row becomes visible again or when the active session is opened.

## Tests

Backend:

- Sidebar index applies `include_automated=true` and returns automated review
  rows.
- Sidebar index excludes automated rows by default.
- Sidebar index preserves single-turn behavior for `include_one_shot`.
- Sidebar index returns subagent/fork/continuation rows needed by root walking.
- Sidebar index computes `is_teammate` from `first_message`.
- SQLite and PostgreSQL sidebar-index endpoints return matching row shapes and
  filter behavior.

Frontend:

- `buildSessionGroups` works against skinny index rows.
- Status-tier ordering remains unchanged.
- Freshness rollup uses all group members.
- Teammate orphan adoption works when using `is_teammate`.
- Starred-only filtering still applies client-side after grouping.
- Store fetches the sidebar index once per filter state instead of paginating
  the full list.
- Initial viewport hydrates before final sidebar rows are shown.
- Stale-version hydration responses are dropped after filter changes.
- Restore refetches the sidebar index.
- Per-session live events do not trigger full index reloads.
- Sync-complete or bulk-change events do trigger one debounced index reload.

Manual verification:

- With Include automated sessions enabled and Hide single-turn disabled,
  roborev/code-review rows appear in the sidebar immediately after the first
  index load.
- Large archives do not cause dozens or hundreds of full-session list requests.

## Conditional Follow-ups

- Add `POST /api/v1/sessions/hydrate` only if slice 1 shows measured pain from
  visible-row `GET /sessions/{id}` fanout.
- Use binary or columnar index transport only if the skinny JSON index becomes a
  measured wire-size or parse-time bottleneck.
- Persist an `is_teammate` column only if teammate classification gets another
  backend consumer or the ad-hoc query becomes a measured bottleneck.
- Add per-session index patch events for upsert/delete when event volume makes
  full debounced index refreshes visibly expensive.
- Move grouping/sorting server-side only if the client-side index model stops
  meeting correctness or performance goals.
