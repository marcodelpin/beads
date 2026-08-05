package dolt

import (
	"context"
	"database/sql"

	"github.com/steveyegge/beads/internal/storage"
	storeops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/issueops"
)

// CycleDetector returns the guarded cycle-report surface for this store.
func (s *DoltStore) CycleDetector() (issueops.CycleDetector, error) {
	if s == nil {
		return nil, &storage.ErrUnsupported{Op: "CycleDetector", Backend: "nil"}
	}
	return &cycleDetector{store: s}, nil
}

// cycleDetector answers the cycle report from one read transaction.
//
// There is no shared constructor package for this role the way there is for the
// count role, and that is not an omission. The work is a graph read plus a
// hydration, both of which need a TRANSACTION — the two planes must be read as
// one snapshot — and a transaction is not reachable through storage.DoltStorage.
// So the sharing happens one level down instead: this body and the embedded
// store's are five lines each around issueops.DetectCycleReportInTx, which is
// the same function both stores' legacy DetectCycles already call. Two wrappers
// over one body is still ONE vote, and the conformance contract says so.
//
// A front door cannot construct this: the type is unexported and the accessor is
// the only door, which is what the cmd-bd-role-constructors depguard rule buys
// the roles whose bodies live in an importable package.
type cycleDetector struct{ store *DoltStore }

var _ issueops.CycleDetector = (*cycleDetector)(nil)

func (c *cycleDetector) DetectCycles(ctx context.Context, _ issueops.DetectCyclesRequest) (issueops.CycleReport, error) {
	var report issueops.CycleReport
	err := c.store.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		report, err = storeops.DetectCycleReportInTx(ctx, tx)
		return err
	})
	if err != nil {
		return issueops.CycleReport{}, err
	}
	return report, nil
}
