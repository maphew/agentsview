// ABOUTME: CLI subcommand that syncs session data into the database
// ABOUTME: without starting the HTTP server.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/ssh"
	"go.kenn.io/agentsview/internal/sync"
)

// SyncConfig holds parsed CLI options for the sync command.
type SyncConfig struct {
	Full bool
	Host string
	User string
	Port int
	// CPUProfile, MemProfile, and Trace are hidden flags that capture a
	// pprof CPU profile, allocation snapshot, and runtime trace for the
	// sync pass. Empty strings disable each independently.
	CPUProfile string
	MemProfile string
	Trace      string
}

func runSync(cfg SyncConfig) {
	if doSync(cfg) {
		os.Exit(1)
	}
}

// doSync performs the sync run and reports whether any configured
// remote host failed. It owns the deferred cleanup (profile stop,
// db close) so runSync can translate the result into a non-zero
// exit code without skipping that cleanup.
func doSync(cfg SyncConfig) (hadRemoteFailures bool) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}

	setupLogFile(appCfg.DataDir)

	stopProfile := startSyncProfile(cfg)
	defer stopProfile()

	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer database.Close()

	if appCfg.CursorSecret != "" {
		secret, decErr := base64.StdEncoding.DecodeString(
			appCfg.CursorSecret,
		)
		if decErr != nil {
			fatal("invalid cursor secret: %v", decErr)
		}
		database.SetCursorSecret(secret)
	}

	if cfg.Host != "" {
		runRemoteSync(appCfg, database, cfg)
		return false
	}

	if err := appCfg.ValidateRemoteHosts(); err != nil {
		fatal("invalid remote_hosts config: %v", err)
	}

	failures := syncLocalAndRemotes(
		appCfg.RemoteHosts, cfg.Full,
		func() bool { return runLocalSync(appCfg, database, cfg.Full) },
		func(rh config.RemoteHost, full bool) error {
			return runRemoteSyncOnce(appCfg, database, rh, full)
		},
	)
	reportRemoteFailures(failures)
	return len(failures) > 0
}

// syncLocalAndRemotes runs the local sync, then the configured
// remote hosts. A local resync (forced via --full or an automatic
// data-version resync) forces every remote sync full as well, so
// remote sessions are re-parsed rather than skipped via the remote
// skip cache. localSync and remoteSync are injected for testing;
// localSync returns whether a full resync was performed.
func syncLocalAndRemotes(
	hosts []config.RemoteHost, cfgFull bool,
	localSync func() bool,
	remoteSync func(config.RemoteHost, bool) error,
) []remoteHostFailure {
	didResync := localSync()
	full := cfgFull || didResync
	return runRemoteHosts(hosts, full, remoteSync)
}

func runRemoteSync(
	appCfg config.Config, database *db.DB, cfg SyncConfig,
) {
	rh := config.RemoteHost{
		Host: cfg.Host,
		User: cfg.User,
		Port: cfg.Port,
	}
	if err := runRemoteSyncOnce(
		appCfg, database, rh, cfg.Full,
	); err != nil {
		fatal("remote sync: %v", err)
	}
}

// runRemoteSyncOnce syncs a single remote host and returns any
// error instead of exiting, so it backs both the single-host
// --host path and the configured-hosts fan-out.
func runRemoteSyncOnce(
	appCfg config.Config, database *db.DB,
	rh config.RemoteHost, full bool,
) error {
	rs := &ssh.RemoteSync{
		Host:                    rh.Host,
		User:                    rh.User,
		Port:                    rh.Port,
		Full:                    full,
		DB:                      database,
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	}
	_, err := rs.Run(context.Background())
	return err
}

// remoteHostFailure records a configured remote host that failed
// to sync. It keeps the full RemoteHost (not just the name) so
// duplicate hostnames that differ by user/port stay distinct.
type remoteHostFailure struct {
	Host config.RemoteHost
	Err  error
}

// runRemoteHosts syncs each configured host in declared order via
// syncFn, continuing past failures, and returns the collected
// failures. It performs no logging so it can be unit-tested
// without capturing the global logger; callers own all output.
func runRemoteHosts(
	hosts []config.RemoteHost, full bool,
	syncFn func(config.RemoteHost, bool) error,
) []remoteHostFailure {
	var failures []remoteHostFailure
	for _, rh := range hosts {
		if err := syncFn(rh, full); err != nil {
			failures = append(failures, remoteHostFailure{
				Host: rh,
				Err:  err,
			})
		}
	}
	return failures
}

// reportRemoteFailures writes per-host failures to the debug log
// and a summary to stderr, so unattended (cron) runs surface them
// even though setupLogFile redirects log output to a file.
func reportRemoteFailures(failures []remoteHostFailure) {
	if len(failures) == 0 {
		return
	}
	for _, f := range failures {
		log.Printf("remote sync %s failed: %v", f.Host.Host, f.Err)
	}
	fmt.Fprintf(os.Stderr,
		"sync: %d remote host(s) failed:\n", len(failures))
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "  %s: %v\n", f.Host.Host, f.Err)
	}
}

// runLocalSync runs a local sync (incremental or full resync).
// It returns true if a full resync was performed, which callers
// can use to force a full PG push (watermarks become stale after
// a local resync).
func runLocalSync(
	appCfg config.Config, database *db.DB, full bool,
) bool {
	for _, def := range parser.Registry {
		if !appCfg.IsUserConfigured(def.Type) {
			continue
		}
		warnMissingDirs(
			appCfg.ResolveDirs(def.Type),
			string(def.Type),
		)
	}

	cleanResyncTemp(appCfg.DBPath)

	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs:               appCfg.AgentDirs,
		Machine:                 "local",
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	})

	didResync := full || database.NeedsResync()
	ctx := context.Background()
	if didResync {
		runInitialResync(ctx, engine)
	} else {
		runInitialSync(ctx, engine)
	}
	engine.PhaseStats().Log("sync")

	fmt.Println()
	stats, err := database.GetStats(
		context.Background(), false, false,
	)
	if err == nil {
		fmt.Printf(
			"Database: %d sessions, %d messages\n",
			stats.SessionCount, stats.MessageCount,
		)
	}
	return didResync
}

func valueOrNever(s string) string {
	if s == "" {
		return "never"
	}
	return s
}
