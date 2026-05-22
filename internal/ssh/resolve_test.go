package ssh

import (
	"os/exec"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/parser"
)

func TestBuildResolveScript(t *testing.T) {
	script := buildResolveScript()

	// Claude has CLAUDE_PROJECTS_DIR env var — must be referenced.
	if !strings.Contains(script, "CLAUDE_PROJECTS_DIR") {
		t.Error("script missing CLAUDE_PROJECTS_DIR")
	}

	// Non-file-based agents must not appear.
	for _, def := range parser.Registry {
		if def.FileBased || def.DiscoverFunc != nil {
			continue
		}
		marker := "echo \"" + string(def.Type) + ":"
		if strings.Contains(script, marker) {
			t.Errorf(
				"non-file-based agent %s in script",
				def.Type,
			)
		}
	}

	// Every file-based agent with DiscoverFunc must appear.
	for _, def := range parser.Registry {
		if !def.FileBased || def.DiscoverFunc == nil {
			continue
		}
		marker := "echo \"" + string(def.Type) + ":"
		if !strings.Contains(script, marker) {
			t.Errorf(
				"file-based agent %s missing from script",
				def.Type,
			)
		}
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
	if err != nil {
		t.Fatalf(
			"resolve script failed: %v\noutput: %s",
			err, out,
		)
	}
	// No dirs should be found.
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf(
			"expected no output, got: %s", s,
		)
	}
}

func TestParseResolvedDirs(t *testing.T) {
	input := "claude:/home/wes/.claude/projects\n" +
		"codex:\n" +
		"copilot:/home/wes/.copilot\n" +
		"\n"

	dirs := parseResolvedDirs(input)

	// codex has empty dir — excluded.
	if _, ok := dirs[parser.AgentCodex]; ok {
		t.Error("codex should be excluded (empty dir)")
	}

	// claude and copilot present.
	if got := dirs[parser.AgentClaude]; len(got) != 1 ||
		got[0] != "/home/wes/.claude/projects" {
		t.Errorf("claude dirs = %v, want [/home/wes/.claude/projects]", got)
	}
	if got := dirs[parser.AgentCopilot]; len(got) != 1 ||
		got[0] != "/home/wes/.copilot" {
		t.Errorf("copilot dirs = %v, want [/home/wes/.copilot]", got)
	}

	if len(dirs) != 2 {
		t.Errorf("got %d entries, want 2", len(dirs))
	}
}
