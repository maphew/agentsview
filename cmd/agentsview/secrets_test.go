package main

import (
	"bytes"
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
	if err := d.InsertMessages([]db.Message{{
		SessionID: "leaky", Ordinal: 0, Role: "user",
		Content: "my key AKIA7QHWN2DKR4FYPLJM here",
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
