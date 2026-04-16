package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

func TestGradeCell(t *testing.T) {
	a := "A"
	tests := []struct {
		name string
		in   *string
		want string
	}{
		{"nil grade renders dash", nil, "-"},
		{"empty grade renders dash", strPtr(""), "-"},
		{"grade preserved", &a, "A"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gradeCell(tc.in); got != tc.want {
				t.Errorf("gradeCell = %q, want %q",
					got, tc.want)
			}
		})
	}
}

func TestFormatPressure(t *testing.T) {
	half := 0.5
	tests := []struct {
		name string
		in   *float64
		want string
	}{
		{"nil renders dash", nil, "-"},
		{"50% rounds correctly", &half, "50%"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatPressure(tc.in); got != tc.want {
				t.Errorf("formatPressure = %q, want %q",
					got, tc.want)
			}
		})
	}
}

func TestFormatScore(t *testing.T) {
	score := 87
	if got := formatScore(nil); got != "" {
		t.Errorf("nil score = %q, want empty", got)
	}
	if got := formatScore(&score); got != " (score 87)" {
		t.Errorf("score = %q, want ' (score 87)'", got)
	}
}

func TestFormatConfidence(t *testing.T) {
	tests := []struct {
		name      string
		conf      string
		endedWith string
		want      string
	}{
		{"both empty returns empty", "", "", ""},
		{
			name: "confidence only",
			conf: "high",
			want: " (high confidence)",
		},
		{
			name:      "ended-with only",
			endedWith: "user",
			want:      " (ended with user)",
		},
		{
			name:      "both joined",
			conf:      "low",
			endedWith: "assistant",
			want:      " (low confidence, ended with assistant)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatConfidence(tc.conf, tc.endedWith)
			if got != tc.want {
				t.Errorf("formatConfidence = %q, want %q",
					got, tc.want)
			}
		})
	}
}

func TestShortDate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty renders dash", "", "-"},
		{
			name: "RFC3339Nano parsed",
			in:   "2026-04-15T20:48:24.123Z",
			want: parseLocalDate(t, "2026-04-15T20:48:24.123Z"),
		},
		{
			name: "garbage passes through",
			in:   "not-a-date",
			want: "not-a-date",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortDate(tc.in); got != tc.want {
				t.Errorf("shortDate(%q) = %q, want %q",
					tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"under limit unchanged", "hello", 10, "hello"},
		{"exact length unchanged", "hello", 5, "hello"},
		{"over limit ellipsized", "hello world", 5, "hell…"},
		{"single char limit", "abc", 1, "a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncate(tc.in, tc.n); got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q",
					tc.in, tc.n, got, tc.want)
			}
		})
	}
}

func TestShortID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain uuid trimmed", "abcdef1234567890", "abcdef12"},
		{"prefixed id stripped", "host~abcdef12345", "abcdef12"},
		{"short id preserved", "abc", "abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortID(tc.in); got != tc.want {
				t.Errorf("shortID(%q) = %q, want %q",
					tc.in, got, tc.want)
			}
		})
	}
}

func TestPrintHealthList(t *testing.T) {
	a := "A"
	d := "D"
	pressure := 0.62
	sessions := []db.Session{
		{
			ID:                 "abc12345-6789-0000",
			Project:            "agentsview",
			Agent:              "claude",
			MessageCount:       42,
			FinalFailureStreak: 0,
			Outcome:            "success",
			HealthGrade:        &a,
			ContextPressureMax: &pressure,
			EndedAt:            strPtr("2026-04-15T20:48:24Z"),
		},
		{
			ID:                 "def67890",
			Project:            "roborev",
			Agent:              "codex",
			MessageCount:       18,
			FinalFailureStreak: 3,
			Outcome:            "failed",
			HealthGrade:        &d,
		},
	}

	var buf bytes.Buffer
	printHealthList(&buf, sessions)

	out := buf.String()
	for _, want := range []string{
		"DATE", "AGENT", "GRADE", "OUTCOME",
		"agentsview", "claude", "A", "success",
		"roborev", "codex", "D", "failed",
		"abc12345", "def67890",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s",
				want, out)
		}
	}
}

func TestPrintHealthDetail(t *testing.T) {
	a := "A"
	score := 92
	pressure := 0.45
	sess := db.Session{
		ID:                     "abc12345",
		Project:                "agentsview",
		Agent:                  "claude",
		StartedAt:              strPtr("2026-04-15T20:48:24Z"),
		EndedAt:                strPtr("2026-04-15T21:30:00Z"),
		MessageCount:           42,
		UserMessageCount:       12,
		HealthGrade:            &a,
		HealthScore:            &score,
		Outcome:                "success",
		OutcomeConfidence:      "high",
		EndedWithRole:          "assistant",
		ToolFailureSignalCount: 1,
		ToolRetryCount:         2,
		EditChurnCount:         3,
		ConsecutiveFailureMax:  4,
		FinalFailureStreak:     0,
		CompactionCount:        1,
		ContextPressureMax:     &pressure,
		GitBranch:              "main",
	}

	var buf bytes.Buffer
	printHealthDetail(&buf, sess)
	out := buf.String()

	for _, want := range []string{
		"Session:  abc12345",
		"Project:  agentsview",
		"Branch:   main",
		"Messages: 42 (12 user)",
		"Grade:   A (score 92)",
		"Outcome: success (high confidence, ended with assistant)",
		"Tool failures:        1",
		"Tool retries:         2",
		"Edit churn:           3",
		"Consecutive fails:    4",
		"Compactions:          1",
		"Context pressure:     45%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s",
				want, out)
		}
	}
}

func TestResolveSessionID(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	upsert := func(id string) {
		t.Helper()
		err := database.UpsertSession(db.Session{
			ID: id, Project: "p", Machine: "m",
			Agent: "claude", MessageCount: 1,
		})
		if err != nil {
			t.Fatalf("upsert %q: %v", id, err)
		}
	}

	// "abcdef12" is both a full session ID and the short-ID
	// (first 8 chars) of another session -- a real display
	// collision in `health` list output.
	upsert("abcdef12")
	upsert("abcdef1234567890")
	upsert("unique-session-id")
	// Host-prefixed remote ID where the full local ID is a
	// substring. Looking up by the full local ID must NOT
	// be ambiguous -- the user typed an exact ID and the
	// remote one displays as a different short ID.
	upsert("local-uuid-aaaa-bbbb")
	upsert("remotehost~local-uuid-aaaa-bbbb")

	ctx := context.Background()

	t.Run("unique substring resolves", func(t *testing.T) {
		got, err := resolveSessionID(ctx, database, "unique")
		if err != nil {
			t.Fatalf("resolveSessionID: %v", err)
		}
		if got != "unique-session-id" {
			t.Errorf("got %q, want unique-session-id", got)
		}
	})

	t.Run("exact full id matching another short id is ambiguous",
		func(t *testing.T) {
			_, err := resolveSessionID(ctx, database, "abcdef12")
			if err == nil {
				t.Fatal("expected ambiguity error, got nil")
			}
			if !strings.Contains(err.Error(), "ambiguous") {
				t.Errorf("error %q lacks 'ambiguous'",
					err.Error())
			}
		})

	t.Run("no match returns empty", func(t *testing.T) {
		got, err := resolveSessionID(ctx, database, "zzznope")
		if err != nil {
			t.Fatalf("resolveSessionID: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("unique full id resolves", func(t *testing.T) {
		got, err := resolveSessionID(
			ctx, database, "abcdef1234567890",
		)
		if err != nil {
			t.Fatalf("resolveSessionID: %v", err)
		}
		if got != "abcdef1234567890" {
			t.Errorf("got %q, want abcdef1234567890", got)
		}
	})

	t.Run("exact id contained in host-prefixed id resolves",
		func(t *testing.T) {
			got, err := resolveSessionID(
				ctx, database, "local-uuid-aaaa-bbbb",
			)
			if err != nil {
				t.Fatalf("resolveSessionID: %v", err)
			}
			if got != "local-uuid-aaaa-bbbb" {
				t.Errorf(
					"got %q, want local-uuid-aaaa-bbbb",
					got,
				)
			}
		})
}

func strPtr(s string) *string { return &s }

func parseLocalDate(t *testing.T, ts string) string {
	t.Helper()
	return shortDate(ts)
}
