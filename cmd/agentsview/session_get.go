// ABOUTME: `session get <id>` subcommand — prints session detail
// ABOUTME: in human or JSON format.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wesm/agentsview/internal/parser"
	"github.com/wesm/agentsview/internal/service"
)

func newSessionGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "get <id>",
		Short:        "Get session metadata and signals",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			detail, err := lookupSessionWithPrefixes(
				cmd.Context(), svc, args[0],
			)
			if err != nil {
				return err
			}
			if detail == nil {
				return fmt.Errorf("session %s not found", args[0])
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(detail)
			}
			return printSessionDetailHuman(cmd.OutOrStdout(), detail)
		},
	}
}

// resolveServiceSessionID returns the canonical session ID matching id,
// accommodating bare UUIDs by retrying with each registered agent
// prefix (codex:, copilot:, gemini:, ...) when the exact lookup
// misses. Stored IDs are prefixed for non-Claude agents, so a user
// copying a UUID from a session file name would otherwise see a
// confusing "not found" error. Returns an error whose message
// begins with "session not found:" when no match exists — callers
// get a clear failure instead of silent empty output.
func resolveServiceSessionID(
	ctx context.Context,
	svc service.SessionService,
	id string,
) (string, error) {
	detail, err := svc.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if detail != nil {
		return id, nil
	}
	// If the user already supplied a prefixed ID (contains ":")
	// or a host-prefixed remote ID ("host~..."), don't second-
	// guess them — the exact lookup is authoritative.
	if strings.ContainsAny(id, ":~") {
		return "", fmt.Errorf("session not found: %s", id)
	}
	for _, def := range parser.Registry {
		if def.IDPrefix == "" {
			continue
		}
		candidate := def.IDPrefix + id
		detail, err := svc.Get(ctx, candidate)
		if err != nil {
			return "", err
		}
		if detail != nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("session not found: %s", id)
}

// lookupSessionWithPrefixes fetches a session detail, trying agent
// prefixes for bare UUIDs. Preserved as a thin wrapper around
// resolveServiceSessionID + svc.Get so `session get` can keep its
// existing "return nil on not-found" semantics (which render the
// "session %s not found" error at the command boundary).
func lookupSessionWithPrefixes(
	ctx context.Context,
	svc service.SessionService,
	id string,
) (*service.SessionDetail, error) {
	resolved, err := resolveServiceSessionID(ctx, svc, id)
	if err != nil {
		if strings.HasPrefix(err.Error(), "session not found:") {
			return nil, nil
		}
		return nil, err
	}
	return svc.Get(ctx, resolved)
}

// printSessionDetailHuman writes a compact key/value summary of
// the session's core fields. Optional *string/*int fields render
// as "-" when nil.
func printSessionDetailHuman(w io.Writer, s *service.SessionDetail) error {
	label := func(name string) string {
		return fmt.Sprintf("%-14s", name+":")
	}
	name := s.ID
	if s.DisplayName != nil && *s.DisplayName != "" {
		name = *s.DisplayName
	}
	fmt.Fprintf(w, "%s %s\n", label("ID"), sanitizeTerminal(s.ID))
	fmt.Fprintf(w, "%s %s\n", label("Name"), sanitizeTerminal(name))
	fmt.Fprintf(w, "%s %s\n", label("Project"), sanitizeTerminal(s.Project))
	fmt.Fprintf(w, "%s %s\n", label("Agent"), sanitizeTerminal(s.Agent))
	fmt.Fprintf(w, "%s %s\n", label("Machine"), sanitizeTerminal(s.Machine))
	fmt.Fprintf(w, "%s %s\n",
		label("Started At"), sanitizeTerminal(derefStringOrDash(s.StartedAt)))
	fmt.Fprintf(w, "%s %s\n",
		label("Ended At"), sanitizeTerminal(derefStringOrDash(s.EndedAt)))
	fmt.Fprintf(w, "%s %d/%d\n",
		label("Messages"), s.UserMessageCount, s.MessageCount)
	if s.Outcome != "" {
		fmt.Fprintf(w, "%s %s [%s]\n", label("Outcome"),
			sanitizeTerminal(s.Outcome), sanitizeTerminal(s.OutcomeConfidence))
	}
	if s.HealthScore != nil {
		grade := "-"
		if s.HealthGrade != nil && *s.HealthGrade != "" {
			grade = *s.HealthGrade
		}
		fmt.Fprintf(w, "%s %d (%s)\n",
			label("Health"), *s.HealthScore, sanitizeTerminal(grade))
	} else {
		fmt.Fprintf(w, "%s -\n", label("Health"))
	}
	return nil
}

// derefStringOrDash returns *p or "-" when p is nil or empty.
func derefStringOrDash(p *string) string {
	if p == nil || *p == "" {
		return "-"
	}
	return *p
}
