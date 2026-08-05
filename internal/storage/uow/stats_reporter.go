package uow

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/workapi"
	publicops "github.com/steveyegge/beads/issueops"
)

// StatsReporterSource is the capability accessor a unit-of-work provider offers
// for the summary-statistics role, the sibling of IssueReaderSource and
// CounterSource.
type StatsReporterSource interface {
	StatsReporter() (publicops.StatsReporter, error)
}

// statsReporter answers summary questions through a unit of work.
type statsReporter struct {
	provider UnitOfWorkProvider
}

// StatsReporter returns the guarded summary-statistics surface for this
// provider.
func (p *doltSQLProvider) StatsReporter() (publicops.StatsReporter, error) {
	return NewStatsReporter(p)
}

// NewStatsReporter constructs a public summary reporter backed by provider.
func NewStatsReporter(provider UnitOfWorkProvider) (publicops.StatsReporter, error) {
	if isNilUnitOfWorkProvider(provider) {
		return nil, fmt.Errorf("new stats reporter: unit-of-work provider must not be nil")
	}
	return &statsReporter{provider: provider}, nil
}

var _ publicops.StatsReporter = (*statsReporter)(nil)

// Stats answers the workspace summary inside one read-only unit of work.
//
// IT DOES NOT TAKE THE SkipBlocked HINT, and that is a property of this seam
// rather than an oversight: domain.IssueUseCase publishes GetStatistics and no
// no-blocked variant, so there is no cheaper query to run. The alternative —
// running the full query and then nilling the two numbers it just computed —
// would answer more slowly and tell the caller less, so the hint is ignored
// and the full summary is returned. issueops.StatsRequest.SkipBlocked states
// that possibility from the caller's side: BlockedIssues being non-nil is how
// a caller learns the hint was not taken.
func (r *statsReporter) Stats(ctx context.Context, _ publicops.StatsRequest) (publicops.StatsResult, error) {
	return RunTxRead(ctx, r.provider, func(ctx context.Context, uw UnitOfWork) (publicops.StatsResult, error) {
		summary, err := uw.IssueUseCase().GetStatistics(ctx)
		if err != nil {
			return publicops.StatsResult{}, err
		}
		if summary == nil {
			return publicops.StatsResult{}, nil
		}
		return publicops.StatsResult{Summary: *summary}, nil
	})
}

// AssigneeStats folds one actor's rows and ready work into a summary, through
// the same builder and the same fold the store-backed body uses — which is
// what makes "your work" mean one thing on both `bd status --assigned` routes.
//
// BOTH QUERIES SHARE ONE UNIT OF WORK here, where the store seam has no
// transaction to share, so this backend's two halves see one snapshot. The
// role deliberately does NOT promise that: a contract clause only one of the
// implementations can meet is not a contract. What all three share is the
// definition of the numbers.
func (r *statsReporter) AssigneeStats(ctx context.Context, req publicops.AssigneeStatsRequest) (publicops.StatsResult, error) {
	assignee, err := workapi.ValidateStatsAssignee(req.Assignee)
	if err != nil {
		return publicops.StatsResult{}, err
	}
	return RunTxRead(ctx, r.provider, func(ctx context.Context, uw UnitOfWork) (publicops.StatsResult, error) {
		page, err := uw.IssueUseCase().SearchIssues(ctx, "", workapi.BuildStatsAssigneeIssueFilter(assignee))
		if err != nil {
			return publicops.StatsResult{}, err
		}
		readyCount := 0
		if ready, readyErr := uw.IssueUseCase().GetReadyWork(ctx, workapi.BuildStatsAssigneeWorkFilter(assignee)); readyErr == nil {
			readyCount = len(ready.Items)
		}
		return publicops.StatsResult{Summary: workapi.FoldStatsAssigneeSummary(page.Items, readyCount)}, nil
	})
}
