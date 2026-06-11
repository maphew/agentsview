package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifierHashStable(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes([]string{"foo", "bar"})
	a := ClassifierHash()
	b := ClassifierHash()
	assert.Equal(t, a, b, "hash unstable")
}

func TestClassifierHashChangesWithUserPrefixes(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes(nil)
	base := ClassifierHash()
	SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	with := ClassifierHash()
	assert.NotEqual(t, base, with,
		"hash did not change when user prefixes changed")
}

func TestClassifierHashOrderIndependent(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes([]string{"alpha", "beta", "gamma"})
	a := ClassifierHash()
	SetUserAutomationPrefixes([]string{"gamma", "alpha", "beta"})
	b := ClassifierHash()
	assert.Equal(t, a, b, "hash not order-independent")
}

// TestClassifierHashTagSeparation guards against the case
// where two different categorizations produce the same hash
// because the tag prefix was dropped from the encoding.
func TestClassifierHashTagSeparation(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	SetUserAutomationPrefixes([]string{"Warmup"})
	got := ClassifierHash()
	SetUserAutomationPrefixes(nil)
	bareBuiltins := ClassifierHash()
	assert.NotEqual(t, got, bareBuiltins,
		"user prefix 'Warmup' collided with built-in exact-match 'Warmup'")
}

// TestClassifierHashCurrentAlgoVersion is a forced-bump
// guard: it pins the algorithm version at construction time.
// If a future change to the matching logic forgets to bump
// classifierAlgorithmVersion, this test still passes (false
// negative) — but if someone bumps the version intentionally
// the test must be updated to match. The check exists to
// surface accidental version-constant edits during review.
func TestClassifierHashCurrentAlgoVersion(t *testing.T) {
	assert.Equal(t, 2, classifierAlgorithmVersion,
		"classifierAlgorithmVersion changed; update this test and confirm "+
			"matching semantics actually changed (not just pattern edits, "+
			"which the hash already detects)")
}
