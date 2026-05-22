package ssh

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// buildResolveScript generates a shell script that echoes each
// file-based agent's resolved directory on the remote host.
// Output format: "agentType:dir\n" per agent.
//
// Only includes agents where FileBased is true and DiscoverFunc
// is non-nil. For each agent with an EnvVar, the script checks
// the env var first and falls back to the default dir. Dirs that
// don't exist on the remote are skipped.
func buildResolveScript() string {
	var b strings.Builder
	for _, def := range parser.Registry {
		if !def.FileBased || def.DiscoverFunc == nil {
			continue
		}
		for _, rel := range def.DefaultDirs {
			defaultDir := "$HOME/" + rel
			if def.EnvVar != "" {
				// env var overrides default
				fmt.Fprintf(&b,
					"dir=\"${%s:-%s}\"; "+
						"[ -d \"$dir\" ] && "+
						"echo \"%s:$dir\"\n",
					def.EnvVar, defaultDir,
					string(def.Type),
				)
			} else {
				fmt.Fprintf(&b,
					"dir=\"%s\"; "+
						"[ -d \"$dir\" ] && "+
						"echo \"%s:$dir\"\n",
					defaultDir,
					string(def.Type),
				)
			}
		}
	}
	// Ensure exit 0 — the last [ -d ] test may fail if that
	// dir doesn't exist, which would make sh exit non-zero.
	b.WriteString("true\n")
	return b.String()
}

// parseResolvedDirs parses script output into a map of agent type
// to directory paths. Skips empty lines and entries with empty dir.
func parseResolvedDirs(
	output string,
) map[parser.AgentType][]string {
	result := make(map[parser.AgentType][]string)
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		agentStr, dir, ok := strings.Cut(line, ":")
		if !ok || dir == "" {
			continue
		}
		at := parser.AgentType(agentStr)
		result[at] = append(result[at], dir)
	}
	return result
}

// resolveDirs runs the resolve script on the remote host via SSH
// and returns the discovered agent directories.
func resolveDirs(
	ctx context.Context,
	host, user string, port int, sshOpts []string,
) (map[parser.AgentType][]string, error) {
	script := buildResolveScript()
	out, err := runSSH(ctx, host, user, port, sshOpts, script)
	if err != nil {
		return nil, fmt.Errorf("resolve dirs: %w", err)
	}
	return parseResolvedDirs(string(out)), nil
}
