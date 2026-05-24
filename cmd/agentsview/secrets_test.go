package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestNewSecretsListCommandFlags(t *testing.T) {
	cmd := newSecretsListCommand()
	// confidence is validated server-side, so cobra must accept any value.
	cmd.SetArgs([]string{"--confidence", "bogus", "--reveal", "--limit", "5"})
	for _, name := range []string{"project", "agent", "rule", "confidence",
		"reveal", "limit", "cursor", "date-from", "date-to"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("secrets list missing --%s flag", name)
		}
	}
}

func TestNewSecretsScanCommandFlags(t *testing.T) {
	cmd := newSecretsScanCommand()
	for _, name := range []string{"backfill", "project", "agent",
		"date-from", "date-to"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("secrets scan missing --%s flag", name)
		}
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
	if err != nil {
		t.Fatal(err)
	}
	secret := syntheticAWSAccessKey(t.Name())
	if err := d.InsertMessages([]db.Message{{
		SessionID: "leaky", Ordinal: 0, Role: "user",
		Content: "my key " + secret + " here",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := executeCommand(newRootCommand(),
		"secrets", "scan", "--backfill", "--format", "json")
	if err != nil {
		t.Fatalf("secrets scan failed (engine not plumbed?): %v", err)
	}
	var got struct {
		Scanned       int `json:"scanned"`
		WithSecrets   int `json:"with_secrets"`
		TotalFindings int `json:"total_findings"`
	}
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("scan output not JSON: %q (%v)", out, jerr)
	}
	if got.Scanned < 1 || got.WithSecrets < 1 || got.TotalFindings < 1 {
		t.Errorf("expected the seeded secret to be found, got %+v", got)
	}
}

func TestSecretsScan_DirectMode_DeniesAgentsviewFixtures(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "fixture", "proj")

	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Join([]string{
		"ghp_", "M7qL8r", "P2sT5u", "V9wX3y",
		"Z6aB1c", "D4eF7g", "H0iJ2k",
	}, "")
	if err := d.InsertMessages([]db.Message{{
		SessionID: "fixture", Ordinal: 0, Role: "user",
		Content: "fixture token " + secret,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := executeCommand(newRootCommand(),
		"secrets", "scan", "--backfill", "--format", "json")
	if err != nil {
		t.Fatalf("secrets scan failed: %v", err)
	}
	var got struct {
		Scanned       int `json:"scanned"`
		WithSecrets   int `json:"with_secrets"`
		TotalFindings int `json:"total_findings"`
	}
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("scan output not JSON: %q (%v)", out, jerr)
	}
	if got.Scanned != 1 || got.WithSecrets != 0 || got.TotalFindings != 0 {
		t.Errorf("fixture should be suppressed, got %+v", got)
	}
}

func TestPrintSecretFindingsHuman(t *testing.T) {
	var buf bytes.Buffer
	res := &service.SecretFindingList{
		Findings: []db.SecretFindingRow{},
	}
	if err := printSecretFindingsHuman(&buf, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "(no findings)") {
		t.Errorf("empty list should print (no findings): %q", buf.String())
	}
}
