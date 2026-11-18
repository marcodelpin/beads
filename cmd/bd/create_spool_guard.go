package main

// createNeedsLiveRole reports whether a bd create depends on work only the
// live create role performs, and therefore must never be routed through the
// offline write-spool.
//
// Fork (GH#4520 spool seam, bda-tfok.4.3 F2): a create under a --parent with
// no id of its own needs the issue-operations role to mint the <parent>.<n>
// child id and to inherit the parent's labels inside its own transaction.
// spoolDispatch replays a queued create through the raw store.CreateIssue,
// which does neither, so a queued child would land later under a flat id with
// no inherited labels. It fails loud on an unreachable server instead, as a
// guarded bd update does (updateIsGuarded). An explicit --id never reaches
// here with a parent: create refuses that combination first.
func createNeedsLiveRole(parentID string) bool {
	return parentID != ""
}
