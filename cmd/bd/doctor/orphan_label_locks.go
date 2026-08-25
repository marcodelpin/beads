package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage/dberrors"
	"github.com/steveyegge/beads/internal/storage/dolt"
)

// OrphanLabelLocksCheckName is the doctor check name for
// label_namespace_locks rows whose issue no longer exists.
const OrphanLabelLocksCheckName = "Orphan Label Namespace Locks"

// maxOrphanLockExamples caps how many orphan rows the detail names.
const maxOrphanLockExamples = 5

// CheckOrphanLabelNamespaceLocksWithStore reports label_namespace_locks rows
// whose issue_id exists in neither issues nor wisps. The delete paths cascade
// the lock rows since bd-7u5ki's follow-up (delete, bulk delete, wisp
// cleanup), so new orphans should not appear - but rows orphaned BEFORE that
// cascade shipped stay in live databases forever: the table is dolt_ignore'd
// (working-set only, never merged), so nothing downstream ever prunes them.
// Harmless to correctness (a lock row is only ever touched by writers into
// its namespace) but each one is a dead row the next same-id issue would
// silently reuse; this check is the retro half the cascade cannot cover.
func CheckOrphanLabelNamespaceLocksWithStore(ss *SharedStore) DoctorCheck {
	store := ss.Store()
	if store == nil {
		return DoctorCheck{
			Name:    OrphanLabelLocksCheckName,
			Status:  StatusOK,
			Message: "No database yet",
		}
	}
	return checkOrphanLabelNamespaceLocks(context.Background(), store)
}

// orphanLockScanFailure is the shared warning shape for every way the scan
// itself can fail (query error, scan error, iteration error).
func orphanLockScanFailure(err error) DoctorCheck {
	return DoctorCheck{
		Name:    OrphanLabelLocksCheckName,
		Status:  StatusWarning,
		Message: "Unable to scan label_namespace_locks for orphan rows",
		Detail:  err.Error(),
	}
}

func checkOrphanLabelNamespaceLocks(ctx context.Context, store *dolt.DoltStore) DoctorCheck {
	rows, err := store.UnderlyingDB().QueryContext(ctx, `
		SELECT l.issue_id, l.namespace
		FROM label_namespace_locks l
		LEFT JOIN issues i ON i.id = l.issue_id
		LEFT JOIN wisps w ON w.id = l.issue_id
		WHERE i.id IS NULL AND w.id IS NULL
		ORDER BY l.issue_id, l.namespace`)
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			// Pre-migration-0066 workspace (no lock table) or pre-wisps
			// schema: nothing to scan, nothing orphaned.
			return DoctorCheck{
				Name:    OrphanLabelLocksCheckName,
				Status:  StatusOK,
				Message: "No label_namespace_locks table (pre-migration workspace)",
			}
		}
		return orphanLockScanFailure(err)
	}
	defer rows.Close()

	var examples []string
	total := 0
	for rows.Next() {
		var issueID, namespace string
		if err := rows.Scan(&issueID, &namespace); err != nil {
			return orphanLockScanFailure(err)
		}
		total++
		if len(examples) < maxOrphanLockExamples {
			examples = append(examples, fmt.Sprintf("%s (%s)", issueID, namespace))
		}
	}
	if err := rows.Err(); err != nil {
		return orphanLockScanFailure(err)
	}
	if total == 0 {
		return DoctorCheck{
			Name:    OrphanLabelLocksCheckName,
			Status:  StatusOK,
			Message: "No orphan label_namespace_locks rows",
		}
	}

	detail := strings.Join(examples, "; ")
	if total > len(examples) {
		detail += fmt.Sprintf(" (+%d more)", total-len(examples))
	}
	return DoctorCheck{
		Name:    OrphanLabelLocksCheckName,
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d label_namespace_locks row(s) reference issues that no longer exist (orphaned before the delete cascade shipped)", total),
		Detail:  detail,
		Fix: "One-shot cleanup (safe: lock rows carry no data beyond the lock itself): " +
			"bd dolt sql -q \"DELETE l FROM label_namespace_locks l " +
			"LEFT JOIN issues i ON i.id = l.issue_id LEFT JOIN wisps w ON w.id = l.issue_id " +
			"WHERE i.id IS NULL AND w.id IS NULL\"",
	}
}
