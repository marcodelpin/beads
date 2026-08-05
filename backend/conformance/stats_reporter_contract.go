package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// This file holds the contract every implementation of
// publicops.StatsReporter must satisfy. Each case asserts what
// issueops/statsreporter.go PROMISES, cited by line, rather than what any one
// backend happens to do today; a backend that disagrees is parked at its own
// wiring site with skipKnownDivergence so the case still runs on the ones that
// agree.
//
// THREE WIRINGS, AND HERE THAT REALLY IS THREE VOTES — which is unusual in
// this suite and worth saying out loud, because the sibling contracts say the
// opposite. For most roles dolt and embeddeddolt share one body
// (internal/workapi/store*) and "both stores agree" is one vote plus an engine
// check. That is still true of the ROLE layer here: both stores hand back
// internal/workapi/storestats. But the queries it delegates to are written out
// THREE times — dolt/queries.go, embeddeddolt/statistics.go and
// domain/db/issue.go each spell the blocked count and the ready subtraction
// themselves, and the two Dolt stores do not even agree on how many
// transactions to spend (dolt takes two read transactions, embeddeddolt one).
// Only the status tally is shared, through
// internal/storage/issueops.ScanIssueCountsInTx. So a disagreement between the
// two stores is reachable here in a way it is not for Counter.
//
// (There is a FOURTH copy of that arithmetic, unreachable: package-level
// issueops.GetStatisticsInTx has no callers at all. Nothing here runs it.)
//
// EVERY WORKSPACE-WIDE CASE ASSERTS A DELTA, never an absolute. The summary
// takes no predicate — that is the point of the role — so there is no IDFilter
// to scope it with the way the Counter contract scopes its cases, and the
// three fixtures each share one database with the other role suites that ran
// before them. A before/after pair around a known seed is the only assertion
// that is about THIS case's rows. The assignee-scoped cases are the exception
// and do assert absolutes: each takes an assignee namespaced by the fixture
// prefix, which no other case can seed.
//
// What is deliberately NOT here:
//   - the fold from an actor's rows to a summary, which is shared by all three
//     through internal/workapi.FoldStatsAssigneeSummary and pinned directly,
//     without a database, in internal/workapi/stats_test.go;
//   - the flag-to-request mapping, which is `bd status`'s job;
//   - whether the two halves of an assignee-scoped answer are one snapshot,
//     which statsreporter.go deliberately does not promise because the store
//     seam cannot offer it.

// StatsReporterFixture supplies adapter-specific storage access for the
// summary assertions. Every field is named and typed exactly like the
// per-backend roleFixtureKit hook it is filled from, so a wiring is kit plus
// accessor plus prefix with no adapter in between.
type StatsReporterFixture struct {
	// IssuePrefix namespaces the ids and the assignees each assertion seeds,
	// so several of them can share one database.
	IssuePrefix   string
	StatsReporter publicops.StatsReporter
	// CreateIssue seeds a durable issue in the issues plane.
	CreateIssue func(context.Context, *types.Issue, string) error
	// CreateWisp seeds an ephemeral issue in the wisps plane. It is a separate
	// field rather than an Ephemeral flag on CreateIssue because the three
	// adapters reach the two planes through different verbs.
	CreateWisp func(context.Context, *types.Issue, string) error
	// AddDependency seeds ONE edge, which is how a case makes a row blocked:
	// the blocked count reads the denormalized is_blocked flag the edge insert
	// maintains, and there is no other way to set it through a public seam.
	AddDependency func(context.Context, *types.Dependency, string) error
	// CountHistory reports how many history entries the fixture's branch has.
	// A nil hook means "this backend cannot observe history", and the case
	// that needs it SKIPS with that reason rather than passing quietly.
	CountHistory func(context.Context) (int, error)
}

// RunStatsReporterCountsEveryDurableRowByStatus pins statsreporter.go:92-98:
// one scan of the durable plane, every row in Total including closed ones, and
// PinnedIssues counting a flag that overlaps a status bucket rather than
// naming a sixth status.
//
// The pinned row seeded here is OPEN, deliberately. A pinned row that was also
// closed would leave "does pinned overlap or replace a status bucket"
// unanswered, and that overlap is the clause a reader is most likely to get
// wrong: the buckets do not sum to Total in either direction.
func RunStatsReporterCountsEveryDurableRowByStatus(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	before := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})

	seedStatsReporterIssue(t, ctx, fixture, statsReporterSeed(fixture, "by-status-open", types.StatusOpen))
	seedStatsReporterIssue(t, ctx, fixture, statsReporterSeed(fixture, "by-status-wip", types.StatusInProgress))
	seedStatsReporterIssue(t, ctx, fixture, statsReporterSeed(fixture, "by-status-closed", types.StatusClosed))
	seedStatsReporterIssue(t, ctx, fixture, statsReporterSeed(fixture, "by-status-deferred", types.StatusDeferred))
	pinned := statsReporterSeed(fixture, "by-status-pinned", types.StatusOpen)
	pinned.Pinned = true
	seedStatsReporterIssue(t, ctx, fixture, pinned)

	after := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})
	assertStatsReporterDelta(t, before, after, statsReporterCounts{
		total: 5, open: 2, inProgress: 1, closed: 1, deferred: 1, pinned: 1,
	})
}

// RunStatsReporterExcludesTheWispTier pins statsreporter.go:116-119: the
// workspace-wide answer counts the durable plane only.
//
// It is the same A/B the Counter contract runs on its own default, and for the
// same reason: the storage seam MERGES the two planes unless told not to, so
// "durable only" is an active decision on every summary rather than something
// that falls out of doing nothing. The assignee-scoped case below is the other
// half of the pair — the two methods answer differently here, and that is the
// contract.
func RunStatsReporterExcludesTheWispTier(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	before := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})

	seedStatsReporterIssue(t, ctx, fixture, statsReporterSeed(fixture, "plane-durable", types.StatusOpen))
	wisp := statsReporterSeed(fixture, "plane-wisp", types.StatusOpen)
	wisp.Ephemeral = true
	seedStatsReporterWisp(t, ctx, fixture, wisp)

	after := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})
	assertStatsReporterDelta(t, before, after, statsReporterCounts{total: 1, open: 1})
}

// RunStatsReporterAStatusOutsideTheTalliesIsCountedOnlyInTotal pins the second
// half of statsreporter.go:92-98 — the tallies are exact equality against the
// stored status, so a row whose status is none of the four lands in Total and
// in no bucket, and the buckets do not sum to Total.
//
// It uses the "blocked" STATUS to do it, which makes this case do double duty:
// that row is also the control for the case below. It has no dependency edge,
// so its is_blocked flag is 0 and it must NOT move BlockedIssues either. A
// backend that counted the status instead of the flag fails here, at the seed
// that looks least like a blocked issue.
func RunStatsReporterAStatusOutsideTheTalliesIsCountedOnlyInTotal(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	before := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})

	seedStatsReporterIssue(t, ctx, fixture, statsReporterSeed(fixture, "status-blocked", types.StatusBlocked))

	after := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})
	assertStatsReporterDelta(t, before, after, statsReporterCounts{total: 1})
}

// RunStatsReporterBlockedCountsTheGraphNotTheStatus pins
// statsreporter.go:99-102: BlockedIssues counts rows whose transitive
// is_blocked flag is set, and an open row with an unfinished blocker is one of
// them while its status is still "open".
//
// Both seeded rows are OPEN, so the case fails in an unmistakable way if a
// backend ever counted the status: the delta it would report is zero blocked
// out of two open rows.
func RunStatsReporterBlockedCountsTheGraphNotTheStatus(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	before := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})

	blocker := statsReporterSeed(fixture, "graph-blocker", types.StatusOpen)
	blocked := statsReporterSeed(fixture, "graph-blocked", types.StatusOpen)
	seedStatsReporterIssue(t, ctx, fixture, blocker)
	seedStatsReporterIssue(t, ctx, fixture, blocked)
	blockStatsReporterIssue(t, ctx, fixture, blocked.ID, blocker.ID)

	after := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})
	assertStatsReporterDelta(t, before, after, statsReporterCounts{total: 2, open: 2, blocked: 1})
}

// RunStatsReporterReadyIsOpenMinusBlocked pins statsreporter.go:103-108:
// ReadyIssues is arithmetic over two numbers in the same answer, not a query.
//
// The identity is asserted on the ABSOLUTE summary rather than on a delta,
// because it is a property of every answer this role gives and not of the rows
// this case seeded. The seed is here so the three numbers are not trivially
// equal: without a blocked row, Ready == Open would hold under an
// implementation that never subtracted anything.
func RunStatsReporterReadyIsOpenMinusBlocked(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	blocker := statsReporterSeed(fixture, "arith-blocker", types.StatusOpen)
	blocked := statsReporterSeed(fixture, "arith-blocked", types.StatusOpen)
	seedStatsReporterIssue(t, ctx, fixture, blocker)
	seedStatsReporterIssue(t, ctx, fixture, blocked)
	blockStatsReporterIssue(t, ctx, fixture, blocked.ID, blocker.ID)

	summary := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})
	blockedCount := statsReporterPointer(t, "BlockedIssues", summary.BlockedIssues)
	ready := statsReporterPointer(t, "ReadyIssues", summary.ReadyIssues)

	want := summary.OpenIssues - blockedCount
	if want < 0 {
		want = 0
	}
	if ready != want {
		t.Fatalf("ReadyIssues = %d, want OpenIssues(%d) - BlockedIssues(%d) clamped at zero = %d",
			ready, summary.OpenIssues, blockedCount, want)
	}
	if blockedCount < 1 {
		t.Fatalf("BlockedIssues = %d after seeding a blocked row; the identity above held vacuously", blockedCount)
	}
}

// RunStatsReporterSkipBlockedPairsTheTwoPointers pins the whole of the
// SkipBlocked promise (statsreporter.go:138-154), which is stated from the
// ANSWER's side because the backends genuinely differ on whether they can take
// the hint:
//
//   - the two pointers are nil together or populated together, never one of
//     each — every front door renders on exactly that pairing;
//   - nothing else in the summary changes;
//   - a populated pair means the hint was not taken, which is a legal answer
//     and not a failure.
//
// So this case does NOT assert that the fast path was used. It asserts what a
// caller may rely on regardless, on all three backends, with no park. Today
// the two store backends take the hint (GetStatisticsNoBlocked) and the
// unit-of-work backend does not (domain.IssueUseCase publishes no no-blocked
// query); a reader wanting to know which is which reads the wiring, not this
// case.
func RunStatsReporterSkipBlockedPairsTheTwoPointers(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	seedStatsReporterIssue(t, ctx, fixture, statsReporterSeed(fixture, "hint-open", types.StatusOpen))

	full := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})
	skipped := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{SkipBlocked: true})

	if (skipped.BlockedIssues == nil) != (skipped.ReadyIssues == nil) {
		t.Fatalf("SkipBlocked answered with BlockedIssues=%v and ReadyIssues=%v; readiness is derived from the blocked count, so the two are nil together or populated together",
			skipped.BlockedIssues, skipped.ReadyIssues)
	}
	if skipped.BlockedIssues != nil {
		if got, want := *skipped.BlockedIssues, statsReporterPointer(t, "BlockedIssues", full.BlockedIssues); got != want {
			t.Errorf("SkipBlocked was not taken and reported BlockedIssues = %d, want the full answer's %d", got, want)
		}
		if got, want := *skipped.ReadyIssues, statsReporterPointer(t, "ReadyIssues", full.ReadyIssues); got != want {
			t.Errorf("SkipBlocked was not taken and reported ReadyIssues = %d, want the full answer's %d", got, want)
		}
	}

	// Nothing else moves. Compared against the full call taken immediately
	// before, on a fixture only this suite writes to.
	assertStatsReporterDelta(t, full, skipped, statsReporterCounts{})
}

// RunStatsReporterExtendedFieldsAreAlwaysZero pins statsreporter.go:109-114:
// EpicsEligibleForClosure and AverageLeadTime are always zero because no
// implementation computes either.
//
// It is a tripwire rather than a behavior test, and it is worth a case
// precisely because it can only fail on GOOD news: the day a backend starts
// computing one of them, this fails and the doc that calls it always zero — and
// the `bd status` render branch that has never fired — get revisited in the
// same change rather than left saying something false.
func RunStatsReporterExtendedFieldsAreAlwaysZero(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	summary := statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})
	if summary.EpicsEligibleForClosure != 0 {
		t.Errorf("EpicsEligibleForClosure = %d; statsreporter.go promises it is always zero because nothing computes it",
			summary.EpicsEligibleForClosure)
	}
	if summary.AverageLeadTime != 0 {
		t.Errorf("AverageLeadTime = %v; statsreporter.go promises it is always zero because nothing computes it",
			summary.AverageLeadTime)
	}
}

// RunStatsReporterWritesNothing pins statsreporter.go:121-125: reporting is a
// read, so neither method records a history entry.
//
// The delta is taken around BOTH methods together. An absolute count would be
// meaningless on a fixture other cases have already seeded, and the seeds this
// case needs are taken before the first reading for the same reason.
func RunStatsReporterWritesNothing(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	if fixture.CountHistory == nil {
		t.Skip("this backend cannot observe history, so the read-only clause cannot be checked here")
	}
	assignee := statsReporterAssignee(fixture, "writes-nothing")
	seed := statsReporterSeed(fixture, "writes-nothing", types.StatusOpen)
	seed.Assignee = assignee
	seedStatsReporterIssue(t, ctx, fixture, seed)

	before, err := fixture.CountHistory(ctx)
	if err != nil {
		t.Fatalf("CountHistory before: %v", err)
	}

	statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{})
	statsReporterSummary(t, ctx, fixture, publicops.StatsRequest{SkipBlocked: true})
	statsReporterAssigneeSummary(t, ctx, fixture, assignee)

	after, err := fixture.CountHistory(ctx)
	if err != nil {
		t.Fatalf("CountHistory after: %v", err)
	}
	if after != before {
		t.Fatalf("history entries went %d -> %d across three summary reads, want no change", before, after)
	}
}

// RunStatsReporterAssigneeStatsScopesToOneActor pins the first and last
// bullets of statsreporter.go:159-179: the answer is one actor's rows, and
// PinnedIssues is always zero on this path along with the two fields that are
// always zero everywhere.
//
// The assignee is namespaced by the fixture prefix, so this case can assert
// ABSOLUTE numbers where the workspace-wide cases cannot: no other case in any
// role suite can seed a row for it.
func RunStatsReporterAssigneeStatsScopesToOneActor(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	mine := statsReporterAssignee(fixture, "scope-mine")
	theirs := statsReporterAssignee(fixture, "scope-theirs")

	for _, seed := range []struct {
		suffix   string
		status   types.Status
		assignee string
		pinned   bool
	}{
		{"scope-open", types.StatusOpen, mine, false},
		{"scope-pinned", types.StatusOpen, mine, true},
		{"scope-closed", types.StatusClosed, mine, false},
		{"scope-other", types.StatusOpen, theirs, false},
	} {
		issue := statsReporterSeed(fixture, seed.suffix, seed.status)
		issue.Assignee = seed.assignee
		issue.Pinned = seed.pinned
		seedStatsReporterIssue(t, ctx, fixture, issue)
	}

	summary := statsReporterAssigneeSummary(t, ctx, fixture, mine)
	if summary.TotalIssues != 3 {
		t.Errorf("TotalIssues = %d, want 3 — the other actor's row is not this actor's work", summary.TotalIssues)
	}
	if summary.OpenIssues != 2 {
		t.Errorf("OpenIssues = %d, want 2", summary.OpenIssues)
	}
	if summary.ClosedIssues != 1 {
		t.Errorf("ClosedIssues = %d, want 1 — an assignee summary counts closed rows too", summary.ClosedIssues)
	}
	if summary.PinnedIssues != 0 {
		t.Errorf("PinnedIssues = %d, want 0 — the assignee fold tallies the five statuses and nothing else, even with a pinned row in the set", summary.PinnedIssues)
	}
	if summary.EpicsEligibleForClosure != 0 || summary.AverageLeadTime != 0 {
		t.Errorf("extended fields = %d/%v, want zeros", summary.EpicsEligibleForClosure, summary.AverageLeadTime)
	}
}

// RunStatsReporterAssigneeBlockedCountsTheStatusNotTheGraph pins the exact
// INVERSE of the workspace-wide blocked clause, which is the sharpest
// statement this contract makes: statsreporter.go:169-172 says an
// assignee-scoped BlockedIssues counts rows whose STATUS is "blocked", where
// the workspace-wide one counts the is_blocked flag.
//
// Both seeds belong to the same actor. One has the status and no edge; the
// other has an edge and the status "open". The answer must be 1, and the two
// backends' bodies can only get it right by using the fold rather than the
// aggregate.
func RunStatsReporterAssigneeBlockedCountsTheStatusNotTheGraph(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	assignee := statsReporterAssignee(fixture, "abl")

	byStatus := statsReporterSeed(fixture, "abl-status", types.StatusBlocked)
	byStatus.Assignee = assignee
	seedStatsReporterIssue(t, ctx, fixture, byStatus)

	blocker := statsReporterSeed(fixture, "abl-blocker", types.StatusOpen)
	blocker.Assignee = assignee
	byGraph := statsReporterSeed(fixture, "abl-graph", types.StatusOpen)
	byGraph.Assignee = assignee
	seedStatsReporterIssue(t, ctx, fixture, blocker)
	seedStatsReporterIssue(t, ctx, fixture, byGraph)
	blockStatsReporterIssue(t, ctx, fixture, byGraph.ID, blocker.ID)

	summary := statsReporterAssigneeSummary(t, ctx, fixture, assignee)
	if got := statsReporterPointer(t, "BlockedIssues", summary.BlockedIssues); got != 1 {
		t.Errorf("BlockedIssues = %d, want 1: the row with status \"blocked\" and not the open row an edge blocks", got)
	}
	if summary.OpenIssues != 2 {
		t.Errorf("OpenIssues = %d, want 2 — a graph-blocked row keeps its status", summary.OpenIssues)
	}
}

// RunStatsReporterAssigneeStatsMergesTheWispTier pins the first bullet of
// statsreporter.go:163-168: this answer's set spans the durable plane AND the
// ephemeral tier, where the workspace-wide answer is durable-only. It is the
// other half of the A/B RunStatsReporterExcludesTheWispTier starts.
func RunStatsReporterAssigneeStatsMergesTheWispTier(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	assignee := statsReporterAssignee(fixture, "amerge")

	durable := statsReporterSeed(fixture, "amerge-durable", types.StatusOpen)
	durable.Assignee = assignee
	seedStatsReporterIssue(t, ctx, fixture, durable)

	wisp := statsReporterSeed(fixture, "amerge-wisp", types.StatusOpen)
	wisp.Assignee = assignee
	wisp.Ephemeral = true
	seedStatsReporterWisp(t, ctx, fixture, wisp)

	summary := statsReporterAssigneeSummary(t, ctx, fixture, assignee)
	if summary.TotalIssues != 2 {
		t.Errorf("TotalIssues = %d, want 2 — the actor's wisp is in this answer even though it is in no workspace-wide one", summary.TotalIssues)
	}
	if summary.OpenIssues != 2 {
		t.Errorf("OpenIssues = %d, want 2", summary.OpenIssues)
	}
}

// RunStatsReporterAssigneeStatsPopulatesBothPointers pins the clause
// FoldStatsAssigneeSummary exists to guarantee: on this path BlockedIssues and
// ReadyIssues are ALWAYS non-nil, including for an actor with no rows at all.
//
// The nil pointers are the workspace-wide answer's skipped-scan signal and
// every front door renders them as "(skipped)". An assignee summary skips
// nothing, so a nil here would print a lie about a query that ran.
func RunStatsReporterAssigneeStatsPopulatesBothPointers(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	summary := statsReporterAssigneeSummary(t, ctx, fixture, statsReporterAssignee(fixture, "nobody"))

	if summary.TotalIssues != 0 {
		t.Fatalf("TotalIssues = %d for an actor with no rows, want 0", summary.TotalIssues)
	}
	if got := statsReporterPointer(t, "BlockedIssues", summary.BlockedIssues); got != 0 {
		t.Errorf("BlockedIssues = %d, want a populated 0", got)
	}
	if got := statsReporterPointer(t, "ReadyIssues", summary.ReadyIssues); got != 0 {
		t.Errorf("ReadyIssues = %d, want a populated 0", got)
	}
}

// RunStatsReporterAssigneeStatsRefusesAnEmptyAssignee pins
// statsreporter.go:46-51: an empty or whitespace-only assignee is
// ErrValidation rather than a query for the rows whose assignee column is
// empty.
//
// The whitespace case is the one that matters. An implementation that checked
// `assignee == ""` would pass the first and answer the second with a summary
// of nothing, which reads exactly like an actor who has no work.
func RunStatsReporterAssigneeStatsRefusesAnEmptyAssignee(t *testing.T, ctx context.Context, fixture StatsReporterFixture) {
	t.Helper()
	for _, blank := range []string{"", "   ", "\t"} {
		_, err := fixture.StatsReporter.AssigneeStats(ctx, publicops.AssigneeStatsRequest{Assignee: blank})
		if !errors.Is(err, publicops.ErrValidation) {
			t.Errorf("AssigneeStats(%q) error = %v, want ErrValidation", blank, err)
		}
	}
}

// statsReporterCounts is the delta a workspace-wide case expects. Every field
// is named so a failure reads as "OpenIssues moved by 2, want 1" rather than
// as two summaries a reader has to diff.
type statsReporterCounts struct {
	total      int
	open       int
	inProgress int
	closed     int
	deferred   int
	pinned     int
	blocked    int
}

// assertStatsReporterDelta compares two summaries field by field against the
// movement a case seeded.
//
// The blocked delta is only checked when BOTH summaries carry the number: a
// skipped scan reports nil, and the pairing case above is where that is
// asserted. Ready is not checked here at all — it is arithmetic over two
// fields this function already compares, and
// RunStatsReporterReadyIsOpenMinusBlocked pins the arithmetic itself.
func assertStatsReporterDelta(t *testing.T, before, after types.Statistics, want statsReporterCounts) {
	t.Helper()
	for _, field := range []struct {
		name          string
		before, after int
		want          int
	}{
		{"TotalIssues", before.TotalIssues, after.TotalIssues, want.total},
		{"OpenIssues", before.OpenIssues, after.OpenIssues, want.open},
		{"InProgressIssues", before.InProgressIssues, after.InProgressIssues, want.inProgress},
		{"ClosedIssues", before.ClosedIssues, after.ClosedIssues, want.closed},
		{"DeferredIssues", before.DeferredIssues, after.DeferredIssues, want.deferred},
		{"PinnedIssues", before.PinnedIssues, after.PinnedIssues, want.pinned},
	} {
		if got := field.after - field.before; got != field.want {
			t.Errorf("%s moved by %d (%d -> %d), want %d", field.name, got, field.before, field.after, field.want)
		}
	}
	if before.BlockedIssues != nil && after.BlockedIssues != nil {
		if got := *after.BlockedIssues - *before.BlockedIssues; got != want.blocked {
			t.Errorf("BlockedIssues moved by %d (%d -> %d), want %d",
				got, *before.BlockedIssues, *after.BlockedIssues, want.blocked)
		}
	}
}

// statsReporterSeed builds one issue with the fixture's prefix on its id, so
// several cases can share a database.
func statsReporterSeed(fixture StatsReporterFixture, suffix string, status types.Status) *types.Issue {
	id := fixture.IssuePrefix + "-" + suffix
	return &types.Issue{
		ID:        id,
		Title:     id,
		Status:    status,
		Priority:  2,
		IssueType: types.TypeTask,
	}
}

// statsReporterAssignee namespaces an actor to this fixture, which is what
// lets the assignee-scoped cases assert absolute numbers.
func statsReporterAssignee(fixture StatsReporterFixture, suffix string) string {
	return fixture.IssuePrefix + "-actor-" + suffix
}

func seedStatsReporterIssue(t *testing.T, ctx context.Context, fixture StatsReporterFixture, issue *types.Issue) {
	t.Helper()
	if err := fixture.CreateIssue(ctx, issue, "conformance"); err != nil {
		t.Fatalf("seed issue %s: %v", issue.ID, err)
	}
}

func seedStatsReporterWisp(t *testing.T, ctx context.Context, fixture StatsReporterFixture, issue *types.Issue) {
	t.Helper()
	if err := fixture.CreateWisp(ctx, issue, "conformance"); err != nil {
		t.Fatalf("seed wisp %s: %v", issue.ID, err)
	}
}

// blockStatsReporterIssue makes issueID blocked by dependsOnID. The edge is
// the only public way to set the denormalized flag the workspace-wide blocked
// count reads.
func blockStatsReporterIssue(t *testing.T, ctx context.Context, fixture StatsReporterFixture, issueID, dependsOnID string) {
	t.Helper()
	if err := fixture.AddDependency(ctx, &types.Dependency{
		IssueID:     issueID,
		DependsOnID: dependsOnID,
		Type:        types.DepBlocks,
	}, "conformance"); err != nil {
		t.Fatalf("block %s on %s: %v", issueID, dependsOnID, err)
	}
}

func statsReporterSummary(t *testing.T, ctx context.Context, fixture StatsReporterFixture, req publicops.StatsRequest) types.Statistics {
	t.Helper()
	result, err := fixture.StatsReporter.Stats(ctx, req)
	if err != nil {
		t.Fatalf("Stats(%+v): %v", req, err)
	}
	return result.Summary
}

func statsReporterAssigneeSummary(t *testing.T, ctx context.Context, fixture StatsReporterFixture, assignee string) types.Statistics {
	t.Helper()
	result, err := fixture.StatsReporter.AssigneeStats(ctx, publicops.AssigneeStatsRequest{Assignee: assignee})
	if err != nil {
		t.Fatalf("AssigneeStats(%q): %v", assignee, err)
	}
	return result.Summary
}

// statsReporterPointer reads one of the two nullable counts, failing loudly
// when it is absent. A case that dereferenced it directly would panic on a
// backend that answered nil, and the panic would name this file rather than
// the clause that was broken.
func statsReporterPointer(t *testing.T, name string, value *int) int {
	t.Helper()
	if value == nil {
		t.Fatalf("%s is nil on a summary that computed it", name)
	}
	return *value
}
