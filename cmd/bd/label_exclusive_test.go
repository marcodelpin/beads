//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/labelns"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dolt"
	storeops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// newExclusiveLabelStore returns a test store with tier: and review:
// configured as exclusive label namespaces (bd-7u5ki).
func newExclusiveLabelStore(t *testing.T) *dolt.DoltStore {
	t.Helper()
	st := newTestStoreWithPrefix(t, filepath.Join(t.TempDir(), "test.db"), "test")
	if err := st.SetConfig(context.Background(), labelns.ConfigKey, "tier:,review"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	return st
}

func mustCreateLabeledIssue(t *testing.T, st *dolt.DoltStore, labels ...string) *types.Issue {
	t.Helper()
	issue := &types.Issue{Title: "x", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Labels: labels}
	if err := st.CreateIssue(context.Background(), issue, "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	return issue
}

func TestExclusiveLabels_AddLabelRejected(t *testing.T) {
	ctx := context.Background()
	st := newExclusiveLabelStore(t)
	issue := mustCreateLabeledIssue(t, st, "tier:fable")

	err := st.AddLabel(ctx, issue.ID, "tier:opus", "tester")
	if err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("expected exclusive-namespace rejection, got %v", err)
	}
	if !strings.Contains(err.Error(), "tier:fable") {
		t.Fatalf("error should name the existing label, got %v", err)
	}

	// Re-adding the same label stays idempotent.
	if err := st.AddLabel(ctx, issue.ID, "tier:fable", "tester"); err != nil {
		t.Fatalf("re-adding existing label: %v", err)
	}
	// The colon-less config form ("review") normalizes to review: and enforces.
	if err := st.AddLabel(ctx, issue.ID, "review:fable", "tester"); err != nil {
		t.Fatalf("first review: label: %v", err)
	}
	if err := st.AddLabel(ctx, issue.ID, "review:opus", "tester"); err == nil {
		t.Fatal("expected second review: label to be rejected")
	}
	// Labels outside exclusive namespaces stay free-form.
	if err := st.AddLabel(ctx, issue.ID, "area:x", "tester"); err != nil {
		t.Fatalf("non-namespaced label: %v", err)
	}
	if err := st.AddLabel(ctx, issue.ID, "area:y", "tester"); err != nil {
		t.Fatalf("second non-namespaced label: %v", err)
	}
}

// The genuine two-concurrent-writer regression for the check-then-insert
// race (bd-7u5ki) lives in internal/storage/domain/db
// (TestLabelExclusiveConcurrentAddsSerializeToOneWinner), not here: *dolt.
// DoltStore pins MaxOpenConns(1) for DOLT_CHECKOUT session affinity (see
// newTestStoreSharedBranch), so two goroutines calling st.AddLabel on the
// SAME store share ONE connection and are serialized by the pool itself
// before either transaction can observe the other's uncommitted state -
// exactly the shape that makes a race test pass whether or not the fix is
// present. The domain/db suite's plain *sql.DB carries no such limit, so two
// goroutines opening their own s.db.BeginTx() there create REAL overlapping
// transactions against the same real Dolt server.

func TestExclusiveLabels_UnconfiguredKeepsFreeFormLabels(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreWithPrefix(t, filepath.Join(t.TempDir(), "test.db"), "test")
	issue := mustCreateLabeledIssue(t, st, "tier:fable")

	if err := st.AddLabel(ctx, issue.ID, "tier:opus", "tester"); err != nil {
		t.Fatalf("unconfigured workspace must accept both labels: %v", err)
	}
	labels, _ := st.GetLabels(ctx, issue.ID)
	if len(labels) != 2 {
		t.Fatalf("expected 2 labels, got %v", labels)
	}
}

func TestExclusiveLabels_CreateWithConflictRejected(t *testing.T) {
	ctx := context.Background()
	st := newExclusiveLabelStore(t)

	issue := &types.Issue{Title: "x", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
		Labels: []string{"tier:fable", "tier:opus"}}
	err := st.CreateIssue(ctx, issue, "tester")
	if err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("expected create to reject conflicting labels, got %v", err)
	}
}

// TestExclusiveLabels_UpdateSwapsInOneCommand exercises the production path
// 'bd update --remove-label tier:fable --add-label tier:opus' now takes:
// issueops.Lifecycle.Update with one IssuePatch carrying both. The direct
// route (issueops.ApplyLabelPatch) computes the target label SET and removes
// before it adds, so a same-call swap must not trip the exclusivity guard.
func TestExclusiveLabels_UpdateSwapsInOneCommand(t *testing.T) {
	ctx := context.Background()
	st := newExclusiveLabelStore(t)
	issue := mustCreateLabeledIssue(t, st, "tier:fable")

	lifecycle, err := st.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle: %v", err)
	}
	if _, err := lifecycle.Update(ctx, issueops.UpdateRequest{
		Actor:   "tester",
		IssueID: issue.ID,
		Patch: issueops.IssuePatch{Labels: issueops.LabelPatch{
			Add:    []string{"tier:opus"},
			Remove: []string{"tier:fable"},
		}},
	}); err != nil {
		t.Fatalf("Update swap: %v", err)
	}
	labels, _ := st.GetLabels(ctx, issue.ID)
	if len(labels) != 1 || labels[0] != "tier:opus" {
		t.Fatalf("expected [tier:opus], got %v", labels)
	}
}

// TestExclusiveLabels_ReplaceSwapsAcrossTwoUpdates exercises the mechanism
// applyLabelAddReplace ('bd label add --replace') uses: remove the
// conflicting label in its OWN Update call, then add the new one in a
// second - the shape needed because the direct route (ApplyLabelPatch)
// removes-then-adds within one patch while the proxied route
// (domain/issue.go ApplyUpdate) adds-then-removes, so a single combined
// patch would swap on only one of the two backends.
func TestExclusiveLabels_ReplaceSwapsAcrossTwoUpdates(t *testing.T) {
	ctx := context.Background()
	st := newExclusiveLabelStore(t)
	issue := mustCreateLabeledIssue(t, st, "tier:fable")

	lifecycle, err := st.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle: %v", err)
	}
	prefixes := []string{"tier:", "review:"}
	toRemove := conflictingExclusiveLabels(prefixes, []string{"tier:opus"}, []string{"tier:fable"})
	if len(toRemove) != 1 || toRemove[0] != "tier:fable" {
		t.Fatalf("conflictingExclusiveLabels: expected [tier:fable], got %v", toRemove)
	}
	if _, err := lifecycle.Update(ctx, issueops.UpdateRequest{
		Actor: "tester", IssueID: issue.ID,
		Patch: issueops.IssuePatch{Labels: issueops.LabelPatch{Remove: toRemove}},
	}); err != nil {
		t.Fatalf("replace remove step: %v", err)
	}
	if _, err := lifecycle.Update(ctx, issueops.UpdateRequest{
		Actor: "tester", IssueID: issue.ID,
		Patch: issueops.IssuePatch{Labels: issueops.LabelPatch{Add: []string{"tier:opus"}}},
	}); err != nil {
		t.Fatalf("replace add step: %v", err)
	}
	labels, _ := st.GetLabels(ctx, issue.ID)
	if len(labels) != 1 || labels[0] != "tier:opus" {
		t.Fatalf("expected [tier:opus], got %v", labels)
	}

	// Without prefixes (no --replace), conflictingExclusiveLabels finds
	// nothing to remove, so the plain add trips the storage guard - matching
	// the un-replaced 'bd label add' behavior.
	if got := conflictingExclusiveLabels(nil, []string{"tier:fable"}, []string{"tier:opus"}); got != nil {
		t.Fatalf("expected no conflicts without configured prefixes, got %v", got)
	}
	if err := st.AddLabel(ctx, issue.ID, "tier:fable", "tester"); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("expected rejection without --replace, got %v", err)
	}
}

func TestExclusiveLabels_ImportWarnsAndKeepsLabels(t *testing.T) {
	ctx := context.Background()
	st := newExclusiveLabelStore(t)

	issue := &types.Issue{ID: "test-imp1", Title: "imported", Status: types.StatusOpen, Priority: 2,
		IssueType: types.TypeTask, Labels: []string{"tier:fable", "tier:opus"}}
	var reported [][3]string
	err := st.CreateIssuesWithFullOptions(ctx, []*types.Issue{issue}, "importer", storage.BatchCreateOptions{
		SkipPrefixValidation:       true,
		ExclusiveLabelConflictWarn: true,
		OnExclusiveLabelConflict: func(issueID, prefix string, labels []string) {
			reported = append(reported, [3]string{issueID, prefix, strings.Join(labels, ",")})
		},
	})
	if err != nil {
		t.Fatalf("import-style create must warn, not fail: %v", err)
	}
	if len(reported) == 0 {
		t.Fatal("expected the violation to be reported through the callback")
	}
	if reported[0][0] != "test-imp1" || reported[0][1] != "tier:" {
		t.Fatalf("unexpected report: %v", reported[0])
	}
	labels, _ := st.GetLabels(ctx, "test-imp1")
	if len(labels) != 2 {
		t.Fatalf("import must keep violating labels (no silent data loss), got %v", labels)
	}
}

func TestFindExclusiveLabelViolations(t *testing.T) {
	ctx := context.Background()
	st := newExclusiveLabelStore(t)
	prefixes := []string{"tier:", "review:"}

	// A violating issue (written import-style, bypassing the write guard),
	// a clean issue, and a closed violating issue that must be skipped.
	violating := &types.Issue{ID: "test-v1", Title: "v", Status: types.StatusOpen, Priority: 2,
		IssueType: types.TypeTask, Labels: []string{"tier:fable", "tier:opus"}}
	closed := &types.Issue{ID: "test-v2", Title: "c", Status: types.StatusClosed, Priority: 2,
		IssueType: types.TypeTask, Labels: []string{"tier:fable", "tier:opus"}}
	if err := st.CreateIssuesWithFullOptions(ctx, []*types.Issue{violating, closed}, "importer", storage.BatchCreateOptions{
		SkipPrefixValidation:       true,
		ExclusiveLabelConflictWarn: true,
	}); err != nil {
		t.Fatalf("seed violations: %v", err)
	}
	mustCreateLabeledIssue(t, st, "tier:fable", "area:x")

	violations, total, err := storeops.FindExclusiveLabelViolations(ctx, st.UnderlyingDB(), prefixes, 0)
	if err != nil {
		t.Fatalf("FindExclusiveLabelViolations: %v", err)
	}
	if len(violations) != 1 || total != 1 {
		t.Fatalf("expected exactly 1 violation (total 1), got %v total %d", violations, total)
	}
	v := violations[0]
	if v.IssueID != "test-v1" || v.Prefix != "tier:" || len(v.Labels) != 2 {
		t.Fatalf("unexpected violation: %+v", v)
	}

	// No prefixes configured: nothing to scan.
	none, noneTotal, err := storeops.FindExclusiveLabelViolations(ctx, st.UnderlyingDB(), nil, 0)
	if err != nil || none != nil || noneTotal != 0 {
		t.Fatalf("expected nil, 0, nil for empty prefixes, got %v, %d, %v", none, noneTotal, err)
	}
}

// TestDeleteCleansLabelNamespaceLocks pins the bda-dhcf cascade: the
// per-(issue,namespace) CAS rows are meaningless once their issue is gone,
// and nothing else deletes them, so DeleteIssue must - else the lock table
// grows with every historical labeled id.
func TestDeleteCleansLabelNamespaceLocks(t *testing.T) {
	ctx := context.Background()
	st := newExclusiveLabelStore(t)
	issue := mustCreateLabeledIssue(t, st, "tier:fable")

	countLocks := func() int {
		t.Helper()
		var n int
		if err := st.UnderlyingDB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM label_namespace_locks WHERE issue_id = ?`, issue.ID).Scan(&n); err != nil {
			t.Fatalf("count locks: %v", err)
		}
		return n
	}
	if got := countLocks(); got == 0 {
		t.Fatalf("positive control failed: creating a tier: label left no lock row - the probe cannot discriminate")
	}
	if err := st.DeleteIssue(ctx, issue.ID); err != nil {
		t.Fatalf("DeleteIssue: %v", err)
	}
	if got := countLocks(); got != 0 {
		t.Fatalf("delete left %d orphan lock row(s) for %s", got, issue.ID)
	}
}

// TestFindExclusiveLabelViolations_StreamingAndEscaping pins the bda-9krh
// rewrite: the scan streams one issue's labels at a time (the LAST group must
// still be flushed - two consecutive violating issues catch a dropped final
// flush) and the prefix pre-filter escapes LIKE metacharacters (a `_` in a
// prefix must not act as a single-character wildcard).
func TestFindExclusiveLabelViolations_StreamingAndEscaping(t *testing.T) {
	ctx := context.Background()
	st := newExclusiveLabelStore(t)

	seed := []*types.Issue{
		{ID: "test-s1", Title: "s1", Status: types.StatusOpen, Priority: 2,
			IssueType: types.TypeTask, Labels: []string{"tier:fable", "tier:opus"}},
		{ID: "test-s2", Title: "s2", Status: types.StatusOpen, Priority: 2,
			IssueType: types.TypeTask, Labels: []string{"tier:a", "tier:b"}},
		// Matches "a_b:" only if the underscore is treated as a wildcard:
		// with correct escaping this issue is OUTSIDE the namespace.
		{ID: "test-s3", Title: "s3", Status: types.StatusOpen, Priority: 2,
			IssueType: types.TypeTask, Labels: []string{"aXb:one", "aXb:two"}},
	}
	if err := st.CreateIssuesWithFullOptions(ctx, seed, "importer", storage.BatchCreateOptions{
		SkipPrefixValidation:       true,
		ExclusiveLabelConflictWarn: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// retain=1 with two real violations also pins the bda-9krh cap: one
	// retained for display, the TOTAL still counted in full.
	violations, total, err := storeops.FindExclusiveLabelViolations(ctx, st.UnderlyingDB(), []string{"tier:", "a_b:"}, 1)
	if err != nil {
		t.Fatalf("FindExclusiveLabelViolations: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected total 2 (s1+s2, s3 excluded by LIKE escaping), got %d (%+v)", total, violations)
	}
	if len(violations) != 1 {
		t.Fatalf("retain=1 must keep exactly 1 violation, got %+v", violations)
	}
	violations, total, err = storeops.FindExclusiveLabelViolations(ctx, st.UnderlyingDB(), []string{"tier:", "a_b:"}, 0)
	if err != nil {
		t.Fatalf("FindExclusiveLabelViolations (retain=0): %v", err)
	}
	if len(violations) != 2 || total != 2 {
		t.Fatalf("expected 2 violations total 2, got %d total %d", len(violations), total)
	}
	got := map[string]bool{}
	for _, v := range violations {
		got[v.IssueID] = true
		if v.Prefix != "tier:" {
			t.Fatalf("unexpected prefix in %+v", v)
		}
	}
	if !got["test-s1"] || !got["test-s2"] {
		t.Fatalf("last-group flush lost a violation: %+v", violations)
	}
}

func TestDropConflictingInheritedLabels(t *testing.T) {
	prefixes := []string{"tier:"}

	got := dropConflictingInheritedLabels([]string{"tier:opus"}, []string{"tier:fable", "area:x"}, prefixes)
	if len(got) != 1 || got[0] != "area:x" {
		t.Fatalf("explicit tier: label should evict inherited one, got %v", got)
	}
	got = dropConflictingInheritedLabels([]string{"area:y"}, []string{"tier:fable"}, prefixes)
	if len(got) != 1 || got[0] != "tier:fable" {
		t.Fatalf("inherited label without explicit conflict should survive, got %v", got)
	}
	got = dropConflictingInheritedLabels([]string{"tier:opus"}, []string{"tier:fable"}, nil)
	if len(got) != 1 || got[0] != "tier:fable" {
		t.Fatalf("no exclusive prefixes: inheritance unchanged, got %v", got)
	}
}
