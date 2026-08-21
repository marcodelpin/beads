package conformance

import (
	"sort"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// This file holds the transaction cases whose whole point is that the two Dolt
// backends must answer them IDENTICALLY. Both are silent-wrong-answer shapes:
// nothing errors, nothing logs, and the caller is handed a plausible result
// that is wrong on one backend and right on the other. A caller cannot tell
// which backend it has, so a divergence here is a correctness bug in whichever
// leg deviates, not a documented difference.
//
// Each case takes its expectation from an ORACLE the two backends already
// agree on — the store-level form of the same operation — rather than from the
// other backend. Making two backends agree on the wrong answer would be worse
// than the divergence, because the disagreement is the only signal there is.

// testTransactionUpdateRecordsHistory pins Transaction.UpdateIssue to the same
// history the store-level UpdateIssue records (ga-1huib.7).
//
// The oracle is DoltStorage.UpdateIssue on the SAME store: both backends route
// it through issueops.UpdateIssueInTx, so an "updated" event for a real field
// change is not a backend opinion. Transaction.UpdateIssue is the same
// operation with a transaction around it, and storage.Transaction documents no
// carve-out for it — where event suppression IS intended on that interface it
// is spelled out (RemoveDependency vs RemoveDependencyWithOptions.EmitEvent).
//
// The untouched issue is the control: it proves the assertion is not satisfied
// trivially by every issue carrying an "updated" event, which would make a
// green run meaningless.
func testTransactionUpdateRecordsHistory(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()

	must(t, s.CreateIssue(c, withDefaults(&types.Issue{ID: "txh-store", Title: "Store edit"}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{ID: "txh-tx", Title: "Tx edit"}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{ID: "txh-untouched", Title: "Never edited"}), "a"))

	// Oracle: the same field change, outside a transaction.
	must(t, s.UpdateIssue(c, "txh-store", map[string]interface{}{"title": "Store edit, revised"}, "a"))

	// Subject: the same field change, inside a transaction.
	must(t, s.RunInTransaction(c, "bd: update txh-tx", func(tx storage.Transaction) error {
		return tx.UpdateIssue(c, "txh-tx", map[string]interface{}{"title": "Tx edit, revised"}, "a")
	}))

	oracle := countIssueEvents(t, s, "txh-store", types.EventUpdated)
	if oracle != 1 {
		t.Fatalf("oracle broken: store-level UpdateIssue recorded %d %q events, want 1", oracle, types.EventUpdated)
	}
	if control := countIssueEvents(t, s, "txh-untouched", types.EventUpdated); control != 0 {
		t.Fatalf("control broken: an issue that was never updated has %d %q events, want 0 — "+
			"the oracle assertion below cannot distinguish anything", control, types.EventUpdated)
	}

	if got := countIssueEvents(t, s, "txh-tx", types.EventUpdated); got != oracle {
		t.Errorf("Transaction.UpdateIssue recorded %d %q events, want %d (what the store-level "+
			"UpdateIssue records for the same change) — a transactional update must not have a "+
			"different audit trail from a plain one", got, types.EventUpdated, oracle)
	}
}

// testTransactionSearchIncludeDependencies pins Transaction.SearchIssues to
// honoring IssueFilter.IncludeDependencies (ga-1huib.8).
//
// A silently dropped filter field is worse than a missing one: a missing field
// is a compile error, a dropped one returns plausible wrong data. Two controls
// keep the positive assertion honest — an issue with NO dependencies must come
// back in the results with an empty slice (so the fixture cannot pass by having
// dependencies everywhere), and the same search with the flag OFF must hydrate
// nothing (so the case cannot pass on a backend that hydrates unconditionally
// and never reads the flag at all).
func testTransactionSearchIncludeDependencies(t *testing.T, f Factory) {
	s := f(t)
	c := ctx()

	must(t, s.CreateIssue(c, withDefaults(&types.Issue{ID: "txdep-blocker", Title: "TxDepHydration Blocker"}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{ID: "txdep-blocked", Title: "TxDepHydration Blocked"}), "a"))
	must(t, s.CreateIssue(c, withDefaults(&types.Issue{ID: "txdep-lone", Title: "TxDepHydration Lone"}), "a"))
	must(t, s.AddDependency(c, &types.Dependency{
		IssueID:     "txdep-blocked",
		DependsOnID: "txdep-blocker",
		Type:        types.DepBlocks,
	}, "a"))

	var on, off map[string][]*types.Dependency
	must(t, s.RunInTransaction(c, "bd: search txdep", func(tx storage.Transaction) error {
		hydrated, err := tx.SearchIssues(c, "TxDepHydration", types.IssueFilter{IncludeDependencies: true})
		if err != nil {
			return err
		}
		on = dependenciesByIssue(hydrated)

		plain, err := tx.SearchIssues(c, "TxDepHydration", types.IssueFilter{})
		if err != nil {
			return err
		}
		off = dependenciesByIssue(plain)
		return nil
	}))

	if len(on) != 3 {
		t.Fatalf("in-tx SearchIssues(IncludeDependencies) returned %d issues %v, want 3", len(on), issueIDsOf(on))
	}

	deps, ok := on["txdep-blocked"]
	if !ok {
		t.Fatalf("in-tx SearchIssues(IncludeDependencies) dropped txdep-blocked entirely")
	}
	if len(deps) != 1 {
		t.Errorf("txdep-blocked came back with %d dependencies, want 1 — IncludeDependencies was "+
			"accepted and ignored", len(deps))
	} else if deps[0].DependsOnID != "txdep-blocker" {
		t.Errorf("txdep-blocked depends on %q, want %q", deps[0].DependsOnID, "txdep-blocker")
	}

	// Control: a dependency-free issue is still returned, with nothing hydrated.
	lone, ok := on["txdep-lone"]
	if !ok {
		t.Errorf("control broken: txdep-lone (no dependencies) was dropped from the results")
	} else if len(lone) != 0 {
		t.Errorf("control broken: txdep-lone has no dependencies but %d were hydrated — the "+
			"fixture cannot tell a hydrated result from an unhydrated one", len(lone))
	}

	// Control: the flag is read, not ignored in the other direction.
	for _, id := range issueIDsOf(off) {
		if len(off[id]) != 0 {
			t.Errorf("control broken: %s hydrated %d dependencies without IncludeDependencies",
				id, len(off[id]))
		}
	}
}

// countIssueEvents counts an issue's history events of one type.
func countIssueEvents(t *testing.T, s storage.DoltStorage, id string, want types.EventType) int {
	t.Helper()
	events, err := s.GetEvents(ctx(), id, 0)
	if err != nil {
		t.Fatalf("GetEvents(%s): %v", id, err)
	}
	n := 0
	for _, e := range events {
		if e.EventType == want {
			n++
		}
	}
	return n
}

// dependenciesByIssue keys search results by issue ID so a case can assert on
// what was hydrated for each one, including the issues that got nothing.
func dependenciesByIssue(issues []*types.Issue) map[string][]*types.Dependency {
	byID := make(map[string][]*types.Dependency, len(issues))
	for _, issue := range issues {
		byID[issue.ID] = issue.Dependencies
	}
	return byID
}

// issueIDsOf returns the map's issue IDs in a stable order so failure messages
// name the same issues in the same order on every run.
func issueIDsOf(byID map[string][]*types.Dependency) []string {
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
