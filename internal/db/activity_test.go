package db

import (
	"context"
	"testing"
)

func TestGetSessionActivity(t *testing.T) {
	d := testDB(t)
	sid := "test-activity"

	err := d.UpsertSession(Session{
		ID:        sid,
		Agent:     "claude",
		StartedAt: Ptr("2026-03-26T10:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Insert messages spanning ~29 minutes.
	msgs := []Message{
		{SessionID: sid, Ordinal: 0, Role: "user", Content: "hello", Timestamp: "2026-03-26T10:00:00Z", ContentLength: 5},
		{SessionID: sid, Ordinal: 1, Role: "assistant", Content: "hi", Timestamp: "2026-03-26T10:00:30Z", ContentLength: 2},
		{SessionID: sid, Ordinal: 2, Role: "user", Content: "next", Timestamp: "2026-03-26T10:01:30Z", ContentLength: 4},
		{SessionID: sid, Ordinal: 3, Role: "assistant", Content: "resp", Timestamp: "2026-03-26T10:02:00Z", ContentLength: 4},
		// Gap: no messages from 10:02 to 10:28.
		{SessionID: sid, Ordinal: 4, Role: "user", Content: "back", Timestamp: "2026-03-26T10:28:00Z", ContentLength: 4},
		{SessionID: sid, Ordinal: 5, Role: "assistant", Content: "wb", Timestamp: "2026-03-26T10:29:00Z", ContentLength: 2},
		// System message — should be excluded from counts.
		{SessionID: sid, Ordinal: 6, Role: "user", Content: "This session is being continued from a previous conversation.", Timestamp: "2026-03-26T10:29:30Z", ContentLength: 60, IsSystem: true},
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}

	resp, err := d.GetSessionActivity(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}

	// 29 min span => 1min buckets (snapInterval(1740) = 60).
	if resp.IntervalSeconds != 60 {
		t.Errorf("interval = %d, want 60", resp.IntervalSeconds)
	}

	// System message should still count toward total (7 total messages).
	if resp.TotalMessages != 7 {
		t.Errorf("total = %d, want 7", resp.TotalMessages)
	}

	// Should have 30 buckets (min 0 to min 29).
	if len(resp.Buckets) < 28 {
		t.Errorf("bucket count = %d, want >= 28", len(resp.Buckets))
	}

	// First bucket (10:00-10:01) should have user=1, assistant=1.
	first := resp.Buckets[0]
	if first.UserCount != 1 || first.AssistantCount != 1 {
		t.Errorf("first bucket: user=%d asst=%d, want 1,1", first.UserCount, first.AssistantCount)
	}
	if first.FirstOrdinal == nil || *first.FirstOrdinal != 0 {
		t.Errorf("first bucket first_ordinal: got %v, want 0", first.FirstOrdinal)
	}

	// Middle empty bucket should have nil FirstOrdinal.
	mid := resp.Buckets[15]
	if mid.UserCount != 0 || mid.AssistantCount != 0 {
		t.Errorf("mid bucket: user=%d asst=%d, want 0,0", mid.UserCount, mid.AssistantCount)
	}
	if mid.FirstOrdinal != nil {
		t.Errorf("mid bucket first_ordinal: got %v, want nil", mid.FirstOrdinal)
	}
}

func TestGetSessionActivity_NoMessages(t *testing.T) {
	d := testDB(t)
	sid := "test-empty"

	err := d.UpsertSession(Session{ID: sid, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := d.GetSessionActivity(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Buckets) != 0 {
		t.Errorf("buckets = %d, want 0", len(resp.Buckets))
	}
}

func TestGetSessionActivity_NullTimestamps(t *testing.T) {
	d := testDB(t)
	sid := "test-null-ts"

	err := d.UpsertSession(Session{ID: sid, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []Message{
		{SessionID: sid, Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2},
		{SessionID: sid, Ordinal: 1, Role: "assistant", Content: "hello", ContentLength: 5},
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}

	resp, err := d.GetSessionActivity(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Buckets) != 0 {
		t.Errorf("buckets = %d, want 0", len(resp.Buckets))
	}
	if resp.TotalMessages != 2 {
		t.Errorf("total = %d, want 2", resp.TotalMessages)
	}
}

func TestGetSessionActivity_SingleMessage(t *testing.T) {
	d := testDB(t)
	sid := "test-single"

	err := d.UpsertSession(Session{ID: sid, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []Message{
		{SessionID: sid, Ordinal: 0, Role: "user", Content: "hi", Timestamp: "2026-03-26T10:00:00Z", ContentLength: 2},
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}

	resp, err := d.GetSessionActivity(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(resp.Buckets))
	}
	if resp.Buckets[0].UserCount != 1 {
		t.Errorf("user count = %d, want 1", resp.Buckets[0].UserCount)
	}
}

func TestGetSessionActivity_MalformedTimestamps(t *testing.T) {
	d := testDB(t)
	sid := "test-malformed-ts"

	err := d.UpsertSession(Session{ID: sid, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []Message{
		{SessionID: sid, Ordinal: 0, Role: "user", Content: "hi", Timestamp: "2026-03-26T10:00:00Z", ContentLength: 2},
		{SessionID: sid, Ordinal: 1, Role: "assistant", Content: "hello", Timestamp: "not-a-timestamp", ContentLength: 5},
		{SessionID: sid, Ordinal: 2, Role: "user", Content: "bye", Timestamp: "2026-03-26T10:00:30Z", ContentLength: 3},
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}

	resp, err := d.GetSessionActivity(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}

	// Malformed timestamp excluded from buckets; valid ones bucketed.
	if len(resp.Buckets) < 1 {
		t.Fatal("expected at least 1 bucket")
	}
	// Both valid user messages (ord 0 and 2) are within 30s,
	// so they land in the same bucket.
	if resp.Buckets[0].UserCount != 2 || resp.Buckets[0].AssistantCount != 0 {
		t.Errorf(
			"first bucket: user=%d asst=%d, want 2,0",
			resp.Buckets[0].UserCount, resp.Buckets[0].AssistantCount,
		)
	}
	if resp.TotalMessages != 3 {
		t.Errorf("total = %d, want 3", resp.TotalMessages)
	}
}

func TestGetSessionActivity_FractionalTimestamps(t *testing.T) {
	d := testDB(t)
	sid := "test-frac-ts"

	err := d.UpsertSession(Session{ID: sid, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	// Two messages within the same 60s bucket but with fractional
	// timestamps that would be mis-bucketed by whole-second truncation.
	// 10:00:00.900 and 10:00:59.100 are 58.2s apart — same 60s bucket.
	msgs := []Message{
		{SessionID: sid, Ordinal: 0, Role: "user", Content: "a", Timestamp: "2026-03-26T10:00:00.900Z", ContentLength: 1},
		{SessionID: sid, Ordinal: 1, Role: "assistant", Content: "b", Timestamp: "2026-03-26T10:00:59.100Z", ContentLength: 1},
		// This message is in the next bucket (60.1s after the anchor).
		{SessionID: sid, Ordinal: 2, Role: "user", Content: "c", Timestamp: "2026-03-26T10:01:01.000Z", ContentLength: 1},
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}

	resp, err := d.GetSessionActivity(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}

	if resp.IntervalSeconds != 60 {
		t.Fatalf("interval = %d, want 60", resp.IntervalSeconds)
	}

	// First bucket should have both fractional-second messages.
	if len(resp.Buckets) < 1 {
		t.Fatal("expected at least 1 bucket")
	}
	first := resp.Buckets[0]
	if first.UserCount != 1 || first.AssistantCount != 1 {
		t.Errorf(
			"first bucket: user=%d asst=%d, want 1,1",
			first.UserCount, first.AssistantCount,
		)
	}

	// Second bucket should have the third message.
	if len(resp.Buckets) < 2 {
		t.Fatal("expected at least 2 buckets")
	}
	second := resp.Buckets[1]
	if second.UserCount != 1 {
		t.Errorf(
			"second bucket user=%d, want 1",
			second.UserCount,
		)
	}
}

func TestSnapInterval(t *testing.T) {
	tests := []struct {
		name     string
		duration int64 // seconds
		want     int64
	}{
		{"30s session", 30, 60},
		{"5m session", 300, 60},
		{"10m session", 600, 60},
		{"20m session", 1200, 60},
		{"30m session", 1800, 60},
		{"1h session", 3600, 120},
		{"2h session", 7200, 300},
		{"4h session", 14400, 600},
		{"8h session", 28800, 900},
		{"12h session", 43200, 1800},
		{"16h session", 57600, 1800},
		{"24h session", 86400, 3600},
		{"48h session", 172800, 7200},
		{"0s session", 0, 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SnapInterval(tt.duration)
			if got != tt.want {
				t.Errorf(
					"SnapInterval(%d) = %d, want %d",
					tt.duration, got, tt.want,
				)
			}
		})
	}
}
