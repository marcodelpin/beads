package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/conformance"
)

// TestConcurrentMergeOpUpdates_NoLostKeys is the MySQL arm of the concurrent
// merge-op lost-update regression: two independent store handles (two pools =
// two lock-domain participants, modeling two bd processes) write DISTINCT
// metadata keys of the SAME issue while a third connection briefly holds the
// row's write lock to force the interleaving. Under REPEATABLE READ a plain
// in-tx read is a consistent (snapshot) read that does not block on that lock,
// so the unpatched read-then-write path merged from a stale snapshot and the
// second commit silently erased the first writer's key. The in-tx
// SELECT … FOR UPDATE read (a locking current read) must keep every key.
// Gated on BEADS_MYSQL_TEST_URL (a server DSN, e.g. user:pass@tcp(host:3306)/).
func TestConcurrentMergeOpUpdates_NoLostKeys(t *testing.T) {
	url := os.Getenv("BEADS_MYSQL_TEST_URL")
	if url == "" {
		t.Skip("BEADS_MYSQL_TEST_URL not set")
	}
	ctx := context.Background()
	database := fmt.Sprintf("lostupd_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if serverDSN, e := withDatabase(url, ""); e == nil {
			if srv, e2 := sql.Open("mysql", serverDSN); e2 == nil {
				_, _ = srv.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+database+"`")
				_ = srv.Close()
			}
		}
	})

	stA, err := Provision(ctx, url, database)
	if err != nil {
		t.Fatalf("Provision(a): %v", err)
	}
	defer stA.Close()
	if err := stA.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		t.Fatalf("SetConfig(issue_prefix): %v", err)
	}
	stB, err := Provision(ctx, url, database)
	if err != nil {
		t.Fatalf("Provision(b): %v", err)
	}
	defer stB.Close()
	stLock, err := Provision(ctx, url, database)
	if err != nil {
		t.Fatalf("Provision(lock): %v", err)
	}
	defer stLock.Close()
	lockDB := stLock.(interface{ DB() *sql.DB }).DB()

	conformance.RunConcurrentMergeOpLostUpdate(t, stA, stB, lockDB)
}
