package db

import (
	"context"
	"testing"
	"time"
)

func TestBackfillIsAutomatedBidirectional(t *testing.T) {
	d := testDB(t)

	// Seed a false negative: single-turn roborev session with
	// is_automated = 0 (simulates pre-migration data).
	insertSession(t, d, "missed", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	// Force is_automated to 0 to simulate pre-migration state.
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'missed'",
	)
	requireNoError(t, err, "force missed to 0")

	// Seed a stale false positive: multi-turn session that was
	// previously marked automated under old broad rules.
	insertSession(t, d, "stale", "proj", func(s *Session) {
		fm := "# Fix Request for login flow"
		s.FirstMessage = &fm
		s.MessageCount = 10
		s.UserMessageCount = 5
	})
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 1 WHERE id = 'stale'",
	)
	requireNoError(t, err, "force stale to 1")

	// Clear the marker so the backfill will run.
	_, err = d.getWriter().Exec(
		"DELETE FROM stats WHERE key = ?",
		ClassifierHashKey,
	)
	requireNoError(t, err, "clear marker")

	// Run backfill.
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	requireNoError(t, err, "first backfill run")

	ctx := context.Background()

	// False negative should now be set.
	missed, err := d.GetSession(ctx, "missed")
	requireNoError(t, err, "get missed")
	if !missed.IsAutomated {
		t.Error("missed session should be automated after backfill")
	}

	// Stale false positive should now be cleared.
	stale, err := d.GetSession(ctx, "stale")
	requireNoError(t, err, "get stale")
	if stale.IsAutomated {
		t.Error("stale session should not be automated after backfill")
	}
}

func TestBackfillIsAutomatedMarkerIdempotent(t *testing.T) {
	d := testDB(t)

	// Seed a roborev session.
	insertSession(t, d, "review", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})

	// Clear the marker and run backfill.
	_, err := d.getWriter().Exec(
		"DELETE FROM stats WHERE key = ?",
		ClassifierHashKey,
	)
	requireNoError(t, err, "clear marker")

	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	requireNoError(t, err, "first run")

	// Manually corrupt the session to verify second run is a no-op.
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'review'",
	)
	requireNoError(t, err, "corrupt")

	// Second run should be a no-op (marker present).
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	requireNoError(t, err, "second run")

	ctx := context.Background()
	review, err := d.GetSession(ctx, "review")
	requireNoError(t, err, "get review")
	if review.IsAutomated {
		t.Error("second run should be no-op; is_automated should still be 0")
	}
}

// TestIncrementalUpdateReclassifiesOnPatternChange covers the
// case where the classifier gained a new pattern after a row
// was originally inserted. The row took the incremental path on
// subsequent parses, so UpsertSession never ran again to set
// is_automated. UpdateSessionIncremental must re-evaluate.
func TestIncrementalUpdateReclassifiesOnPatternChange(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Simulate a pre-existing single-turn session whose
	// first_message now matches a classifier pattern but whose
	// stored is_automated is stale at 0.
	insertSession(t, d, "changelog-inc", "proj", func(s *Session) {
		fm := "You are generating a changelog for myrepo version 1.0.0."
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'changelog-inc'",
	)
	requireNoError(t, err, "force stale is_automated=0")

	// Incremental update with umc still <= 1.
	err = d.UpdateSessionIncremental(
		"changelog-inc", nil, 2, 1, 1024, 100, 0, 0, false, false,
	)
	requireNoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "changelog-inc")
	requireNoError(t, err, "get changelog-inc")
	if !got.IsAutomated {
		t.Error("is_automated should be re-set after incremental update")
	}
}

// TestIncrementalUpdateClearsWhenCountGrows covers the existing
// guard: when user_message_count grows past 1, is_automated must
// be cleared even if first_message still matches a pattern.
func TestIncrementalUpdateClearsWhenCountGrows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "grew-past-one", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	// After UpsertSession the row is correctly is_automated=1.
	pre, err := d.GetSession(ctx, "grew-past-one")
	requireNoError(t, err, "get pre")
	if !pre.IsAutomated {
		t.Fatalf("precondition: expected is_automated=1 after upsert")
	}

	// Incremental update pushes umc > 1 — must clear.
	err = d.UpdateSessionIncremental(
		"grew-past-one", nil, 7, 3, 2048, 200, 0, 0, false, false,
	)
	requireNoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "grew-past-one")
	requireNoError(t, err, "get grew-past-one")
	if got.IsAutomated {
		t.Error("is_automated should be cleared when umc grows > 1")
	}
}

// TestIncrementalUpdateLeavesNonMatching covers a non-automated
// single-turn session: re-evaluation must NOT set is_automated
// when first_message doesn't match any pattern.
func TestIncrementalUpdateLeavesNonMatching(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "normal-single", "proj", func(s *Session) {
		fm := "Fix the login bug please"
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})

	err := d.UpdateSessionIncremental(
		"normal-single", nil, 2, 1, 1024, 100, 0, 0, false, false,
	)
	requireNoError(t, err, "incremental update")

	got, err := d.GetSession(ctx, "normal-single")
	requireNoError(t, err, "get normal-single")
	if got.IsAutomated {
		t.Error("is_automated should stay 0 for non-matching first_message")
	}
}

func TestBackfillIsAutomatedBumpsLocalModifiedAt(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Seed a single-turn roborev session that the new classifier
	// will flip to is_automated = 1.
	insertSession(t, d, "to-flip", "proj", func(s *Session) {
		fm := "You are a code reviewer. Review the code."
		s.FirstMessage = &fm
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	// Force is_automated = 0 so the backfill has work to do.
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'to-flip'",
	)
	requireNoError(t, err, "force to-flip to 0")

	// Snapshot local_modified_at before the backfill.
	before, err := d.GetSessionFull(ctx, "to-flip")
	requireNoError(t, err, "get to-flip before")
	var beforeLM string
	if before.LocalModifiedAt != nil {
		beforeLM = *before.LocalModifiedAt
	}

	// SQLite's strftime('now') ticks at millisecond precision.
	// Sleep a few ms so a re-set produces a strictly later value.
	// (Mirrors internal/db/signals_test.go:164.)
	time.Sleep(5 * time.Millisecond)

	// Clear the marker so the backfill runs.
	_, err = d.getWriter().Exec(
		"DELETE FROM stats WHERE key = ?",
		ClassifierHashKey,
	)
	requireNoError(t, err, "clear marker")

	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	requireNoError(t, err, "backfill run")

	after, err := d.GetSessionFull(ctx, "to-flip")
	requireNoError(t, err, "get to-flip after")
	if !after.IsAutomated {
		t.Fatal("to-flip should be automated after backfill")
	}
	if after.LocalModifiedAt == nil || *after.LocalModifiedAt == "" {
		t.Fatal("local_modified_at not set after backfill")
	}
	if *after.LocalModifiedAt <= beforeLM {
		t.Errorf(
			"local_modified_at not bumped: before=%q after=%q",
			beforeLM, *after.LocalModifiedAt,
		)
	}
}

// TestBackfillIsAutomatedRerunsOnHashChange verifies that a
// classifier change (here, adding a user prefix) invalidates
// the stored hash and re-runs the backfill on next open,
// without any manual marker bump.
func TestBackfillIsAutomatedRerunsOnHashChange(t *testing.T) {
	t.Cleanup(func() { SetUserAutomationPrefixes(nil) })
	d := testDB(t)

	// Seed a session whose first_message would match a
	// user prefix once added. With the empty user-prefix
	// list it should be is_automated=0.
	insertSession(t, d, "essay", "proj", func(s *Session) {
		fm := "You are analyzing an essay about epistemology."
		s.FirstMessage = &fm
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	ctx := context.Background()
	pre, err := d.GetSession(ctx, "essay")
	requireNoError(t, err, "get essay before")
	if pre.IsAutomated {
		t.Fatalf("precondition: essay should be is_automated=0")
	}

	// Add a user prefix and re-run backfill. The new hash
	// should not equal the stored hash, so the backfill
	// runs and flips is_automated to 1.
	SetUserAutomationPrefixes([]string{"You are analyzing an essay"})
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	requireNoError(t, err, "backfill after prefix add")

	got, err := d.GetSession(ctx, "essay")
	requireNoError(t, err, "get essay after")
	if !got.IsAutomated {
		t.Error("essay should be is_automated=1 after user prefix added")
	}

	// A second backfill (no further classifier change) is a
	// no-op: stored hash now matches.
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET is_automated = 0 WHERE id = 'essay'",
	)
	requireNoError(t, err, "force back to 0")
	d.mu.Lock()
	err = d.backfillIsAutomatedLocked(d.getWriter())
	d.mu.Unlock()
	requireNoError(t, err, "second backfill")
	got, err = d.GetSession(ctx, "essay")
	requireNoError(t, err, "get essay second")
	if got.IsAutomated {
		t.Error("second backfill must be a no-op when hash unchanged")
	}
}
