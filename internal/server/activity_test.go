package server_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"go.kenn.io/agentsview/internal/db"
)

func TestGetSessionActivity(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 10)
	te.seedMessages(t, "s1", 10)

	w := te.get(t, "/api/v1/sessions/s1/activity")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var body db.SessionActivityResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if body.TotalMessages == 0 {
		t.Error("expected non-zero total_messages")
	}
	if len(body.Buckets) == 0 {
		t.Error("expected non-empty buckets")
	}
	if body.IntervalSeconds <= 0 {
		t.Error("expected positive interval_seconds")
	}
}

func TestGetSessionActivity_NotFound(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/sessions/nonexistent/activity")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var body db.SessionActivityResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Buckets) != 0 {
		t.Error("expected empty buckets for nonexistent session")
	}
}
