// ABOUTME: `session sync` subcommand — triggers a one-off sync for
// ABOUTME: a single session, either by path or by id. Refuses
// ABOUTME: against read-only daemons (pg serve).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/service"
	"github.com/wesm/agentsview/internal/sync"
)

func newSessionSyncCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "sync <path-or-id>",
		Short:        "Parse and insert a single session into the database",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if remote, _ := cmd.Flags().GetString("server"); remote != "" {
				return errors.New("--server not yet implemented")
			}
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			tr, err := detectTransport(cfg.DataDir, 0)
			if err != nil {
				return err
			}
			if tr.Mode == transportHTTP && tr.ReadOnly {
				return fmt.Errorf(
					"daemon at %s is read-only (pg serve); cannot sync: "+
						"stop 'pg serve' and run 'agentsview sync' against "+
						"the local DB, or start a local daemon",
					tr.URL,
				)
			}
			if tr.Mode == transportDirect && tr.DirectReadOnly {
				// A daemon is active but its TCP probe failed.
				// Opening a writable engine here would race the
				// daemon for SQLite write ownership, so refuse
				// rather than compete.
				return errors.New(
					"local daemon is active but not responding; " +
						"refusing to sync directly to avoid competing " +
						"for write ownership. Retry once the daemon " +
						"is reachable, or stop it to sync locally",
				)
			}

			svc, cleanup, err := syncService(cfg, tr)
			if err != nil {
				return err
			}
			defer cleanup()

			detail, err := svc.Sync(cmd.Context(), classifySyncArg(args[0]))
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(detail)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "synced: %s\n",
				sanitizeTerminal(detail.ID))
			return nil
		},
	}
}

// syncService resembles newService but constructs a real
// *sync.Engine for the direct-mode case so `session sync` can
// actually write. The default newService path passes a nil engine
// (reads don't need it), which would make Sync return
// db.ErrReadOnly.
func syncService(
	cfg config.Config, tr transport,
) (service.SessionService, func(), error) {
	if tr.Mode == transportHTTP {
		return service.NewHTTPBackend(tr.URL, cfg.AuthToken, tr.ReadOnly),
			func() {}, nil
	}
	applyClassifierConfig(cfg)
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening db: %w", err)
	}
	engine := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: cfg.AgentDirs,
		Machine:   "local",
	})
	cleanup := func() { d.Close() }
	return service.NewDirectBackend(d, engine), cleanup, nil
}

// classifySyncArg returns {Path: arg} when arg is clearly a path:
// absolute, rooted in "." / "..", or containing a path separator,
// AND points at an existing regular file. Otherwise it's treated
// as a session id. This avoids CWD-dependent ambiguity where a
// session id that happens to match a file in the current directory
// would silently become a path.
func classifySyncArg(arg string) service.SyncInput {
	if !looksLikePath(arg) {
		return service.SyncInput{ID: arg}
	}
	fi, err := os.Stat(arg)
	if err != nil || !fi.Mode().IsRegular() {
		return service.SyncInput{ID: arg}
	}
	return service.SyncInput{Path: arg}
}

// looksLikePath returns true when arg has explicit path shape:
// absolute path, ./ or ../ prefix, or contains a separator. Bare
// names without any separator are treated as session IDs. Both '/'
// and '\\' count as separators so Windows users writing forward-slash
// relative paths (e.g. "./session.jsonl") are still recognized.
func looksLikePath(arg string) bool {
	if filepath.IsAbs(arg) {
		return true
	}
	if arg == "." || arg == ".." ||
		strings.HasPrefix(arg, "./") ||
		strings.HasPrefix(arg, "../") ||
		strings.HasPrefix(arg, `.\`) ||
		strings.HasPrefix(arg, `..\`) {
		return true
	}
	return strings.ContainsAny(arg, `/\`)
}
