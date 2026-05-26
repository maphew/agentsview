package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, renderSessionUsageHuman(&b, out))
	s := b.String()
	assert.Contains(t, s, "~$0.42", "output missing cost")
	assert.Contains(t, s, "claude-opus-4-6", "output missing model")
}

func TestRenderSessionUsageHuman_NoCostNoModels(t *testing.T) {
	out := &sessionUsageOutput{
		SessionUsage: db.SessionUsage{
			SessionID: "claude:s3", Agent: "claude-code",
			HasTokenData: true, HasCost: false,
		},
	}
	var b strings.Builder
	require.NoError(t, renderSessionUsageHuman(&b, out))
	s := b.String()
	assert.Contains(t, s, "n/a", "expected bare 'n/a' cost line")
	assert.NotContains(t, s, "unpriced",
		"should not mention unpriced when none")
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
	require.NoError(t, renderSessionUsageHuman(&b, out))
	s := b.String()
	assert.NotContains(t, s, "$", "no-cost output should not contain '$'")
	assert.Contains(t, s, "local-llama-99",
		"output should note unpriced model")
}
