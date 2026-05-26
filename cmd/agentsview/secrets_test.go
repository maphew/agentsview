package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestNewSecretsListCommandFlags(t *testing.T) {
	cmd := newSecretsListCommand()
	// confidence is validated server-side, so cobra must accept any value.
	cmd.SetArgs([]string{"--confidence", "bogus", "--reveal", "--limit", "5"})
	for _, name := range []string{"project", "agent", "rule", "confidence",
		"reveal", "limit", "cursor", "date-from", "date-to"} {
		assert.NotNil(t, cmd.Flags().Lookup(name),
			"secrets list missing --%s flag", name)
	}
}

func TestNewSecretsScanCommandFlags(t *testing.T) {
	cmd := newSecretsScanCommand()
	for _, name := range []string{"backfill", "project", "agent",
		"date-from", "date-to"} {
		assert.NotNil(t, cmd.Flags().Lookup(name),
			"secrets scan missing --%s flag", name)
	}
}

func syntheticAWSAccessKey(seed string) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	sum := sha256.Sum256([]byte(seed))
	body := make([]byte, 16)
	for i := range body {
		body[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return "AKIA" + string(body)
}

// TestSecretsScan_DirectMode_Scans verifies `secrets scan` is wired with a
// real sync.Engine in direct mode (no daemon). A nil-engine direct backend
// would make ScanSecrets return db.ErrReadOnly; instead the scan must run and
// find the seeded secret.
func TestSecretsScan_DirectMode_Scans(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "leaky", "proj")

	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	secret := syntheticAWSAccessKey(t.Name())
	require.NoError(t, d.InsertMessages([]db.Message{{
		SessionID: "leaky", Ordinal: 0, Role: "user",
		Content: "my key " + secret + " here",
	}}))
	require.NoError(t, d.Close())

	out, err := executeCommand(newRootCommand(),
		"secrets", "scan", "--backfill", "--format", "json")
	require.NoError(t, err, "secrets scan failed (engine not plumbed?)")
	var got struct {
		Scanned       int `json:"scanned"`
		WithSecrets   int `json:"with_secrets"`
		TotalFindings int `json:"total_findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"scan output not JSON: %q", out)
	assert.GreaterOrEqual(t, got.Scanned, 1,
		"expected the seeded secret to be found, got %+v", got)
	assert.GreaterOrEqual(t, got.WithSecrets, 1,
		"expected the seeded secret to be found, got %+v", got)
	assert.GreaterOrEqual(t, got.TotalFindings, 1,
		"expected the seeded secret to be found, got %+v", got)
}

func TestSecretsScan_DirectMode_DeniesAgentsviewFixtures(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "fixture", "proj")

	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	secret := strings.Join([]string{
		"ghp_", "M7qL8r", "P2sT5u", "V9wX3y",
		"Z6aB1c", "D4eF7g", "H0iJ2k",
	}, "")
	require.NoError(t, d.InsertMessages([]db.Message{{
		SessionID: "fixture", Ordinal: 0, Role: "user",
		Content: "fixture token " + secret,
	}}))
	require.NoError(t, d.Close())

	out, err := executeCommand(newRootCommand(),
		"secrets", "scan", "--backfill", "--format", "json")
	require.NoError(t, err, "secrets scan failed")
	var got struct {
		Scanned       int `json:"scanned"`
		WithSecrets   int `json:"with_secrets"`
		TotalFindings int `json:"total_findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"scan output not JSON: %q", out)
	assert.Equal(t, 1, got.Scanned, "fixture should be suppressed, got %+v", got)
	assert.Equal(t, 0, got.WithSecrets, "fixture should be suppressed, got %+v", got)
	assert.Equal(t, 0, got.TotalFindings, "fixture should be suppressed, got %+v", got)
}

func TestPrintSecretFindingsHuman(t *testing.T) {
	var buf bytes.Buffer
	res := &service.SecretFindingList{
		Findings: []db.SecretFindingRow{},
	}
	require.NoError(t, printSecretFindingsHuman(&buf, res))
	assert.Contains(t, buf.String(), "(no findings)",
		"empty list should print (no findings)")
}
