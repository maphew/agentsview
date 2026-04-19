package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// sessionFixture is a compact description of a seeded session used by
// session-stats tests. Fields mirror the subset of sessions-table
// columns the stats pipeline actually reads; extend in future tasks.
type sessionFixture struct {
	id           string
	project      string
	agent        string
	userMsgs     int
	messageCount int
	startedAt    string // RFC3339; required to place row in window
	endedAt      string // RFC3339 or ""
	// durationMin, when > 0 and endedAt is empty, derives endedAt as
	// startedAt + durationMin minutes. Ignored if endedAt is set.
	durationMin      float64
	peakContext      int
	hasPeakContext   bool
	totalOutputTok   int
	isAutomated      bool
	relationshipType string
	// totalToolCalls seeds that many rows in the tool_calls table for
	// this session, each attached to a synthetic assistant message.
	totalToolCalls int
	// assistantTurns seeds that many assistant-role messages for this
	// session. Set alongside totalToolCalls so tests can control the
	// tools_per_turn denominator precisely.
	assistantTurns int
}

// hoursAgo returns an RFC3339 timestamp N hours before now in UTC.
// Used to place fixture rows safely inside the default 28-day window.
func hoursAgo(n int) string {
	return time.Now().UTC().Add(-time.Duration(n) * time.Hour).
		Format(time.RFC3339)
}

// insertSessionFixture inserts a sessionFixture via the standard
// UpsertSession path so triggers and defaults stay authoritative.
// Defaults mirror insertSession in db_test.go (machine=local,
// agent=claude) but let tests override agent/project.
func insertSessionFixture(t *testing.T, d *DB, f sessionFixture) {
	t.Helper()
	project := f.project
	if project == "" {
		project = "proj"
	}
	agent := f.agent
	if agent == "" {
		agent = defaultAgent
	}
	// message_count must be > 0 so analytics WHERE clauses don't skip
	// the row; default to userMsgs*2 when not set explicitly.
	mc := f.messageCount
	if mc == 0 {
		mc = f.userMsgs * 2
		if mc == 0 {
			mc = 1
		}
	}
	endedAt := f.endedAt
	if endedAt == "" && f.durationMin > 0 && f.startedAt != "" {
		start, err := time.Parse(time.RFC3339, f.startedAt)
		if err != nil {
			t.Fatalf(
				"insertSessionFixture %s: parsing startedAt %q: %v",
				f.id, f.startedAt, err,
			)
		}
		dur := time.Duration(f.durationMin * float64(time.Minute))
		endedAt = start.Add(dur).UTC().Format(time.RFC3339Nano)
	}
	insertSession(t, d, f.id, project, func(s *Session) {
		s.Agent = agent
		s.UserMessageCount = f.userMsgs
		s.MessageCount = mc
		if f.startedAt != "" {
			s.StartedAt = Ptr(f.startedAt)
		}
		if endedAt != "" {
			s.EndedAt = Ptr(endedAt)
		}
		s.PeakContextTokens = f.peakContext
		s.HasPeakContextTokens = f.hasPeakContext
		s.TotalOutputTokens = f.totalOutputTok
		s.IsAutomated = f.isAutomated
		s.RelationshipType = f.relationshipType
	})
	seedAssistantActivity(t, d, f.id, f.assistantTurns, f.totalToolCalls)
}

// seedAssistantActivity inserts `turns` assistant messages and
// spreads `toolCalls` rows across them (or across a single synthetic
// message when turns==0 but toolCalls>0). Purpose: let stats tests
// control both the assistant-turn count (denominator of
// tools_per_turn) and the total tool-call count (numerator) without
// reaching into the full parser pipeline.
func seedAssistantActivity(
	t *testing.T, d *DB, sessionID string, turns, toolCalls int,
) {
	t.Helper()
	if turns == 0 && toolCalls == 0 {
		return
	}
	n := turns
	if n == 0 {
		n = 1 // need at least one host message for tool_calls FK
	}
	msgs := make([]Message, 0, n)
	for i := range n {
		msgs = append(msgs, asstMsg(sessionID, i+1, "reply"))
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatalf("seedAssistantActivity %s: InsertMessages: %v",
			sessionID, err)
	}
	if toolCalls == 0 {
		return
	}
	// Distribute tool_calls round-robin across inserted messages so
	// they all attach to a real message row. Rely on the router-like
	// INSERT ... SELECT ordinal to find the message_id.
	for i := range toolCalls {
		ord := (i % n) + 1
		if _, err := d.getWriter().Exec(`
			INSERT INTO tool_calls
				(message_id, session_id, tool_name, category)
			SELECT id, session_id, 'Read', 'file'
			FROM messages
			WHERE session_id = ? AND ordinal = ?`,
			sessionID, ord,
		); err != nil {
			t.Fatalf("seedAssistantActivity %s: tool_call: %v",
				sessionID, err)
		}
	}
}

// floatsClose reports whether a and b are within eps of each other.
// Used by stats tests that compare arithmetic means.
func floatsClose(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// seedToolCallsByCategory inserts one assistant message per entry in
// categories and a matching tool_calls row. Used by tool_mix tests
// that need precise control over category values (unlike
// seedAssistantActivity, which always writes category='file').
func seedToolCallsByCategory(
	t *testing.T, d *DB, sessionID string, categories []string,
) {
	t.Helper()
	if len(categories) == 0 {
		return
	}
	msgs := make([]Message, 0, len(categories))
	for i, cat := range categories {
		msgs = append(msgs, asstMsg(sessionID, i+1, "reply-"+cat))
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatalf("seedToolCallsByCategory %s: InsertMessages: %v",
			sessionID, err)
	}
	for i, cat := range categories {
		ord := i + 1
		if _, err := d.getWriter().Exec(`
			INSERT INTO tool_calls
				(message_id, session_id, tool_name, category)
			SELECT id, session_id, ?, ?
			FROM messages
			WHERE session_id = ? AND ordinal = ?`,
			cat, cat, sessionID, ord,
		); err != nil {
			t.Fatalf("seedToolCallsByCategory %s: %q: %v",
				sessionID, cat, err)
		}
	}
}

// seedModelMessages inserts one assistant message per (model, tokens)
// pair so the model_mix query sees a stable per-message row with known
// output_tokens. Ordinals are taken relative to startOrd so callers can
// layer multiple seed passes onto the same session without colliding.
func seedModelMessages(
	t *testing.T, d *DB, sessionID string, startOrd int,
	pairs []struct {
		model  string
		tokens int
	},
) {
	t.Helper()
	if len(pairs) == 0 {
		return
	}
	msgs := make([]Message, 0, len(pairs))
	for i, p := range pairs {
		m := asstMsg(sessionID, startOrd+i, "reply")
		m.Model = p.model
		m.OutputTokens = p.tokens
		m.HasOutputTokens = true
		msgs = append(msgs, m)
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatalf("seedModelMessages %s: InsertMessages: %v",
			sessionID, err)
	}
}

func TestArchetypeLabel(t *testing.T) {
	cases := []struct {
		userMsgs int
		want     string
	}{
		{0, "automation"},
		{1, "automation"},
		{2, "quick"},
		{5, "quick"},
		{6, "standard"},
		{15, "standard"},
		{16, "deep"},
		{50, "deep"},
		{51, "marathon"},
		{1000, "marathon"},
	}
	for _, c := range cases {
		got := archetypeLabel(c.userMsgs)
		if got != c.want {
			t.Errorf(
				"archetypeLabel(%d): got %q, want %q",
				c.userMsgs, got, c.want,
			)
		}
	}
}

func TestPickMaxLabel_TiesBreakByPriority(t *testing.T) {
	// automation (2) vs deep (2) — priority says automation wins.
	counts := map[string]int{"automation": 2, "deep": 2, "quick": 1}
	priority := []string{
		"automation", "marathon", "deep", "standard", "quick",
	}
	if got := pickMaxLabel(counts, priority); got != "automation" {
		t.Errorf("tie break: got %q want automation", got)
	}
	// PrimaryHuman excludes automation; marathon should win a 1/1/1
	// tie over deep/standard/quick.
	humanCounts := map[string]int{
		"quick": 1, "standard": 1, "deep": 1, "marathon": 1,
	}
	humanPriority := []string{"marathon", "deep", "standard", "quick"}
	if got := pickMaxLabel(humanCounts, humanPriority); got != "marathon" {
		t.Errorf("human tie break: got %q want marathon", got)
	}
	// Strictly greater wins regardless of priority.
	c2 := map[string]int{"quick": 5, "deep": 2}
	if got := pickMaxLabel(c2, priority); got != "quick" {
		t.Errorf("strict max: got %q want quick", got)
	}
}

func TestGetSessionStats_TotalsAndArchetypes(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// 5 sessions: 2 automation (userMsgs 0,1),
	//             2 deep (userMsgs 20, 40),
	//             1 marathon (userMsgs 100).
	fixtures := []sessionFixture{
		{id: "s1", userMsgs: 0, startedAt: hoursAgo(5)},
		{id: "s2", userMsgs: 1, startedAt: hoursAgo(5)},
		{id: "s3", userMsgs: 20, startedAt: hoursAgo(5)},
		{id: "s4", userMsgs: 40, startedAt: hoursAgo(5)},
		{id: "s5", userMsgs: 100, startedAt: hoursAgo(5)},
	}
	for _, f := range fixtures {
		insertSessionFixture(t, d, f)
	}

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	if stats.SchemaVersion != 1 {
		t.Errorf("schema_version: got %d want 1", stats.SchemaVersion)
	}
	if stats.Totals.SessionsAll != 5 {
		t.Errorf("sessions_all: got %d want 5",
			stats.Totals.SessionsAll)
	}
	if stats.Totals.SessionsAutomation != 2 {
		t.Errorf("sessions_automation: got %d want 2",
			stats.Totals.SessionsAutomation)
	}
	if stats.Totals.SessionsHuman != 3 {
		t.Errorf("sessions_human: got %d want 3",
			stats.Totals.SessionsHuman)
	}
	// Invariant: human + automation must equal all.
	if stats.Totals.SessionsHuman+stats.Totals.SessionsAutomation !=
		stats.Totals.SessionsAll {
		t.Errorf(
			"invariant: human (%d) + automation (%d) != all (%d)",
			stats.Totals.SessionsHuman,
			stats.Totals.SessionsAutomation,
			stats.Totals.SessionsAll,
		)
	}
	if got := stats.Totals.UserMessagesTotal; got != 0+1+20+40+100 {
		t.Errorf("user_messages_total: got %d want 161", got)
	}

	if stats.Archetypes.Automation != 2 {
		t.Errorf("archetypes.automation: got %d want 2",
			stats.Archetypes.Automation)
	}
	if stats.Archetypes.Quick != 0 {
		t.Errorf("archetypes.quick: got %d want 0",
			stats.Archetypes.Quick)
	}
	if stats.Archetypes.Standard != 0 {
		t.Errorf("archetypes.standard: got %d want 0",
			stats.Archetypes.Standard)
	}
	if stats.Archetypes.Deep != 2 {
		t.Errorf("archetypes.deep: got %d want 2",
			stats.Archetypes.Deep)
	}
	if stats.Archetypes.Marathon != 1 {
		t.Errorf("archetypes.marathon: got %d want 1",
			stats.Archetypes.Marathon)
	}
	// 2 automation, 2 deep — tie broken by priority: automation first.
	if stats.Archetypes.Primary != "automation" {
		t.Errorf("archetypes.primary: got %q want automation",
			stats.Archetypes.Primary)
	}
	// Human subset: 2 deep, 1 marathon. Deep wins.
	if stats.Archetypes.PrimaryHuman != "deep" {
		t.Errorf("archetypes.primary_human: got %q want deep",
			stats.Archetypes.PrimaryHuman)
	}

	// Window bookkeeping: Since = now-28d, Until = now, days = 28.
	if stats.Window.Days != 28 {
		t.Errorf("window.days: got %d want 28", stats.Window.Days)
	}
	if stats.Window.Since == "" || stats.Window.Until == "" {
		t.Errorf("window bounds empty: since=%q until=%q",
			stats.Window.Since, stats.Window.Until)
	}
	if _, err := time.Parse(time.RFC3339, stats.Window.Since); err != nil {
		t.Errorf("window.since not RFC3339: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, stats.Window.Until); err != nil {
		t.Errorf("window.until not RFC3339: %v", err)
	}

	// Filters echo the inputs and default Agent to "all".
	if stats.Filters.Agent != "all" {
		t.Errorf("filters.agent: got %q want all", stats.Filters.Agent)
	}
	if stats.Filters.Timezone != "UTC" {
		t.Errorf("filters.timezone: got %q want UTC",
			stats.Filters.Timezone)
	}
	if stats.Filters.ProjectsExcluded == nil {
		t.Errorf("filters.projects_excluded must be non-nil slice")
	}

	if stats.GeneratedAt == "" {
		t.Errorf("generated_at empty")
	}
}

func TestGetSessionStats_FilterByAgent(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSessionFixture(t, d, sessionFixture{
		id: "c1", agent: "claude", userMsgs: 10,
		startedAt: hoursAgo(3),
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "x1", agent: "codex", userMsgs: 10,
		startedAt: hoursAgo(3),
	})

	all, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats all: %v", err)
	}
	if all.Totals.SessionsAll != 2 {
		t.Errorf("all agents: got %d want 2",
			all.Totals.SessionsAll)
	}

	onlyClaude, err := d.GetSessionStats(
		ctx, StatsFilter{Since: "28d", Agent: "claude"},
	)
	if err != nil {
		t.Fatalf("GetSessionStats claude: %v", err)
	}
	if onlyClaude.Totals.SessionsAll != 1 {
		t.Errorf("agent=claude: got %d want 1",
			onlyClaude.Totals.SessionsAll)
	}
	if onlyClaude.Filters.Agent != "claude" {
		t.Errorf("agent filter echoed: got %q want claude",
			onlyClaude.Filters.Agent)
	}
}

func TestGetSessionStats_FilterByProject(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	for i, p := range []string{"alpha", "alpha", "beta", "gamma"} {
		insertSessionFixture(t, d, sessionFixture{
			id:        fmt.Sprintf("p%d", i),
			project:   p,
			userMsgs:  10,
			startedAt: hoursAgo(2),
		})
	}

	includeAlpha, err := d.GetSessionStats(ctx, StatsFilter{
		Since:           "28d",
		IncludeProjects: []string{"alpha"},
	})
	if err != nil {
		t.Fatalf("include alpha: %v", err)
	}
	if includeAlpha.Totals.SessionsAll != 2 {
		t.Errorf("include=alpha: got %d want 2",
			includeAlpha.Totals.SessionsAll)
	}

	excludeAlpha, err := d.GetSessionStats(ctx, StatsFilter{
		Since:           "28d",
		ExcludeProjects: []string{"alpha"},
	})
	if err != nil {
		t.Fatalf("exclude alpha: %v", err)
	}
	if excludeAlpha.Totals.SessionsAll != 2 {
		t.Errorf("exclude=alpha: got %d want 2 (beta + gamma)",
			excludeAlpha.Totals.SessionsAll)
	}
}

func TestWindowBounds(t *testing.T) {
	// Fixed reference time so the tests are deterministic.
	now := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)

	t.Run("default 28d", func(t *testing.T) {
		from, to, days, err := windowBounds(StatsFilter{}, now)
		if err != nil {
			t.Fatalf("windowBounds: %v", err)
		}
		if days != 28 {
			t.Errorf("days: got %d want 28", days)
		}
		if !to.Equal(now) {
			t.Errorf("until: got %v want %v", to, now)
		}
		wantFrom := now.Add(-28 * 24 * time.Hour)
		if !from.Equal(wantFrom) {
			t.Errorf("since: got %v want %v", from, wantFrom)
		}
	})

	t.Run("Nd duration", func(t *testing.T) {
		_, _, days, err := windowBounds(
			StatsFilter{Since: "7d"}, now,
		)
		if err != nil {
			t.Fatalf("windowBounds: %v", err)
		}
		if days != 7 {
			t.Errorf("days: got %d want 7", days)
		}
	})

	t.Run("Nh duration", func(t *testing.T) {
		from, to, _, err := windowBounds(
			StatsFilter{Since: "48h"}, now,
		)
		if err != nil {
			t.Fatalf("windowBounds: %v", err)
		}
		if got := to.Sub(from); got != 48*time.Hour {
			t.Errorf("span: got %v want 48h", got)
		}
	})

	t.Run("bare date", func(t *testing.T) {
		from, _, _, err := windowBounds(
			StatsFilter{Since: "2026-04-01"}, now,
		)
		if err != nil {
			t.Fatalf("windowBounds: %v", err)
		}
		if from.Year() != 2026 || from.Month() != 4 || from.Day() != 1 {
			t.Errorf("since parsed: got %v want 2026-04-01", from)
		}
	})

	t.Run("invalid since", func(t *testing.T) {
		if _, _, _, err := windowBounds(
			StatsFilter{Since: "bogus"}, now,
		); err == nil {
			t.Error("expected error for invalid Since")
		}
	})
}

func TestGetSessionStats_Distributions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Five sessions chosen to place one row in each interesting bucket
	// for duration and peak_context. userMsgs drives archetype/scope:
	// a,b → automation (userMsgs <= 1); c,d,e → human.
	fixtures := []struct {
		id             string
		userMsgs       int
		peakCtx        int
		durMin         float64
		toolCalls      int
		assistantTurns int
	}{
		{"a", 0, 2_000, 0.5, 0, 0},
		{"b", 1, 8_000, 0.9, 1, 1},
		{"c", 3, 25_000, 10.0, 6, 3},
		{"d", 10, 60_000, 25.0, 15, 10},
		{"e", 30, 150_000, 120.0, 30, 30},
	}
	for _, f := range fixtures {
		insertSessionFixture(t, d, sessionFixture{
			id:             f.id,
			agent:          "claude",
			userMsgs:       f.userMsgs,
			peakContext:    f.peakCtx,
			hasPeakContext: true,
			durationMin:    f.durMin,
			startedAt:      hoursAgo(10),
			totalToolCalls: f.toolCalls,
			assistantTurns: f.assistantTurns,
		})
	}

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	// duration scope_all: 0.5→bucket0, 0.9→bucket0, 10→bucket2,
	// 25→bucket3, 120→bucket5 (top).
	gotAll := stats.Distributions.DurationMinutes.ScopeAll.Buckets
	wantCountsAll := []int{2, 0, 1, 1, 0, 1}
	if len(gotAll) != len(wantCountsAll) {
		t.Fatalf("duration scope_all: got %d buckets, want %d",
			len(gotAll), len(wantCountsAll))
	}
	for i, w := range wantCountsAll {
		if gotAll[i].Count != w {
			t.Errorf("duration scope_all bucket %d: got %d want %d",
				i, gotAll[i].Count, w)
		}
	}
	// duration scope_human (c,d,e): bucket2=1, bucket3=1, bucket5=1.
	gotHuman := stats.Distributions.DurationMinutes.ScopeHuman.Buckets
	wantCountsHuman := []int{0, 0, 1, 1, 0, 1}
	if len(gotHuman) != len(wantCountsHuman) {
		t.Fatalf("duration scope_human: got %d buckets, want %d",
			len(gotHuman), len(wantCountsHuman))
	}
	for i, w := range wantCountsHuman {
		if gotHuman[i].Count != w {
			t.Errorf("duration scope_human bucket %d: got %d want %d",
				i, gotHuman[i].Count, w)
		}
	}

	// Means (arithmetic over included sessions).
	wantAllMean := (0.5 + 0.9 + 10 + 25 + 120) / 5.0
	gotAllMean := stats.Distributions.DurationMinutes.ScopeAll.Mean
	if !floatsClose(gotAllMean, wantAllMean, 0.01) {
		t.Errorf("duration scope_all mean: got %v want %v",
			gotAllMean, wantAllMean)
	}
	wantHumanMean := (10.0 + 25.0 + 120.0) / 3.0
	gotHumanMean := stats.Distributions.DurationMinutes.ScopeHuman.Mean
	if !floatsClose(gotHumanMean, wantHumanMean, 0.01) {
		t.Errorf("duration scope_human mean: got %v want %v",
			gotHumanMean, wantHumanMean)
	}

	// user_messages scope_all uses userMessagesEdgesAll
	// ([0,2),[2,6),[6,16),[16,31),[31,51),[51,inf)):
	// 0→0, 1→0, 3→1, 10→2, 30→3.
	gotUM := stats.Distributions.UserMessages.ScopeAll.Buckets
	wantUM := []int{2, 1, 1, 1, 0, 0}
	if len(gotUM) != len(wantUM) {
		t.Fatalf("user_messages scope_all: got %d buckets, want %d",
			len(gotUM), len(wantUM))
	}
	for i, w := range wantUM {
		if gotUM[i].Count != w {
			t.Errorf("user_messages scope_all bucket %d: got %d want %d",
				i, gotUM[i].Count, w)
		}
	}
	// user_messages scope_human uses userMessagesEdgesHuman (5 buckets,
	// dropping the automation band): 3→0, 10→1, 30→2.
	gotUMH := stats.Distributions.UserMessages.ScopeHuman.Buckets
	wantUMH := []int{1, 1, 1, 0, 0}
	if len(gotUMH) != len(wantUMH) {
		t.Fatalf("user_messages scope_human: got %d buckets, want %d",
			len(gotUMH), len(wantUMH))
	}
	for i, w := range wantUMH {
		if gotUMH[i].Count != w {
			t.Errorf("user_messages scope_human bucket %d: got %d want %d",
				i, gotUMH[i].Count, w)
		}
	}

	// peak_context scope_all: 2k→0, 8k→0, 25k→1, 60k→2, 150k→4.
	gotPCAll := stats.Distributions.PeakContextTokens.ScopeAll.Buckets
	wantPCAll := []int{2, 1, 1, 0, 1, 0}
	for i, w := range wantPCAll {
		if gotPCAll[i].Count != w {
			t.Errorf("peak_context scope_all bucket %d: got %d want %d",
				i, gotPCAll[i].Count, w)
		}
	}
	// peak_context scope_human (c,d,e): 25k→1, 60k→2, 150k→4.
	gotPC := stats.Distributions.PeakContextTokens.ScopeHuman.Buckets
	if gotPC[1].Count != 1 || gotPC[2].Count != 1 || gotPC[4].Count != 1 {
		t.Errorf("peak_context scope_human: %+v", gotPC)
	}
	if !stats.Distributions.PeakContextTokens.ClaudeOnly {
		t.Errorf("peak_context.claude_only: got false want true")
	}
	if stats.Distributions.PeakContextTokens.NullCount != 0 {
		t.Errorf("peak_context.null_count: got %d want 0",
			stats.Distributions.PeakContextTokens.NullCount)
	}

	// tools_per_turn: a skipped (assistantTurns==0),
	// b=1/1=1, c=6/3=2, d=15/10=1.5, e=30/30=1.
	// toolsPerTurnEdges = [0,1,2,4,7,11,+Inf].
	gotTPT := stats.Distributions.ToolsPerTurn.ScopeAll.Buckets
	wantTPT := []int{0, 3, 1, 0, 0, 0}
	if len(gotTPT) != len(wantTPT) {
		t.Fatalf("tools_per_turn scope_all: got %d buckets, want %d",
			len(gotTPT), len(wantTPT))
	}
	for i, w := range wantTPT {
		if gotTPT[i].Count != w {
			t.Errorf("tools_per_turn scope_all bucket %d: got %d want %d",
				i, gotTPT[i].Count, w)
		}
	}
}

func TestGetSessionStats_Distributions_NullPeakContext(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// One Claude session lacks peak-context data; it must land in
	// NullCount rather than any peak_context bucket (including bucket 0).
	insertSessionFixture(t, d, sessionFixture{
		id: "np1", agent: "claude", userMsgs: 5,
		startedAt:   hoursAgo(5),
		durationMin: 3.0,
		// peakContext left at zero value AND hasPeakContext=false
	})
	insertSessionFixture(t, d, sessionFixture{
		id: "wp1", agent: "claude", userMsgs: 5,
		startedAt:      hoursAgo(5),
		durationMin:    3.0,
		peakContext:    20_000,
		hasPeakContext: true,
	})
	// Non-Claude session without peak-context must NOT increment
	// NullCount: peak_context is Claude-only, so codex/cursor rows are
	// outside the metric entirely. Guards against regressions that
	// remove the r.agent == "claude" gate on the null branch.
	insertSessionFixture(t, d, sessionFixture{
		id: "cx1", agent: "codex", userMsgs: 5,
		startedAt:   hoursAgo(5),
		durationMin: 3.0,
		// hasPeakContext left at false
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	pc := stats.Distributions.PeakContextTokens
	if pc.NullCount != 1 {
		t.Errorf("null_count: got %d want 1 "+
			"(only np1; codex cx1 must not count)", pc.NullCount)
	}
	total := 0
	for _, b := range pc.ScopeAll.Buckets {
		total += b.Count
	}
	if total != 1 {
		t.Errorf("scope_all bucket total: got %d want 1 "+
			"(the one Claude session with hasPeakContext=true)", total)
	}
}

// seedVelocityMessages inserts len(offsetsSec) messages for sessionID,
// alternating user/assistant starting at role[0], with timestamps at
// startedAt+offsetsSec[i]. Used by velocity tests that need precise
// intervals between adjacent messages. Returns nothing; panics via t
// on any insert error.
func seedVelocityMessages(
	t *testing.T, d *DB, sessionID, startedAt string,
	offsetsSec []int,
) {
	t.Helper()
	start, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		t.Fatalf("seedVelocityMessages %s: parse startedAt %q: %v",
			sessionID, startedAt, err)
	}
	msgs := make([]Message, 0, len(offsetsSec))
	for i, off := range offsetsSec {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		ts := start.Add(time.Duration(off) * time.Second).
			UTC().Format(time.RFC3339)
		msgs = append(msgs, Message{
			SessionID:     sessionID,
			Ordinal:       i,
			Role:          role,
			Content:       fmt.Sprintf("m%d", i),
			ContentLength: 5,
			Timestamp:     ts,
		})
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatalf("seedVelocityMessages %s: InsertMessages: %v",
			sessionID, err)
	}
}

func TestGetSessionStats_Velocity(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Two sessions with carefully chosen per-message gaps so the
	// expected percentile/mean/hourly values are determined.
	//
	// Session v1: 6 msgs at offsets 0,10,20,25,35,50 (seconds).
	//   Turn cycles (user→assistant): 10, 5, 15.
	//   First response: 10.
	//   Adjacent gaps: 10,10,5,10,15 = 50s active.
	// Session v2: 4 msgs at offsets 0,30,60,80.
	//   Turn cycles: 30, 20.
	//   First response: 30.
	//   Adjacent gaps: 30,30,20 = 80s active.
	//
	// Combined: turn cycles=[5,10,15,20,30], first responses=[10,30],
	// active seconds=130, messages=10.
	start := time.Now().UTC().Add(-5 * time.Hour).
		Format(time.RFC3339)

	insertSessionFixture(t, d, sessionFixture{
		id: "v1", agent: "claude", userMsgs: 3,
		messageCount: 6, startedAt: start,
	})
	seedVelocityMessages(t, d, "v1", start,
		[]int{0, 10, 20, 25, 35, 50})

	insertSessionFixture(t, d, sessionFixture{
		id: "v2", agent: "claude", userMsgs: 2,
		messageCount: 4, startedAt: start,
	})
	seedVelocityMessages(t, d, "v2", start,
		[]int{0, 30, 60, 80})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	// Turn cycle seconds, sorted = [5,10,15,20,30].
	// percentileFloat: P50 idx=int(5*0.5)=2 → 15, P90 idx=4 → 30.
	// Mean = (5+10+15+20+30)/5 = 16.
	tc := stats.Velocity.TurnCycleSeconds
	if tc.P50 != 15.0 {
		t.Errorf("TurnCycleSeconds.P50: got %v want 15", tc.P50)
	}
	if tc.P90 != 30.0 {
		t.Errorf("TurnCycleSeconds.P90: got %v want 30", tc.P90)
	}
	if !floatsClose(tc.Mean, 16.0, 0.001) {
		t.Errorf("TurnCycleSeconds.Mean: got %v want 16", tc.Mean)
	}

	// First response seconds, sorted = [10,30].
	// percentileFloat: P50 idx=int(2*0.5)=1 → 30, P90 idx=1 → 30.
	// Mean = (10+30)/2 = 20.
	fr := stats.Velocity.FirstResponseSeconds
	if fr.P50 != 30.0 {
		t.Errorf("FirstResponseSeconds.P50: got %v want 30", fr.P50)
	}
	if fr.P90 != 30.0 {
		t.Errorf("FirstResponseSeconds.P90: got %v want 30", fr.P90)
	}
	if !floatsClose(fr.Mean, 20.0, 0.001) {
		t.Errorf("FirstResponseSeconds.Mean: got %v want 20", fr.Mean)
	}

	// MessagesPerActiveHour: active seconds=130, messages=10.
	// activeMinutes = 130/60, per-hour = 10 / (activeMinutes/60)
	//               = 10 * 60 / (130/60) = 36000/130 ≈ 276.923.
	want := 36000.0 / 130.0
	if !floatsClose(stats.Velocity.MessagesPerActiveHour, want, 0.01) {
		t.Errorf("MessagesPerActiveHour: got %v want %v",
			stats.Velocity.MessagesPerActiveHour, want)
	}
}

// Empty case: no sessions at all. The velocity accumulator stays zeroed
// and every output field must read as 0 rather than NaN / unset.
func TestGetSessionStats_Velocity_Empty(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	tc := stats.Velocity.TurnCycleSeconds
	if tc.P50 != 0 || tc.P90 != 0 || tc.Mean != 0 {
		t.Errorf("TurnCycleSeconds: got %+v want all zero", tc)
	}
	fr := stats.Velocity.FirstResponseSeconds
	if fr.P50 != 0 || fr.P90 != 0 || fr.Mean != 0 {
		t.Errorf("FirstResponseSeconds: got %+v want all zero", fr)
	}
	if stats.Velocity.MessagesPerActiveHour != 0 {
		t.Errorf("MessagesPerActiveHour: got %v want 0",
			stats.Velocity.MessagesPerActiveHour)
	}
}

// Single session with one user→assistant turn. One sample point feeds
// both the turn-cycle and first-response series, so P50 / P90 / Mean
// must all collapse to the same value.
func TestGetSessionStats_Velocity_SingleTurn(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// 2 msgs at offsets 0,60 (seconds): user→assistant delta = 60s.
	// Adjacent gap = 60s → activeMinutes = 1, totalMsgs = 2,
	// MessagesPerActiveHour = 2 / (1/60) = 120.
	start := time.Now().UTC().Add(-3 * time.Hour).
		Format(time.RFC3339)
	insertSessionFixture(t, d, sessionFixture{
		id: "s1", agent: "claude", userMsgs: 1,
		messageCount: 2, startedAt: start,
	})
	seedVelocityMessages(t, d, "s1", start, []int{0, 60})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	tc := stats.Velocity.TurnCycleSeconds
	if tc.P50 != 60.0 || tc.P90 != 60.0 {
		t.Errorf("TurnCycleSeconds: got p50=%v p90=%v want both 60",
			tc.P50, tc.P90)
	}
	if !floatsClose(tc.Mean, 60.0, 0.001) {
		t.Errorf("TurnCycleSeconds.Mean: got %v want 60", tc.Mean)
	}
	fr := stats.Velocity.FirstResponseSeconds
	if fr.P50 != 60.0 || fr.P90 != 60.0 {
		t.Errorf("FirstResponseSeconds: got p50=%v p90=%v want both 60",
			fr.P50, fr.P90)
	}
	if !floatsClose(fr.Mean, 60.0, 0.001) {
		t.Errorf("FirstResponseSeconds.Mean: got %v want 60", fr.Mean)
	}
	if stats.Velocity.MessagesPerActiveHour <= 0 {
		t.Errorf("MessagesPerActiveHour: got %v want > 0",
			stats.Velocity.MessagesPerActiveHour)
	}
	want := 120.0
	if !floatsClose(
		stats.Velocity.MessagesPerActiveHour, want, 0.001,
	) {
		t.Errorf("MessagesPerActiveHour: got %v want %v",
			stats.Velocity.MessagesPerActiveHour, want)
	}
}

// Zero-active-minutes boundary: two messages share a timestamp so the
// only adjacent gap is 0 (failing the gap > 0 guard). activeMinutes
// stays 0, totalMsgs is never bumped, and MessagesPerActiveHour must
// remain 0 even though the session survived the len(msgs) >= 2 filter.
func TestGetSessionStats_Velocity_ZeroActive(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	start := time.Now().UTC().Add(-2 * time.Hour).
		Format(time.RFC3339)
	insertSessionFixture(t, d, sessionFixture{
		id: "z1", agent: "claude", userMsgs: 1,
		messageCount: 2, startedAt: start,
	})
	seedVelocityMessages(t, d, "z1", start, []int{0, 0})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	if stats.Velocity.MessagesPerActiveHour != 0 {
		t.Errorf("MessagesPerActiveHour: got %v want 0",
			stats.Velocity.MessagesPerActiveHour)
	}
}

func TestGetSessionStats_ToolMixAndModelMix(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Session tm1: 4 tool_calls across 3 categories (Bash×2, Edit, Read).
	insertSessionFixture(t, d, sessionFixture{
		id: "tm1", agent: "claude", userMsgs: 5,
		startedAt: hoursAgo(4),
	})
	seedToolCallsByCategory(t, d, "tm1",
		[]string{"Bash", "Bash", "Edit", "Read"})

	// Session tm2: 2 tool_calls (Grep, Bash).
	insertSessionFixture(t, d, sessionFixture{
		id: "tm2", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(3),
	})
	seedToolCallsByCategory(t, d, "tm2",
		[]string{"Grep", "Bash"})

	// Session mm1: 2 claude-opus-4-7 assistant messages (1000 + 2000).
	insertSessionFixture(t, d, sessionFixture{
		id: "mm1", agent: "claude", userMsgs: 2,
		startedAt: hoursAgo(2),
	})
	seedModelMessages(t, d, "mm1", 1, []struct {
		model  string
		tokens int
	}{
		{"claude-opus-4-7", 1000},
		{"claude-opus-4-7", 2000},
	})

	// Session mm2: 1 claude-sonnet-4-6 assistant message (500 tokens).
	insertSessionFixture(t, d, sessionFixture{
		id: "mm2", agent: "claude", userMsgs: 2,
		startedAt: hoursAgo(2),
	})
	seedModelMessages(t, d, "mm2", 1, []struct {
		model  string
		tokens int
	}{
		{"claude-sonnet-4-6", 500},
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	wantCats := map[string]int{
		"Bash": 3,
		"Edit": 1,
		"Read": 1,
		"Grep": 1,
	}
	gotCats := stats.ToolMix.ByCategory
	if len(gotCats) != len(wantCats) {
		t.Errorf("ToolMix.ByCategory len: got %d want %d (got=%v)",
			len(gotCats), len(wantCats), gotCats)
	}
	for cat, want := range wantCats {
		if gotCats[cat] != want {
			t.Errorf("ToolMix.ByCategory[%q]: got %d want %d",
				cat, gotCats[cat], want)
		}
	}
	if stats.ToolMix.TotalCalls != 6 {
		t.Errorf("ToolMix.TotalCalls: got %d want 6",
			stats.ToolMix.TotalCalls)
	}

	wantTokens := map[string]int64{
		"claude-opus-4-7":   3000,
		"claude-sonnet-4-6": 500,
	}
	gotTokens := stats.ModelMix.ByTokens
	if len(gotTokens) != len(wantTokens) {
		t.Errorf("ModelMix.ByTokens len: got %d want %d (got=%v)",
			len(gotTokens), len(wantTokens), gotTokens)
	}
	for model, want := range wantTokens {
		if gotTokens[model] != want {
			t.Errorf("ModelMix.ByTokens[%q]: got %d want %d",
				model, gotTokens[model], want)
		}
	}
}

// Window and agent filters must gate both mixes: tool_calls and
// messages attached to sessions outside the window or not matching
// the agent filter must not appear in ToolMix or ModelMix.
func TestGetSessionStats_ToolMixAndModelMix_Filters(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// In-window claude session: should contribute to both mixes.
	// seedToolCallsByCategory uses ordinals 1..2; seedModelMessages
	// starts at 3 to avoid the UNIQUE(session_id, ordinal) collision.
	insertSessionFixture(t, d, sessionFixture{
		id: "in1", agent: "claude", userMsgs: 3,
		startedAt: hoursAgo(2),
	})
	seedToolCallsByCategory(t, d, "in1", []string{"Bash", "Read"})
	seedModelMessages(t, d, "in1", 3, []struct {
		model  string
		tokens int
	}{
		{"claude-opus-4-7", 800},
	})

	// Out-of-window session (50 days old): must be excluded entirely.
	oldStart := time.Now().UTC().Add(-50 * 24 * time.Hour).
		Format(time.RFC3339)
	insertSessionFixture(t, d, sessionFixture{
		id: "old1", agent: "claude", userMsgs: 3,
		startedAt: oldStart,
	})
	seedToolCallsByCategory(t, d, "old1", []string{"Edit", "Edit"})
	seedModelMessages(t, d, "old1", 3, []struct {
		model  string
		tokens int
	}{
		{"claude-opus-4-7", 9000},
	})

	// Wrong-agent session inside the window: excluded by Agent=claude.
	insertSessionFixture(t, d, sessionFixture{
		id: "cx1", agent: "codex", userMsgs: 3,
		startedAt: hoursAgo(2),
	})
	seedToolCallsByCategory(t, d, "cx1", []string{"Grep"})
	seedModelMessages(t, d, "cx1", 2, []struct {
		model  string
		tokens int
	}{
		{"codex-gpt-5", 7000},
	})

	stats, err := d.GetSessionStats(ctx, StatsFilter{
		Since: "28d", Agent: "claude",
	})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}

	// Only in1's 2 tool_calls survive.
	if stats.ToolMix.TotalCalls != 2 {
		t.Errorf("ToolMix.TotalCalls: got %d want 2",
			stats.ToolMix.TotalCalls)
	}
	if stats.ToolMix.ByCategory["Bash"] != 1 ||
		stats.ToolMix.ByCategory["Read"] != 1 {
		t.Errorf("ToolMix.ByCategory: got %v want Bash=1 Read=1",
			stats.ToolMix.ByCategory)
	}
	if stats.ToolMix.ByCategory["Edit"] != 0 {
		t.Errorf("out-of-window Edit leaked: got %d",
			stats.ToolMix.ByCategory["Edit"])
	}
	if stats.ToolMix.ByCategory["Grep"] != 0 {
		t.Errorf("wrong-agent Grep leaked: got %d",
			stats.ToolMix.ByCategory["Grep"])
	}

	// Only in1's 800 tokens survive.
	if got := stats.ModelMix.ByTokens["claude-opus-4-7"]; got != 800 {
		t.Errorf("ModelMix.ByTokens[claude-opus-4-7]: got %d want 800",
			got)
	}
	if _, ok := stats.ModelMix.ByTokens["codex-gpt-5"]; ok {
		t.Errorf("wrong-agent model leaked: %v",
			stats.ModelMix.ByTokens)
	}
}

// Empty-window case: no sessions → both mixes must serialize as empty
// maps (not nil) so the JSON output keeps stable keys.
func TestGetSessionStats_ToolMixAndModelMix_Empty(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	stats, err := d.GetSessionStats(ctx, StatsFilter{Since: "28d"})
	if err != nil {
		t.Fatalf("GetSessionStats: %v", err)
	}
	if stats.ToolMix.ByCategory == nil {
		t.Errorf("ToolMix.ByCategory: got nil want non-nil map")
	}
	if stats.ToolMix.TotalCalls != 0 {
		t.Errorf("ToolMix.TotalCalls: got %d want 0",
			stats.ToolMix.TotalCalls)
	}
	if stats.ModelMix.ByTokens == nil {
		t.Errorf("ModelMix.ByTokens: got nil want non-nil map")
	}
}
