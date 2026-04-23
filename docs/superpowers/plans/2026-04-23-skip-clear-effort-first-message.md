# Skip /clear and /effort in Claude first_message — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the agentsview sidebar preview the first *real* user message for
Claude sessions that open with `/clear` or `/effort` commands, instead of the
command text.

**Architecture:** Centralize Claude's `first_message` computation in one helper
that filters a hardcoded list of "preview-skipped" command envelopes with a
word-boundary match. Bump `dataVersion` so existing DBs re-parse on next start.
Add a Claude-gated full-parse fallback to `tryIncrementalJSONL` so live sessions
that opened with `/clear` get repaired the next time a real message is appended.

**Tech Stack:** Go (stdlib only for the new logic), SQLite via
`mattn/go-sqlite3`, existing test helpers in `internal/testjsonl` and
`internal/parser`.

______________________________________________________________________

## File Structure

**Modify:**

- `internal/parser/claude.go` — add `previewSkippedCommands`,
  `isSkippablePreviewCommand`, `firstMessageAndUserCount`; replace two inline
  loops (`:519-538` and `:693-706`); add `unicode/utf8` import.
- `internal/parser/claude_parser_test.go` — add unit test for the skip predicate
  and E2E tests for the new behavior.
- `internal/db/db.go` — bump `dataVersion` from 17 to 18 and update the
  explanatory comment.
- `internal/db/sessions.go` — add `FirstMessage string` to `IncrementalInfo`;
  extend `GetSessionForIncremental` to select `first_message`.
- `internal/sync/engine.go` — add Claude-gated full-parse fall-through in
  `tryIncrementalJSONL`.
- `internal/sync/engine_integration_test.go` — add integration test covering the
  full-parse fallback.

No new files. No frontend changes.

______________________________________________________________________

## Task 1: Add `isSkippablePreviewCommand` helper (TDD)

**Files:**

- Modify: `internal/parser/claude.go`

- Test: `internal/parser/claude_parser_test.go`

- [ ] **Step 1.1: Write the failing unit test**

Append to `internal/parser/claude_parser_test.go` (above the
`TestExtractCommandText` block if present, otherwise at the end of the file):

```go
func TestIsSkippablePreviewCommand(t *testing.T) {
    cases := []struct {
        name    string
        content string
        want    bool
    }{
        {"bare /clear", "/clear", true},
        {"bare /effort", "/effort", true},
        {"/clear with trailing space", "/clear ", true},
        {"/clear with args", "/clear foo", true},
        {"/effort with args", "/effort max", true},
        {"surrounded by whitespace", "  /clear  ", true},
        {"/clear with tab", "/clear\tfoo", true},
        {"/clear with newline", "/clear\nfoo", true},
        {"empty string", "", false},
        {"/clearcache (no word boundary)", "/clearcache", false},
        {"/effortless (no word boundary)", "/effortless", false},
        {"/cleareffort", "/cleareffort", false},
        {"unrelated command", "/unrelated", false},
        {"prose containing /clear", "hello /clear", false},
        {"/clear-xyz (dash not whitespace)", "/clear-xyz", false},
        {"plain text", "Fix the login bug", false},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            got := isSkippablePreviewCommand(tc.content)
            assert.Equal(t, tc.want, got,
                "content=%q", tc.content)
        })
    }
}
```

- [ ] **Step 1.2: Run the test to verify it fails (compile error)**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run TestIsSkippablePreviewCommand ./internal/parser/...`

Expected: build failure — `undefined: isSkippablePreviewCommand`.

- [ ] **Step 1.3: Implement the helper**

Open `internal/parser/claude.go`. Update the imports block at lines 5-17 to add
`"unicode/utf8"`. The final import group should read:

```go
import (
    "encoding/json"
    "fmt"
    "log"
    "os"
    "path/filepath"
    "regexp"
    "strings"
    "time"
    "unicode"
    "unicode/utf8"

    "github.com/tidwall/gjson"
)
```

Then, immediately after the `extractCommandText` / `isCommandEnvelope` block
(i.e. just after the closing brace of `isCommandEnvelope` at approximately line
1159), add:

```go
// previewSkippedCommands lists Claude Code commands that should
// not be used as a session's first_message preview. When a
// session opens with one of these, the parser skips past it and
// picks the next real user message so the sidebar shows
// something descriptive.
var previewSkippedCommands = []string{"/clear", "/effort"}

// isSkippablePreviewCommand returns true when content is a known
// Claude Code command (optionally followed by arguments), for the
// purpose of skipping it when computing first_message. Match is
// word-boundary: the trimmed content must equal the command
// exactly or be followed by a whitespace rune, so "/clearcache"
// does not match "/clear".
func isSkippablePreviewCommand(content string) bool {
    trimmed := strings.TrimSpace(content)
    for _, cmd := range previewSkippedCommands {
        if !strings.HasPrefix(trimmed, cmd) {
            continue
        }
        if len(trimmed) == len(cmd) {
            return true
        }
        r, _ := utf8.DecodeRuneInString(trimmed[len(cmd):])
        if unicode.IsSpace(r) {
            return true
        }
    }
    return false
}
```

- [ ] **Step 1.4: Run the test and verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run TestIsSkippablePreviewCommand ./internal/parser/...`

Expected: `PASS` — all subtests pass.

- [ ] **Step 1.5: Commit**

```bash
go fmt ./internal/parser/...
go vet -tags fts5 ./internal/parser/...
git add internal/parser/claude.go internal/parser/claude_parser_test.go
git commit -m "feat(parser): add isSkippablePreviewCommand for Claude first_message"
```

______________________________________________________________________

## Task 2: Extract shared `firstMessageAndUserCount`; skip in parser (TDD)

**Files:**

- Modify: `internal/parser/claude.go:519-538`,
  `internal/parser/claude.go:693-706`

- Test: `internal/parser/claude_parser_test.go`

- [ ] **Step 2.1: Write the failing E2E tests**

Append to `internal/parser/claude_parser_test.go`:

```go
func TestParseClaudeSession_SkipClearEffortFirstMessage(t *testing.T) {
    t.Run("single /clear followed by real message", func(t *testing.T) {
        content := testjsonl.JoinJSONL(
            testjsonl.ClaudeUserJSON(
                "<command-name>/clear</command-name>",
                tsZero,
            ),
            testjsonl.ClaudeUserJSON("Fix the login bug", tsZeroS1),
            testjsonl.ClaudeAssistantJSON([]map[string]any{
                {"type": "text", "text": "ok"},
            }, tsZeroS2),
        )
        sess, _ := runClaudeParserTest(t, "test.jsonl", content)
        assert.Equal(t, "Fix the login bug", sess.FirstMessage)
        assert.Equal(t, 2, sess.UserMessageCount,
            "skipped commands still count as user turns")
    })

    t.Run("cascade /effort then /clear then real", func(t *testing.T) {
        content := testjsonl.JoinJSONL(
            testjsonl.ClaudeUserJSON(
                "<command-name>/effort</command-name>\n<command-args>max</command-args>",
                tsZero,
            ),
            testjsonl.ClaudeUserJSON(
                "<command-name>/clear</command-name>",
                tsZeroS1,
            ),
            testjsonl.ClaudeUserJSON("Real question", tsZeroS2),
        )
        sess, _ := runClaudeParserTest(t, "test.jsonl", content)
        assert.Equal(t, "Real question", sess.FirstMessage)
        assert.Equal(t, 3, sess.UserMessageCount)
    })

    t.Run("all messages are skipped commands", func(t *testing.T) {
        content := testjsonl.JoinJSONL(
            testjsonl.ClaudeUserJSON(
                "<command-name>/clear</command-name>",
                tsZero,
            ),
            testjsonl.ClaudeUserJSON(
                "<command-name>/effort</command-name>",
                tsZeroS1,
            ),
        )
        sess, _ := runClaudeParserTest(t, "test.jsonl", content)
        assert.Equal(t, "", sess.FirstMessage)
        assert.Equal(t, 2, sess.UserMessageCount)
    })

    t.Run("non-skipped command still becomes first_message", func(t *testing.T) {
        content := testjsonl.JoinJSONL(
            testjsonl.ClaudeUserJSON(
                "<command-message>roborev-fix</command-message>\n<command-name>/roborev-fix</command-name>\n<command-args>450</command-args>",
                tsZero,
            ),
            testjsonl.ClaudeUserJSON("follow-up", tsZeroS1),
        )
        sess, _ := runClaudeParserTest(t, "test.jsonl", content)
        assert.Equal(t, "/roborev-fix 450", sess.FirstMessage)
    })
}
```

- [ ] **Step 2.2: Run the tests to verify they fail**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run TestParseClaudeSession_SkipClearEffortFirstMessage ./internal/parser/...`

Expected: the first three subtests FAIL — `sess.FirstMessage` equals the command
text (`/clear` or `/effort max`) instead of the real message / empty string. The
fourth subtest PASSES (control).

- [ ] **Step 2.3: Add the `firstMessageAndUserCount` helper**

Open `internal/parser/claude.go`. Immediately after `isSkippablePreviewCommand`
(added in Task 1), append:

```go
// firstMessageAndUserCount returns the preview string and the
// total number of real (non-system) user turns. The preview skips
// known Claude Code command envelopes like /clear and /effort so
// sessions that begin with a command still show a meaningful
// preview; the user count always reflects every non-system user
// turn, including skipped commands.
func firstMessageAndUserCount(
    messages []ParsedMessage,
) (string, int) {
    firstMsg := ""
    userCount := 0
    for _, m := range messages {
        if m.IsSystem {
            continue
        }
        if m.Role != RoleUser || m.Content == "" {
            continue
        }
        userCount++
        if firstMsg == "" &&
            !isSkippablePreviewCommand(m.Content) {
            firstMsg = truncate(
                strings.ReplaceAll(m.Content, "\n", " "), 300,
            )
        }
    }
    return firstMsg, userCount
}
```

- [ ] **Step 2.4: Replace the linear-path inline loop**

In `internal/parser/claude.go`, replace the block at approximately lines
519-538:

Old:

```go
	userCount := 0
	firstMsg := ""
	for _, m := range messages {
		if m.IsSystem {
			// Promoted system messages (continuation/resume/
			// interrupted/task_notification/stop_hook) carry
			// Role=user so role-keyed analytics ignore them,
			// but they are not real user turns — skip them
			// when computing user_message_count / first_message.
			continue
		}
		if m.Role == RoleUser && m.Content != "" {
			userCount++
			if firstMsg == "" {
				firstMsg = truncate(
					strings.ReplaceAll(m.Content, "\n", " "), 300,
				)
			}
		}
	}
```

New:

```go
	// Promoted system messages (continuation/resume/interrupted/
	// task_notification/stop_hook) carry Role=user so role-keyed
	// analytics ignore them, but they are not real user turns;
	// firstMessageAndUserCount skips them when computing
	// user_message_count / first_message. It also skips leading
	// /clear and /effort command envelopes so the sidebar shows
	// the next real message instead of the command.
	firstMsg, userCount := firstMessageAndUserCount(messages)
```

- [ ] **Step 2.5: Replace the fork-path inline loop**

In `internal/parser/claude.go`, replace the block at approximately lines
692-706:

Old:

```go
		userCount := 0
		firstMsg := ""
		for _, m := range messages {
			if m.IsSystem {
				continue
			}
			if m.Role == RoleUser && m.Content != "" {
				userCount++
				if firstMsg == "" {
					firstMsg = truncate(
						strings.ReplaceAll(m.Content, "\n", " "), 300,
					)
				}
			}
		}
```

New:

```go
		firstMsg, userCount := firstMessageAndUserCount(messages)
```

- [ ] **Step 2.6: Run the new tests and existing Claude tests; verify all pass**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run "TestParseClaudeSession" ./internal/parser/...`

Expected: `PASS` for all four new subtests and all existing
`TestParseClaudeSession_*` tests (including the
`skill invocation shown as user message` case that asserts `/roborev-fix 450` is
the first_message — this must still pass).

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/parser/...`

Expected: `PASS` — the full parser package suite still passes.

- [ ] **Step 2.7: Commit**

```bash
go fmt ./internal/parser/...
go vet -tags fts5 ./internal/parser/...
git add internal/parser/claude.go internal/parser/claude_parser_test.go
git commit -m "feat(parser): skip /clear and /effort when computing Claude first_message"
```

______________________________________________________________________

## Task 3: Bump `dataVersion` to 18

**Files:**

- Modify: `internal/db/db.go:22-39`

- [ ] **Step 3.1: Edit the `dataVersion` constant and its comment**

Open `internal/db/db.go`. Replace lines 22-39:

Old:

```go
// dataVersion tracks parser changes that require a full
// re-sync. Increment this when parsing logic changes in ways
// that affect stored data (e.g. new fields extracted, content
// formatting changes). Old databases with a lower user_version
// trigger a non-destructive re-sync (mtime reset + skip cache
// clear) so existing session data is preserved.
//
// Bumped to 17: Codex parser now filters <skill> template
// injections from the user-message stream. Prior rows had
// inflated user_message_count for sessions where the model
// invoked a skill (Codex writes the skill template content as a
// role=user JSONL entry), which prevented IsAutomatedSession
// from gating them on the single-turn requirement. Re-parsing
// rewrites user_message_count so the is_automated backfill can
// classify them correctly.
//
// (16: same migration for <turn_aborted> system messages.)
const dataVersion = 17
```

New:

```go
// dataVersion tracks parser changes that require a full
// re-sync. Increment this when parsing logic changes in ways
// that affect stored data (e.g. new fields extracted, content
// formatting changes). Old databases with a lower user_version
// trigger a non-destructive re-sync (mtime reset + skip cache
// clear) so existing session data is preserved.
//
// Bumped to 18: Claude parser now skips /clear and /effort
// command envelopes when computing first_message, so sessions
// that opened with one of those commands show the next real
// user message in the sidebar instead of the command text.
// Re-parsing rewrites first_message with the new logic.
//
// (17: Codex <skill> template filtering.)
// (16: <turn_aborted> system messages.)
const dataVersion = 18
```

- [ ] **Step 3.2: Run the DB test suite to confirm nothing regressed**

Run: `CGO_ENABLED=1 go test -tags fts5 -short ./internal/db/...`

Expected: `PASS`.

- [ ] **Step 3.3: Commit**

```bash
git add internal/db/db.go
git commit -m "chore(db): bump dataVersion to 18 for Claude first_message skip logic"
```

______________________________________________________________________

## Task 4: Add `FirstMessage` to `IncrementalInfo`

**Files:**

- Modify: `internal/db/sessions.go` (around lines 1000-1070)

- [ ] **Step 4.1: Extend the `IncrementalInfo` struct**

Open `internal/db/sessions.go`. Replace the existing struct (lines 1000-1014):

Old:

```go
// IncrementalInfo holds the data needed for incremental
// re-parsing of an append-only session file.
type IncrementalInfo struct {
	ID                   string
	FileSize             int64
	FileMtime            int64
	FileInode            int64
	FileDevice           int64
	MsgCount             int
	UserMsgCount         int
	TotalOutputTokens    int
	PeakContextTokens    int
	HasTotalOutputTokens bool
	HasPeakContextTokens bool
}
```

New:

```go
// IncrementalInfo holds the data needed for incremental
// re-parsing of an append-only session file. FirstMessage is
// the currently stored preview text; the sync engine uses it to
// decide whether the Claude parser's skip-command path has left
// the preview empty and a full parse should be forced.
type IncrementalInfo struct {
	ID                   string
	FileSize             int64
	FileMtime            int64
	FileInode            int64
	FileDevice           int64
	MsgCount             int
	UserMsgCount         int
	FirstMessage         string
	TotalOutputTokens    int
	PeakContextTokens    int
	HasTotalOutputTokens bool
	HasPeakContextTokens bool
}
```

- [ ] **Step 4.2: Extend `GetSessionForIncremental` to select `first_message`**

In the same file, replace the SELECT/Scan block inside
`GetSessionForIncremental` (lines 1035-1053):

Old:

```go
	var info IncrementalInfo
	var fs, fm, fi, fd sql.NullInt64
	err = db.getReader().QueryRow(
		`SELECT id, file_size, file_mtime,
			file_inode, file_device,
			message_count, user_message_count,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		 FROM sessions WHERE file_path = ?`,
		path,
	).Scan(
		&info.ID, &fs, &fm, &fi, &fd,
		&info.MsgCount, &info.UserMsgCount,
		&info.TotalOutputTokens, &info.PeakContextTokens,
		&info.HasTotalOutputTokens, &info.HasPeakContextTokens,
	)
	if err != nil {
		return nil, false
	}
```

New:

```go
	var info IncrementalInfo
	var fs, fm, fi, fd sql.NullInt64
	var firstMsg sql.NullString
	err = db.getReader().QueryRow(
		`SELECT id, file_size, file_mtime,
			file_inode, file_device,
			message_count, user_message_count,
			first_message,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		 FROM sessions WHERE file_path = ?`,
		path,
	).Scan(
		&info.ID, &fs, &fm, &fi, &fd,
		&info.MsgCount, &info.UserMsgCount,
		&firstMsg,
		&info.TotalOutputTokens, &info.PeakContextTokens,
		&info.HasTotalOutputTokens, &info.HasPeakContextTokens,
	)
	if err != nil {
		return nil, false
	}
	if firstMsg.Valid {
		info.FirstMessage = firstMsg.String
	}
```

- [ ] **Step 4.3: Verify the package still builds and tests pass**

Run:
`CGO_ENABLED=1 go test -tags fts5 -short ./internal/db/... ./internal/sync/...`

Expected: `PASS`. No existing code constructs `IncrementalInfo` as a struct
literal — the type is populated field-by-field only in
`GetSessionForIncremental` — so adding the field is a non-breaking change.

- [ ] **Step 4.4: Commit**

```bash
go fmt ./internal/db/...
go vet -tags fts5 ./internal/db/...
git add internal/db/sessions.go
git commit -m "refactor(db): add FirstMessage to IncrementalInfo"
```

______________________________________________________________________

## Task 5: Force full parse when Claude `first_message` is empty (TDD)

**Files:**

- Modify: `internal/sync/engine.go` (around lines 2168-2214)

- Test: `internal/sync/engine_integration_test.go` (append to end)

- [ ] **Step 5.1: Write the failing integration test**

Append to `internal/sync/engine_integration_test.go`:

```go
func TestIncrementalSync_ClaudeClearOnlyRepairedOnAppend(t *testing.T) {
    env := setupTestEnv(t)

    // Initial sync: session opens with only a /clear command
    // envelope. Under the new parser rule, first_message is
    // empty even though UserMsgCount is 1.
    initial := testjsonl.JoinJSONL(
        testjsonl.ClaudeUserJSON(
            "<command-name>/clear</command-name>",
            tsZero,
        ),
    )
    path := env.writeClaudeSession(
        t, "proj", "clear-only.jsonl", initial,
    )
    env.engine.SyncAll(context.Background(), nil)

    full, err := env.db.GetSessionFull(
        context.Background(), "clear-only",
    )
    if err != nil {
        t.Fatalf("GetSessionFull after initial sync: %v", err)
    }
    if full.FirstMessage != nil && *full.FirstMessage != "" {
        t.Fatalf(
            "initial FirstMessage = %q, want empty",
            *full.FirstMessage,
        )
    }
    if full.UserMessageCount != 1 {
        t.Fatalf(
            "initial UserMessageCount = %d, want 1",
            full.UserMessageCount,
        )
    }

    // Append a real user message — incremental sync must now
    // fall back to a full parse so first_message gets populated.
    appended := testjsonl.ClaudeUserJSON(
        "Fix the login bug", tsZeroS1,
    ) + "\n"
    f, err := os.OpenFile(
        path, os.O_APPEND|os.O_WRONLY, 0o644,
    )
    if err != nil {
        t.Fatalf("open for append: %v", err)
    }
    _, err = f.WriteString(appended)
    f.Close()
    if err != nil {
        t.Fatalf("append: %v", err)
    }

    env.engine.SyncPaths([]string{path})

    updated, err := env.db.GetSessionFull(
        context.Background(), "clear-only",
    )
    if err != nil {
        t.Fatalf("GetSessionFull after append: %v", err)
    }
    if updated.FirstMessage == nil ||
        *updated.FirstMessage != "Fix the login bug" {
        got := ""
        if updated.FirstMessage != nil {
            got = *updated.FirstMessage
        }
        t.Errorf(
            "FirstMessage after append = %q, want %q",
            got, "Fix the login bug",
        )
    }
    if updated.UserMessageCount != 2 {
        t.Errorf(
            "UserMessageCount after append = %d, want 2",
            updated.UserMessageCount,
        )
    }
}
```

- [ ] **Step 5.2: Run the new test and verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run TestIncrementalSync_ClaudeClearOnlyRepairedOnAppend ./internal/sync/...`

Expected: `FAIL` — after the append, `FirstMessage` is still empty because the
incremental path doesn't rewrite it.

- [ ] **Step 5.3: Add the Claude-gated fall-through**

Open `internal/sync/engine.go`. Locate `tryIncrementalJSONL` (starts around line
2168). Immediately after the existing data-version fall-through block at lines
2186-2189:

```go
	// Existing rows from an older parser lack new metadata
	// columns. Force a full parse so the rewrite picks them
	// up rather than appending new rows on top of stale ones.
	if e.db.GetSessionDataVersion(inc.ID) <
		db.CurrentDataVersion() {
		return processResult{}, false
	}
```

…insert a new block:

```go
	// Claude-only: if the stored preview is empty despite the
	// session already having user turns, the parser skipped
	// every user message so far (e.g. a session that opens with
	// /clear or /effort). Fall back to a full parse so any real
	// user message appended this sync becomes first_message.
	//
	// Other agents can legitimately have UserMsgCount > 0 with
	// an empty first_message — for example Codex inserts orphan
	// subagent notifications as Role=user messages that bypass
	// firstMessage — so this fall-through is gated on Claude.
	if agent == parser.AgentClaude &&
		inc.FirstMessage == "" && inc.UserMsgCount > 0 {
		return processResult{}, false
	}
```

- [ ] **Step 5.4: Run the test and verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run TestIncrementalSync_ClaudeClearOnlyRepairedOnAppend ./internal/sync/...`

Expected: `PASS`.

- [ ] **Step 5.5: Run related sync integration tests to confirm nothing
  regressed**

Run:
`CGO_ENABLED=1 go test -tags fts5 -run "TestIncrementalSync" ./internal/sync/...`

Expected: `PASS` for all `TestIncrementalSync_*` tests — in particular
`TestIncrementalSync_ClaudeAppend`, `TestIncrementalSync_CodexAppend`, and
`TestIncrementalSync_CodexSubagentAppendFallsBackToFullParse` must still pass,
confirming the Claude-only gate doesn't spuriously trigger for Codex.

- [ ] **Step 5.6: Commit**

```bash
go fmt ./internal/sync/...
go vet -tags fts5 ./internal/sync/...
git add internal/sync/engine.go internal/sync/engine_integration_test.go
git commit -m "feat(sync): force full parse when Claude first_message is empty with user turns"
```

______________________________________________________________________

## Task 6: Full verification

**Files:** none modified; this is a verification-only task.

- [ ] **Step 6.1: Run the full Go test suite**

Run: `make test`

Expected: `PASS`. If any test fails, diagnose and fix before proceeding.

- [ ] **Step 6.2: Run the linter**

Run: `make lint`

Expected: no findings.

- [ ] **Step 6.3: Run vet**

Run: `make vet`

Expected: no findings.

- [ ] **Step 6.4: Manual smoke check (optional, skip if no dev DB available)**

Start the server against a local DB that contains pre-existing sessions:

Run: `make install && ~/.local/bin/agentsview`

Expected log line on startup:
`... migration: re-sync needed (dataVersion 17 → 18) ...` (or similar — the
exact wording comes from the existing migration path). Open the sidebar and
confirm sessions whose first message was `/clear` or `/effort max` now show the
next real message. If no eligible sessions exist, rely on Task 2/5 tests.

- [ ] **Step 6.5: Remove the spec and plan before pushing**

Per the project's convention (memory `feedback_remove_specs_before_push`), the
`docs/superpowers/specs/` and `docs/superpowers/plans/` files are internal
scratch and should not land on `main`. When the user is ready to open a PR:

```bash
git rm docs/superpowers/specs/2026-04-23-skip-clear-effort-first-message-design.md \
       docs/superpowers/plans/2026-04-23-skip-clear-effort-first-message.md
# If the docs/superpowers/ directory tree has nothing left, that's fine —
# git rm will leave it to be cleaned up naturally.
git commit -m "chore: remove internal design and plan scratch files"
```

Do not push without confirming with the user first.

______________________________________________________________________

## Notes

- **No frontend changes.** The sidebar, pinned page, trash page, command
  palette, and analytics top-sessions view all already read
  `session.first_message`; changing the stored value is sufficient.
- **No custom backfill.** The `dataVersion` bump in Task 3 triggers the existing
  non-destructive re-sync path, which re-parses all sessions with the new logic
  on next start.
- **Claude-only scope.** `/clear` and `/effort` are Claude Code commands; other
  agents' parsers (Codex, Cursor, iFlow, etc.) are untouched. The incremental
  fall-through in Task 5 is gated on `parser.AgentClaude` to avoid spurious full
  parses for Codex sessions that legitimately have `UserMsgCount > 0` with an
  empty `first_message` (orphan subagent notifications inserted via
  `insertMessage` in `internal/parser/codex.go:451`).
