// ABOUTME: CLI subcommand that returns token usage data for a
// ABOUTME: session, syncing on-demand if no server is running.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/parser"
	"github.com/wesm/agentsview/internal/server"
	"github.com/wesm/agentsview/internal/sync"
)

// Exit codes for the token-use subcommand.
const (
	tokenUseExitOK            = 0
	tokenUseExitErr           = 1
	tokenUseExitNotFound      = 2
	tokenUseExitNoTokenData   = 3
	tokenUseResolveMatchLimit = 2
)

// resolveRawSessionID translates a user-supplied session ID into
// the canonical form stored in sessions.id. Callers may pass
// either a canonical ID ("codex:<uuid>") or a bare raw ID as
// emitted by the underlying agent — including raw IDs that
// themselves contain colons (Kimi: "<project-hash>:<session-uuid>",
// OpenClaw: "<agentId>:<sessionId>", legacy Kiro IDE).
//
// Resolution order (short-circuit only on host-prefixed IDs, which
// are unambiguously remote; any other input — even one that begins
// with a registered prefix — flows through DB and disk probes
// because the first colon-delimited component can legitimately be
// part of a raw ID):
//
//  1. Host-prefixed input -> returned unchanged.
//  2. DB lookup: exact row (if any) sorts ahead of suffix matches
//     in SQL; suffix matches come back in most-recent order. If
//     multiple suffix matches exist without an exact row, the
//     most recent wins and an ambiguity warning is emitted.
//  3. Canonical disk probe: when input begins with a registered
//     agent prefix, strip the prefix and call that agent's
//     FindSourceFunc so a truly canonical-but-unsynced ID on disk
//     still resolves.
//  4. Raw disk probe: call every file-based agent's FindSourceFunc
//     with the raw input; the first hit yields "<prefix><input>".
//  5. No match anywhere: returned unchanged with known=false.
//
// known reports whether resolution found evidence for the ID.
// When false, the caller should skip on-demand sync because it
// cannot produce meaningful output.
func resolveRawSessionID(
	ctx context.Context,
	database *db.DB,
	agentDirs map[parser.AgentType][]string,
	input string,
) (resolved string, known bool) {
	if host, _ := parser.StripHostPrefix(input); host != "" {
		return input, true
	}

	matches, err := database.FindSessionIDsByRawSuffix(
		ctx, input, tokenUseResolveMatchLimit,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: session id lookup failed: %v\n", err)
	}
	if len(matches) > 0 {
		if matches[0] == input {
			return input, true
		}
		if len(matches) > 1 {
			fmt.Fprintf(os.Stderr,
				"warning: ambiguous session id %q matches "+
					"multiple sessions, using most recent (%s)\n",
				input, matches[0],
			)
		}
		return matches[0], true
	}

	// Canonical disk probe: if the input starts with a known
	// agent prefix, trust that interpretation first and strip
	// before calling FindSourceFunc (which rejects IDs with
	// colons via IsValidSessionID).
	for _, def := range parser.Registry {
		if def.IDPrefix == "" || !def.FileBased ||
			def.FindSourceFunc == nil {
			continue
		}
		if !strings.HasPrefix(input, def.IDPrefix) {
			continue
		}
		bareID := strings.TrimPrefix(input, def.IDPrefix)
		for _, dir := range agentDirs[def.Type] {
			if def.FindSourceFunc(dir, bareID) != "" {
				return input, true
			}
		}
	}

	// Raw disk probe: treat input as a raw agent ID. Agents
	// whose raw IDs cannot contain ':' (most of them) reject
	// the input via IsValidSessionID; agents that accept
	// colon-bearing raw IDs (Kimi, OpenClaw, Kiro IDE) may
	// match.
	for _, def := range parser.Registry {
		if !def.FileBased || def.FindSourceFunc == nil {
			continue
		}
		for _, dir := range agentDirs[def.Type] {
			if def.FindSourceFunc(dir, input) != "" {
				return def.IDPrefix + input, true
			}
		}
	}

	return input, false
}

// tokenUseExitCode classifies a session record into an exit code:
// 0 when token metrics are present, 2 when the session is not in
// the DB, and 3 when the session exists but has no token data
// yet (e.g. the parser hasn't ingested it or the agent never
// emitted usage metadata).
func tokenUseExitCode(sess *db.Session) int {
	if sess == nil {
		return tokenUseExitNotFound
	}
	if sess.HasTotalOutputTokens || sess.HasPeakContextTokens {
		return tokenUseExitOK
	}
	return tokenUseExitNoTokenData
}

// tokenUseOutput is the JSON structure written to stdout.
// This format is experimental and may change.
type tokenUseOutput struct {
	SessionID         string `json:"session_id"`
	Agent             string `json:"agent"`
	Project           string `json:"project"`
	TotalOutputTokens int    `json:"total_output_tokens"`
	PeakContextTokens int    `json:"peak_context_tokens"`
	HasTokenData      bool   `json:"has_token_data"`
	ServerRunning     bool   `json:"server_running"`
}

// startupWaitTimeout is how long token-use will wait for a
// starting server to become ready before falling back to
// on-demand sync.
const startupWaitTimeout = 30 * time.Second

func runTokenUse(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr,
			"usage: agentsview token-use <session-id>")
		os.Exit(tokenUseExitErr)
	}

	code, err := tokenUse(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(tokenUseExitErr)
	}
	os.Exit(code)
}

func tokenUse(sessionID string) (int, error) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return tokenUseExitErr, fmt.Errorf("loading config: %w", err)
	}

	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return tokenUseExitErr,
			fmt.Errorf("creating data dir: %w", err)
	}

	serverActive := server.IsServerActive(appCfg.DataDir)

	// If a server is actively starting up (startup lock
	// present), wait for it to finish so we read fresh data
	// rather than returning stale results or "not found".
	// We only wait when the startup lock is the reason
	// IsServerActive returned true — if a state file has a
	// live PID but the TCP probe is transiently failing,
	// the server is running and we should just read the DB.
	if serverActive &&
		server.FindRunningServer(appCfg.DataDir) == nil {
		if server.IsStartupLocked(appCfg.DataDir) {
			fmt.Fprintf(os.Stderr,
				"server is starting up, waiting...\n")
			if !server.WaitForStartup(
				appCfg.DataDir, startupWaitTimeout,
			) {
				if server.IsStartupLocked(appCfg.DataDir) {
					// Lock still live after timeout:
					// the server is active (still
					// syncing, or state file write
					// failed). Don't compete — read
					// the DB as-is.
					fmt.Fprintf(os.Stderr,
						"server still starting after "+
							"%s, reading DB as-is\n",
						startupWaitTimeout,
					)
				} else {
					// Lock cleared but no running
					// server. Re-check in case of
					// transient TCP failure.
					serverActive = server.IsServerActive(
						appCfg.DataDir,
					)
				}
			}
		} else if !server.IsServerActive(appCfg.DataDir) {
			// The server that was alive at the first check
			// has since exited. Fall back to on-demand sync.
			serverActive = false
		}
	}

	database, err := db.Open(appCfg.DBPath)
	if err != nil {
		return tokenUseExitErr,
			fmt.Errorf("opening database: %w", err)
	}
	defer database.Close()

	if appCfg.CursorSecret != "" {
		secret, decErr := base64.StdEncoding.DecodeString(
			appCfg.CursorSecret,
		)
		if decErr != nil {
			return tokenUseExitErr, fmt.Errorf(
				"invalid cursor secret: %w", decErr,
			)
		}
		database.SetCursorSecret(secret)
	}

	ctx := context.Background()
	resolvedID, known := resolveRawSessionID(
		ctx, database, appCfg.AgentDirs, sessionID,
	)

	// If no server is managing the DB, do an on-demand sync
	// for this session so the data is fresh. Re-check right
	// before syncing to close the TOCTOU window where a
	// server could have started since our initial probe.
	// If the re-check detects a starting server, wait for
	// it rather than reading potentially stale data.
	if !serverActive {
		serverActive = server.IsServerActive(appCfg.DataDir)
		if serverActive &&
			server.FindRunningServer(appCfg.DataDir) == nil &&
			server.IsStartupLocked(appCfg.DataDir) {
			fmt.Fprintf(os.Stderr,
				"server is starting up, waiting...\n")
			if server.WaitForStartup(
				appCfg.DataDir, startupWaitTimeout,
			) {
				// Server is ready; read DB below.
			} else if !server.IsStartupLocked(
				appCfg.DataDir,
			) {
				// Lock cleared, no running server
				// via TCP. Re-check: a live state
				// file (transient probe failure)
				// still means the server is active.
				serverActive = server.IsServerActive(
					appCfg.DataDir,
				)
			}
			// Lock still live after timeout: server is
			// active but slow. Read DB as-is.
		}
	}
	// Skip sync entirely when we have no evidence of the
	// session (known=false) — SyncSingleSession would just
	// log a misleading "source file not found" warning.
	if !serverActive && known {
		engine := sync.NewEngine(database, sync.EngineConfig{
			AgentDirs:               appCfg.AgentDirs,
			Machine:                 "local",
			BlockedResultCategories: appCfg.ResultContentBlockedCategories,
		})
		if syncErr := engine.SyncSingleSession(
			resolvedID,
		); syncErr != nil {
			// Not fatal: session may already be in the DB
			// from a previous sync, or may not exist at all.
			fmt.Fprintf(os.Stderr,
				"warning: sync failed: %v\n", syncErr)
		}
	}

	sess, err := database.GetSession(ctx, resolvedID)
	if err != nil {
		return tokenUseExitErr,
			fmt.Errorf("querying session: %w", err)
	}
	if sess == nil {
		fmt.Fprintf(os.Stderr,
			"session not found: %s\n", sessionID)
		return tokenUseExitNotFound, nil
	}

	agent := sess.Agent
	if agent == "" {
		if def, ok := parser.AgentByPrefix(sess.ID); ok {
			agent = string(def.Type)
		}
	}

	out := tokenUseOutput{
		SessionID:         sess.ID,
		Agent:             agent,
		Project:           sess.Project,
		TotalOutputTokens: sess.TotalOutputTokens,
		PeakContextTokens: sess.PeakContextTokens,
		HasTokenData: sess.HasTotalOutputTokens ||
			sess.HasPeakContextTokens,
		ServerRunning: serverActive,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return tokenUseExitErr, err
	}
	return tokenUseExitCode(sess), nil
}
