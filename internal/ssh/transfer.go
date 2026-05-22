package ssh

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

// buildTarCommand generates the remote tar command for the given
// agent directories. Uses -C / so paths are relative to root.
// Strips leading / from each dir and shell-quotes each path.
func buildTarCommand(
	dirs map[parser.AgentType][]string,
) string {
	var paths []string
	for _, agentDirs := range dirs {
		for _, d := range agentDirs {
			p := strings.TrimPrefix(d, "/")
			paths = append(paths, shellQuote(p))
		}
	}
	return "tar cf - -C / -- " + strings.Join(paths, " ")
}

// shellQuote wraps s in single quotes, escaping any embedded
// single quotes. Safe for passing paths through sh -c.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// downloadAndExtract tars remote agent dirs and extracts to a local
// temp dir. Returns the temp dir path; caller must clean up.
func downloadAndExtract(
	ctx context.Context,
	host, user string, port int, sshOpts []string,
	dirs map[parser.AgentType][]string,
) (string, error) {
	tarCmd := buildTarCommand(dirs)
	stdout, cleanup, err := runSSHStream(
		ctx, host, user, port, sshOpts, tarCmd,
	)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "agentsview-ssh-*")
	if err != nil {
		stdout.Close()
		_ = cleanup()
		return "", fmt.Errorf("create temp dir: %w", err)
	}

	// Wrap stdout with a progress counter so the user
	// can see data flowing during the transfer.
	pr := &progressReader{r: stdout}
	done := make(chan struct{})
	go pr.printLoop(done)

	extract := exec.CommandContext(
		ctx, "tar", "xf", "-", "-C", tmpDir,
	)
	extract.Stdin = pr

	extractErr := extract.Run()
	close(done)
	pr.printFinal()

	if extractErr != nil {
		stdout.Close()
		os.RemoveAll(tmpDir)
		_ = cleanup()
		return "", fmt.Errorf("extract tar: %w", extractErr)
	}

	// stdout is consumed by tar; close it so the SSH
	// process can exit cleanly.
	stdout.Close()
	if err := cleanup(); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("ssh tar: %w", err)
	}
	return tmpDir, nil
}

// remapToRemotePath converts a temp-dir path back to the original
// remote path. Strips the temp dir prefix so the remainder is the
// absolute path as it existed on the remote host.
//
// Example:
//
//	tempDir="/tmp/sync-123"
//	localPath="/tmp/sync-123/home/wes/.claude/foo.jsonl"
//	result="/home/wes/.claude/foo.jsonl"
func remapToRemotePath(tempDir, remoteDir, localPath string) string {
	_ = remoteDir // reserved for future use; tar -C / preserves full paths
	rel, err := filepath.Rel(tempDir, localPath)
	if err != nil {
		return localPath
	}
	return "/" + filepath.ToSlash(rel)
}

// progressReader wraps a reader and tracks bytes read.
type progressReader struct {
	r     io.Reader
	bytes atomic.Int64
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.bytes.Add(int64(n))
	return n, err
}

func (pr *progressReader) printLoop(done <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			fmt.Printf(
				"\r  Received %s...",
				formatBytes(pr.bytes.Load()),
			)
		}
	}
}

func (pr *progressReader) printFinal() {
	fmt.Printf(
		"\r  Received %s   \n",
		formatBytes(pr.bytes.Load()),
	)
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", b)
	}
}

// remappedDir returns the temp-dir equivalent of a remote dir.
//
// Example:
//
//	tempDir="/tmp/sync-123"
//	remoteDir="/home/wes/.claude"
//	result="/tmp/sync-123/home/wes/.claude"
func remappedDir(tempDir, remoteDir string) string {
	return filepath.Join(
		tempDir, strings.TrimPrefix(remoteDir, "/"),
	)
}
