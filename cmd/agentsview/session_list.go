// ABOUTME: `session list` subcommand — lists sessions with the
// ABOUTME: full set of HTTP query-param equivalents as CLI flags.
package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/service"
)

func newSessionListCommand() *cobra.Command {
	var (
		project, excludeProject, machine, agent string
		date, dateFrom, dateTo, activeSince     string
		minMessages, maxMessages                int
		minUserMessages                         int
		includeOneShot                          bool
		includeAutomated, includeChildren       bool
		outcome, healthGrade                    string
		minToolFailures                         int
		cursor                                  string
		limit                                   int
	)
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List sessions with filters",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			f := service.ListFilter{
				Project:          project,
				ExcludeProject:   excludeProject,
				Machine:          machine,
				Agent:            agent,
				Date:             date,
				DateFrom:         dateFrom,
				DateTo:           dateTo,
				ActiveSince:      activeSince,
				MinMessages:      minMessages,
				MaxMessages:      maxMessages,
				MinUserMessages:  minUserMessages,
				IncludeOneShot:   includeOneShot,
				IncludeAutomated: includeAutomated,
				IncludeChildren:  includeChildren,
				Outcome:          outcome,
				HealthGrade:      healthGrade,
				Cursor:           cursor,
				Limit:            limit,
			}
			if cmd.Flags().Changed("min-tool-failures") {
				f.MinToolFailures = &minToolFailures
			}

			list, err := svc.List(cmd.Context(), f)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(list)
			}
			return printSessionListHuman(cmd.OutOrStdout(), list)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&project, "project", "",
		"Filter by project name")
	flags.StringVar(&excludeProject, "exclude-project", "",
		"Exclude sessions from the given project")
	flags.StringVar(&machine, "machine", "",
		"Filter by machine name")
	flags.StringVar(&agent, "agent", "",
		"Filter by agent (claude, codex, cursor, ...)")
	flags.StringVar(&date, "date", "",
		"Filter sessions started on YYYY-MM-DD")
	flags.StringVar(&dateFrom, "date-from", "",
		"Filter sessions started on or after YYYY-MM-DD")
	flags.StringVar(&dateTo, "date-to", "",
		"Filter sessions started on or before YYYY-MM-DD")
	flags.StringVar(&activeSince, "active-since", "",
		"Filter sessions active since RFC3339 timestamp")
	flags.IntVar(&minMessages, "min-messages", 0,
		"Minimum total message count")
	flags.IntVar(&maxMessages, "max-messages", 0,
		"Maximum total message count")
	flags.IntVar(&minUserMessages, "min-user-messages", 0,
		"Minimum user message count")
	flags.BoolVar(&includeOneShot, "include-one-shot", false,
		"Include one-shot sessions (excluded by default)")
	flags.BoolVar(&includeAutomated, "include-automated", false,
		"Include automated sessions (excluded by default)")
	flags.BoolVar(&includeChildren, "include-children", false,
		"Include subagent/child sessions")
	flags.StringVar(&outcome, "outcome", "",
		"Filter by outcome (comma-separated: success,failure,...)")
	flags.StringVar(&healthGrade, "health-grade", "",
		"Filter by health grade (comma-separated: A,B,C,D,F)")
	flags.IntVar(&minToolFailures, "min-tool-failures", 0,
		"Minimum tool-failure signal count (0 is a valid filter)")
	flags.StringVar(&cursor, "cursor", "",
		"Pagination cursor from a previous response")
	flags.IntVar(&limit, "limit", 0,
		fmt.Sprintf(
			"Maximum sessions to return (default %d, max %d)",
			db.DefaultSessionLimit, db.MaxSessionLimit,
		))

	return cmd
}

// printSessionListHuman writes a compact columnar summary of the
// session list, with a trailing hint when another page is
// available. Prints "(no sessions)" for empty lists.
func printSessionListHuman(
	w io.Writer, list *service.SessionList,
) error {
	if len(list.Sessions) == 0 {
		fmt.Fprintln(w, "(no sessions)")
		return nil
	}
	fmt.Fprintf(w, "%-40s  %-20s  %-15s  %s\n",
		"ID", "PROJECT", "AGENT", "STARTED")
	for _, s := range list.Sessions {
		started := "-"
		if s.StartedAt != nil && len(*s.StartedAt) >= 16 {
			started = (*s.StartedAt)[:16]
		}
		fmt.Fprintf(w, "%-40s  %-20s  %-15s  %s\n",
			sanitizeTerminal(s.ID),
			sanitizeTerminal(s.Project),
			sanitizeTerminal(s.Agent),
			sanitizeTerminal(started))
	}
	if list.NextCursor != "" {
		// Cursor is an opaque server-minted string. Sanitize too
		// so a malicious DB row can't feed escapes through a hint.
		fmt.Fprintf(w, "\nMore results: --cursor %s\n",
			sanitizeTerminal(list.NextCursor))
	}
	return nil
}
