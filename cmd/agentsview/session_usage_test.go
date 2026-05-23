package main

import (
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/db"
)

func TestRenderSessionUsageHuman_WithCost(t *testing.T) {
	out := &sessionUsageOutput{
		SessionUsage: db.SessionUsage{
			SessionID: "claude:s1", Agent: "claude-code", Project: "proj",
			TotalOutputTokens: 28800, PeakContextTokens: 118000,
			HasTokenData: true, CostUSD: 0.42, HasCost: true,
			Models: []string{"claude-opus-4-6"},
		},
	}
	var b strings.Builder
	if err := renderSessionUsageHuman(&b, out); err != nil {
		t.Fatalf("render: %v", err)
	}
	s := b.String()
	if !strings.Contains(s, "~$0.42") {
		t.Errorf("output missing cost:\n%s", s)
	}
	if !strings.Contains(s, "claude-opus-4-6") {
		t.Errorf("output missing model:\n%s", s)
	}
}

func TestRenderSessionUsageHuman_NoCostNoModels(t *testing.T) {
	out := &sessionUsageOutput{
		SessionUsage: db.SessionUsage{
			SessionID: "claude:s3", Agent: "claude-code",
			HasTokenData: true, HasCost: false,
		},
	}
	var b strings.Builder
	if err := renderSessionUsageHuman(&b, out); err != nil {
		t.Fatalf("render: %v", err)
	}
	s := b.String()
	if !strings.Contains(s, "n/a") {
		t.Errorf("expected bare 'n/a' cost line:\n%s", s)
	}
	if strings.Contains(s, "unpriced") {
		t.Errorf("should not mention unpriced when none:\n%s", s)
	}
}

func TestRenderSessionUsageHuman_NoCost(t *testing.T) {
	out := &sessionUsageOutput{
		SessionUsage: db.SessionUsage{
			SessionID: "claude:s2", Agent: "claude-code",
			HasTokenData: true, HasCost: false,
			UnpricedModels: []string{"local-llama-99"},
		},
	}
	var b strings.Builder
	if err := renderSessionUsageHuman(&b, out); err != nil {
		t.Fatalf("render: %v", err)
	}
	s := b.String()
	if strings.Contains(s, "$") {
		t.Errorf("no-cost output should not contain '$':\n%s", s)
	}
	if !strings.Contains(s, "local-llama-99") {
		t.Errorf("output should note unpriced model:\n%s", s)
	}
}
