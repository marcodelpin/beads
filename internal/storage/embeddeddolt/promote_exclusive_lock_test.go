//go:build cgo

package embeddeddolt_test

import (
	"testing"

	"github.com/steveyegge/beads/internal/labelns"
	"github.com/steveyegge/beads/internal/types"
)

// TestPromoteLocksExclusiveNamespaces is the promote half of bda-qvff: the
// promotion label copy (INSERT IGNORE ... SELECT FROM wisp_labels)
// materializes a never-enforced wisp label set into the enforced labels
// table without going through AddLabelInTx, so before the fix it never
// touched label_namespace_locks - a concurrent add into the same exclusive
// namespace could not collide with it at commit. The copy must acquire the
// per-(issue, namespace) lock for every configured-exclusive namespace in
// the copied set; the observable is the lock row.
func TestPromoteLocksExclusiveNamespaces(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	ctx := t.Context()

	t.Run("configured namespace locks on promote", func(t *testing.T) {
		te := newTestEnv(t, "pxl")
		te.exec(t, ctx, "REPLACE INTO config (`key`, value) VALUES (?, ?)", labelns.ConfigKey, "tier:")
		if err := te.store.CreateIssue(ctx, &types.Issue{
			ID: "pxl-w", Title: "exclusive-labelled wisp", Status: types.StatusOpen,
			Priority: 2, IssueType: types.TypeTask, Ephemeral: true,
			Labels: []string{"tier:fable", "plain"},
		}, "tester"); err != nil {
			t.Fatalf("create wisp: %v", err)
		}
		te.assertRowExists(t, ctx, "wisps", "pxl-w")

		// The wisp-side add path already wrote a lock row at create time
		// (the shared choke point covers wisp_labels too). Clear it so the
		// assertion below can only be satisfied by PROMOTION's own lock
		// write - without this the test passes with the fix reverted.
		te.exec(t, ctx, "DELETE FROM label_namespace_locks WHERE issue_id = ?", "pxl-w")

		if err := te.store.PromoteFromEphemeral(ctx, "pxl-w", "tester"); err != nil {
			t.Fatalf("promote wisp: %v", err)
		}
		te.assertRowExists(t, ctx, "issues", "pxl-w")
		te.assertLabelCount(t, ctx, "labels", "pxl-w", 2)

		var lockRows int
		te.queryScalar(t, ctx,
			"SELECT COUNT(*) FROM label_namespace_locks WHERE issue_id = ? AND namespace = ?",
			[]any{"pxl-w", "tier:"}, &lockRows)
		if lockRows != 1 {
			t.Errorf("label_namespace_locks rows for (pxl-w, tier:) = %d, want 1 (promotion must lock the namespaces its label copy touches)", lockRows)
		}
	})

	t.Run("unconfigured stays lock-free", func(t *testing.T) {
		te := newTestEnv(t, "pxn")
		if err := te.store.CreateIssue(ctx, &types.Issue{
			ID: "pxn-w", Title: "wisp without exclusive config", Status: types.StatusOpen,
			Priority: 2, IssueType: types.TypeTask, Ephemeral: true,
			Labels: []string{"tier:fable"},
		}, "tester"); err != nil {
			t.Fatalf("create wisp: %v", err)
		}
		if err := te.store.PromoteFromEphemeral(ctx, "pxn-w", "tester"); err != nil {
			t.Fatalf("promote wisp: %v", err)
		}
		var lockRows int
		te.queryScalar(t, ctx,
			"SELECT COUNT(*) FROM label_namespace_locks WHERE issue_id = ?",
			[]any{"pxn-w"}, &lockRows)
		if lockRows != 0 {
			t.Errorf("label_namespace_locks rows for pxn-w = %d, want 0 (no configured prefixes means the fast path: no lock writes)", lockRows)
		}
	})
}
