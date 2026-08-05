package main

import (
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// The closed-candidate recheck used to live here in full, shared by `bd gc`,
// `bd purge` and `bd prune`. purge and prune are behind issueops.Sweeper now
// and the role applies it below both front doors; `bd gc` is not, so what is
// left here is a thin call into the SAME pure function
// (workapi.FilterSweepCandidates) plus gc's own warning line.
//
// One definition, two callers: gc and the role cannot come to disagree about
// what "a closed bead safe to delete" means, which is exactly the drift this
// file existed to prevent between purge and prune in the first place.

type closedDeletionCandidateStats = issueops.SweepSkips

// filterClosedDeletionCandidates keeps the closed, unpinned, old-enough
// candidates and reports what it held back. `bd gc` matches no glob, so it
// passes the empty pattern, which admits everything.
func filterClosedDeletionCandidates(issues []*types.Issue, cutoff *time.Time) ([]*types.Issue, closedDeletionCandidateStats) {
	return workapi.FilterSweepCandidates(issues, "", cutoff)
}

func warnClosedDeletionSafetySkips(stats closedDeletionCandidateStats) {
	if workapi.SweepDefenseSkips(stats) == 0 {
		return
	}
	WarnError("skipped %d deletion candidate(s) after closed_at safety recheck (nil=%d, non_closed=%d, missing_closed_at=%d, too_recent=%d)",
		workapi.SweepDefenseSkips(stats),
		stats.Unreadable,
		stats.NotClosed,
		stats.UnknownClosedAt,
		stats.ClosedAtOrAfterCutoff,
	)
}
