// internal/db/session_stats.go
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// StatsFilter mirrors the service-layer StatsFilter but lives in db
// because db functions take typed filters without cross-package deps.
type StatsFilter struct {
	Since           string
	Until           string
	Agent           string
	IncludeProjects []string
	ExcludeProjects []string
	Timezone        string
	GHToken         string
}

// GetSessionStats computes the v1 session-stats JSON response.
// Sections not yet wired (distributions, velocity, tool_mix, etc.)
// remain at their zero values until the tasks that implement them
// land.
func (db *DB) GetSessionStats(
	ctx context.Context, f StatsFilter,
) (*SessionStats, error) {
	tz, err := resolveTimezone(f.Timezone)
	if err != nil {
		return nil, fmt.Errorf("resolving timezone: %w", err)
	}
	from, to, days, err := windowBounds(f, time.Now())
	if err != nil {
		return nil, fmt.Errorf("resolving window: %w", err)
	}

	rows, err := db.loadSessionsInWindow(ctx, f, from, to)
	if err != nil {
		return nil, err
	}

	stats := &SessionStats{
		SchemaVersion: 1,
		Window: StatsWindow{
			Since: from.UTC().Format(time.RFC3339),
			Until: to.UTC().Format(time.RFC3339),
			Days:  days,
		},
		Filters: StatsFilters{
			Agent:            orDefault(f.Agent, "all"),
			ProjectsIncluded: f.IncludeProjects,
			ProjectsExcluded: nonNilSlice(f.ExcludeProjects),
			Timezone:         tz.String(),
		},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	computeTotalsAndArchetypes(stats, rows)

	return stats, nil
}

// resolveTimezone loads an IANA zone name, defaulting to UTC when
// empty. Unknown zones are an error — silently falling back would
// hide typos in user input.
func resolveTimezone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf(
			"loading timezone %q: %w", name, err,
		)
	}
	return loc, nil
}

// windowBounds resolves Since/Until into absolute time bounds.
// Supported inputs: "Nd" (days), "Nh" (hours), or "YYYY-MM-DD".
// Until defaults to now; Since defaults to 28 days before Until.
// Returned days is the calendar-style span in whole days, rounded
// up when Since is a non-integer-day duration (e.g. "48h" → 2).
func windowBounds(
	f StatsFilter, now time.Time,
) (from, to time.Time, days int, err error) {
	to = now
	if f.Until != "" {
		to, err = parseWindowPoint(f.Until, now)
		if err != nil {
			return time.Time{}, time.Time{}, 0,
				fmt.Errorf("parsing until %q: %w", f.Until, err)
		}
	}

	from = to.Add(-28 * 24 * time.Hour)
	if f.Since != "" {
		// Durations anchor relative to Until; dates stand alone.
		if d, ok := parseDurationShort(f.Since); ok {
			from = to.Add(-d)
		} else {
			from, err = parseWindowPoint(f.Since, now)
			if err != nil {
				return time.Time{}, time.Time{}, 0,
					fmt.Errorf(
						"parsing since %q: %w",
						f.Since, err,
					)
			}
		}
	}

	if !from.Before(to) {
		return time.Time{}, time.Time{}, 0, fmt.Errorf(
			"window since (%s) must precede until (%s)",
			from.Format(time.RFC3339),
			to.Format(time.RFC3339),
		)
	}

	span := to.Sub(from)
	days = int(span / (24 * time.Hour))
	if span%(24*time.Hour) != 0 {
		days++
	}
	return from, to, days, nil
}

// parseWindowPoint accepts either a duration-relative-to-now form
// ("28d", "12h") or an absolute YYYY-MM-DD date (interpreted as
// the start of that UTC day). Used by Since and Until.
func parseWindowPoint(s string, now time.Time) (time.Time, error) {
	if d, ok := parseDurationShort(s); ok {
		return now.Add(-d), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf(
		"expected Nd, Nh, or YYYY-MM-DD, got %q", s,
	)
}

// parseDurationShort recognises the compact "Nd" / "Nh" forms the
// stats CLI advertises. Returns ok=false when s is not a compact
// duration so callers can try the date path.
func parseDurationShort(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	unit := s[len(s)-1]
	num, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || num <= 0 {
		return 0, false
	}
	switch unit {
	case 'd':
		return time.Duration(num) * 24 * time.Hour, true
	case 'h':
		return time.Duration(num) * time.Hour, true
	default:
		return 0, false
	}
}

// sessionStatsRow is the compact per-session projection used by all
// stats sections. Only the columns this task reads are populated;
// later tasks extend the struct (and loadSessionsInWindow's SELECT)
// in place rather than duplicating the scan.
type sessionStatsRow struct {
	id                string
	agent             string
	project           string
	startedAt         time.Time
	endedAt           sql.NullTime
	messageCount      int
	userMessageCount  int
	totalOutputTokens int64
	peakContextTokens int64
	hasPeakContext    bool
}

// loadSessionsInWindow returns the rows the stats pipeline needs.
// Matches the analytics.go convention: exclude subagent/fork rows
// and soft-deleted rows, require non-empty message_count, and bound
// by started_at within [from, to).
func (db *DB) loadSessionsInWindow(
	ctx context.Context, f StatsFilter, from, to time.Time,
) ([]sessionStatsRow, error) {
	preds := []string{
		"message_count > 0",
		"relationship_type NOT IN ('subagent', 'fork')",
		"deleted_at IS NULL",
		"started_at IS NOT NULL",
		"started_at != ''",
		"started_at >= ?",
		"started_at < ?",
	}
	args := []any{
		from.UTC().Format(time.RFC3339Nano),
		to.UTC().Format(time.RFC3339Nano),
	}

	if f.Agent != "" {
		agents := strings.Split(f.Agent, ",")
		if len(agents) == 1 {
			preds = append(preds, "agent = ?")
			args = append(args, agents[0])
		} else {
			ph := make([]string, len(agents))
			for i, a := range agents {
				ph[i] = "?"
				args = append(args, a)
			}
			preds = append(preds,
				"agent IN ("+strings.Join(ph, ",")+")")
		}
	}

	if len(f.IncludeProjects) > 0 {
		ph, inArgs := inPlaceholders(f.IncludeProjects)
		preds = append(preds, "project IN "+ph)
		args = append(args, inArgs...)
	}
	if len(f.ExcludeProjects) > 0 {
		ph, inArgs := inPlaceholders(f.ExcludeProjects)
		preds = append(preds, "project NOT IN "+ph)
		args = append(args, inArgs...)
	}

	query := `SELECT id, agent, project, started_at, ended_at,
		message_count, user_message_count,
		total_output_tokens, peak_context_tokens,
		has_peak_context_tokens
		FROM sessions WHERE ` + strings.Join(preds, " AND ")

	sqlRows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying sessions for stats window: %w", err,
		)
	}
	defer sqlRows.Close()

	var out []sessionStatsRow
	for sqlRows.Next() {
		var r sessionStatsRow
		var startedAt string
		var endedAt sql.NullString
		var hasPeak int
		if err := sqlRows.Scan(
			&r.id, &r.agent, &r.project,
			&startedAt, &endedAt,
			&r.messageCount, &r.userMessageCount,
			&r.totalOutputTokens, &r.peakContextTokens,
			&hasPeak,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning session stats row: %w", err,
			)
		}
		t, err := parseTimestamp(startedAt)
		if err != nil {
			return nil, fmt.Errorf(
				"session %s: parsing started_at %q: %w",
				r.id, startedAt, err,
			)
		}
		r.startedAt = t
		if endedAt.Valid && endedAt.String != "" {
			et, err := parseTimestamp(endedAt.String)
			if err != nil {
				return nil, fmt.Errorf(
					"session %s: parsing ended_at %q: %w",
					r.id, endedAt.String, err,
				)
			}
			r.endedAt = sql.NullTime{Time: et, Valid: true}
		}
		r.hasPeakContext = hasPeak == 1
		out = append(out, r)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating session stats rows: %w", err,
		)
	}
	return out, nil
}

// parseTimestamp accepts RFC3339 and RFC3339Nano — the two forms
// the session table writes via timeutil.Format / Ptr.
func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// archetypeLabel classifies a session by its user_message_count per
// the session-analytics v1 spec. Boundaries are inclusive on both
// sides of each band.
func archetypeLabel(userMsgs int) string {
	switch {
	case userMsgs <= 1:
		return "automation"
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

// computeTotalsAndArchetypes fills SessionStats.Totals and
// .Archetypes in a single pass over rows.
func computeTotalsAndArchetypes(
	s *SessionStats, rows []sessionStatsRow,
) {
	archMax := map[string]int{}
	humanMax := map[string]int{}
	for _, r := range rows {
		s.Totals.SessionsAll++
		s.Totals.MessagesTotal += r.messageCount
		s.Totals.UserMessagesTotal += r.userMessageCount

		label := archetypeLabel(r.userMessageCount)
		switch label {
		case "automation":
			s.Archetypes.Automation++
			s.Totals.SessionsAutomation++
		case "quick":
			s.Archetypes.Quick++
			s.Totals.SessionsHuman++
			humanMax[label]++
		case "standard":
			s.Archetypes.Standard++
			s.Totals.SessionsHuman++
			humanMax[label]++
		case "deep":
			s.Archetypes.Deep++
			s.Totals.SessionsHuman++
			humanMax[label]++
		case "marathon":
			s.Archetypes.Marathon++
			s.Totals.SessionsHuman++
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

// pickMaxLabel returns the key with the strictly highest count.
// Ties are broken by iterating priority in order — the earlier
// priority entry wins.
func pickMaxLabel(counts map[string]int, priority []string) string {
	best := ""
	bestN := -1
	for _, k := range priority {
		if counts[k] > bestN {
			best = k
			bestN = counts[k]
		}
	}
	return best
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
