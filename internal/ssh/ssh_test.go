package ssh

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// nonInteractive is the trailing option block buildSSHArgs appends
// to every invocation (see sshConnectTimeoutSecs). Kept here so the
// table below documents the full expected arg vector.
var nonInteractive = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=10",
}

func TestBuildSSHArgs(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		user    string
		port    int
		sshOpts []string
		cmd     string
		want    []string
	}{
		{
			name: "host only",
			host: "devbox1",
			cmd:  "echo hello",
			want: []string{
				"ssh", "-o", "BatchMode=yes",
				"-o", "ConnectTimeout=10",
				"devbox1", "--", "sh -c 'echo hello'",
			},
		},
		{
			name: "host and user",
			host: "devbox1",
			user: "wes",
			cmd:  "ls -la",
			want: []string{
				"ssh", "-o", "BatchMode=yes",
				"-o", "ConnectTimeout=10",
				"wes@devbox1", "--", "sh -c 'ls -la'",
			},
		},
		{
			name: "with port",
			host: "devbox1",
			user: "wes",
			port: 2222,
			cmd:  "echo hi",
			want: []string{
				"ssh", "-p", "2222",
				"-o", "BatchMode=yes",
				"-o", "ConnectTimeout=10",
				"wes@devbox1", "--", "sh -c 'echo hi'",
			},
		},
		{
			name: "zero port ignored",
			host: "devbox1",
			cmd:  "echo hi",
			want: []string{
				"ssh", "-o", "BatchMode=yes",
				"-o", "ConnectTimeout=10",
				"devbox1", "--", "sh -c 'echo hi'",
			},
		},
		{
			name: "with ssh opts before defaults",
			host: "devbox1",
			user: "wes",
			port: 2222,
			sshOpts: []string{
				"-i", "/tmp/key",
				"-o", "StrictHostKeyChecking=no",
			},
			cmd: "ls",
			want: []string{
				"ssh", "-p", "2222",
				"-i", "/tmp/key",
				"-o", "StrictHostKeyChecking=no",
				"-o", "BatchMode=yes",
				"-o", "ConnectTimeout=10",
				"wes@devbox1", "--", "sh -c 'ls'",
			},
		},
		{
			name: "escapes single quotes",
			host: "devbox1",
			cmd:  `printf "it's fine"`,
			want: []string{
				"ssh", "-o", "BatchMode=yes",
				"-o", "ConnectTimeout=10",
				"devbox1", "--",
				`sh -c 'printf "it'\''s fine"'`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSSHArgs(
				tt.host, tt.user, tt.port,
				tt.sshOpts, tt.cmd,
			)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestBuildSSHArgs_NonInteractiveDefaults locks the security intent
// independent of arg ordering: remote sync never prompts (BatchMode)
// and bounds the connect phase (ConnectTimeout).
func TestBuildSSHArgs_NonInteractiveDefaults(t *testing.T) {
	got := buildSSHArgs("devbox1", "", 0, nil, "true")
	assert.Subset(t, got, nonInteractive,
		"ssh invocation must include non-interactive defaults")
}
