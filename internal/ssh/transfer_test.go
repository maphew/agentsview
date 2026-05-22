package ssh

import (
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/parser"
)

func TestBuildTarCommand(t *testing.T) {
	dirs := map[parser.AgentType][]string{
		parser.AgentClaude: {"/home/wes/.claude/projects"},
		parser.AgentCodex:  {"/home/wes/.codex/sessions"},
	}
	cmd := buildTarCommand(dirs)

	if !strings.HasPrefix(cmd, "tar cf - -C / -- ") {
		t.Errorf("bad prefix: %s", cmd)
	}
	// Paths are shell-quoted.
	if !strings.Contains(cmd, "'home/wes/.claude/projects'") {
		t.Error("missing quoted claude dir")
	}
	if !strings.Contains(cmd, "'home/wes/.codex/sessions'") {
		t.Error("missing quoted codex dir")
	}
	// No leading slash in dir args.
	if strings.Contains(cmd, "'/home/") {
		t.Errorf("dir has leading slash: %s", cmd)
	}
}

func TestRemapPath(t *testing.T) {
	// Use filepath.Join so the local paths are OS-native.
	// remapToRemotePath always returns forward-slash paths.
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := "/home/wes/.claude"
	localPath := filepath.Join(
		"tmp", "sync-123", "home", "wes", ".claude", "foo.jsonl",
	)
	got := remapToRemotePath(tempDir, remoteDir, localPath)
	want := "/home/wes/.claude/foo.jsonl"
	if got != want {
		t.Errorf("remapToRemotePath = %q, want %q", got, want)
	}
}

func TestRemappedDir(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := "/home/wes/.claude"
	got := remappedDir(tempDir, remoteDir)
	want := filepath.Join("tmp", "sync-123", "home", "wes", ".claude")
	if got != want {
		t.Errorf("remappedDir = %q, want %q", got, want)
	}
}
