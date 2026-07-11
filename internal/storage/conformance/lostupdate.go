package conformance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// RunConcurrentMergeOpLostUpdate is the cross-backend regression body for the
// concurrent merge-op lost-update defect: two writers merging DISTINCT metadata
// keys into the SAME issue, both reporting success, must both have their keys
// in the final metadata.
//
// stA and stB must be two INDEPENDENT store handles (separate connection
// pools) on the same database, modeling two bd processes. lockDB must be a
// THIRD dialect-wrapped pool on that database (`?` placeholders): the test
// holds the target row's write lock through it so both writers are in flight
// before either can commit. On the unpatched read-then-write path this loss is
// deterministic on MVCC backends (Postgres/MySQL): a plain in-tx read does not
// block on the held row lock, so both writers merge from the pre-lock snapshot,
// queue on the lock at UPDATE time, and the second commit erases the first's
// key — both exit 0. The fix reads the row with SELECT … FOR UPDATE
// (issueops.GetIssueForUpdateInTx), which serializes the read itself.
func RunConcurrentMergeOpLostUpdate(t *testing.T, stA, stB storage.DoltStorage, lockDB *sql.DB) {
	t.Helper()
	ctx := context.Background()

	issue := withDefaults(&types.Issue{ID: "test-mergeop-race", Title: "merge-op race target"})
	must(t, stA.CreateIssue(ctx, issue, "actor"))

	const rounds = 5
	for i := 0; i < rounds; i++ {
		lockTx, err := lockDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("round %d: begin lock tx: %v", i, err)
		}
		// A self-assignment UPDATE takes the row's exclusive lock without
		// changing data; rolling back releases it.
		if _, err := lockTx.ExecContext(ctx,
			"UPDATE issues SET title = title WHERE id = ?", issue.ID); err != nil {
			t.Fatalf("round %d: acquire row lock: %v", i, err)
		}

		var wg sync.WaitGroup
		errCh := make(chan error, 2)
		for prefix, st := range map[string]storage.DoltStorage{"a": stA, "b": stB} {
			wg.Add(1)
			go func(prefix string, st storage.DoltStorage) {
				defer wg.Done()
				updates := map[string]interface{}{
					issueops.OpSetMetadata: []string{fmt.Sprintf("%s%d=1", prefix, i)},
				}
				if err := st.UpdateIssue(ctx, issue.ID, updates, "writer-"+prefix); err != nil {
					errCh <- fmt.Errorf("round %d writer %s: %w", i, prefix, err)
				}
			}(prefix, st)
		}

		// Give both writers time to reach the lock point, then release.
		time.Sleep(400 * time.Millisecond)
		if err := lockTx.Rollback(); err != nil {
			t.Fatalf("round %d: release row lock: %v", i, err)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}
	}

	final, err := stA.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	got := map[string]any{}
	if len(final.Metadata) > 0 {
		if err := json.Unmarshal(final.Metadata, &got); err != nil {
			t.Fatalf("unmarshal metadata %q: %v", final.Metadata, err)
		}
	}
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
		t.Fatalf("silent lost update: %d of %d successful merge-op writes missing from final metadata: %v",
			len(missing), rounds*2, missing)
	}
}
