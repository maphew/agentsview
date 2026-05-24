package secrets

import "sync/atomic"

// agentsviewTestFixtures holds every literal secret-shaped string that
// appears in agentsview's own unit-test files. Each value is
// random-looking by design so it exercises the positive path of a
// rule's regex + validator — but as soon as an LLM transcript records
// the test source, the value flows through to every subsequent scan
// of that conversation as a definite "leak". Scan filters matches
// whose span equals one of these literals so production scans don't
// surface agentsview's own test data.
//
// Adding a new fixture to a test file? Add the literal here too — the
// next scan of the development conversation that recorded the fixture
// would otherwise report it.
//
// Old fixtures that have been replaced are kept here on purpose: an
// older session transcript may still quote them. The map is read-only
// after init, so an entry never costs more than one pointer.
var agentsviewTestFixtures = map[string]struct{}{
	// AWS access keys.
	"AKIA1234567890ABCDEF": {},
	"AKIA12345678ABCDEFGH": {},
	"AKIA3VBMK8XJZ6WPCNQH": {},
	"AKIA7QHWN2DKR4FYPLJM": {},
	"AKIAMMKKJJRRPPNN22FF": {},
	"AKIAQQHHKKBB22NN77XX": {},
	"AKIAZYXWVUTSRQPONMLK": {},
	"ASIA5GTKD7RPYNXQVMBL": {},
	"ASIAFFHHKKBB22NN77XX": {},
	"ASIAQWERTYUIOPASDFGH": {},

	// Anthropic keys.
	"sk-ant-api03-Nc6Mp1Hj9Bg3Tf5Ds8Lr0E": {},
	"sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8Zb4": {},
	"sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8ZbE": {},
	"sk-ant-api03-ZyXwVuTsRqPoNmLkJiHgFe": {},
	"sk-ant-api03-QrStUvWxYz0987654321Ab": {},

	// Slack tokens.
	"xoxb-123456789012-abcdefABCDEFc8Jp":      {},
	"xoxb-549271836401-fHk7Bm3Pz9Wt5Vx2Yq8Nc": {},
	"xoxs-302846159270-xPk9Bm3Wv8Qt5Lz2Yh7Fc": {},
	"xoxs-987654321098-fedcbaFEDCBAxYz9":      {},

	// GitHub PATs.
	"ghp_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4Hg":             {},
	"github_pat_8Hk3Wn7Dz4Rp2Vx9Mb6Tj0Qc5Lm1Yp8Bv4HgN_X2cWp9": {},

	// Stripe secrets.
	"sk_live_7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm": {},

	// Google API keys (with and without trailing dash).
	"AIza7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm1Yp8Bv4H": {},
	"AIza7Qh3Wn8Dk4Rp9Vx2Mb6Tj0Qc5Lm1Yp8Bv4-": {},
}

// isAgentsviewTestFixture reports whether s is a literal value from
// agentsview's own test files. Constant-time map lookup.
func isAgentsviewTestFixture(s string) bool {
	_, ok := agentsviewTestFixtures[s]
	return ok
}

// fixtureDenyEnabled controls whether Scan filters matches against
// agentsviewTestFixtures. False by default so unit tests (which build
// their input around the same random-looking fixtures the rules need
// to verify positive paths against) pass without per-test
// boilerplate. The agentsview binary calls EnableFixtureDeny at
// startup so production scans automatically suppress agentsview's own
// noise.
var fixtureDenyEnabled atomic.Bool

// EnableFixtureDeny turns on the agentsview-test-fixture deny-list
// for subsequent Scan and ScanDefinite calls. Wired into the CLI
// entrypoint so the long-running server, ad-hoc CLI commands, and
// sync engine all filter the fixture noise. Off by default so unit
// tests can assert positive rule paths against the same values.
func EnableFixtureDeny() {
	fixtureDenyEnabled.Store(true)
}

// disableFixtureDenyForTest restores fixtureDenyEnabled to its
// previous value after the cleanup runs. Used by the secrets package
// own tests that want to exercise the deny-list path explicitly.
func disableFixtureDenyForTest(cleanup func(func())) {
	prev := fixtureDenyEnabled.Swap(false)
	cleanup(func() { fixtureDenyEnabled.Store(prev) })
}
