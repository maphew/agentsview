package parser

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
)

// TestExtractProjectFromCwd_AutofsUnresolved_SkipsWalk verifies
// that a cwd under an autofs prefix whose first component does
// not resolve locally (the classic "Linux cwd on a macOS box"
// case) pays for exactly one stat — the probe — and skips the
// full git-root walk. Without the skip, statting every ancestor
// hammers automountd/opendirectoryd via /usr/libexec/od_user_homes.
func TestExtractProjectFromCwd_AutofsUnresolved_SkipsWalk(t *testing.T) {
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = []string{"/home/"}
	resetAutofsProbes()

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		return nil, os.ErrNotExist
	}

	cwd := "/home/wes/code/example-project"
	want := "example_project"
	got := ExtractProjectFromCwdWithBranch(cwd, "")
	if got != want {
		t.Errorf("ExtractProjectFromCwdWithBranch(%q) = %q, want %q",
			cwd, got, want)
	}
	if n := count.Load(); n != 1 {
		t.Errorf("osStat called %d times for unresolved autofs cwd "+
			"%q; expected 1 (probe only, walk skipped)", n, cwd)
	}
}

// TestExtractProjectFromCwd_AutofsUnresolved_ProbeCached checks
// that a bulk sync with many cwds under the same autofs first
// component pays for only one stat across the batch.
func TestExtractProjectFromCwd_AutofsUnresolved_ProbeCached(t *testing.T) {
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = []string{"/home/"}
	resetAutofsProbes()

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		return nil, os.ErrNotExist
	}

	for _, cwd := range []string{
		"/home/wes/code/proj-a",
		"/home/wes/code/proj-b",
		"/home/wes/code/nested/proj-c/src",
	} {
		_ = ExtractProjectFromCwdWithBranch(cwd, "")
	}
	if n := count.Load(); n != 1 {
		t.Errorf("osStat called %d times across 3 cwds sharing "+
			"/home/wes; expected 1 (cached probe)", n)
	}
}

// TestExtractProjectFromCwd_AutofsResolved_Walks is the regression
// guard for the reviewer's concern: enterprise hosts where an
// autofs prefix has a real backing (e.g. NFS-mounted /home) must
// still resolve projects to their repository roots. A resolving
// probe lets the git-root walk proceed normally.
func TestExtractProjectFromCwd_AutofsResolved_Walks(t *testing.T) {
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = []string{"/home/"}
	resetAutofsProbes()

	// Use a real directory's FileInfo so IsDir() answers true
	// when the probe "resolves".
	realDir := t.TempDir()
	realInfo, err := os.Stat(realDir)
	if err != nil {
		t.Fatal(err)
	}

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		if path == "/home/wes" {
			return realInfo, nil
		}
		return nil, os.ErrNotExist
	}

	cwd := "/home/wes/code/example"
	_ = ExtractProjectFromCwdWithBranch(cwd, "")
	if n := count.Load(); n < 2 {
		t.Errorf("with resolving autofs probe, expected the walk "+
			"to stat multiple paths (probe + walk); got %d", n)
	}
}

// TestExtractProjectFromCwd_NativePath_StillWalks confirms that
// paths outside any autofs-managed prefix still trigger the
// git-root walk.
func TestExtractProjectFromCwd_NativePath_StillWalks(t *testing.T) {
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = []string{"/home/"}

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		return orig(path)
	}

	cwd := "/Users/nobody-agentsview-test/code/example"
	_ = ExtractProjectFromCwdWithBranch(cwd, "")
	if count.Load() == 0 {
		t.Errorf("osStat never called for %q; "+
			"git-root walk should run for non-autofs paths", cwd)
	}
}

// TestExtractProjectFromCwd_HomePathWithoutAutofs_StillWalks covers
// the edge case flagged in review: a user with a real filesystem
// mounted at /home (no autofs entry) should still get git-root
// resolution, not a basename-only fallback.
func TestExtractProjectFromCwd_HomePathWithoutAutofs_StillWalks(t *testing.T) {
	origPrefixes := autofsPrefixes
	defer func() { autofsPrefixes = origPrefixes }()
	autofsPrefixes = nil

	orig := osStat
	defer func() { osStat = orig }()
	var count atomic.Int64
	osStat = func(path string) (os.FileInfo, error) {
		count.Add(1)
		return orig(path)
	}

	cwd := "/home/nobody-agentsview-test/code/example"
	_ = ExtractProjectFromCwdWithBranch(cwd, "")
	if count.Load() == 0 {
		t.Errorf("osStat never called for /home path with empty " +
			"autofs config; walk must proceed for a real mount")
	}
}

// TestDetectAutofsPrefixes verifies that /etc/auto_master is parsed
// into the prefix set. Only darwin is expected to populate this;
// other platforms return an empty list.
func TestDetectAutofsPrefixes(t *testing.T) {
	origPath := autoMasterPath
	defer func() { autoMasterPath = origPath }()

	tmp := t.TempDir()
	fixture := filepath.Join(tmp, "auto_master")
	content := "#\n" +
		"# Automounter master map\n" +
		"#\n" +
		"+auto_master\n" +
		"#/net           -hosts    -nobrowse\n" +
		"/home           auto_home -nobrowse,hidefromfinder\n" +
		"/Network/Servers -fstab\n" +
		"/-              -static\n"
	if err := os.WriteFile(fixture, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	autoMasterPath = fixture

	got := detectAutofsPrefixes()
	if runtime.GOOS != "darwin" {
		if got != nil {
			t.Errorf("detectAutofsPrefixes() = %v on %s, want nil",
				got, runtime.GOOS)
		}
		return
	}

	want := []string{"/home/", "/Network/Servers/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("detectAutofsPrefixes() = %v, want %v", got, want)
	}
}

// TestDetectAutofsPrefixes_MissingFile confirms that a missing
// auto_master file (unusual but possible) yields an empty list
// rather than crashing.
func TestDetectAutofsPrefixes_MissingFile(t *testing.T) {
	origPath := autoMasterPath
	defer func() { autoMasterPath = origPath }()
	autoMasterPath = filepath.Join(t.TempDir(), "does-not-exist")

	if got := detectAutofsPrefixes(); got != nil {
		t.Errorf("detectAutofsPrefixes() with missing file = %v, want nil",
			got)
	}
}
