package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestWorkBuddyRegistryUsesRecursiveWatch(t *testing.T) {
	def, ok := parser.AgentByType(parser.AgentWorkBuddy)
	require.True(t, ok, "AgentWorkBuddy missing from Registry")
	require.False(t, def.ShallowWatch, "WorkBuddy should use recursive watch for nested sessions")
}

func TestEngineClassifyWorkBuddyPaths(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWorkBuddy: {root},
		},
		Machine: "local",
	})

	mainPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	subPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	toolPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "tool-results", "tool_123.txt")
	for _, path := range []string{mainPath, subPath, toolPath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	}

	got, ok := engine.classifyOnePath(mainPath, nil)
	require.True(t, ok, "main path did not classify")
	assert.Equal(t, mainPath, got.Path)
	assert.Equal(t, "proj", got.Project)
	assert.Equal(t, parser.AgentWorkBuddy, got.Agent)

	got, ok = engine.classifyOnePath(subPath, nil)
	require.True(t, ok, "subagent path did not classify")
	assert.Equal(t, subPath, got.Path)
	assert.Equal(t, "proj", got.Project)
	assert.Equal(t, parser.AgentWorkBuddy, got.Agent)

	got, ok = engine.classifyOnePath(toolPath, nil)
	assert.False(t, ok, "tool result classified as %+v", got)
}

func TestEngineClassifyWorkBuddyProjectNamedSubagentsAsMainSession(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWorkBuddy: {root},
		},
		Machine: "local",
	})

	path := filepath.Join(root, "subagents", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))

	got, ok := engine.classifyOnePath(path, nil)
	require.True(t, ok, "path did not classify")
	assert.Equal(t, path, got.Path)
	assert.Equal(t, "subagents", got.Project)
	assert.Equal(t, parser.AgentWorkBuddy, got.Agent)
}
