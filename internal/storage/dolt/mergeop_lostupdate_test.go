package dolt

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// TestConcurrentMergeOpLostUpdate is a regression guard for the merge-op
// lost-update defect: read-merge-write operations (--set-metadata,
// append_notes) are resolved INSIDE the mutation transaction, so a
// commit-conflict retry re-derives the merge against the winning writer's
// committed row instead of a stale snapshot. The rationale, and the incident
// it guards, are documented at issueops.readIssueAndResolveMergeOps
// (internal/storage/issueops/update.go): "Merging outside the mutation
// transaction -- the CLI's old behavior -- silently erased concurrent
// committed writes to sibling keys (GH audit: 7 of 200 exit-0 --set-metadata
// writes lost)."
//
// # WHY THERE IS NO INTERLEAVING FORCER
//
// There is no way to force a deterministic interleave against Dolt. An open
// transaction holding a no-op write ("UPDATE issues SET title = title WHERE
// id = ?") does NOT block a concurrent writer on the same row. Measured
// directly on both dolt 2.1.6 and 2.2.0: baseline write with no holder 14ms,
// concurrent write while a holder sat in an open transaction 15ms -- i.e.
// indistinguishable. Dolt has no real row locking; FOR UPDATE and SKIP
// LOCKED are parse-only no-ops (see the "no real row locking" comments in
// issues.go and issueops/get_issue.go). A lock-based forcer would force
// NOTHING, and a test built on one would pass identically with and without
// the defect -- worse than no test, because it would look like coverage.
//
// # WHAT THIS TEST RELIES ON INSTEAD
//
// Genuine goroutine concurrency: several writers, each contributing a
// DISTINCT metadata key (or note line) to the SAME issue. Note the real
// concurrency ceiling: setupConcurrentTestStore sets MaxOpenConns=2, so at
// most TWO transactions are ever in flight regardless of how many goroutines
// are launched. That is enough -- the defect needs only two overlapping
// read/commit windows -- but it means this exercises PAIRWISE overlap, not
// N-way. Rounds exist to get many independent chances at that overlap.
//
// # WHY EVERY ROUND ASSERTS A MINIMUM SUCCESS COUNT
//
// The invariant below only examines writers that reported success. That
// creates a vacuous-pass hole: if contention starves all but one writer (the
// rest exhausting withRetryTx), the surviving writer never had to merge
// against a sibling, so its key is trivially present and the round goes green
// WITH the defect fully present. The degenerate version is worse -- if the
// harness is misconfigured and every write errors, zero keys are checked,
// "missing" is empty, and the test passes having verified nothing.
//
// requireInformativeRound closes that hole: a round with fewer than two
// successful concurrent writers cannot say anything about the invariant, so
// it is a hard failure rather than a silent pass. Note this is NECESSARY, not
// sufficient: two successes could still have been serialized. It rules out
// the uninformative round, it does not prove overlap occurred.
func TestConcurrentMergeOpLostUpdate(t *testing.T) {
	t.Run("metadata_keys", testConcurrentMergeOpLostUpdateMetadata)
	t.Run("note_appends", testConcurrentMergeOpLostUpdateNotes)
}

const (
	lostUpdateRounds = 5
	lostUpdateWriter = 8

	// minConcurrentSuccesses is how many writers must BOTH report success in
	// a round for that round to be informative about the merge invariant.
	minConcurrentSuccesses = 2
)

// requireInformativeRound fails the round if too few writers succeeded for the
// invariant check to mean anything, and always logs the observed count so a
// green run at 2/8 is visually distinguishable from one at 8/8.
func requireInformativeRound(t *testing.T, round, successCount int) {
	t.Helper()
	t.Logf("round %d: %d/%d writers reported success", round, successCount, lostUpdateWriter)
	if successCount < minConcurrentSuccesses {
		t.Fatalf(
			"round %d: only %d/%d writers succeeded -- fewer than %d concurrent successes means the "+
				"invariant check below is UNINFORMATIVE (a lone writer never merges against a sibling, "+
				"so its key is trivially present). Failing loudly rather than reporting a green run that "+
				"verified nothing. Investigate contention/retry exhaustion or a broken test harness.",
			round, successCount, lostUpdateWriter, minConcurrentSuccesses)
	}
}

// testConcurrentMergeOpLostUpdateMetadata is the OpSetMetadata arm: writers
// each merge a DISTINCT "key=value" pair into the same issue, and every
// successful writer's key must survive in the final merged metadata.
func testConcurrentMergeOpLostUpdateMetadata(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx, cancel := concurrentTestContext(t)
	defer cancel()

	for round := 0; round < lostUpdateRounds; round++ {
		issueID := fmt.Sprintf("mergeop-lostupdate-meta-%d", round)
		issue := &types.Issue{
			ID:        issueID,
			Title:     "merge-op lost-update guard (metadata)",
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		}
		if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
			t.Fatalf("round %d: failed to create seed issue %s: %v", round, issueID, err)
		}

		var wg sync.WaitGroup
		succeeded := make([]bool, lostUpdateWriter)
		wantKey := make([]string, lostUpdateWriter)
		wantVal := make([]string, lostUpdateWriter)
		for i := 0; i < lostUpdateWriter; i++ {
			wantKey[i] = fmt.Sprintf("k%d", i)
			wantVal[i] = fmt.Sprintf("round%d-writer%d", round, i)
		}

		for n := 0; n < lostUpdateWriter; n++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				updates := map[string]interface{}{
					issueops.OpSetMetadata: []string{wantKey[n] + "=" + wantVal[n]},
				}
				// Each goroutine writes only its OWN index, so this slice needs
				// no mutex. t.Logf is safe from a non-test goroutine; t.Fatalf
				// would NOT be, which is why failures are recorded and asserted
				// after wg.Wait() rather than raised here.
				if err := store.UpdateIssue(ctx, issueID, updates, fmt.Sprintf("writer-%d", n)); err != nil {
					t.Logf("round %d writer %d: UpdateIssue failed (excluded from the invariant): %v", round, n, err)
					return
				}
				succeeded[n] = true
			}(n)
		}
		wg.Wait()

		successCount := 0
		for _, ok := range succeeded {
			if ok {
				successCount++
			}
		}
		requireInformativeRound(t, round, successCount)

		final, err := store.GetIssue(ctx, issueID)
		if err != nil {
			t.Fatalf("round %d: failed to read back issue %s after concurrent writes: %v", round, issueID, err)
		}
		got := map[string]any{}
		if len(final.Metadata) > 0 {
			if err := json.Unmarshal(final.Metadata, &got); err != nil {
				t.Fatalf("round %d: failed to unmarshal final metadata %q: %v", round, string(final.Metadata), err)
			}
		}

		var missing []string
		for n := 0; n < lostUpdateWriter; n++ {
			if !succeeded[n] {
				continue
			}
			gotVal, ok := got[wantKey[n]]
			if !ok || gotVal != wantVal[n] {
				missing = append(missing, fmt.Sprintf("%s=%s (found: %v)", wantKey[n], wantVal[n], gotVal))
			}
		}

		if len(missing) > 0 {
			t.Errorf(
				"round %d: LOST UPDATE on issue %s -- %d of %d SUCCESSFUL --set-metadata writes vanished: %s\n"+
					"every listed writer got UpdateIssue()==nil yet its key is absent or wrong in the final "+
					"merged metadata, which means some writer's merge ran against a stale snapshot and "+
					"overwrote a sibling key instead of folding it in",
				round, issueID, len(missing), successCount, strings.Join(missing, "; "))
		}
	}
}

// testConcurrentMergeOpLostUpdateNotes is the OpAppendNotes arm: writers each
// append a DISTINCT line to the same issue, and every successful writer's line
// must be present in the final Notes.
func testConcurrentMergeOpLostUpdateNotes(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx, cancel := concurrentTestContext(t)
	defer cancel()

	for round := 0; round < lostUpdateRounds; round++ {
		issueID := fmt.Sprintf("mergeop-lostupdate-notes-%d", round)
		issue := &types.Issue{
			ID:        issueID,
			Title:     "merge-op lost-update guard (notes)",
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
		}
		if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
			t.Fatalf("round %d: failed to create seed issue %s: %v", round, issueID, err)
		}

		var wg sync.WaitGroup
		succeeded := make([]bool, lostUpdateWriter)
		wantLine := make([]string, lostUpdateWriter)
		for i := 0; i < lostUpdateWriter; i++ {
			wantLine[i] = fmt.Sprintf("round%d-line-from-writer-%d", round, i)
		}

		for n := 0; n < lostUpdateWriter; n++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				updates := map[string]interface{}{
					issueops.OpAppendNotes: wantLine[n],
				}
				if err := store.UpdateIssue(ctx, issueID, updates, fmt.Sprintf("writer-%d", n)); err != nil {
					t.Logf("round %d writer %d: UpdateIssue failed (excluded from the invariant): %v", round, n, err)
					return
				}
				succeeded[n] = true
			}(n)
		}
		wg.Wait()

		successCount := 0
		for _, ok := range succeeded {
			if ok {
				successCount++
			}
		}
		requireInformativeRound(t, round, successCount)

		final, err := store.GetIssue(ctx, issueID)
		if err != nil {
			t.Fatalf("round %d: failed to read back issue %s after concurrent writes: %v", round, issueID, err)
		}

		var missing []string
		for n := 0; n < lostUpdateWriter; n++ {
			if !succeeded[n] {
				continue
			}
			if !strings.Contains(final.Notes, wantLine[n]) {
				missing = append(missing, wantLine[n])
			}
		}

		if len(missing) > 0 {
			t.Errorf(
				"round %d: LOST UPDATE on issue %s -- %d of %d SUCCESSFUL append_notes writes vanished: %s\n"+
					"every listed writer got UpdateIssue()==nil yet its line is absent from the final Notes, "+
					"which means some writer's merge ran against a stale snapshot and dropped a sibling line "+
					"instead of folding it in",
				round, issueID, len(missing), successCount, strings.Join(missing, "; "))
		}
	}
}
