// ABOUTME: `session messages <id>` subcommand — prints a window of
// ABOUTME: messages in JSON or human format.
package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/wesm/agentsview/internal/service"
)

func newSessionMessagesCommand() *cobra.Command {
	var (
		from      int
		limit     int
		direction string
	)
	cmd := &cobra.Command{
		Use:          "messages <id>",
		Short:        "Show a window of messages from a session",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if direction != "asc" && direction != "desc" {
				return fmt.Errorf(
					"invalid --direction %q: must be asc or desc", direction,
				)
			}
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			filter := service.MessageFilter{
				Limit:     limit,
				Direction: direction,
			}
			// Preserve presence: an explicit --from 0 means "start
			// at ordinal 0", not "use the default tail/head".
			if cmd.Flags().Changed("from") {
				filter.From = &from
			}

			list, err := svc.Messages(cmd.Context(), args[0], filter)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(list)
			}
			return printMessagesHuman(cmd.OutOrStdout(), list)
		},
	}
	flags := cmd.Flags()
	flags.IntVar(&from, "from", 0,
		"Starting ordinal (inclusive). Omit for the newest page in "+
			"--direction desc; explicit 0 starts at ordinal 0.")
	flags.IntVar(&limit, "limit", 0,
		"Maximum messages to return (0 = server default)")
	flags.StringVar(&direction, "direction", "asc",
		"Sort direction: asc or desc")
	return cmd
}

// printMessagesHuman prints each message as a header block followed
// by its content. Timestamp is trimmed to YYYY-MM-DDTHH:MM:SS.
// Session-derived fields are sanitized so escape sequences embedded
// in agent output can't spoof the terminal.
func printMessagesHuman(w io.Writer, list *service.MessageList) error {
	for _, m := range list.Messages {
		ts := m.Timestamp
		if len(ts) >= 19 {
			ts = ts[:19]
		}
		fmt.Fprintf(w, "--- #%d  %s  %s ---\n",
			m.Ordinal, sanitizeTerminal(m.Role), sanitizeTerminal(ts))
		fmt.Fprintln(w, sanitizeTerminal(m.Content))
		fmt.Fprintln(w)
	}
	return nil
}
