package main

import "github.com/steveyegge/beads/issueops"

// updateIsGuarded reports whether a bd update carries a precondition that must
// hold at write time: --claim (its own compare-and-set) or any of the
// --if-assignee, --if-status and --if-revision guards.
//
// Fork (GH#4520 spool seam): a guarded update is never routed through the
// offline write-spool. The spool payload carries the field updates only and
// spoolDispatch replays them through UpdateIssue with no guard, so a guarded
// write queued on a transient failure would later land unconditionally - the
// overwrite the guard exists to prevent. It fails loud on an unreachable
// server instead, as --claim always has on this route.
func updateIsGuarded(claim bool, ifAssignee *string, expectedStatus *issueops.Status, ifRevision *int64) bool {
	return claim || ifAssignee != nil || expectedStatus != nil || ifRevision != nil
}
