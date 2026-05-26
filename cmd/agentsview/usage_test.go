package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestFmtCost(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{"zero is $0.00", 0, "$0.00"},
		{"under half a cent shows <$0.01", 0.001, "<$0.01"},
		{"half a cent rounds up to $0.01", 0.005, "$0.01"},
		{"typical cents", 0.45, "$0.45"},
		{"dollars", 12.34, "$12.34"},
		{"rounds to two decimals", 1.23456, "$1.23"},
		{"large value", 1234.56, "$1234.56"},
		// A negative input shouldn't hit the <$0.01 branch.
		{"negative passes through", -0.42, "$-0.42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, fmtCost(tc.in),
				"fmtCost(%v)", tc.in)
		})
	}
}

func TestResolveDefaultSince(t *testing.T) {
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	const utc = "UTC"

	tests := []struct {
		name  string
		since string
		until string
		all   bool
		want  string
	}{
		{
			name: "no flags returns 30-day window",
			want: "2024-05-17",
		},
		{
			name:  "explicit since preserved",
			since: "2024-01-01",
			want:  "2024-01-01",
		},
		{
			name: "all flag disables default",
			all:  true,
			want: "",
		},
		{
			name:  "until without since does not backfill since",
			until: "2024-01-31",
			want:  "",
		},
		{
			name:  "explicit range preserved",
			since: "2024-01-01",
			until: "2024-01-31",
			want:  "2024-01-01",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDefaultSince(
				tc.since, tc.until, tc.all, now, utc,
			)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFormatDailyUsageJSON(t *testing.T) {
	result := db.DailyUsageResult{
		Daily: []db.DailyUsageEntry{
			{
				Date:                "2024-06-15",
				InputTokens:         50000,
				OutputTokens:        12000,
				CacheCreationTokens: 8000,
				CacheReadTokens:     30000,
				TotalCost:           0.45,
				ModelsUsed:          []string{"claude-sonnet-4-20250514"},
				ModelBreakdowns: []db.ModelBreakdown{
					{
						ModelName:           "claude-sonnet-4-20250514",
						InputTokens:         50000,
						OutputTokens:        12000,
						CacheCreationTokens: 8000,
						CacheReadTokens:     30000,
						Cost:                0.45,
					},
				},
			},
		},
		Totals: db.UsageTotals{
			InputTokens:         50000,
			OutputTokens:        12000,
			CacheCreationTokens: 8000,
			CacheReadTokens:     30000,
			TotalCost:           0.45,
		},
	}

	out, err := json.Marshal(result)
	require.NoError(t, err, "json.Marshal failed")

	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &decoded),
		"json.Unmarshal failed")

	assert.Contains(t, decoded, "daily", "missing 'daily' key in JSON output")
	assert.Contains(t, decoded, "totals", "missing 'totals' key in JSON output")

	// Verify daily array has expected entry
	var daily []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(decoded["daily"], &daily),
		"parsing daily array")
	require.Len(t, daily, 1, "daily length")

	// Check expected fields exist in daily entry
	wantFields := []string{
		"date", "inputTokens", "outputTokens",
		"cacheCreationTokens", "cacheReadTokens",
		"totalCost", "modelsUsed", "modelBreakdowns",
	}
	for _, f := range wantFields {
		assert.Contains(t, daily[0], f,
			"missing field %q in daily entry", f)
	}

	// Verify totals fields
	var totals map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(decoded["totals"], &totals),
		"parsing totals")
	totalFields := []string{
		"inputTokens", "outputTokens",
		"cacheCreationTokens", "cacheReadTokens",
		"totalCost",
	}
	for _, f := range totalFields {
		assert.Contains(t, totals, f,
			"missing field %q in totals", f)
	}
}
