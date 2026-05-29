package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tarEntry describes one entry to write into a test tar archive.
type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
}

// buildTestTar serializes entries into an in-memory tar archive.
func buildTestTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Mode:     0o644,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if e.typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func extract(t *testing.T, data []byte, dst string) (int, error) {
	t.Helper()
	return extractTarStream(context.Background(), bytes.NewReader(data), dst)
}

func TestExtractTarStreamSkipsSelfHardlink(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/wes/good.txt", typeflag: tar.TypeReg, body: "hello"},
		// Self-referential hardlink: the Antigravity case bsdtar
		// reports as "hardlink pointing to itself".
		{
			name:     "home/wes/loop.jsonl",
			typeflag: tar.TypeLink,
			linkname: "home/wes/loop.jsonl",
		},
		{name: "home/wes/after.txt", typeflag: tar.TypeReg, body: "world"},
	})

	skipped, err := extract(t, data, dst)
	require.NoError(t, err)
	assert.Equal(t, 1, skipped)

	good, err := os.ReadFile(filepath.Join(dst, "home/wes/good.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(good))
	after, err := os.ReadFile(filepath.Join(dst, "home/wes/after.txt"))
	require.NoError(t, err)
	assert.Equal(t, "world", string(after))

	_, statErr := os.Lstat(filepath.Join(dst, "home/wes/loop.jsonl"))
	assert.True(
		t, os.IsNotExist(statErr),
		"self-referential hardlink should not be created",
	)
}

func TestExtractTarStreamTruncatedMidBodyFails(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/a.txt", typeflag: tar.TypeReg, body: "aaa"},
		{
			name:     "home/big.txt",
			typeflag: tar.TypeReg,
			body:     strings.Repeat("x", 4096),
		},
	})
	// First entry (1024B) intact; cut inside the second file's body.
	truncated := data[:1024+512+100]

	_, err := extract(t, truncated, dst)
	require.Error(t, err, "truncated transfer must fail, not be accepted")
	assert.NoFileExists(
		t, filepath.Join(dst, "home/big.txt"),
		"a truncated file must not be left as if complete",
	)
}

func TestExtractTarStreamTruncatedMidHeaderFails(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/a.txt", typeflag: tar.TypeReg, body: "aaa"},
		{name: "home/b.txt", typeflag: tar.TypeReg, body: "bbb"},
	})
	// Keep first entry whole; cut partway through the second header.
	truncated := data[:1024+200]

	_, err := extract(t, truncated, dst)
	require.Error(t, err)
}

func TestExtractTarStreamCorruptHeaderFails(t *testing.T) {
	dst := t.TempDir()
	garbage := bytes.Repeat([]byte("A"), 1024)

	_, err := extract(t, garbage, dst)
	require.Error(t, err, "corrupt/unrecognized archive must fail")
}

func TestExtractTarStreamRejectsRelativePathEscape(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "../escape.txt", typeflag: tar.TypeReg, body: "pwned"},
	})

	_, err := extract(t, data, dst)
	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(filepath.Dir(dst), "escape.txt"))
}

func TestExtractTarStreamRejectsRelativeSymlinkEscape(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{
			name:     "home/evil",
			typeflag: tar.TypeSymlink,
			linkname: "../../../../etc",
		},
	})

	_, err := extract(t, data, dst)
	require.Error(t, err)
}

func TestExtractTarStreamRejectsAbsoluteSymlinkEscape(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{
			name:     "home/evil",
			typeflag: tar.TypeSymlink,
			linkname: "/etc/passwd",
		},
	})

	_, err := extract(t, data, dst)
	require.Error(t, err)
}

func TestExtractTarStreamNormalHardlink(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/a.txt", typeflag: tar.TypeReg, body: "shared"},
		{name: "home/b.txt", typeflag: tar.TypeLink, linkname: "home/a.txt"},
	})

	skipped, err := extract(t, data, dst)
	require.NoError(t, err)
	assert.Equal(t, 0, skipped)

	b, err := os.ReadFile(filepath.Join(dst, "home/b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "shared", string(b))
}

func TestExtractTarStreamSymlinkWithinDst(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/target.txt", typeflag: tar.TypeReg, body: "data"},
		{
			name:     "home/link.txt",
			typeflag: tar.TypeSymlink,
			linkname: "target.txt",
		},
	})

	skipped, err := extract(t, data, dst)
	require.NoError(t, err)
	assert.Equal(t, 0, skipped)

	got, err := os.Readlink(filepath.Join(dst, "home/link.txt"))
	require.NoError(t, err)
	assert.Equal(t, "target.txt", got)
}

func TestExtractTarStreamCreatesDirsAndFiles(t *testing.T) {
	dst := t.TempDir()
	data := buildTestTar(t, []tarEntry{
		{name: "home/wes/.claude/", typeflag: tar.TypeDir},
		{
			name:     "home/wes/.claude/s.jsonl",
			typeflag: tar.TypeReg,
			body:     "{}",
		},
	})

	skipped, err := extract(t, data, dst)
	require.NoError(t, err)
	assert.Equal(t, 0, skipped)

	info, err := os.Stat(filepath.Join(dst, "home/wes/.claude"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	body, err := os.ReadFile(filepath.Join(dst, "home/wes/.claude/s.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, "{}", string(body))
}
