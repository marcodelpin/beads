package httpapi

import (
	"net/http"

	"github.com/steveyegge/beads/internal/httpapi/apigen"
	"github.com/steveyegge/beads/issueops"
)

// The summary operation. It decodes two parameters, hands the whole request to
// the summary role, and shapes the answer onto the wire.
//
// No filter is built, no assignee predicate is assembled, no ready-work query
// is issued and no status fold is performed: all of that is inside
// issueops.StatsReporter. The fold in particular is where "your work" is
// DEFINED, and a second copy here would be a second definition that nothing
// compares against the first.
//
// WHICH METHOD IS TRANSPORT'S CALL, and it is the only decision this file makes
// about meaning: an absent `assignee` is the workspace-wide question, a present
// one is the actor question, and an EMPTY one is neither, so it is refused here
// rather than passed down. The role would refuse it too (ErrValidation), but a
// 400 that names the parameter is what a client can act on.

// handleStats answers GET /v0/beads/stats.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	q := newQuery(r.URL.Query())

	assignee := q.str("assignee")
	skipBlocked := q.boolean("skip_blocked")

	// Supplied-but-empty is distinguishable from absent only at the raw values:
	// q.str answers "" for both. The refusal goes through the query's own
	// first-refusal slot so a request malformed elsewhere too reports one
	// parameter rather than depending on read order.
	if assignee == "" && r.URL.Query().Has("assignee") {
		q.invalid("assignee", "an assignee must not be empty; omit the parameter for the workspace-wide summary")
	}

	if !s.acceptQuery(w, r, q) {
		return
	}

	reporter, err := s.statsReporter(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	var result issueops.StatsResult
	if assignee != "" {
		// skip_blocked is not forwarded, and there is nothing to forward it
		// to: AssigneeStatsRequest has no such field, because that answer
		// computes both numbers by a route with no fast path. The document
		// says the parameter is ignored here.
		result, err = reporter.AssigneeStats(r.Context(), issueops.AssigneeStatsRequest{Assignee: assignee})
	} else {
		result, err = reporter.Stats(r.Context(), issueops.StatsRequest{SkipBlocked: skipBlocked})
	}
	if err != nil {
		s.failErr(w, r, err)
		return
	}

	writeJSON(w, apigen.StatsResponse{
		Summary: result.Summary,
		// DERIVED from the answer, never echoed from the request. skip_blocked
		// is a hint: a backend with no cheaper path returns the full numbers,
		// and a flag echoing the request would tell a client the scan was
		// skipped while the numbers beside it prove it was not.
		BlockedCountSkipped: result.Summary.BlockedIssues == nil,
	})
}
