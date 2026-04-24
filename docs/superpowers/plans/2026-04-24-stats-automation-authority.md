# Stats Pipeline: `is_automated` as Authority — Implementation Plan (agentsview)

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `is_automated` the single authority for automation classification
in agentsview's `stats --format json` output, and expose human-scoped peer
fields on `agent_portfolio`.

**Architecture:** Thread the existing `sessions.is_automated` column through
`sessionStatsRow` into `computeTotalsAndArchetypes`, `computeDistributions`, and
`computeAgentPortfolio`. `archetypeLabel` becomes a shape-only helper
(`sessionShapeLabel`); automation is decided upstream by the flag. New peer
fields `by_sessions_human`/`by_tokens_human`/`by_messages_human`/`primary_human`
on `StatsAgentPortfolio` carry the human-scoped view.

**Tech Stack:** Go 1.21+, SQLite via `database/sql`, `testing` stdlib.

**Spec:**
`docs/superpowers/specs/2026-04-24-stats-automation-authority-design.md`

**Branch:** `stats/is-automated-authority` (already created off origin/main).

______________________________________________________________________

## Task 1: Test fixture override for `is_automated`

**Why first:** `UpsertSession` recomputes `is_automated` from `FirstMessage`, so
the existing `sessionFixture.isAutomated` assignment silently loses for any
fixture whose first message isn't classifier-matching. All later tasks rely on
precise fixture control. Add a post-insert SQL patch path so `f.isAutomated`
becomes authoritative.

**Files:**

- Modify: `internal/db/session_stats_test.go` (lines 59–118,
  `insertSessionFixture`)

- Test: `internal/db/session_stats_test.go` (new test
  `Test_insertSessionFixture_isAutomated_patch`)

- [ ] **Step 1: Write the failing test**

Add at the top of `session_stats_test.go` (after imports, before existing
tests):

```go
func Test_insertSessionFixture_isAutomated_patch(t *testing.T) {
	d := newTestDB(t)
	insertSessionFixture(t, d, sessionFixture{
		id: "auto-1", userMsgs: 5, startedAt: hoursAgo(1),
		isAutomated: true,
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "human-1", userMsgs: 1, startedAt: hoursAgo(1),
		isAutomated: false,
	})

	var autoFlag, humanFlag int
	if err := d.getReader().QueryRow(
		"SELECT is_automated FROM sessions WHERE id = ?", "auto-1",
	).Scan(&autoFlag); err != nil {
		t.Fatalf("read auto-1: %v", err)
	}
	if err := d.getReader().QueryRow(
		"SELECT is_automated FROM sessions WHERE id = ?", "human-1",
	).Scan(&humanFlag); err != nil {
		t.Fatalf("read human-1: %v", err)
	}
	if autoFlag != 1 {
		t.Fatalf("auto-1 is_automated = %d, want 1", autoFlag)
	}
	if humanFlag != 0 {
		t.Fatalf("human-1 is_automated = %d, want 0", humanFlag)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
`go test ./internal/db/ -run Test_insertSessionFixture_isAutomated_patch -v`
Expected: FAIL — `auto-1` is_automated will be `0` because `UpsertSession`
overwrote the field (first message empty → `IsAutomatedSession` false).

- [ ] **Step 3: Extend `insertSessionFixture` to patch post-insert**

After the `insertSession(...)` / `seedAssistantActivity(...)` lines in
`insertSessionFixture` (currently line 117), add:

```go
	// UpsertSession recomputes is_automated from FirstMessage, so a
	// fixture's f.isAutomated alone would be silently clobbered when
	// no first message is set. Patch the column after the upsert so
	// f.isAutomated is the authoritative value the stats pipeline
	// reads. Test-only path; production ingest always flows through
	// UpsertSession's classifier.
	var want int
	if f.isAutomated {
		want = 1
	}
	if _, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = ? WHERE id = ?",
		want, f.id,
	); err != nil {
		t.Fatalf("insertSessionFixture %s: patch is_automated: %v",
			f.id, err)
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run:
`go test ./internal/db/ -run Test_insertSessionFixture_isAutomated_patch -v`
Expected: PASS.

- [ ] **Step 5: Run the full package test suite to catch regressions**

Run: `go test ./internal/db/` Expected: PASS. Any existing test whose fixture
relied on the old implicit behavior (e.g. userMsgs\<=1 sessions silently flagged
as automated) will keep working because those sessions still bucket as
"automation" via the userMsgs heuristic until Task 3 lands. The patch writes `0`
for fixtures with `isAutomated: false`, which is idempotent with SQLite's
default.

- [ ] **Step 6: Commit**

```bash
git add internal/db/session_stats_test.go
git commit -m "test(stats): make fixture is_automated authoritative via post-insert patch"
```

______________________________________________________________________

## Task 2: Project `is_automated` into `sessionStatsRow`

**Files:**

- Modify: `internal/db/session_stats.go` (struct at line 344; SELECT at line
  434; scan at line 466)

- Test: `internal/db/session_stats_test.go` (new test
  `Test_loadSessionsInWindow_isAutomated`)

- [ ] **Step 1: Write the failing test**

```go
func Test_loadSessionsInWindow_isAutomated(t *testing.T) {
	d := newTestDB(t)
	insertSessionFixture(t, d, sessionFixture{
		id: "auto", userMsgs: 5, startedAt: hoursAgo(1),
		isAutomated: true,
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "human", userMsgs: 1, startedAt: hoursAgo(1),
		isAutomated: false,
	})

	ctx := t.Context()
	from := time.Now().Add(-24 * time.Hour)
	to := time.Now().Add(1 * time.Hour)
	rows, err := d.loadSessionsInWindow(ctx, StatsFilter{}, from, to)
	if err != nil {
		t.Fatalf("loadSessionsInWindow: %v", err)
	}
	byID := map[string]bool{}
	for _, r := range rows {
		byID[r.id] = r.isAutomated
	}
	if got, want := byID["auto"], true; got != want {
		t.Fatalf("auto.isAutomated = %v, want %v", got, want)
	}
	if got, want := byID["human"], false; got != want {
		t.Fatalf("human.isAutomated = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run Test_loadSessionsInWindow_isAutomated -v`
Expected: FAIL — `r.isAutomated` doesn't compile (field missing).

- [ ] **Step 3: Extend the struct**

In `internal/db/session_stats.go`, add to `sessionStatsRow` (right after
`cwd string` at line 369):

```go
	// isAutomated mirrors sessions.is_automated. Consumed by
	// computeTotalsAndArchetypes, computeDistributions, and
	// computeAgentPortfolio as the single source of truth for
	// whether a session is automated.
	isAutomated bool
```

- [ ] **Step 4: Extend the SELECT**

In `internal/db/session_stats.go`, extend the `SELECT` in `loadSessionsInWindow`
(the trailing projected columns starting at line 447):

```go
		s.outcome, COALESCE(s.health_grade, ''),
		s.tool_retry_count, s.compaction_count, s.edit_churn_count,
		COALESCE(s.cwd, ''),
		s.is_automated
		FROM sessions s WHERE ` + strings.Join(preds, " AND ")
```

- [ ] **Step 5: Extend the scan**

In `loadSessionsInWindow`, extend the local variable declaration and
`sqlRows.Scan` call. Before the scan (around line 465), change
`var hasTotalTokens, hasPeak int` to:

```go
		var hasTotalTokens, hasPeak, isAutomated int
```

Add `&isAutomated` as the last `Scan` argument (after `&r.cwd`):

```go
		if err := sqlRows.Scan(
			&r.id, &r.agent, &r.project,
			&startedAt, &endedAt,
			&r.messageCount, &r.userMessageCount,
			&r.totalOutputTokens, &hasTotalTokens,
			&r.peakContextTokens, &hasPeak,
			&r.totalToolCalls, &r.assistantTurns,
			&r.outcome, &r.healthGrade,
			&r.toolRetryCount, &r.compactionCount, &r.editChurnCount,
			&r.cwd,
			&isAutomated,
		); err != nil {
```

After the existing conversions (lines 499–500), add:

```go
		r.isAutomated = isAutomated == 1
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/db/ -run Test_loadSessionsInWindow_isAutomated -v`
Expected: PASS.

- [ ] **Step 7: Run the full package test suite**

Run: `go test ./internal/db/` Expected: PASS. No behavior consumers of
`r.isAutomated` yet, so no other test changes should be necessary.

- [ ] **Step 8: Commit**

```bash
git add internal/db/session_stats.go internal/db/session_stats_test.go
git commit -m "stats: project is_automated into sessionStatsRow"
```

______________________________________________________________________

## Task 3: Rename `archetypeLabel` → `sessionShapeLabel`; switch totals/archetypes to the flag

**Files:**

- Modify: `internal/db/session_stats.go` (function at line 523; caller at line
  540–580)

- Test: `internal/db/session_stats_test.go` (new test
  `Test_computeTotalsAndArchetypes_flagAuthority`)

- [ ] **Step 1: Write the failing test**

```go
func Test_computeTotalsAndArchetypes_flagAuthority(t *testing.T) {
	d := newTestDB(t)
	// Short non-automated session — must count as human, bucket as "quick".
	insertSessionFixture(t, d, sessionFixture{
		id: "short-human", userMsgs: 1, startedAt: hoursAgo(1),
		isAutomated: false,
	})
	// Automated session (userMsgs doesn't matter) — must count as
	// automation, bucket as "automation".
	insertSessionFixture(t, d, sessionFixture{
		id: "auto", userMsgs: 3, startedAt: hoursAgo(1),
		isAutomated: true,
	})

	got, err := d.GetSessionStats(t.Context(), StatsFilter{Since: "1d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}
	if got.Totals.SessionsHuman != 1 {
		t.Fatalf("SessionsHuman = %d, want 1", got.Totals.SessionsHuman)
	}
	if got.Totals.SessionsAutomation != 1 {
		t.Fatalf("SessionsAutomation = %d, want 1",
			got.Totals.SessionsAutomation)
	}
	if got.Archetypes.Quick != 1 {
		t.Fatalf("Archetypes.Quick = %d, want 1 (short non-automated)",
			got.Archetypes.Quick)
	}
	if got.Archetypes.Automation != 1 {
		t.Fatalf("Archetypes.Automation = %d, want 1", got.Archetypes.Automation)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
`go test ./internal/db/ -run Test_computeTotalsAndArchetypes_flagAuthority -v`
Expected: FAIL — `short-human` (userMsgs=1) currently buckets as "automation"
via the userMsgs heuristic, so `Archetypes.Quick = 0`,
`Archetypes.Automation = 2`, `SessionsHuman = 0`.

- [ ] **Step 3: Rename `archetypeLabel` → `sessionShapeLabel` and drop the
  automation branch**

Replace the function at `session_stats.go:520–536`:

```go
// sessionShapeLabel classifies a *non-automated* session by its
// user_message_count. Automated sessions are handled upstream (the
// caller assigns "automation" based on sessions.is_automated) and
// never pass through this helper, so the lower band starts at 0
// rather than 1. Boundaries are inclusive on both sides of each band.
func sessionShapeLabel(userMsgs int) string {
	switch {
	case userMsgs <= 5:
		return "quick"
	case userMsgs <= 15:
		return "standard"
	case userMsgs <= 50:
		return "deep"
	default:
		return "marathon"
	}
}
```

- [ ] **Step 4: Update `computeTotalsAndArchetypes` to consult `isAutomated`**

Replace the body at `session_stats.go:540–580`:

```go
func computeTotalsAndArchetypes(
	s *SessionStats, rows []sessionStatsRow,
) {
	archMax := map[string]int{}
	humanMax := map[string]int{}
	for _, r := range rows {
		s.Totals.SessionsAll++
		s.Totals.MessagesTotal += r.messageCount
		s.Totals.UserMessagesTotal += r.userMessageCount

		var label string
		if r.isAutomated {
			label = "automation"
			s.Archetypes.Automation++
			s.Totals.SessionsAutomation++
		} else {
			label = sessionShapeLabel(r.userMessageCount)
			s.Totals.SessionsHuman++
			switch label {
			case "quick":
				s.Archetypes.Quick++
			case "standard":
				s.Archetypes.Standard++
			case "deep":
				s.Archetypes.Deep++
			case "marathon":
				s.Archetypes.Marathon++
			}
			humanMax[label]++
		}
		archMax[label]++
	}
	s.Archetypes.Primary = pickMaxLabel(archMax, []string{
		"automation", "marathon", "deep", "standard", "quick",
	})
	s.Archetypes.PrimaryHuman = pickMaxLabel(humanMax, []string{
		"marathon", "deep", "standard", "quick",
	})
}
```

- [ ] **Step 5: Run test to verify it passes**

Run:
`go test ./internal/db/ -run Test_computeTotalsAndArchetypes_flagAuthority -v`
Expected: PASS.

- [ ] **Step 6: Run the full package test suite**

Run: `go test ./internal/db/` Expected: PASS. Existing tests seed fixtures where
userMsgs≤1 implied automation. Now that the patch from Task 1 makes
`f.isAutomated` authoritative, those fixtures will count as human (71-session
style cases). If any existing test hard-coded `Archetypes.Automation = N` based
on the old heuristic, update it to set `isAutomated: true` on the fixture or
adjust the expectation. Inspect failures individually — do not bulk-edit.

- [ ] **Step 7: Commit**

```bash
git add internal/db/session_stats.go internal/db/session_stats_test.go
git commit -m "stats: route totals/archetypes through is_automated flag"
```

______________________________________________________________________

## Task 4: `computeDistributions` `scope_human` uses `!isAutomated`

**Files:**

- Modify: `internal/db/session_stats.go` (comment at 648; gate at 674)

- Test: `internal/db/session_stats_test.go` (new test
  `Test_computeDistributions_scopeHuman_flag`)

- [ ] **Step 1: Write the failing test**

The test uses the scope_human **mean** rather than bucket-sum counts: under the
old rule and the new rule, bucket-sum sizes can coincidentally collide (one
session either way), but the mean differs sharply because `short-human` is 3 min
and `auto-long` is 30 min.

```go
func Test_computeDistributions_scopeHuman_flag(t *testing.T) {
	d := newTestDB(t)
	// Short non-automated: must count in scope_human.
	insertSessionFixture(t, d, sessionFixture{
		id: "short-human", userMsgs: 1, durationMin: 3,
		startedAt: hoursAgo(1), isAutomated: false,
	})
	// Multi-turn automated: must be excluded from scope_human.
	insertSessionFixture(t, d, sessionFixture{
		id: "auto-long", userMsgs: 4, durationMin: 30,
		startedAt: hoursAgo(1), isAutomated: true,
	})

	got, err := d.GetSessionStats(t.Context(), StatsFilter{Since: "1d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}
	// scope_all has both rows — mean ~= 16.5.
	allMean := got.Distributions.DurationMinutes.ScopeAll.Mean
	if allMean < 15 || allMean > 18 {
		t.Fatalf("scope_all duration mean = %.2f, want ~16.5", allMean)
	}
	// scope_human has only the non-automated short session — mean ~= 3.
	humanMean := got.Distributions.DurationMinutes.ScopeHuman.Mean
	if humanMean < 2 || humanMean > 4 {
		t.Fatalf("scope_human duration mean = %.2f, want ~3 (short-human only)",
			humanMean)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run Test_computeDistributions_scopeHuman_flag -v`
Expected: FAIL with `scope_human duration mean = 30.00, want ~3` — the current
rule `human := r.userMessageCount >= 2` excludes `short-human` (userMsgs=1) and
includes `auto-long` (userMsgs=4), so scope_human mean reflects only
`auto-long`.

- [ ] **Step 3: Update `computeDistributions`**

In `session_stats.go`, change line 674:

```go
		human := !r.isAutomated
```

Update the doc comment at line 648:

```go
//   - ScopeHuman excludes any row where is_automated is set. This
//     aligns scope_human with the single authority for automation
//     classification; the old userMessageCount >= 2 heuristic is
//     gone.
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/db/ -run Test_computeDistributions_scopeHuman_flag -v`
Expected: PASS — `mean ~= 3`.

- [ ] **Step 5: Run the full package test suite**

Run: `go test ./internal/db/` Expected: PASS. Audit existing
distribution-scope_human tests for fixtures whose userMsgs\<=1 rows were
implicitly excluded; those now count as human when `isAutomated: false`. Adjust
expected bucket counts or fixtures as needed per failure.

- [ ] **Step 6: Commit**

```bash
git add internal/db/session_stats.go internal/db/session_stats_test.go
git commit -m "stats: scope_human reads is_automated instead of userMessageCount"
```

______________________________________________________________________

## Task 5: Extend `StatsAgentPortfolio` with human-scoped peer fields and populate them

**Files:**

- Modify: `internal/db/session_stats_types.go` (struct at line 114)

- Modify: `internal/db/session_stats.go` (`computeAgentPortfolio` at line 768)

- Test: `internal/db/session_stats_test.go` (new test
  `Test_computeAgentPortfolio_humanScoped`)

- [ ] **Step 1: Write the failing test**

```go
func Test_computeAgentPortfolio_humanScoped(t *testing.T) {
	d := newTestDB(t)
	insertSessionFixture(t, d, sessionFixture{
		id: "claude-human", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(1), totalOutputTok: 100,
		isAutomated: false,
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "codex-auto", agent: "codex", userMsgs: 1,
		startedAt: hoursAgo(1), totalOutputTok: 50,
		isAutomated: true,
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "gemini-auto", agent: "gemini", userMsgs: 1,
		startedAt: hoursAgo(1), totalOutputTok: 25,
		isAutomated: true,
	})

	got, err := d.GetSessionStats(t.Context(), StatsFilter{Since: "1d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}
	ap := got.AgentPortfolio

	// All-sessions view: every agent present.
	if ap.BySessions["claude"] != 1 || ap.BySessions["codex"] != 1 ||
		ap.BySessions["gemini"] != 1 {
		t.Fatalf("BySessions = %v, want claude=1,codex=1,gemini=1",
			ap.BySessions)
	}
	// primary ties on count; lexicographic min wins → claude.
	if ap.Primary != "claude" {
		t.Fatalf("Primary = %q, want claude", ap.Primary)
	}

	// Human-scoped view: only claude.
	if _, ok := ap.BySessionsHuman["codex"]; ok {
		t.Fatalf("BySessionsHuman must exclude codex: %v",
			ap.BySessionsHuman)
	}
	if _, ok := ap.BySessionsHuman["gemini"]; ok {
		t.Fatalf("BySessionsHuman must exclude gemini: %v",
			ap.BySessionsHuman)
	}
	if ap.BySessionsHuman["claude"] != 1 {
		t.Fatalf("BySessionsHuman[claude] = %d, want 1",
			ap.BySessionsHuman["claude"])
	}
	if ap.ByTokensHuman["claude"] != 100 {
		t.Fatalf("ByTokensHuman[claude] = %d, want 100",
			ap.ByTokensHuman["claude"])
	}
	if ap.PrimaryHuman != "claude" {
		t.Fatalf("PrimaryHuman = %q, want claude", ap.PrimaryHuman)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run Test_computeAgentPortfolio_humanScoped -v`
Expected: FAIL — `ap.BySessionsHuman` undefined field.

- [ ] **Step 3: Extend the type**

In `internal/db/session_stats_types.go`, replace the `StatsAgentPortfolio`
struct (line 114):

```go
type StatsAgentPortfolio struct {
	BySessions map[string]int   `json:"by_sessions"`
	ByTokens   map[string]int64 `json:"by_tokens"`
	ByMessages map[string]int   `json:"by_messages"`
	Primary    string           `json:"primary"`

	// Human-scoped peer fields. Populated alongside the all-sessions
	// maps and filtered to rows where is_automated = 0. Introduced in
	// the flag-authority pipeline change; tkmx-server's renderer
	// prefers these when every portfolio-bearing blob in a user's
	// machine set carries them.
	BySessionsHuman map[string]int   `json:"by_sessions_human"`
	ByTokensHuman   map[string]int64 `json:"by_tokens_human"`
	ByMessagesHuman map[string]int   `json:"by_messages_human"`
	PrimaryHuman    string           `json:"primary_human"`
}
```

- [ ] **Step 4: Extend `computeAgentPortfolio`**

Replace the function body at `session_stats.go:768–786`:

```go
func computeAgentPortfolio(s *SessionStats, rows []sessionStatsRow) {
	bySessions := map[string]int{}
	byMessages := map[string]int{}
	byTokens := map[string]int64{}
	bySessionsHuman := map[string]int{}
	byMessagesHuman := map[string]int{}
	byTokensHuman := map[string]int64{}
	for _, r := range rows {
		if r.agent == "" {
			continue
		}
		bySessions[r.agent]++
		byMessages[r.agent] += r.messageCount
		if r.hasTotalOutputTokens {
			byTokens[r.agent] += r.totalOutputTokens
		}
		if !r.isAutomated {
			bySessionsHuman[r.agent]++
			byMessagesHuman[r.agent] += r.messageCount
			if r.hasTotalOutputTokens {
				byTokensHuman[r.agent] += r.totalOutputTokens
			}
		}
	}
	s.AgentPortfolio.BySessions = bySessions
	s.AgentPortfolio.ByMessages = byMessages
	s.AgentPortfolio.ByTokens = byTokens
	s.AgentPortfolio.Primary = pickPrimaryAgent(bySessions)
	s.AgentPortfolio.BySessionsHuman = bySessionsHuman
	s.AgentPortfolio.ByMessagesHuman = byMessagesHuman
	s.AgentPortfolio.ByTokensHuman = byTokensHuman
	s.AgentPortfolio.PrimaryHuman = pickPrimaryAgent(bySessionsHuman)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/db/ -run Test_computeAgentPortfolio_humanScoped -v`
Expected: PASS.

- [ ] **Step 6: Run the full package test suite**

Run: `go test ./internal/db/` Expected: PASS. Existing `computeAgentPortfolio`
tests assert only the all-sessions maps; the new peers are additive. Any
JSON-shape / golden-file tests elsewhere in the repo may need the new fields
added to their golden output — walk failures one at a time.

- [ ] **Step 7: Commit**

```bash
git add internal/db/session_stats.go internal/db/session_stats_types.go internal/db/session_stats_test.go
git commit -m "stats: add human-scoped peer fields to agent_portfolio"
```

______________________________________________________________________

## Task 6: Update schema-contract comment on `SessionStats`

**Files:**

- Modify: `internal/db/session_stats_types.go` (lines 3–5)

- [ ] **Step 1: Update the contract comment**

Replace the comment block at the top of `SessionStats` (lines 3–5):

```go
// SessionStats is the top-level v1 output of GetSessionStats.
// schema_version is locked at 1. Additive fields (new keys that
// old consumers can ignore) and semantic tightening (e.g., routing
// an existing field through a stricter definition) are allowed
// within v1 without a bump as long as the field *shape* stays
// compatible. Incompatible shape changes or bucket-boundary shifts
// still require a version bump.
//
// Feature detection by consumers should use the presence of
// specific fields (e.g., agent_portfolio.by_sessions_human) or the
// reporter's agentsview_version, not schema_version, for non-bump
// changes.
```

- [ ] **Step 2: Build to confirm no syntax error**

Run: `go build ./...` Expected: PASS.

- [ ] **Step 3: Run the full package test suite**

Run: `go test ./internal/db/` Expected: PASS (comment change only).

- [ ] **Step 4: Commit**

```bash
git add internal/db/session_stats_types.go
git commit -m "docs(stats): clarify v1 schema contract — additive + tightening OK"
```

______________________________________________________________________

## Task 7: Run the full repo suite and push

- [ ] **Step 1: Full test suite**

Run: `make test` Expected: PASS. If anything under `cmd/agentsview/stats*` or
`internal/insight/` fails because of the archetype rebucketing
(71-sessions-style shift), investigate per failure — fix the fixture or the
expected value, not by weakening the new behavior.

- [ ] **Step 2: Lint**

Run: `make lint` Expected: PASS.

- [ ] **Step 3: Push the branch**

```bash
git push -u origin stats/is-automated-authority
```

- [ ] **Step 4: Open the PR via `gh`**

```bash
gh pr create --title "stats: make is_automated the authority across totals/archetypes/distributions/portfolio" --body "$(cat <<'EOF'
## Summary

- Project `is_automated` through `sessionStatsRow` so the stats pipeline consumes the flag instead of re-deriving automation from `userMessageCount`.
- Rename `archetypeLabel` to `sessionShapeLabel` and drop its automation branch; automation is now decided upstream by the flag.
- Switch `computeTotalsAndArchetypes`, `computeDistributions` (`scope_human`), and `computeAgentPortfolio` to the flag.
- Add human-scoped peer fields to `StatsAgentPortfolio`: `by_sessions_human`, `by_tokens_human`, `by_messages_human`, `primary_human`.
- `schema_version` stays at 1; new fields are additive and semantic tightening of existing fields is permitted in v1.

## Why

The `stats --format json` pipeline's "human vs automation" split was derived from `userMessageCount >= 2` — a proxy that predates the dedicated `is_automated` flag. 0.24.0 broadened `is_automated` classification (#369, #387) but those improvements never reached stats output. With the flag authoritative, future classifier improvements flow through automatically.

Design doc: `docs/superpowers/specs/2026-04-24-stats-automation-authority-design.md`

## Test plan

- [x] New unit tests for flag-authority in totals, archetypes, distributions scope_human, and agent_portfolio human-scoped peers.
- [x] `make test` passes.
- [x] `make lint` passes.

EOF
)"
```

______________________________________________________________________

## Self-review checklist (run after all tasks are in place, before execution)

- [ ] Every spec section under `Changes — agentsview` has a matching task.
- [ ] No "TBD", "fill in later", or "similar to Task N" in any step.
- [ ] Type names consistent: `sessionShapeLabel` (not
  `nonAutomationArchetypeLabel`), `BySessionsHuman` (Go) ↔ `by_sessions_human`
  (JSON).
- [ ] Every test step shows the actual test code; every code change step shows
  the actual code.
- [ ] Commit messages match repo style (lowercase prefix + colon, no
  "Co-Authored-By" trailer per AGENTS.md commit expectations).
