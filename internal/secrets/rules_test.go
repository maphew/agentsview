package secrets

import (
	"strings"
	"testing"
)

func TestDefiniteRules(t *testing.T) {
	cases := []struct {
		name string
		rule string
		text string
		want bool
	}{
		{"github classic", "github-pat",
			"tok ghp_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg x", true},
		{"github fine-grained", "github-pat",
			"github_pat_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4HgN_X2cWp9", true},
		{"slack bot", "slack-token",
			"xoxb-549271836401-fHk7Bm3Pz9Wt5Vx2Yq8Nc", true},
		{"stripe live", "stripe-secret",
			"sk_live_7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm", true},
		{"google api", "google-api-key",
			"AIza7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm1Yp8Bv4H", true},
		{"google api ending dash", "google-api-key",
			"key AIza7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm1Yp8Bv4- end", true},
		{"pem block", "private-key-block",
			"-----BEGIN RSA PRIVATE KEY-----\n" +
				rep("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 5) +
				"-----END RSA PRIVATE KEY-----", true},
		{"plain prose", "", "the quick brown fox jumps over", false},
		{"short ghp", "", "ghp_tooShort", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Scan(c.text)
			found := ""
			for _, m := range got {
				if m.Rule == c.rule {
					found = m.Rule
				}
			}
			if c.want && found == "" {
				t.Errorf("expected rule %q to match %q; got %+v", c.rule, c.text, got)
			}
			if !c.want && len(got) != 0 {
				t.Errorf("expected no match for %q; got %+v", c.text, got)
			}
		})
	}
}

func TestCandidateRules(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dumm_Sig-Value12345"
	cases := []struct {
		name string
		rule string
		text string
		want bool
	}{
		{"jwt", "jwt", "auth: " + jwt, true},
		{"high entropy assignment", "high-entropy-assignment",
			"SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6", true},
		{"low entropy assignment", "high-entropy-assignment",
			"NAME=aaaaaaaaaaaaaaaaaaaa", false},
		{"short assignment", "high-entropy-assignment", "X=ab12", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Scan(c.text)
			found := false
			for _, m := range got {
				if m.Rule == c.rule {
					found = true
					if m.Confidence != ConfidenceCandidate {
						t.Errorf("%s confidence = %q, want candidate", c.rule, m.Confidence)
					}
				}
			}
			if found != c.want {
				t.Errorf("rule %q match=%v want=%v for %q (got %+v)",
					c.rule, found, c.want, c.text, got)
			}
		})
	}
}

// TestScanDefiniteReturnsOnlyDefinite confirms the inline-sync scan path
// reports definite vendor formats and skips the FP-prone candidate heuristics
// (high-entropy assignments, JWTs, basic-auth URLs) entirely.
func TestScanDefiniteReturnsOnlyDefinite(t *testing.T) {
	// One definite AWS key and one candidate high-entropy assignment.
	text := "aws AKIA7QHWN2DKR4FYPLJM and SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6"
	if full := Scan(text); len(full) != 2 {
		t.Fatalf("precondition: Scan should report 2 matches (1 definite, 1 "+
			"candidate), got %d: %+v", len(full), full)
	}
	got := ScanDefinite(text)
	if len(got) != 1 {
		t.Fatalf("ScanDefinite returned %d matches, want 1: %+v", len(got), got)
	}
	if got[0].Rule != "aws-access-key" {
		t.Errorf("rule = %q, want aws-access-key", got[0].Rule)
	}
	for _, m := range got {
		if m.Confidence != ConfidenceDefinite {
			t.Errorf("ScanDefinite returned non-definite match: %+v", m)
		}
	}
}

// TestScanDefiniteMatchesScanDefiniteSubset confirms ScanDefinite yields the
// same spans (rule, offsets, redaction) that Scan reports for definite rules,
// so findings stored by the inline path and the full scan stay consistent.
func TestScanDefiniteMatchesScanDefiniteSubset(t *testing.T) {
	text := "key AKIA7QHWN2DKR4FYPLJM tok ghp_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg" +
		" SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6"
	var wantDef []Match
	for _, m := range Scan(text) {
		if m.Confidence == ConfidenceDefinite {
			wantDef = append(wantDef, m)
		}
	}
	got := ScanDefinite(text)
	if len(got) != len(wantDef) {
		t.Fatalf("ScanDefinite count = %d, Scan definite count = %d (%+v vs %+v)",
			len(got), len(wantDef), got, wantDef)
	}
	for i := range got {
		if got[i].Rule != wantDef[i].Rule || got[i].Start != wantDef[i].Start ||
			got[i].End != wantDef[i].End || got[i].Redacted != wantDef[i].Redacted {
			t.Errorf("match %d differs: ScanDefinite=%+v Scan=%+v",
				i, got[i], wantDef[i])
		}
	}
}

// TestDefiniteRulesVersionDistinctFromFull pins the split-versioning contract:
// the inline definite-only scan stamps a version that differs from the full
// ruleset version, so secrets scan --backfill (which treats RulesVersion as
// current) re-scans inline-only sessions to pick up candidate findings.
func TestDefiniteRulesVersionDistinctFromFull(t *testing.T) {
	def := DefiniteRulesVersion()
	full := RulesVersion()
	if def == full {
		t.Fatalf("DefiniteRulesVersion must differ from RulesVersion (both %q)", def)
	}
	if def == "" || full == "" {
		t.Fatal("versions must be non-empty")
	}
	if def != DefiniteRulesVersion() {
		t.Error("DefiniteRulesVersion not stable across calls")
	}
	if len(def) != 64 {
		t.Errorf("DefiniteRulesVersion length = %d, want 64 hex chars: %q", len(def), def)
	}
	for _, c := range def {
		if !isLowerHex(c) {
			t.Fatalf("DefiniteRulesVersion has non-hex char %q in %q", c, def)
		}
	}
}

func TestRulesVersionStableAndHex(t *testing.T) {
	v1 := RulesVersion()
	v2 := RulesVersion()
	if v1 != v2 {
		t.Fatalf("RulesVersion not stable: %q != %q", v1, v2)
	}
	if len(v1) != 64 { // sha256 hex
		t.Fatalf("RulesVersion length = %d, want 64 hex chars: %q", len(v1), v1)
	}
	for _, c := range v1 {
		if !isLowerHex(c) {
			t.Fatalf("RulesVersion has non-hex char %q in %q", c, v1)
		}
	}
}

func TestVerify(t *testing.T) {
	// Non-grouped rule: the stored span is the full regex match.
	awsSrc := "export KEY=AKIA7QHWN2DKR4FYPLJM done"
	s := strings.Index(awsSrc, "AKIA")
	e := s + len("AKIA7QHWN2DKR4FYPLJM")
	if !Verify("aws-access-key", awsSrc, s, e) {
		t.Error("Verify should accept a valid AWS key at its coordinates")
	}
	if Verify("aws-access-key", awsSrc, 0, 6) {
		t.Error("Verify should reject coordinates that are not the key")
	}
	if Verify("nonexistent-rule", awsSrc, s, e) {
		t.Error("Verify should reject an unknown rule")
	}
	if Verify("aws-access-key", awsSrc, s, len(awsSrc)+10) {
		t.Error("Verify should reject out-of-bounds coordinates")
	}
	// Grouped rule: the stored span is the captured group (the password),
	// not the full URL match. Verify must still accept it.
	urlSrc := "db=postgres://user:s3cretP4ss@host:5432/db"
	ps := strings.Index(urlSrc, "s3cretP4ss")
	pe := ps + len("s3cretP4ss")
	if !Verify("basic-auth-url", urlSrc, ps, pe) {
		t.Error("Verify should accept a grouped finding at its group coordinates")
	}
}

// TestVerifyDetectsChangedSource locks in the core --reveal guarantee: a scan
// produces coordinates, Verify accepts them on the unchanged source, and
// rejects them once the bytes at those coordinates are no longer the secret.
func TestVerifyDetectsChangedSource(t *testing.T) {
	source := "export AWS=AKIA7QHWN2DKR4FYPLJM"
	// Seed from canonical Scan (what produces findings and what Verify uses).
	matches := Scan(source)
	if len(matches) == 0 {
		t.Fatal("expected at least one match in source")
	}
	m := matches[0]
	if !Verify(m.Rule, source, m.Start, m.End) {
		t.Errorf("Verify should accept unchanged source at [%d,%d)", m.Start, m.End)
	}
	// Same length, but the secret at [Start,End) is replaced by a zero-entropy
	// run that matches no rule, so Verify must reject the stale coordinates.
	changed := source[:m.Start] + strings.Repeat("X", m.End-m.Start)
	if Verify(m.Rule, changed, m.Start, m.End) {
		t.Error("Verify should reject when the source changed at those coords")
	}
}

// TestVerifyRejectsSuppressedCandidate ensures Verify mirrors canonical Scan,
// not raw scanning: a candidate that overlaps a definite is suppressed by Scan,
// so Verify must reject its coordinates even though scanRaw reports it.
func TestVerifyRejectsSuppressedCandidate(t *testing.T) {
	src := "https://user:sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8Zb4@example.com"
	var cand Match
	for _, m := range scanRaw(src) {
		if m.Rule == "basic-auth-url" {
			cand = m
			break
		}
	}
	if cand.Rule == "" {
		t.Fatal("precondition: scanRaw should report a basic-auth-url candidate")
	}
	if Verify("basic-auth-url", src, cand.Start, cand.End) {
		t.Error("Verify must reject a candidate that canonical Scan suppresses")
	}
}

// isLowerHex reports whether c is a lowercase hexadecimal digit, the alphabet
// a SHA-256 hex digest is built from.
func isLowerHex(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// rep returns s repeated n times (test helper for building token bodies).
func rep(s string, n int) string {
	var out strings.Builder
	for range n {
		out.WriteString(s)
	}
	return out.String()
}

// TestHasRepeatingBlock pins the seed-pattern detector that catches
// placeholders built by repeating a short string. Block size 1 is the
// "ghp_aaaa…" shape; sizes 2..6 cover "A1b2A1b2…", "aB3_xaB3_x…", etc.
func TestHasRepeatingBlock(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want bool
	}{
		{"single byte dominating", strings.Repeat("a", 36), true},
		{"block size 4 A1b2", strings.Repeat("A1b2", 20), true},
		{"block size 4 a1B2", strings.Repeat("a1B2", 8), true},
		{"block size 5 aB3_x", strings.Repeat("aB3_x", 7), true},
		{"block size 2", strings.Repeat("xy", 10), true},
		{"random body", "7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm", false},
		{"random aws body", "7QHWN2DKR4FYPLJM", false},
		{"random pat body", "8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg", false},
		{"too short", "abcd", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasRepeatingBlock(c.s); got != c.want {
				t.Errorf("hasRepeatingBlock(%q) = %v, want %v", c.s, got, c.want)
			}
		})
	}
}

// TestHasMonotoneRun pins the alphabet/digit-run detector that catches
// placeholders built from sequential ASCII ("abcdef", "1234567890",
// "ZYXWVU"). The 6-char minimum is small enough to catch the dominant
// noise shapes without flagging random secrets that happen to include a
// short run by chance.
func TestHasMonotoneRun(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want bool
	}{
		{"abcdef", "abcdef", true},
		{"1234567890", "1234567890", true},
		{"ZYXWVU", "ZYXWVU", true},
		{"fedcba", "fedcba", true},
		{"abcde (only 5)", "abcde", false},
		{"random", "7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm", false},
		{"isolated +1 transitions", "549271836401", false},
		{"embedded run", "Xabcdef9", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasMonotoneRun(c.s, 6); got != c.want {
				t.Errorf("hasMonotoneRun(%q, 6) = %v, want %v", c.s, got, c.want)
			}
		})
	}
}
