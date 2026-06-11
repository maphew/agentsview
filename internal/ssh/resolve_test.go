package ssh

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestBuildResolveScript(t *testing.T) {
	script := buildResolveScript()

	// Claude has CLAUDE_PROJECTS_DIR env var — must be referenced.
	assert.Contains(t, script, "CLAUDE_PROJECTS_DIR")

	// Non-file-based agents must not appear.
	for _, def := range parser.Registry {
		if def.FileBased || def.DiscoverFunc != nil {
			continue
		}
		marker := "echo \"" + string(def.Type) + ":"
		assert.NotContains(t, script, marker,
			"non-file-based agent %s in script", def.Type)
	}

	// Every file-based agent with DiscoverFunc must appear.
	for _, def := range parser.Registry {
		if !def.FileBased || def.DiscoverFunc == nil {
			continue
		}
		marker := "echo \"" + string(def.Type) + ":"
		assert.Contains(t, script, marker,
			"file-based agent %s missing from script", def.Type)
	}
}

func TestResolveScriptExitsZero(t *testing.T) {
	// The resolve script must exit 0 even when no agent
	// dirs exist. Verify by running it against an empty
	// HOME so no default dirs are found.
	script := buildResolveScript()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"HOME=/nonexistent"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "resolve script failed: output: %s", out)
	// No dirs should be found.
	assert.Empty(t, strings.TrimSpace(string(out)))
}

func TestParseResolvedDirs(t *testing.T) {
	input := "claude:/home/wes/.claude/projects\n" +
		"codex:\n" +
		"copilot:/home/wes/.copilot\n" +
		"\n"

	dirs := parseResolvedDirs(input)

	// codex has empty dir — excluded.
	_, ok := dirs[parser.AgentCodex]
	assert.False(t, ok, "codex should be excluded (empty dir)")

	// claude and copilot present.
	assert.Equal(t, []string{"/home/wes/.claude/projects"}, dirs[parser.AgentClaude])
	assert.Equal(t, []string{"/home/wes/.copilot"}, dirs[parser.AgentCopilot])

	assert.Len(t, dirs, 2)
}
