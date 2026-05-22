package server_test

import (
	"net/http"
	"net/url"
	"slices"
	"testing"

	"go.kenn.io/agentsview/internal/db"
)

func TestTrendsTermsEndpoint(t *testing.T) {
	te := setup(t)
	start := "2024-06-01T09:00:00Z"
	te.seedSession(t, "s1", "alpha", 2, func(s *db.Session) {
		s.StartedAt = &start
		s.EndedAt = &start
	})
	te.seedMessages(t, "s1", 2, func(i int, m *db.Message) {
		m.Timestamp = "2024-06-01T09:00:00Z"
		if i == 0 {
			m.Content = "load bearing seam"
		}
		if i == 1 {
			m.Content = "load-bearing seams"
		}
		m.ContentLength = len(m.Content)
	})

	w := te.get(t, trendsURL(url.Values{
		"from":        {"2024-06-01"},
		"to":          {"2024-06-02"},
		"timezone":    {"UTC"},
		"granularity": {"week"},
		"term":        {"load bearing | load-bearing", "seam"},
	}))
	assertStatus(t, w, http.StatusOK)
	resp := decode[db.TrendsTermsResponse](t, w)
	if want := []string{"2024-05-27"}; !slices.Equal(trendBucketDates(resp.Buckets), want) {
		t.Fatalf("bucket dates = %#v, want %#v", trendBucketDates(resp.Buckets), want)
	}
	byTerm := trendSeriesByTerm(resp.Series)
	if got := byTerm["load bearing"].Total; got != 2 {
		t.Fatalf("load bearing total = %d, want 2", got)
	}
	if got := byTerm["seam"].Total; got != 2 {
		t.Fatalf("seam total = %d, want 2", got)
	}
}

func TestTrendsTermsValidation(t *testing.T) {
	te := setup(t)

	t.Run("missing term", func(t *testing.T) {
		w := te.get(t, trendsURL(url.Values{
			"from": {"2024-06-01"},
			"to":   {"2024-06-02"},
		}))
		assertStatus(t, w, http.StatusBadRequest)
	})

	t.Run("invalid granularity", func(t *testing.T) {
		w := te.get(t, trendsURL(url.Values{
			"from":        {"2024-06-01"},
			"to":          {"2024-06-02"},
			"granularity": {"hour"},
			"term":        {"seam"},
		}))
		assertStatus(t, w, http.StatusBadRequest)
	})
}

func TestTrendsTermsProjectFilter(t *testing.T) {
	te := setup(t)
	start := "2024-06-01T09:00:00Z"
	for _, seeded := range []struct {
		id      string
		project string
	}{
		{"s1", "alpha"},
		{"s2", "beta"},
	} {
		te.seedSession(t, seeded.id, seeded.project, 1, func(s *db.Session) {
			s.StartedAt = &start
			s.EndedAt = &start
		})
		te.seedMessages(t, seeded.id, 1, func(_ int, m *db.Message) {
			m.Timestamp = start
			m.Content = "seam"
			m.ContentLength = len(m.Content)
		})
	}

	w := te.get(t, trendsURL(url.Values{
		"from":     {"2024-06-01"},
		"to":       {"2024-06-01"},
		"timezone": {"UTC"},
		"project":  {"alpha"},
		"term":     {"seam"},
	}))
	assertStatus(t, w, http.StatusOK)
	resp := decode[db.TrendsTermsResponse](t, w)
	if got := trendSeriesByTerm(resp.Series)["seam"].Total; got != 1 {
		t.Fatalf("project-filtered total = %d, want 1", got)
	}
}

func trendsURL(q url.Values) string {
	return "/api/v1/trends/terms?" + q.Encode()
}

func trendBucketDates(buckets []db.TrendBucket) []string {
	dates := make([]string, len(buckets))
	for i, bucket := range buckets {
		dates[i] = bucket.Date
	}
	return dates
}

func trendSeriesByTerm(series []db.TrendSeries) map[string]db.TrendSeries {
	byTerm := make(map[string]db.TrendSeries, len(series))
	for _, entry := range series {
		byTerm[entry.Term] = entry
	}
	return byTerm
}
