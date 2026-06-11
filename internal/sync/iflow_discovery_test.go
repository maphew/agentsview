package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestDiscoverIflowProjects(t *testing.T) {
	// Create a temporary directory structure for testing
	tmpDir := t.TempDir()

	// Create project directories
	proj1 := filepath.Join(tmpDir, "project1")
	proj2 := filepath.Join(tmpDir, "project2")

	require.NoError(t, os.MkdirAll(proj1, 0o755))
	require.NoError(t, os.MkdirAll(proj2, 0o755))

	// Create session files in project1
	session1 := filepath.Join(proj1, "session-abc123.jsonl")
	session2 := filepath.Join(proj1, "session-def456.jsonl")

	require.NoError(t, os.WriteFile(session1, []byte(`{"test":"data"}`), 0o644))
	require.NoError(t, os.WriteFile(session2, []byte(`{"test":"data"}`), 0o644))

	// Create a session file in project2
	session3 := filepath.Join(proj2, "session-ghi789.jsonl")
	require.NoError(t, os.WriteFile(session3, []byte(`{"test":"data"}`), 0o644))

	// Create a non-session file (should be ignored)
	otherFile := filepath.Join(proj1, "other.txt")
	require.NoError(t, os.WriteFile(otherFile, []byte(`not a session`), 0o644))

	// Create a directory (should be ignored)
	subDir := filepath.Join(proj1, "subdir")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	// Run discovery
	files := parser.DiscoverIflowProjects(tmpDir)

	// Verify results
	assert.Len(t, files, 3)

	// Verify file paths
	paths := make(map[string]bool)
	for _, f := range files {
		paths[f.Path] = true
	}

	assert.True(t, paths[session1], "session1 not found in results")
	assert.True(t, paths[session2], "session2 not found in results")
	assert.True(t, paths[session3], "session3 not found in results")
	assert.False(t, paths[otherFile], "other.txt should not be in results")

	// Verify project names
	projects := make(map[string]bool)
	for _, f := range files {
		projects[f.Project] = true
	}

	assert.True(t, projects["project1"], "project1 not found in projects")
	assert.True(t, projects["project2"], "project2 not found in projects")

	// Verify agent type
	for _, f := range files {
		assert.Equal(t, parser.AgentType("iflow"), f.Agent)
	}
}

func TestFindIflowSourceFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a project directory
	proj := filepath.Join(tmpDir, "test-project")
	require.NoError(t, os.MkdirAll(proj, 0o755))

	// Create a session file
	sessionID := "abc123-def456"
	sessionFile := filepath.Join(proj, "session-"+sessionID+".jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte(`{"test":"data"}`), 0o644))

	// Test finding the file
	found := parser.FindIflowSourceFile(tmpDir, sessionID)
	assert.Equal(t, sessionFile, found)

	// Test finding a non-existent file
	notFound := parser.FindIflowSourceFile(tmpDir, "nonexistent")
	assert.Empty(t, notFound)

	// Test finding a fork ID (should extract base session ID)
	// Fork IDs have format: <baseUUID>-<childUUID>
	// The file lookup should use only the base UUID
	baseSessionID := "96e6d875-92eb-40b9-b193-a9ba99f0f709"
	forkSessionID := baseSessionID + "-12345678-1234-5678-9abc-def012345678"
	forkSessionFile := filepath.Join(proj, "session-"+baseSessionID+".jsonl")
	require.NoError(t, os.WriteFile(forkSessionFile, []byte(`{"test":"fork"}`), 0o644))

	// Test finding the fork session - should find the base file
	foundFork := parser.FindIflowSourceFile(tmpDir, forkSessionID)
	assert.Equal(t, forkSessionFile, foundFork, "for fork ID %s", forkSessionID)
}
