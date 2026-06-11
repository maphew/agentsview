package server

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
)

func TestInsightAgentConfigMapsBinaryOverrides(t *testing.T) {
	got := insightAgentConfig(map[string]config.AgentConfig{
		"claude": {Binary: "/opt/claude"},
	})

	assert.Equal(t, "/opt/claude", got["claude"].Binary)
}
