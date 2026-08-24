package uow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/labelns"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// TestImporterUOW pins the Importer role's contract on the unit-of-work
// backend: one ImportBatch call is one transaction and one history entry —
// rows with their aux data, memories, and the issue_prefix reconciliation
// together — and the in-transaction stale guard reports what it kept.
//
// ONE PROVIDER FOR THE WHOLE SUITE (it boots a real Dolt sql-server) and NO
// t.Parallel: dolt_log is database-global here, so a parallel subtest would
// move another subtest's history delta.
func TestImporterUOW(t *testing.T) {
	ctx := context.Background()
	provider := newUOWRoleFixtureProvider(t, ctx, "imp")
	kit := newUOWRoleFixtureKit(provider, "imp")

	source, ok := provider.(ImporterSource)
	if !ok {
		t.Fatalf("provider %T does not offer the Importer accessor", provider)
	}
	imp, err := source.Importer()
	if err != nil {
		t.Fatalf("Importer(): %v", err)
	}

	countHistory := func(t *testing.T) int {
		t.Helper()
		n, err := kit.CountHistory(ctx)
		if err != nil {
			t.Fatalf("CountHistory: %v", err)
		}
		return n
	}
	queryInt := func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := kit.QueryScalar(ctx, query, args, &n); err != nil {
			t.Fatalf("QueryScalar %q: %v", query, err)
		}
		return n
	}
	queryString := func(t *testing.T, query string, args ...any) string {
		t.Helper()
		var s string
		if err := kit.QueryScalar(ctx, query, args, &s); err != nil {
			t.Fatalf("QueryScalar %q: %v", query, err)
		}
		return s
	}

	t.Run("OneBatchIsOneHistoryEntryWithAuxDataAndMemories", func(t *testing.T) {
		before := countHistory(t)
		when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		result, err := imp.ImportBatch(ctx, publicops.ImportBatchRequest{
			Actor: "importer-test",
			Issues: []*types.Issue{
				{
					ID: "imp-one", Title: "Imported one", Status: types.StatusOpen,
					IssueType: types.TypeTask, Priority: 2,
					Labels:    []string{"lane:test", "imported"},
					Comments:  []*types.Comment{{ID: "imp-one-c1", Author: "importer-test", Text: "carried comment", CreatedAt: when}},
					CreatedAt: when, UpdatedAt: when,
				},
				{
					ID: "imp-two", Title: "Imported two", Status: types.StatusOpen,
					IssueType: types.TypeBug, Priority: 1,
					Dependencies: []*types.Dependency{{IssueID: "imp-two", DependsOnID: "imp-one", Type: types.DepBlocks}},
					CreatedAt:    when, UpdatedAt: when,
				},
			},
			Memories: []publicops.ImportMemory{{Key: "kv.memory.importer-probe", Value: "remembered"}},
			Source:   "importer_uow_test.jsonl",
		})
		if err != nil {
			t.Fatalf("ImportBatch: %v", err)
		}
		if result.Created != 2 {
			t.Errorf("Created = %d, want 2", result.Created)
		}
		if result.MemoriesImported != 1 {
			t.Errorf("MemoriesImported = %d, want 1", result.MemoriesImported)
		}
		if len(result.StaleRejectedIDs) != 0 {
			t.Errorf("StaleRejectedIDs = %v, want none", result.StaleRejectedIDs)
		}

		if after := countHistory(t); after != before+1 {
			t.Errorf("history entries = %d, want %d (ONE commit for the whole batch)", after, before+1)
		}
		// Direct content assertions, not just row counts: the aux data must
		// actually be present on the imported rows.
		if got := queryInt(t, "SELECT COUNT(*) FROM issues WHERE id IN ('imp-one','imp-two')"); got != 2 {
			t.Errorf("issue rows = %d, want 2", got)
		}
		if got := queryInt(t, "SELECT COUNT(*) FROM labels WHERE issue_id = 'imp-one'"); got != 2 {
			t.Errorf("imp-one labels = %d, want 2", got)
		}
		if got := queryInt(t, "SELECT COUNT(*) FROM comments WHERE issue_id = 'imp-one'"); got != 1 {
			t.Errorf("imp-one comments = %d, want 1", got)
		}
		if got := queryInt(t, "SELECT COUNT(*) FROM dependencies WHERE issue_id = 'imp-two' AND depends_on_issue_id = 'imp-one' AND type = 'blocks'"); got != 1 {
			t.Errorf("imp-two blocks edge = %d, want 1", got)
		}
		if got := queryString(t, "SELECT value FROM config WHERE `key` = 'kv.memory.importer-probe'"); got != "remembered" {
			t.Errorf("memory value = %q, want %q", got, "remembered")
		}
		if msg := queryString(t, "SELECT message FROM dolt_log ORDER BY date DESC, commit_hash LIMIT 1"); !strings.Contains(msg, "bd import: 2 issues, 1 memories from importer_uow_test.jsonl") {
			t.Errorf("history message = %q, want the bd import message", msg)
		}
	})

	t.Run("ReimportConvergesWithoutDuplicatingAuxData", func(t *testing.T) {
		when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		row := func() *types.Issue {
			return &types.Issue{
				ID: "imp-one", Title: "Imported one", Status: types.StatusOpen,
				IssueType: types.TypeTask, Priority: 2,
				Labels:    []string{"lane:test", "imported"},
				Comments:  []*types.Comment{{ID: "imp-one-c1", Author: "importer-test", Text: "carried comment", CreatedAt: when}},
				CreatedAt: when, UpdatedAt: when,
			}
		}
		if _, err := imp.ImportBatch(ctx, publicops.ImportBatchRequest{
			Actor: "importer-test", Issues: []*types.Issue{row()}, Source: "again.jsonl",
		}); err != nil {
			t.Fatalf("re-import: %v", err)
		}
		if got := queryInt(t, "SELECT COUNT(*) FROM labels WHERE issue_id = 'imp-one'"); got != 2 {
			t.Errorf("labels after re-import = %d, want 2 (idempotent merge)", got)
		}
		if got := queryInt(t, "SELECT COUNT(*) FROM comments WHERE issue_id = 'imp-one'"); got != 1 {
			t.Errorf("comments after re-import = %d, want 1 (idempotent merge)", got)
		}
	})

	t.Run("StaleGuardRejectsInsideTheTransactionUnlessAllowed", func(t *testing.T) {
		old := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		staleRow := func() *types.Issue {
			return &types.Issue{
				ID: "imp-one", Title: "Stale snapshot title", Status: types.StatusOpen,
				IssueType: types.TypeTask, Priority: 3,
				CreatedAt: old, UpdatedAt: old,
			}
		}
		result, err := imp.ImportBatch(ctx, publicops.ImportBatchRequest{
			Actor: "importer-test", Issues: []*types.Issue{staleRow()}, Source: "stale.jsonl",
		})
		if err != nil {
			t.Fatalf("stale import: %v", err)
		}
		if len(result.StaleRejectedIDs) != 1 || result.StaleRejectedIDs[0] != "imp-one" {
			t.Fatalf("StaleRejectedIDs = %v, want [imp-one]", result.StaleRejectedIDs)
		}
		if result.Created != 0 {
			t.Errorf("Created = %d, want 0 (the only row was rejected)", result.Created)
		}
		if got := queryString(t, "SELECT title FROM issues WHERE id = 'imp-one'"); got != "Imported one" {
			t.Errorf("title after stale import = %q, want local row kept", got)
		}

		if _, err := imp.ImportBatch(ctx, publicops.ImportBatchRequest{
			Actor: "importer-test", Issues: []*types.Issue{staleRow()}, AllowStale: true, Source: "stale.jsonl",
		}); err != nil {
			t.Fatalf("allow-stale import: %v", err)
		}
		if got := queryString(t, "SELECT title FROM issues WHERE id = 'imp-one'"); got != "Stale snapshot title" {
			t.Errorf("title after --allow-stale = %q, want the older snapshot restored", got)
		}
	})

	t.Run("EmptyBatchCommitsNothing", func(t *testing.T) {
		before := countHistory(t)
		result, err := imp.ImportBatch(ctx, publicops.ImportBatchRequest{Actor: "importer-test", Source: "empty.jsonl"})
		if err != nil {
			t.Fatalf("empty ImportBatch: %v", err)
		}
		if result.Created != 0 || result.MemoriesImported != 0 || result.PrefixSynced {
			t.Errorf("empty batch result = %+v, want zero outcome", result)
		}
		if after := countHistory(t); after != before {
			t.Errorf("history entries moved %d -> %d on an empty batch", before, after)
		}
	})

	t.Run("PrefixSyncAloneCommitsUnderTheSyncMessage", func(t *testing.T) {
		result, err := imp.ImportBatch(ctx, publicops.ImportBatchRequest{
			Actor: "importer-test", SyncIssuePrefix: "impx", Source: "prefix.jsonl",
		})
		if err != nil {
			t.Fatalf("prefix-sync ImportBatch: %v", err)
		}
		if !result.PrefixSynced {
			t.Fatalf("PrefixSynced = false, want true")
		}
		if got := queryString(t, "SELECT value FROM config WHERE `key` = 'issue_prefix'"); got != "impx" {
			t.Errorf("issue_prefix = %q, want %q", got, "impx")
		}
		// Restore for any later subtest.
		if err := kit.SetConfig(ctx, "issue_prefix", "imp"); err != nil {
			t.Fatalf("restore issue_prefix: %v", err)
		}
	})

	t.Run("EmptyActorIsRefused", func(t *testing.T) {
		if _, err := imp.ImportBatch(ctx, publicops.ImportBatchRequest{Source: "noactor.jsonl"}); err == nil {
			t.Fatal("ImportBatch with empty actor should be refused")
		}
	})

	t.Run("ExclusiveNamespaceViolationWarnsAndKeepsLabels", func(t *testing.T) {
		// bda-bt21: the proxied importer honors the import contract for
		// exclusive label namespaces - warn and keep the labels as written -
		// instead of hard-failing the batch the way interactive creates do.
		// The policy is installed BEFORE the import so the incoming row
		// violates it, the historical-data case the contract exists for.
		if err := kit.SetConfig(ctx, labelns.ConfigKey, "tier:"); err != nil {
			t.Fatalf("SetConfig(%s): %v", labelns.ConfigKey, err)
		}
		defer func() {
			if err := kit.SetConfig(ctx, labelns.ConfigKey, ""); err != nil {
				t.Fatalf("reset %s: %v", labelns.ConfigKey, err)
			}
		}()
		when := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
		result, err := imp.ImportBatch(ctx, publicops.ImportBatchRequest{
			Actor: "importer-test",
			Issues: []*types.Issue{{
				ID: "imp-excl", Title: "Historic labels predate the policy", Status: types.StatusOpen,
				IssueType: types.TypeTask, Priority: 2,
				Labels:    []string{"tier:fable", "tier:opus"},
				CreatedAt: when, UpdatedAt: when,
			}},
			Source: "importer_uow_test.jsonl",
		})
		if err != nil {
			t.Fatalf("ImportBatch with exclusive-namespace violation: %v (import must warn, not fail)", err)
		}
		if result.Created != 1 {
			t.Errorf("Created = %d, want 1", result.Created)
		}
		// The violating labels are KEPT (no silent data loss)...
		if got := queryInt(t, "SELECT COUNT(*) FROM labels WHERE issue_id = 'imp-excl' AND label LIKE 'tier:%'"); got != 2 {
			t.Errorf("imp-excl tier labels = %d, want 2 (labels kept as written)", got)
		}
		// ...and the violation is REPORTED, in the classic import's format.
		if len(result.ExclusiveLabelConflicts) != 1 {
			t.Fatalf("ExclusiveLabelConflicts = %v, want exactly 1 entry", result.ExclusiveLabelConflicts)
		}
		want := `imp-excl: namespace "tier:" has tier:fable, tier:opus`
		if result.ExclusiveLabelConflicts[0] != want {
			t.Errorf("conflict = %q, want %q", result.ExclusiveLabelConflicts[0], want)
		}
	})
}
