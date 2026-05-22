package server

import (
	"testing"

	"go.kenn.io/agentsview/internal/config"
)

func TestInsightAgentConfigMapsBinaryOverrides(t *testing.T) {
	got := insightAgentConfig(map[string]config.AgentConfig{
		"claude": {Binary: "/opt/claude"},
	})

	if got["claude"].Binary != "/opt/claude" {
		t.Fatalf("claude binary = %q", got["claude"].Binary)
	}
}
