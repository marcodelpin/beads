package dolt

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
)

// TestSweeperContract runs the Sweeper contract against the server-backed
// store, which reaches internal/storage/issueops.SweepInTx through its own
// write transaction and is the ONE wiring that records a version-control entry
// for a sweep.
//
// The cases are subtests of one parent so the whole role suite shares one
// store and one copy-on-write branch. That sharing is what makes the id
// pattern in every case load-bearing rather than tidy: a sweep is asked of a
// whole TIER, so a case that swept without one would delete the next case's
// seeds. setupTestStore already marks the PARENT parallel; no subtest here
// calls t.Parallel, and RecordsAtMostOneHistoryEntry takes a before/after
// delta that is only meaningful while they run sequentially.
func TestSweeperContract(t *testing.T) {
	fixture, ctx, cleanup := newDoltSweeperFixture(t, "swp")
	defer cleanup()

	t.Run("RefusesAnUnfilteredDurableSweep", func(t *testing.T) {
		conformance.RunSweeperRefusesAnUnfilteredDurableSweep(t, ctx, fixture)
	})
	t.Run("RefusesAMalformedRequest", func(t *testing.T) {
		conformance.RunSweeperRefusesAMalformedRequest(t, ctx, fixture)
	})
	t.Run("ClearsOneTierAndLeavesTheOther", func(t *testing.T) {
		conformance.RunSweeperClearsOneTierAndLeavesTheOther(t, ctx, fixture)
	})
	t.Run("ProtectsPinnedRows", func(t *testing.T) {
		conformance.RunSweeperProtectsPinnedRows(t, ctx, fixture)
	})
	t.Run("HonorsTheCutoffAndThePattern", func(t *testing.T) {
		conformance.RunSweeperHonorsTheCutoffAndThePattern(t, ctx, fixture)
	})
	t.Run("DryRunChangesNothing", func(t *testing.T) {
		conformance.RunSweeperDryRunChangesNothing(t, ctx, fixture)
	})
	t.Run("ProtectsCitedRows", func(t *testing.T) {
		conformance.RunSweeperProtectsCitedRows(t, ctx, fixture)
	})
	t.Run("EmptyMatchIsZeroAndNil", func(t *testing.T) {
		conformance.RunSweeperEmptyMatchIsZeroAndNil(t, ctx, fixture)
	})
	t.Run("RecordsAtMostOneHistoryEntry", func(t *testing.T) {
		conformance.RunSweeperRecordsAtMostOneHistoryEntry(t, ctx, fixture)
	})
	t.Run("DoesNotMutateTheCallerRequest", func(t *testing.T) {
		conformance.RunSweeperDoesNotMutateTheCallerRequest(t, ctx, fixture)
	})
}

// newDoltSweeperFixture composes the frozen role kit with this backend's
// accessor.
func newDoltSweeperFixture(t *testing.T, prefix string) (conformance.SweeperFixture, context.Context, func()) {
	t.Helper()
	store, storeCleanup := setupTestStore(t)
	ctx, cancel := testContext(t)
	stop := func() {
		cancel()
		storeCleanup()
	}
	sweeper, err := store.Sweeper()
	if err != nil {
		stop()
		t.Fatalf("Sweeper(): %v", err)
	}
	kit := newDoltRoleFixtureKit(store, prefix)
	return conformance.SweeperFixture{
		IssuePrefix:  kit.IssuePrefix,
		Sweeper:      sweeper,
		CreateIssue:  kit.CreateIssue,
		CreateWisp:   kit.CreateWisp,
		QueryScalar:  kit.QueryScalar,
		CountHistory: kit.CountHistory,
	}, ctx, stop
}
