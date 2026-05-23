# Design: `session usage` command with single-session cost estimate

Date: 2026-05-22 Status: Approved (design) Repos touched: `agentsview` (this
repo), `roborev` (`~/code/roborev`)

## Summary

Add a per-session cost estimate to agentsview and surface it in roborev next to
the existing token counts. Introduce a new `agentsview session usage <id>`
command that returns token statistics **and** a cost estimate, and deprecate the
existing top-level `token-use` command (kept working as an alias).

roborev currently shells out to `agentsview token-use <id>` to display
`[118.0k ctx · 28.8k out]` per review. After this change it calls
`agentsview session --format json usage <id>` and renders
`[118.0k ctx · 28.8k out · ~$0.42]` when a cost estimate is available.

## Goals

- A single-session cost estimate computed from the same pricing pipeline that
  already powers the dashboard (`GetTopSessionsByCost` / `GetDailyUsage`), so
  numbers are consistent across agentsview.
- A `session usage` command that is a strict superset of `token-use`: same token
  fields, plus cost. JSON stays lean.
- Cost is shown only when it is actually known for that agent/model ("available
  for that agent type"). No silent partial totals.
- Graceful deprecation of `token-use`; no regression for roborev users running
  an older agentsview.

## Non-goals

- No new pricing source or per-agent pricing config. We reuse the existing
  `model_pricing` table (seeded from fallback + LiteLLM at startup).
- No service-layer / HTTP-daemon `Usage` endpoint (Approach B, rejected below).
- No cost-basis token breakdown (input / cache-read / cache-write) in the JSON.

## Background (current state)

### agentsview cost pipeline

- Cost is computed in `internal/db/usage.go` by `usageAmounts(row, pricing)`: it
  uses `usage_events.cost_usd` when an agent reports cost directly (e.g.
  Hermes), otherwise `tokens × model_pricing` rates, pricing
  input/output/cache-creation/cache-read separately.
- `usageRowSelect()` is the shared row source (messages `UNION ALL`
  usage_events). `GetTopSessionsByCost` aggregates per-session cost over a
  **date range**, with Claude `message.id + request.id` dedup across
  fork/subagent boundaries. There is no single-session lookup today.
- `model_pricing` is seeded by `seedPricing()` (fallback synchronously, LiteLLM
  in the background) and by the `usage` command's `ensurePricing()` — **but only
  in the server-startup path and the `usage`/`pg` commands**. The `token-use`
  direct-DB path (`db.Open` at `token_use.go:231`) seeds nothing and applies no
  custom rates, because token-use never computed cost. `session usage` adds
  cost, so it must seed pricing itself (see Design §2). `loadPricingMap` also
  merges `db.customPricing`, set via `SetCustomPricing` from
  `cfg.CustomModelPricing`.
- **Trap (per review):** `usageAmounts()` reads `pricing[r.model]`, which
  returns a zero-value `modelRates` for an unknown model — so a missing price
  silently contributes `$0` rather than signalling "unpriced". New code must do
  an explicit map lookup to distinguish the two.

### `token-use` command

- `cmd/agentsview/token_use.go` is a top-level command (not under `session`). It
  does NOT use the `service.SessionService` layer. It:
  1. Resolves a bare/raw session ID to canonical form (`resolveRawSessionID`,
     with DB suffix match + disk probes).
  1. Detects a running/starting daemon and waits for startup if needed.
  1. **Syncs the session on demand** via `engine.SyncSingleSession` when no
     daemon owns the DB, so callers get fresh data right after a session ends.
  1. Reads `db.Session` directly and prints JSON.
- Output:
  `session_id, agent, project, total_output_tokens, peak_context_tokens, has_token_data, server_running`.
- `total_output_tokens` / `peak_context_tokens` are precomputed session-level
  aggregates on `db.Session` (sum of output tokens; peak context window). They
  are independent of the per-message `token_usage` rows the cost pipeline scans.

### Why not the generic `session` service path

The other `session` subcommands (`get`, `list`, ...) go through `resolveService`
→ `newService`, which constructs the direct backend with **`engine: nil`**
(`cmd/agentsview/transport.go`): "CLI reads don't need it, and Sync is handled
via the HTTP daemon when one is running." That path has **no on-demand sync**.
`token-use`'s direct-DB + sync/startup-wait handling is the valuable behavior
roborev depends on, and it is exactly what `session usage` must preserve.
Therefore `session usage` reuses/extracts the `token-use` code path — it does
**not** build on the `SessionService` abstraction.

### roborev integration

- `internal/tokens/tokens.go` is the dedicated agentsview integration layer:
  resolves the `agentsview` binary, version-gates on `minVersion = {0,15,0}`
  (the release that introduced `token-use`, `tokens.go:59`), execs
  `token-use <id>` (10s timeout), parses `agentsviewResponse`, stores
  `Usage{OutputTokens, PeakContextTokens}`, renders via `FormatSummary()`.
- Token fetch happens on job completion (`internal/daemon/worker.go`, fresh
  sessions only) and via `cmd/roborev/backfill_tokens.go`. Usage JSON is stored
  on `ReviewJob.TokenUsage`.
- Display: `cmd/roborev/show.go` and TUI `cmd/roborev/tui/render_review.go`,
  both via `Usage.FormatSummary()` → `[118.0k ctx · 28.8k out]`.
- No cost handling exists in roborev today.

## Approaches considered

- **A. `session usage` reuses the `token-use` direct-DB + sync path, plus cost
  (chosen).** Extract the `token-use` core into a shared implementation; add a
  cost aggregation; expose under the `session` group. Keep `token-use` as a
  deprecated alias. Smallest change, preserves the sync behavior, keeps pricing
  in one place.
- **B. First-class `SessionService.Usage()` + HTTP endpoint + sync engine wired
  into the direct backend.** Cleaner abstraction and supports a future daemon
  transport, but `session --server` is explicitly unimplemented, roborev runs
  locally against the same SQLite file, and it adds a large surface for no
  current benefit. Rejected (over-engineering).
- **C. Compute cost in roborev.** Duplicates the pricing catalog and the
  per-message/per-model logic; roborev lacks the per-message model breakdown.
  Rejected.

## Design

### 1. agentsview — `db.GetSessionUsage`

New method in `internal/db/usage.go` (next to `GetTopSessionsByCost`):

```go
// SessionUsage is the per-session token + cost summary returned by
// `session usage`. Cost is an estimate from the model_pricing catalog
// unless an agent reported cost directly (usage_events.cost_usd).
type SessionUsage struct {
    SessionID         string   `json:"session_id"`
    Agent             string   `json:"agent"`
    Project           string   `json:"project"`
    TotalOutputTokens int      `json:"total_output_tokens"`
    PeakContextTokens int      `json:"peak_context_tokens"`
    HasTokenData      bool     `json:"has_token_data"`
    CostUSD           float64  `json:"cost_usd"`
    HasCost           bool     `json:"has_cost"`
    Models            []string `json:"models"`
    UnpricedModels    []string `json:"unpriced_models,omitempty"`
}

func (db *DB) GetSessionUsage(ctx context.Context, sessionID string) (*SessionUsage, error)
```

Behavior:

1. **Start from `GetSession(sessionID)`** for metadata and the existing
   `TotalOutputTokens` / `PeakContextTokens` / `Has*` fields. If the session is
   not found, return `(nil, nil)`. This guarantees sessions that carry
   session-level token aggregates but have no per-message `token_usage` rows
   still report metadata and token output (the cost scan alone would miss them).
   `HasTokenData = HasTotalOutputTokens || HasPeakContextTokens`.
1. **Aggregate cost** over `usageRowSelect()` filtered to this one session
   (`AND u.session_id = ?`, no date range), reusing `scanUsageRow`, the existing
   Claude `message.id + request.id` / usage dedup, and `loadPricingMap`. For
   each cost-contributing row (one with tokens or an explicit cost):
   - If `cost_usd` is present → add it; mark the row priced.
   - Else look up `rates, ok := pricing[model]` **explicitly**. If `ok`, add
     `tokens × rates` and mark priced. If not `ok`, mark the row unpriced and
     record its model in `UnpricedModels`.
   - Collect distinct contributing models into `Models` (sorted).
1. **`HasCost` / `CostUSD` semantics:** `HasCost` is `true` iff there is at
   least one cost-contributing row **and every** cost-contributing row was
   priced or had an explicit `cost_usd`. `CostUSD` is the full total **only when
   `HasCost` is true; otherwise it is forced to `0`** — we never emit a partial
   total, so a downstream consumer that ignores `HasCost` still cannot read a
   misleadingly-low number. The diagnostic is preserved in `UnpricedModels`:
   - empty + `HasCost:false` → no cost data at all ("not available for this
     agent/model").
   - populated + `HasCost:false` → some models were priced but others not; the
     partial total is intentionally suppressed (`CostUSD = 0`).

The per-row cost+priced logic is a small focused helper; `usageAmounts()` (used
by the hot daily/top-sessions paths) is left untouched.

**Dedup scope (intentional, per review).** `GetTopSessionsByCost` dedups Claude
`message.id + request.id` across the *entire* queried row set, so a row shared
across a fork/subagent boundary is credited to whichever session sorts first.
`session usage` filters to the target session first and dedups only *within*
that session — it reports the target session's **own** usage, which can exceed
its dashboard-"credited" share for forked/subagent sessions. This is deliberate:
"credit" is defined relative to a queried set and date range, neither of which
exists for an all-time single-session lookup; the self-contained per-session
total is well-defined, matches roborev's fetch (fresh, non-resumed sessions
only), and avoids scanning unrelated sessions on every call. The command help
states this so the divergence is not surprising.

### 2. agentsview — `session usage <id>` command

New `cmd/agentsview/session_usage.go`, registered on the `session` group in
`session.go`. It reuses the `token-use` direct-DB + sync/startup-wait plumbing
(extracted into a shared helper so both commands share it), then calls
`GetSessionUsage` and renders.

- Resolution + on-demand sync identical to today's `token-use` (resolve raw ID,
  detect daemon, wait for startup, `SyncSingleSession` when no daemon owns the
  DB). This is the behavior roborev relies on and must not change.

- **Pricing setup (required, per review).** The direct path opens SQLite via
  `db.Open`, which — unlike `openDB` — does not apply custom pricing or seed
  `model_pricing`. Two distinct steps with different write semantics:

  - `applyCustomPricing(db, cfg)` — **always** run. It only sets the handle's
    in-memory `customPricing` map (`SetCustomPricing`), no DB write, so custom
    rates apply to this handle's `loadPricingMap` whether or not a daemon is
    running.
  - Fallback presence — a DB write, so run it **only when no writable local
    daemon / startup lock owns the DB** (the same `!serverActive` condition,
    re-checked, under which `token-use` performs its on-demand
    `SyncSingleSession`). When a daemon is active, rely on its startup
    `seedPricing()` and read the DB as-is — never compete for the write lock.
  - That write must be **non-destructive**: a new
    `InsertMissingModelPricing(fallback)` using
    `INSERT ... ON CONFLICT (model_pattern) DO NOTHING`, **not**
    `UpsertModelPricing` (which does `ON CONFLICT DO UPDATE` and would overwrite
    a fallback-listed model's fresher LiteLLM row — and `ensurePricing()` can
    leave LiteLLM rows present with no `_fallback_version`, so a version guard
    is not a safe substitute). Insert-missing is idempotent, needs no version
    guard, does **no** LiteLLM fetch (offline-safe, no latency on roborev's 10s
    budget), and never clobbers existing LiteLLM/custom/fallback rows.
    Corrective fallback-rate updates (on a `FallbackVersion` bump) remain the
    server `seedPricing()` path's job.

  Without this, a fresh CLI-only data dir would report `has_cost:false` for
  models that are in the fallback catalog.

- `--format` is inherited from the `session` group (default `human`). roborev
  passes `--format json` explicitly.

- JSON output embeds `SessionUsage` and adds `server_running` (the command knows
  the transport/sync state; the DB method does not). The result is a strict
  superset of today's `token-use` JSON.

- Human output: a compact key/value summary (Output, Peak ctx, Cost). Cost line
  shows `~$0.42 (claude-opus-4-6)` when `HasCost`, else `n/a` (with
  `unpriced: <models>` when applicable).

- **Exit codes (cost-aware, per review).** A thin cobra `Run` wrapper (not
  `RunE`) calls the shared core and `os.Exit`s with its code, mirroring
  `token-use` — cobra `RunE` errors all collapse to exit 1 via `main`'s `fatal`,
  which would lose the 2/3 codes. The wrapper reads `--format` from the
  inherited persistent flag. Codes:

  - `0` — session found AND (`HasTokenData` OR `HasCost`). Useful JSON on
    stdout.
  - `2` — session not found.
  - `3` — session found but neither token data nor cost.
  - `1` — operational error.

  Exit `3` therefore **never** co-occurs with `has_cost:true`: a cost-only
  session (e.g. Hermes via `usage_events.cost_usd` with zero session-level token
  aggregates) returns `0`, so roborev does not discard its cost. As today, JSON
  is still written to stdout on exit `3` (with zeros).

### 3. agentsview — deprecate `token-use`

- Refactor `token_use.go` so the resolve+sync+read core is shared with
  `session usage`. `token-use` keeps its current JSON-only output but now also
  includes the cost fields (free, since it shares the implementation).
- `token-use` prints a one-line stderr deprecation notice on each run:
  `note: 'token-use' is deprecated; use 'session usage <id>' instead`. stdout
  (the JSON contract) is unchanged, so existing parsers keep working.
- Document `session usage` as canonical in help text / README / CHANGELOG.
  `token-use` is not removed.

### 4. roborev — wiring (`~/code/roborev`)

`internal/tokens/tokens.go`:

- Add a second version threshold `sessionUsageMinVersion = {0,30,0}` (the
  agentsview release that introduces `session usage`; confirm against the
  agentsview CHANGELOG at implementation time). Keep `minVersion = {0,15,0}` as
  the floor for any token data.
- Command selection based on the already-parsed agentsview version:
  - `>= {0,30,0}` → `session --format json usage <id>` (tokens + cost).
  - `>= {0,15,0}` and `< {0,30,0}` → `token-use <id>` (tokens only, no cost).
    This is graceful: roborev keeps working on older agentsview, gains cost when
    new enough. No regression.
- **Fix exit-code handling (existing bug, per review).** `FetchForSession`
  currently special-cases exit `1` for not-found, but agentsview has always used
  `2` (not found) and `3` (no data) — so not-found/no-data sessions surface as
  hard errors today. Rework it to: capture stdout even on `*exec.ExitError`;
  treat exit `2` and `3` as "usage unavailable" → return `(nil, nil)` (no error,
  no log noise); on exit `0`, parse JSON and return `Usage` when tokens > 0 OR
  `HasCost`; return an error only for exit `1` / unexpected codes /
  non-ExitError failures. Applies uniformly to the `session usage` and legacy
  `token-use` invocations.
- Extend the parsed response and stored `Usage` with cost fields:

```go
type agentsviewResponse struct {
    SessionID         string  `json:"session_id"`
    Agent             string  `json:"agent"`
    Project           string  `json:"project"`
    OutputTokens      int64   `json:"total_output_tokens"`
    PeakContextTokens int64   `json:"peak_context_tokens"`
    CostUSD           float64 `json:"cost_usd"`
    HasCost           bool    `json:"has_cost"`
}

type Usage struct {
    OutputTokens      int64   `json:"total_output_tokens,omitempty"`
    PeakContextTokens int64   `json:"peak_context_tokens,omitempty"`
    CostUSD           float64 `json:"cost_usd,omitempty"`
    HasCost           bool    `json:"has_cost,omitempty"`
}
```

- `Usage.FormatSummary()` appends ` · ~$0.42` only when `HasCost` is true. The
  tilde marks it as an estimate (agentsview's model-pricing catalog), even
  though some agents occasionally report explicit cost. `show.go` and the TUI
  render via `FormatSummary()` and need no further change.
- Stored `TokenUsage` JSON on existing rows lacks cost; `HasCost` defaults to
  `false`, so old rows render exactly as before (tokens only) — backfill can
  re-fetch to add cost.

### Output examples

`agentsview session --format json usage claude:abc-123`:

```json
{
  "session_id": "claude:abc-123",
  "agent": "claude-code",
  "project": "roborev",
  "total_output_tokens": 28800,
  "peak_context_tokens": 118000,
  "has_token_data": true,
  "cost_usd": 0.42,
  "has_cost": true,
  "models": ["claude-opus-4-6"],
  "server_running": false
}
```

roborev display (cost available): `[118.0k ctx · 28.8k out · ~$0.42]` roborev
display (no cost / unpriced model): `[118.0k ctx · 28.8k out]`

## Testing

agentsview:

- `GetSessionUsage` table-driven unit tests (`internal/db`, `testDB(t)`, seed
  sessions + messages + `model_pricing`):
  - priced single model → `HasCost true`, expected `CostUSD`.
  - unpriced model → `HasCost false`, `CostUSD 0`, `UnpricedModels` set.
  - mixed priced + unpriced → `HasCost false`, `CostUSD 0` (partial suppressed),
    `UnpricedModels` lists the unpriced one.
  - explicit `usage_events.cost_usd`, zero session-level token aggregates →
    `HasCost true`, `CostUSD` = reported (the cost-only / Hermes case).
  - session with session-level token aggregates but no `token_usage` rows →
    metadata + `total_output_tokens` / `peak_context_tokens` preserved,
    `HasCost false`.
  - session not found → `(nil, nil)`.
- Pricing-seed tests: on a `db.Open`-only handle with an empty `model_pricing`
  table, the shared usage path inserts fallback and a fallback-catalog model
  (e.g. `claude-opus-4-6`) prices to `HasCost true`; custom rates from
  `cfg.CustomModelPricing` apply. `InsertMissingModelPricing` is
  non-destructive: a pre-existing row (e.g. a distinct LiteLLM rate for a
  fallback-listed model) is **not** overwritten. Fallback seeding is skipped
  when a writable local daemon is active (no DB write).
- `session usage` CLI test: JSON shape + human format; on-demand sync path; exit
  `0` (tokens or cost), `2` (not found), `3` (neither); cost-only session → exit
  `0` with `has_cost:true`; `token-use` still emits unchanged stdout JSON plus
  the stderr deprecation notice.

roborev:

- `tokens` unit tests: parse response with/without cost; `FormatSummary`
  with/without cost; version-based command selection (`session usage` vs
  `token-use` vs unsupported); exit-code handling — exit `2` and `3` →
  `(nil, nil)` (unavailable), exit `1` → error, exit `0` with cost → `Usage`
  carrying `CostUSD`/`HasCost`.
- Live end-to-end: run a roborev review against an agentsview build from this
  branch and confirm `~$X.XX` renders in `show` and the TUI.

## Rollout / version notes

- agentsview ships `session usage` in the next release (v0.30.0, after the
  current v0.29.0 tag). `token-use` remains as a deprecated alias.
- roborev's `sessionUsageMinVersion` must match that release version.

## Resolved decisions

- Command surface: new `session usage`, deprecate `token-use` (user direction).
- Display: `~$0.42` in roborev; numeric `cost_usd` in JSON.
- JSON scope: lean; only optional diagnostic field is `unpriced_models`.
- roborev invokes JSON explicitly: `session --format json usage <id>`.
- Dedup scope: `session usage` reports the target session's **own** usage
  (intra-session dedup only), intentionally diverging from the dashboard's
  cross-session credited total — well-defined for an all-time single-session
  lookup and matches roborev's usage. (Review finding 1.)
- Pricing on the direct path: always apply custom pricing (in-memory); seed
  fallback **only when no writable local daemon owns the DB**, via a
  non-destructive `InsertMissingModelPricing` (`ON CONFLICT DO NOTHING`, no
  network, never clobbers LiteLLM/custom rows). Corrective fallback updates stay
  with the server `seedPricing()`. (Review findings 2 + follow-ups 1–2.)
- Exit codes are cost-aware: `0` when token data **or** cost is present, so a
  cost-only session is never hidden behind exit `3`; roborev treats `2`/`3` as
  "unavailable" rather than errors (fixing an existing not-found bug). (Review
  finding 3.)
- Partial cost: when `HasCost` is false, `CostUSD` is forced to `0` (no partial
  total emitted); `UnpricedModels` carries the diagnostic. (Review open question
  — chose zeroing over carrying the partial, to prevent downstream misuse.)
