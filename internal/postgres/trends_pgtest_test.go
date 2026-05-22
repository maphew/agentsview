//go:build pgtest

package postgres

import (
	"context"
	"slices"
	"testing"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreGetTrendsTerms(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_trends_terms_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'trends-pg-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:00:00Z'::timestamptz,
			3, 2
		)`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system
		) VALUES
			('trends-pg-001', 0, 'user', 'load bearing seam',
			 '2024-06-01T09:00:00Z'::timestamptz, 17, FALSE),
			('trends-pg-001', 1, 'assistant', 'load-bearing seams seam',
			 '2024-06-08T09:00:00Z'::timestamptz, 23, FALSE),
			('trends-pg-001', 2, 'user', 'seam system',
			 '2024-06-08T09:00:00Z'::timestamptz, 11, TRUE)`)
	if err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	terms, err := db.ParseTrendTerms([]string{"load bearing | load-bearing", "seam"})
	if err != nil {
		t.Fatalf("ParseTrendTerms: %v", err)
	}
	got, err := store.GetTrendsTerms(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-09", Timezone: "UTC",
	}, terms, "week")
	if err != nil {
		t.Fatalf("GetTrendsTerms: %v", err)
	}
	if want := []string{"2024-05-27", "2024-06-03"}; !slices.Equal(trendBucketDates(got.Buckets), want) {
		t.Fatalf("bucket dates = %#v, want %#v", trendBucketDates(got.Buckets), want)
	}
	if want := []int{1, 1}; !slices.Equal(trendBucketMessageCounts(got.Buckets), want) {
		t.Fatalf("bucket message counts = %#v, want %#v", trendBucketMessageCounts(got.Buckets), want)
	}
	if got.MessageCount != 2 {
		t.Fatalf("message count = %d, want 2", got.MessageCount)
	}
	byTerm := trendSeriesByTerm(got.Series)
	if got := byTerm["load bearing"].Total; got != 2 {
		t.Fatalf("load bearing total = %d, want 2", got)
	}
	if got := byTerm["seam"].Total; got != 3 {
		t.Fatalf("seam total = %d, want 3", got)
	}
	if want := []int{1, 1}; !slices.Equal(trendPointCounts(byTerm["load bearing"].Points), want) {
		t.Fatalf("load bearing points = %#v, want %#v", trendPointCounts(byTerm["load bearing"].Points), want)
	}
	if want := []int{1, 2}; !slices.Equal(trendPointCounts(byTerm["seam"].Points), want) {
		t.Fatalf("seam points = %#v, want %#v", trendPointCounts(byTerm["seam"].Points), want)
	}
}

func TestStoreGetTrendsTermsUsesMessageTimestampFilters(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_trends_terms_message_filters_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'trends-pg-message-filters-001', 'test-machine',
			'alpha', 'claude',
			'2024-06-04T08:00:00Z'::timestamptz, 2, 2
		)`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system
		) VALUES
			('trends-pg-message-filters-001', 0, 'user', 'seam',
			 '2024-06-05T09:00:00Z'::timestamptz, 4, FALSE),
			('trends-pg-message-filters-001', 1, 'user', 'seam',
			 '2024-06-05T10:00:00Z'::timestamptz, 4, FALSE)`)
	if err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	terms, err := db.ParseTrendTerms([]string{"seam"})
	if err != nil {
		t.Fatalf("ParseTrendTerms: %v", err)
	}
	dow := 2
	hour := 9
	got, err := store.GetTrendsTerms(ctx, db.AnalyticsFilter{
		From:      "2024-06-05",
		To:        "2024-06-05",
		Timezone:  "UTC",
		DayOfWeek: &dow,
		Hour:      &hour,
	}, terms, "day")
	if err != nil {
		t.Fatalf("GetTrendsTerms: %v", err)
	}
	if got.MessageCount != 1 {
		t.Fatalf("message count = %d, want 1", got.MessageCount)
	}
	if got := trendSeriesByTerm(got.Series)["seam"].Total; got != 1 {
		t.Fatalf("message timestamp filtered total = %d, want 1", got)
	}
}

func trendBucketDates(buckets []db.TrendBucket) []string {
	dates := make([]string, len(buckets))
	for i, bucket := range buckets {
		dates[i] = bucket.Date
	}
	return dates
}

func trendBucketMessageCounts(buckets []db.TrendBucket) []int {
	counts := make([]int, len(buckets))
	for i, bucket := range buckets {
		counts[i] = bucket.MessageCount
	}
	return counts
}

func trendSeriesByTerm(series []db.TrendSeries) map[string]db.TrendSeries {
	byTerm := make(map[string]db.TrendSeries, len(series))
	for _, entry := range series {
		byTerm[entry.Term] = entry
	}
	return byTerm
}

func trendPointCounts(points []db.TrendPoint) []int {
	counts := make([]int, len(points))
	for i, point := range points {
		counts[i] = point.Count
	}
	return counts
}
