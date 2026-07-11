package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// provisionPair opens TWO independent stores on the same SQLite file. Each store
// holds its own *sql.DB, so the pair models two bd processes: separate connection
// pools, separate lock-domain participants, serialized only by SQLite's file
// locks (_txlock=immediate) — not by Go-side pooling.
func provisionPair(t *testing.T) (storage.DoltStorage, storage.DoltStorage) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mergeops.db")
	a, err := Provision(ctx, dbPath)
	if err != nil {
		t.Fatalf("Provision(a): %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := Provision(ctx, dbPath)
	if err != nil {
		t.Fatalf("Provision(b): %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if err := a.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		t.Fatalf("SetConfig(issue_prefix): %v", err)
	}
	return a, b
}

func metadataMap(t *testing.T, st storage.DoltStorage, id string) map[string]any {
	t.Helper()
	issue, err := st.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", id, err)
	}
	if issue == nil {
		t.Fatalf("GetIssue(%s): issue not found", id)
	}
	out := map[string]any{}
	if len(issue.Metadata) > 0 {
		if err := json.Unmarshal(issue.Metadata, &out); err != nil {
			t.Fatalf("unmarshal metadata %q: %v", issue.Metadata, err)
		}
	}
	return out
}

// TestConcurrentDistinctKeyMetadataUpdates_TwoHandles is the regression test for
// the concurrent-metadata lost-update defect: two processes writing DIFFERENT
// metadata keys of the SAME issue must both survive. Before the fix, the CLI
// merged --set-metadata into a snapshot of the issue read in an EARLIER
// transaction and then overwrote the entire metadata column, so exit-0 writes
// were silently erased by concurrent writers (7 of 200 lost in the audit
// hammer). The fix resolves the merge INSIDE the mutation transaction via the
// "_set_metadata"/"_unset_metadata" update operations, which the two handles'
// write transactions serialize (SQLite: _txlock=immediate BEGIN→COMMIT).
func TestConcurrentDistinctKeyMetadataUpdates_TwoHandles(t *testing.T) {
	ctx := context.Background()
	storeA, storeB := provisionPair(t)

	issue := withDefaults(&types.Issue{ID: "test-meta-race", Title: "metadata race target"})
	if err := storeA.CreateIssue(ctx, issue, "actor"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	const rounds = 25
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		errCh := make(chan error, 2)
		for prefix, st := range map[string]storage.DoltStorage{"a": storeA, "b": storeB} {
			wg.Add(1)
			go func(prefix string, st storage.DoltStorage) {
				defer wg.Done()
				updates := map[string]interface{}{
					"_set_metadata": []string{fmt.Sprintf("%s%d=1", prefix, i)},
				}
				if err := st.UpdateIssue(ctx, issue.ID, updates, "writer-"+prefix); err != nil {
					errCh <- fmt.Errorf("round %d writer %s: %w", i, prefix, err)
				}
			}(prefix, st)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}
	}

	got := metadataMap(t, storeA, issue.ID)
	var missing []string
	for i := 0; i < rounds; i++ {
		for _, prefix := range []string{"a", "b"} {
			key := fmt.Sprintf("%s%d", prefix, i)
			if _, ok := got[key]; !ok {
				missing = append(missing, key)
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("lost %d of %d committed metadata keys (silent lost update): missing %v",
			len(missing), rounds*2, missing)
	}
}

// TestConcurrentAppendNotes_TwoHandles covers the sibling snapshot-merge pattern
// for notes: --append-notes used to concatenate onto a stale CLI-side snapshot,
// so concurrent appends from two processes erased each other. With the
// "append_notes" operation resolved in-tx, every appended line must survive.
func TestConcurrentAppendNotes_TwoHandles(t *testing.T) {
	ctx := context.Background()
	storeA, storeB := provisionPair(t)

	issue := withDefaults(&types.Issue{ID: "test-notes-race", Title: "notes race target"})
	if err := storeA.CreateIssue(ctx, issue, "actor"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	const rounds = 15
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		errCh := make(chan error, 2)
		for prefix, st := range map[string]storage.DoltStorage{"a": storeA, "b": storeB} {
			wg.Add(1)
			go func(prefix string, st storage.DoltStorage) {
				defer wg.Done()
				updates := map[string]interface{}{
					"append_notes": fmt.Sprintf("line-%s%d", prefix, i),
				}
				if err := st.UpdateIssue(ctx, issue.ID, updates, "writer-"+prefix); err != nil {
					errCh <- fmt.Errorf("round %d writer %s: %w", i, prefix, err)
				}
			}(prefix, st)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}
	}

	final, err := storeA.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	var missing []string
	for i := 0; i < rounds; i++ {
		for _, prefix := range []string{"a", "b"} {
			line := fmt.Sprintf("line-%s%d", prefix, i)
			if !containsLine(final.Notes, line) {
				missing = append(missing, line)
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("lost %d of %d appended note lines: missing %v\nnotes:\n%s",
			len(missing), rounds*2, missing, final.Notes)
	}
}

func containsLine(notes, line string) bool {
	return slices.Contains(strings.Split(notes, "\n"), line)
}

// TestUpdateIssueMergeOps_InTxSemantics pins the semantics of the in-tx merge
// operations that replaced the CLI-side snapshot merge. All backends share this
// code via issueops.UpdateIssueInTx; SQLite is the cheapest real backend.
func TestUpdateIssueMergeOps_InTxSemantics(t *testing.T) {
	ctx := context.Background()
	st, _ := provisionPair(t)

	create := func(id string, metadata string, notes string) {
		t.Helper()
		issue := withDefaults(&types.Issue{ID: id, Title: "mergeops " + id, Notes: notes})
		if metadata != "" {
			issue.Metadata = json.RawMessage(metadata)
		}
		if err := st.CreateIssue(ctx, issue, "actor"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", id, err)
		}
	}

	t.Run("set_metadata_preserves_existing_keys", func(t *testing.T) {
		create("test-set", `{"keep":"x"}`, "")
		err := st.UpdateIssue(ctx, "test-set", map[string]interface{}{
			"_set_metadata": []string{"team=platform", "score=5"},
		}, "actor")
		if err != nil {
			t.Fatalf("UpdateIssue(_set_metadata): %v", err)
		}
		got := metadataMap(t, st, "test-set")
		if got["keep"] != "x" {
			t.Errorf("metadata[keep]: got %v, want %q", got["keep"], "x")
		}
		if got["team"] != "platform" {
			t.Errorf("metadata[team]: got %v, want %q", got["team"], "platform")
		}
		if got["score"] != float64(5) {
			t.Errorf("metadata[score]: got %v, want 5 (number-typed)", got["score"])
		}
	})

	t.Run("unset_metadata_removes_key", func(t *testing.T) {
		create("test-unset", `{"keep":"yes","drop":"yes"}`, "")
		err := st.UpdateIssue(ctx, "test-unset", map[string]interface{}{
			"_unset_metadata": []string{"drop"},
		}, "actor")
		if err != nil {
			t.Fatalf("UpdateIssue(_unset_metadata): %v", err)
		}
		got := metadataMap(t, st, "test-unset")
		if _, present := got["drop"]; present {
			t.Errorf("metadata[drop]: still present after unset: %v", got)
		}
		if got["keep"] != "yes" {
			t.Errorf("metadata[keep]: got %v, want %q", got["keep"], "yes")
		}
	})

	t.Run("merge_metadata_overlays_keys", func(t *testing.T) {
		create("test-merge", `{"existing":"old","stay":"put"}`, "")
		err := st.UpdateIssue(ctx, "test-merge", map[string]interface{}{
			"_merge_metadata": json.RawMessage(`{"existing":"new","added":1}`),
		}, "actor")
		if err != nil {
			t.Fatalf("UpdateIssue(_merge_metadata): %v", err)
		}
		got := metadataMap(t, st, "test-merge")
		if got["existing"] != "new" {
			t.Errorf("metadata[existing]: got %v, want %q", got["existing"], "new")
		}
		if got["stay"] != "put" {
			t.Errorf("metadata[stay]: got %v, want %q", got["stay"], "put")
		}
		if got["added"] != float64(1) {
			t.Errorf("metadata[added]: got %v, want 1", got["added"])
		}
	})

	t.Run("merge_metadata_onto_empty", func(t *testing.T) {
		create("test-merge-empty", "", "")
		err := st.UpdateIssue(ctx, "test-merge-empty", map[string]interface{}{
			"_merge_metadata": json.RawMessage(`{"fresh":"start"}`),
		}, "actor")
		if err != nil {
			t.Fatalf("UpdateIssue(_merge_metadata onto empty): %v", err)
		}
		got := metadataMap(t, st, "test-merge-empty")
		if got["fresh"] != "start" {
			t.Errorf("metadata[fresh]: got %v, want %q", got["fresh"], "start")
		}
	})

	t.Run("append_notes_concatenates_with_newline", func(t *testing.T) {
		create("test-append", "", "line1")
		err := st.UpdateIssue(ctx, "test-append", map[string]interface{}{
			"append_notes": "line2",
		}, "actor")
		if err != nil {
			t.Fatalf("UpdateIssue(append_notes): %v", err)
		}
		issue, err := st.GetIssue(ctx, "test-append")
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		if issue.Notes != "line1\nline2" {
			t.Errorf("notes: got %q, want %q", issue.Notes, "line1\nline2")
		}
	})

	t.Run("append_notes_onto_empty_has_no_leading_newline", func(t *testing.T) {
		create("test-append-empty", "", "")
		err := st.UpdateIssue(ctx, "test-append-empty", map[string]interface{}{
			"append_notes": "first",
		}, "actor")
		if err != nil {
			t.Fatalf("UpdateIssue(append_notes onto empty): %v", err)
		}
		issue, err := st.GetIssue(ctx, "test-append-empty")
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		if issue.Notes != "first" {
			t.Errorf("notes: got %q, want %q", issue.Notes, "first")
		}
	})

	t.Run("notes_and_append_notes_conflict", func(t *testing.T) {
		create("test-conflict-notes", "", "orig")
		err := st.UpdateIssue(ctx, "test-conflict-notes", map[string]interface{}{
			"notes":        "replace",
			"append_notes": "append",
		}, "actor")
		if err == nil {
			t.Fatal("expected error combining notes with append_notes, got nil")
		}
	})

	t.Run("metadata_and_set_metadata_conflict", func(t *testing.T) {
		create("test-conflict-meta", `{"a":"b"}`, "")
		err := st.UpdateIssue(ctx, "test-conflict-meta", map[string]interface{}{
			"metadata":      json.RawMessage(`{"x":"y"}`),
			"_set_metadata": []string{"k=v"},
		}, "actor")
		if err == nil {
			t.Fatal("expected error combining metadata with _set_metadata, got nil")
		}
	})
}
