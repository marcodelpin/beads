package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestCheckOrphanLabelNamespaceLocks seeds one lock row whose issue exists
// (must NOT be flagged - the positive control that the join is right) and one
// whose issue never existed (must be flagged), and pins both the count and
// the example naming in the resulting check.
func TestCheckOrphanLabelNamespaceLocks(t *testing.T) {
	store := newTestDoltStore(t, "oll")
	ctx := context.Background()

	live := &types.Issue{ID: "oll-live", Title: "Live", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := store.CreateIssue(ctx, live, "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	for _, row := range [][2]string{
		{"oll-live", "tier:"},    // live issue: not an orphan
		{"oll-ghost", "tier:"},   // no such issue: orphan
		{"oll-ghost", "review:"}, // second namespace, same ghost: second orphan row
	} {
		if _, err := store.UnderlyingDB().ExecContext(ctx,
			"INSERT INTO label_namespace_locks (issue_id, namespace, row_lock) VALUES (?, ?, ?)",
			row[0], row[1], 42); err != nil {
			t.Fatalf("seed lock row %v: %v", row, err)
		}
	}

	check := checkOrphanLabelNamespaceLocks(ctx, store)
	if check.Status != StatusWarning {
		t.Fatalf("status = %s, want %s (check: %+v)", check.Status, StatusWarning, check)
	}
	if !strings.Contains(check.Message, "2 label_namespace_locks row(s)") {
		t.Errorf("message should count exactly the 2 orphan rows, got: %s", check.Message)
	}
	if !strings.Contains(check.Detail, "oll-ghost (review:)") || !strings.Contains(check.Detail, "oll-ghost (tier:)") {
		t.Errorf("detail should name both orphan rows, got: %s", check.Detail)
	}
	if strings.Contains(check.Detail, "oll-live") {
		t.Errorf("detail must not flag the live issue's lock row, got: %s", check.Detail)
	}
	if !strings.Contains(check.Fix, "DELETE l FROM label_namespace_locks") {
		t.Errorf("fix should carry the one-shot cleanup, got: %s", check.Fix)
	}

	// Run the Fix's own DELETE, then the check must go OK - proving the
	// suggested cleanup and the detection agree on the population.
	if _, err := store.UnderlyingDB().ExecContext(ctx,
		`DELETE l FROM label_namespace_locks l
		 LEFT JOIN issues i ON i.id = l.issue_id
		 LEFT JOIN wisps w ON w.id = l.issue_id
		 WHERE i.id IS NULL AND w.id IS NULL`); err != nil {
		t.Fatalf("cleanup DELETE: %v", err)
	}
	after := checkOrphanLabelNamespaceLocks(ctx, store)
	if after.Status != StatusOK {
		t.Fatalf("post-cleanup status = %s, want %s (check: %+v)", after.Status, StatusOK, after)
	}

	// The live row must have survived the cleanup (the DELETE's join is the
	// same predicate the check uses; both must spare live issues).
	var liveRows int
	if err := store.UnderlyingDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM label_namespace_locks WHERE issue_id = ?", "oll-live").Scan(&liveRows); err != nil {
		t.Fatalf("count live lock rows: %v", err)
	}
	if liveRows != 1 {
		t.Errorf("live issue's lock row count = %d, want 1 (cleanup must not touch it)", liveRows)
	}
}
