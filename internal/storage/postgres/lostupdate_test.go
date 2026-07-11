package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/conformance"
	"github.com/steveyegge/beads/internal/storage/pgdialect"
)

// TestConcurrentMergeOpUpdates_NoLostKeys is the Postgres arm of the concurrent
// merge-op lost-update regression: two independent store handles (two pools =
// two lock-domain participants, modeling two bd processes) write DISTINCT
// metadata keys of the SAME issue while a third connection briefly holds the
// row's write lock to force the interleaving. Under READ COMMITTED a plain
// in-tx read does not block on that lock, so the unpatched read-then-write
// path merged from a stale snapshot and the second commit silently erased the
// first writer's key. The in-tx SELECT … FOR UPDATE read must keep every key.
// Gated on BEADS_PG_TEST_URL.
func TestConcurrentMergeOpUpdates_NoLostKeys(t *testing.T) {
	url := os.Getenv("BEADS_PG_TEST_URL")
	if url == "" {
		t.Skip("BEADS_PG_TEST_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("lostupd_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if raw, err := pgdialect.OpenRaw(url, "public"); err == nil {
			_, _ = raw.ExecContext(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema))
			_ = raw.Close()
		}
	})

	stA, err := Provision(ctx, url, schema)
	if err != nil {
		t.Fatalf("Provision(a): %v", err)
	}
	defer stA.Close()
	if err := stA.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		t.Fatalf("SetConfig(issue_prefix): %v", err)
	}
	stB, err := Provision(ctx, url, schema)
	if err != nil {
		t.Fatalf("Provision(b): %v", err)
	}
	defer stB.Close()
	stLock, err := Provision(ctx, url, schema)
	if err != nil {
		t.Fatalf("Provision(lock): %v", err)
	}
	defer stLock.Close()
	lockDB := stLock.(interface{ DB() *sql.DB }).DB()

	conformance.RunConcurrentMergeOpLostUpdate(t, stA, stB, lockDB)
}
