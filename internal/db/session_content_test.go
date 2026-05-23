package db

import (
	"context"
	"testing"
)

func TestReplaceSessionContentAtomic(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	msgs := []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "key AKIA7QHWN2DKR4FYPLJM"},
	}
	signals := SessionSignalUpdate{Outcome: "success", SecretLeakCount: 1,
		SecretsRulesVersion: "rulesv1"}
	findings := []SecretFinding{{
		SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 4, MatchEnd: 24,
		MatchIndex: 0, RedactedMatch: "AKIA…MPLE", RulesVersion: "rulesv1",
	}}
	if err := d.ReplaceSessionContent("s1", msgs, signals, findings); err != nil {
		t.Fatalf("ReplaceSessionContent: %v", err)
	}
	got, _ := d.GetAllMessages(context.Background(), "s1")
	if len(got) != 1 {
		t.Fatalf("messages: got %d, want 1", len(got))
	}
	f, _ := d.SessionSecretFindings(context.Background(), "s1")
	if len(f) != 1 {
		t.Fatalf("findings: got %d, want 1", len(f))
	}
	s, _ := d.GetSession(context.Background(), "s1")
	if s.SecretLeakCount != 1 {
		t.Errorf("SecretLeakCount = %d, want 1", s.SecretLeakCount)
	}
}
