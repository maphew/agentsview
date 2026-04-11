package main

import (
	"encoding/json"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

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
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if _, ok := decoded["daily"]; !ok {
		t.Error("missing 'daily' key in JSON output")
	}
	if _, ok := decoded["totals"]; !ok {
		t.Error("missing 'totals' key in JSON output")
	}

	// Verify daily array has expected entry
	var daily []map[string]json.RawMessage
	if err := json.Unmarshal(decoded["daily"], &daily); err != nil {
		t.Fatalf("parsing daily array: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("daily length = %d, want 1", len(daily))
	}

	// Check expected fields exist in daily entry
	wantFields := []string{
		"date", "inputTokens", "outputTokens",
		"cacheCreationTokens", "cacheReadTokens",
		"totalCost", "modelsUsed", "modelBreakdowns",
	}
	for _, f := range wantFields {
		if _, ok := daily[0][f]; !ok {
			t.Errorf("missing field %q in daily entry", f)
		}
	}

	// Verify totals fields
	var totals map[string]json.RawMessage
	if err := json.Unmarshal(decoded["totals"], &totals); err != nil {
		t.Fatalf("parsing totals: %v", err)
	}
	totalFields := []string{
		"inputTokens", "outputTokens",
		"cacheCreationTokens", "cacheReadTokens",
		"totalCost",
	}
	for _, f := range totalFields {
		if _, ok := totals[f]; !ok {
			t.Errorf("missing field %q in totals", f)
		}
	}
}
