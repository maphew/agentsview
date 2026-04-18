package parser

import (
	"os"
	"runtime"
	"sync/atomic"
	"testing"
)

// TestExtractProjectFromCwd_ForeignPath_SkipsStatWalk verifies that
// a cwd using a path convention foreign to the running OS does not
// trigger any filesystem stat calls. On macOS, statting under /home
// fires autofs, which cascades into opendirectoryd/automountd
// lookups via /usr/libexec/od_user_homes. At bulk-remote-sync scale
// this pegs both daemons at 100s of % CPU, so the git-root walk
// must be skipped for paths that can't correspond to a local
// filesystem location anyway.
func TestExtractProjectFromCwd_ForeignPath_SkipsStatWalk(t *testing.T) {
	var cwd, want string
	switch runtime.GOOS {
	case "darwin":
		cwd = "/home/wes/code/example-project"
		want = "example_project"
	case "linux":
		cwd = "/Users/wes/code/example-project"
		want = "example_project"
	default:
		t.Skipf("test only applies to darwin/linux, got %s",
			runtime.GOOS)
	}

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		return orig(path)
	}

	got := ExtractProjectFromCwdWithBranch(cwd, "")
	if got != want {
		t.Errorf("ExtractProjectFromCwdWithBranch(%q) = %q, want %q",
			cwd, got, want)
	}
	if n := count.Load(); n != 0 {
		t.Errorf("osStat called %d times for foreign cwd %q; "+
			"expected 0 (git-root walk should be skipped)",
			n, cwd)
	}
}

// TestExtractProjectFromCwd_NativePath_StillWalks confirms the
// skip is path-specific: a native-prefix cwd still triggers the
// git-root walk so ExtractProjectFromCwd can find real repos.
func TestExtractProjectFromCwd_NativePath_StillWalks(t *testing.T) {
	var cwd string
	switch runtime.GOOS {
	case "darwin":
		cwd = "/Users/nobody-agentsview-test/code/example"
	case "linux":
		cwd = "/home/nobody-agentsview-test/code/example"
	default:
		t.Skipf("test only applies to darwin/linux, got %s",
			runtime.GOOS)
	}

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		return orig(path)
	}

	_ = ExtractProjectFromCwdWithBranch(cwd, "")
	if count.Load() == 0 {
		t.Errorf("osStat never called for native cwd %q; "+
			"git-root walk should run for local-style paths",
			cwd)
	}
}
