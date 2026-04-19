package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// writeGhStub drops a shell script named "gh" into dir that picks its canned
// response based on whether the args mention `created:` (opened query) or
// `merged:` (merged query). Returns the stub's directory so callers can
// prepend it to PATH. Tests are skipped cleanly on Windows (no /bin/sh) and
// when `sh` isn't on PATH.
func writeGhStub(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script mock unsupported on windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available on PATH: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
	// On some filesystems WriteFile's mode is masked; ensure +x explicitly.
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod gh stub: %v", err)
	}
	return dir
}

// prependPath puts dir at the front of PATH for the duration of the test.
func prependPath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAggregatePRs_CountsBothQueries(t *testing.T) {
	stub := `#!/bin/sh
case "$*" in
    *created:*) echo '[{"state":"OPEN"},{"state":"MERGED"},{"state":"OPEN"}]' ;;
    *merged:*)  echo '[{"state":"MERGED"},{"state":"MERGED"}]' ;;
    *)          echo '[]' ;;
esac
`
	dir := writeGhStub(t, stub)
	prependPath(t, dir)

	got, err := AggregatePRs(
		context.Background(),
		t.TempDir(),
		"2026-01-01", "2026-02-01",
		"fake-token",
	)
	if err != nil {
		t.Fatalf("AggregatePRs: %v", err)
	}
	if got == nil {
		t.Fatalf("AggregatePRs returned nil result with non-empty token")
	}
	want := PRResult{Opened: 3, Merged: 2}
	if *got != want {
		t.Fatalf("AggregatePRs = %+v, want %+v", *got, want)
	}
}

func TestAggregatePRs_EmptyTokenShortCircuits(t *testing.T) {
	// Install a stub that would fail if invoked, to prove we never exec it.
	stub := `#!/bin/sh
echo "stub must not run" >&2
exit 97
`
	dir := writeGhStub(t, stub)
	prependPath(t, dir)

	got, err := AggregatePRs(
		context.Background(),
		t.TempDir(),
		"2026-01-01", "2026-02-01",
		"",
	)
	if err != nil {
		t.Fatalf("AggregatePRs (empty token): %v", err)
	}
	if got != nil {
		t.Fatalf("AggregatePRs (empty token) = %+v, want nil", got)
	}
}

func TestAggregatePRs_ExecFailurePropagates(t *testing.T) {
	stub := `#!/bin/sh
echo "boom" >&2
exit 1
`
	dir := writeGhStub(t, stub)
	prependPath(t, dir)

	_, err := AggregatePRs(
		context.Background(),
		t.TempDir(),
		"2026-01-01", "2026-02-01",
		"fake-token",
	)
	if err == nil {
		t.Fatalf("AggregatePRs: expected error from failing gh, got nil")
	}
}

func TestAggregatePRs_EmptyArrayCountsZero(t *testing.T) {
	stub := `#!/bin/sh
echo '[]'
`
	dir := writeGhStub(t, stub)
	prependPath(t, dir)

	got, err := AggregatePRs(
		context.Background(),
		t.TempDir(),
		"2026-01-01", "2026-02-01",
		"fake-token",
	)
	if err != nil {
		t.Fatalf("AggregatePRs: %v", err)
	}
	if got == nil {
		t.Fatalf("AggregatePRs returned nil with non-empty token")
	}
	want := PRResult{Opened: 0, Merged: 0}
	if *got != want {
		t.Fatalf("AggregatePRs = %+v, want %+v", *got, want)
	}
}

func TestAggregatePRs_BadJSONIsError(t *testing.T) {
	stub := `#!/bin/sh
echo 'not json at all'
`
	dir := writeGhStub(t, stub)
	prependPath(t, dir)

	_, err := AggregatePRs(
		context.Background(),
		t.TempDir(),
		"2026-01-01", "2026-02-01",
		"fake-token",
	)
	if err == nil {
		t.Fatalf("AggregatePRs: expected parse error, got nil")
	}
}

func TestAggregatePRs_InjectsGHTokenIntoEnv(t *testing.T) {
	// Stub writes GH_TOKEN to a side-channel file so we can verify it was
	// injected into the exec environment (and not leaked via argv, which
	// this test doesn't check directly but the implementation doesn't
	// construct argv with the token).
	sideChannel := filepath.Join(t.TempDir(), "token.txt")
	stub := `#!/bin/sh
printf '%s' "$GH_TOKEN" > ` + sideChannel + `
echo '[]'
`
	dir := writeGhStub(t, stub)
	prependPath(t, dir)

	_, err := AggregatePRs(
		context.Background(),
		t.TempDir(),
		"2026-01-01", "2026-02-01",
		"injected-token-123",
	)
	if err != nil {
		t.Fatalf("AggregatePRs: %v", err)
	}
	got, err := os.ReadFile(sideChannel)
	if err != nil {
		t.Fatalf("read side channel: %v", err)
	}
	if string(got) != "injected-token-123" {
		t.Fatalf("GH_TOKEN in env = %q, want %q", got, "injected-token-123")
	}
}
