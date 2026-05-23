// ABOUTME: `session usage <id>` subcommand — prints per-session
// ABOUTME: token statistics and a cost estimate (JSON or human).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newSessionUsageCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "usage <id>",
		Short:        "Show token usage and cost estimate for a session",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		// usage uses the direct token-use path (local SQLite +
		// on-demand sync), not the SessionService layer, so it cannot
		// honor --server. Reject it here with the same "--server not
		// yet implemented" error the service-backed session commands
		// return via resolveService, rather than silently querying
		// local data for a daemon-targeted request. PreRunE surfaces
		// the error through Execute (exit 1); Run keeps os.Exit for
		// the 0/2/3 usage codes.
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if remote, _ := cmd.Flags().GetString("server"); remote != "" {
				return errors.New("--server not yet implemented")
			}
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			runSessionUsage(args[0], outputFormat(cmd))
		},
	}
}

// runSessionUsage computes usage for one session and renders it,
// exiting with the shared usage exit code (0 = token data or cost,
// 2 = not found, 3 = neither). Uses Run + os.Exit (not RunE) so the
// 2/3 codes survive — cobra RunE errors collapse to exit 1.
func runSessionUsage(sessionID, format string) {
	out, code, err := sessionUsageData(sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(tokenUseExitErr)
	}
	if out != nil {
		if format == "json" {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if encErr := enc.Encode(out); encErr != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", encErr)
				os.Exit(tokenUseExitErr)
			}
		} else if rerr := renderSessionUsageHuman(
			os.Stdout, out,
		); rerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", rerr)
			os.Exit(tokenUseExitErr)
		}
	}
	os.Exit(code)
}

// renderSessionUsageHuman writes a compact key/value summary. The
// cost line shows "~$X.XX (models)" when a complete estimate exists,
// otherwise "n/a" (noting any unpriced models). The tilde marks the
// figure as a model-pricing estimate.
func renderSessionUsageHuman(w io.Writer, out *sessionUsageOutput) error {
	label := func(name string) string {
		return fmt.Sprintf("%-14s", name+":")
	}
	fmt.Fprintf(w, "%s %s\n", label("Session"),
		sanitizeTerminal(out.SessionID))
	fmt.Fprintf(w, "%s %s\n", label("Agent"),
		sanitizeTerminal(out.Agent))
	fmt.Fprintf(w, "%s %d\n", label("Output"), out.TotalOutputTokens)
	fmt.Fprintf(w, "%s %d\n", label("Peak ctx"), out.PeakContextTokens)
	if out.HasCost {
		models := strings.Join(out.Models, ", ")
		fmt.Fprintf(w, "%s ~$%.2f (%s)\n", label("Cost"),
			out.CostUSD, sanitizeTerminal(models))
	} else if len(out.UnpricedModels) > 0 {
		fmt.Fprintf(w, "%s n/a (unpriced: %s)\n", label("Cost"),
			sanitizeTerminal(strings.Join(out.UnpricedModels, ", ")))
	} else {
		fmt.Fprintf(w, "%s n/a\n", label("Cost"))
	}
	return nil
}
