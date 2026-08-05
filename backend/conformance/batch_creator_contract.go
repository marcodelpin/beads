package conformance

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// This file holds the semantic contract every implementation of
// publicops.BatchCreator must satisfy. Each case asserts what
// issueops/batchcreator.go PROMISES, cited by line, rather than what any one
// backend happens to do; a backend that disagrees is parked at its own wiring
// site with skipKnownDivergence so the case still runs on the ones that agree.
//
// THERE ARE TWO BODIES BEHIND THE THREE WIRINGS, and here they are genuinely
// far apart. dolt and embeddeddolt share issueops.ExecuteCreateBatch, which
// prepares every item, assigns every id, and then hands the whole slice to ONE
// CreateIssuesInTxWithResult — rows first, edges after. The unit-of-work
// provider creates item by item through the domain use case, writing each
// item's edges as it writes that item. So "all or nothing" is the same promise
// kept two different ways: one by a single engine call inside a transaction,
// the other by returning early out of a loop and letting the transaction roll
// back. That difference is exactly why the atomicity cases below assert on the
// STORE afterwards rather than on the error alone.
//
// IT IS ALSO WHY NO CASE ASKS FOR A FORWARD REFERENCE. An item's edge may name
// an item created EARLIER in the same batch (issueops/batchcreator.go:36-45)
// and nothing more: the shared store body could resolve a later item too, the
// unit-of-work body provably cannot, and a case written to the stronger reading
// would be pinning one backend's implementation rather than the role.
//
// EVERY CASE NAMES ITS OWN IDS UNDER THE FIXTURE PREFIX and passes
// ForceIDPrefix, the way the Lifecycle create contract does: an explicit id is
// the only way to observe the create-only guard and the only way to give an
// edge a target it can name, and the fixture's prefix is not the workspace's
// configured one.

// BatchCreatorFixture supplies adapter-specific storage access for the
// batch-create assertions. Every field is named and typed exactly like the
// per-backend roleFixtureKit hook it is filled from, so a wiring is kit plus
// accessor plus prefix with no adapter in between.
type BatchCreatorFixture struct {
	// IssuePrefix namespaces the ids each assertion seeds, so several of them
	// can share one database.
	IssuePrefix string
	// BatchCreator is the surface under test.
	BatchCreator publicops.BatchCreator
	// CreateIssue seeds a durable issue in the issues plane, which is how a
	// case occupies an id before asking the role to create over it.
	CreateIssue func(context.Context, *types.Issue, string) error
	// QueryScalar runs a single-row query and scans it, and RETURNS the error
	// rather than failing the test, so a case can assert on a query that is
	// expected to fail.
	QueryScalar func(context.Context, string, []any, ...any) error
	// CountHistory reports how many history entries the fixture's branch has.
	// A nil hook means "this backend cannot observe history", and the cases
	// that need it SKIP with that reason rather than passing quietly.
	CountHistory func(context.Context) (int, error)
}

// RunBatchCreatorCreatesEveryItemAsOneAct pins the shape of a successful batch
// (issueops/batchcreator.go:88-98): one entry per item, in request order, never
// nil, each a hydrated post-create snapshot, and the generated ids readable off
// the result.
//
// The generated ids are the point of the case rather than a detail of it. They
// are the ONE fact the request cannot carry — a caller that cannot read them
// back has created rows it can never name again — and `bd create --file`, which
// is the front door this role exists for, prints them.
//
// The labels are asserted for the same reason CreateResult.Issue promises them:
// the front door prints the issue it got back rather than re-reading it, so a
// snapshot that dropped its labels would be a silently emptier `--json` than
// the single create's.
func RunBatchCreatorCreatesEveryItemAsOneAct(t *testing.T, ctx context.Context, fixture BatchCreatorFixture) {
	t.Helper()
	request := publicops.CreateBatchRequest{
		Actor: "batch-writer",
		Items: []publicops.BatchCreateItem{
			batchCreatorItem(&types.Issue{Title: "first", Priority: 1, IssueType: types.TypeTask, Labels: []string{"alpha"}}),
			batchCreatorItem(&types.Issue{Title: "second", Priority: 2, IssueType: types.TypeBug}),
			batchCreatorItem(&types.Issue{Title: "third", Priority: 3, IssueType: types.TypeFeature}),
		},
	}
	result, err := fixture.BatchCreator.CreateBatch(ctx, request)
	if err != nil {
		t.Fatalf("CreateBatch(3 items): %v", err)
	}
	if len(result.Issues) != len(request.Items) {
		t.Fatalf("CreateBatch returned %d issues for %d items; the result is promised one entry per item at the same index",
			len(result.Issues), len(request.Items))
	}
	seen := map[string]bool{}
	for i, issue := range result.Issues {
		if issue == nil {
			t.Fatalf("CreateBatch result issue %d is nil; a batch that could not create every item creates none, "+
				"so no index has nothing to put at it", i)
		}
		if issue.Title != request.Items[i].Issue.Title {
			t.Errorf("result issue %d title = %q, want %q: the result is promised in REQUEST ORDER", i, issue.Title, request.Items[i].Issue.Title)
		}
		if issue.ID == "" {
			t.Fatalf("result issue %d carries no id; the generated id is the one fact the request cannot carry", i)
		}
		if seen[issue.ID] {
			t.Fatalf("result issue %d repeats id %q", i, issue.ID)
		}
		seen[issue.ID] = true
		assertBatchCreatorRowCount(t, ctx, fixture, "issues", issue.ID, 1)
	}
	if labels := result.Issues[0].Labels; len(labels) != 1 || labels[0] != "alpha" {
		t.Errorf("result issue 0 labels = %v, want [alpha]: the snapshot is promised hydrated with labels", labels)
	}
}

// RunBatchCreatorRefusesEverythingWhenOneItemRefuses is the case this role
// exists for (issueops/batchcreator.go:106-116, 117-124): a batch is ALL OR
// NOTHING, so an item the batch cannot create leaves the items around it
// uncreated too.
//
// The refusing item is in the MIDDLE on purpose. An implementation that
// validated everything up front and then wrote would pass a case whose bad item
// came first; an implementation that wrote as it went would pass one whose bad
// item came last. Only an item with landed work on both sides of it can tell
// "the batch refused" from "the batch stopped".
//
// It asserts on the STORE and not on the error, because the error is the half
// both readings agree on. The item after the refusal was never going to be
// there; the item BEFORE it is the whole question.
func RunBatchCreatorRefusesEverythingWhenOneItemRefuses(t *testing.T, ctx context.Context, fixture BatchCreatorFixture) {
	t.Helper()
	occupied := fixture.IssuePrefix + "-bcall-occupied"
	seedBatchCreatorIssue(t, ctx, fixture, occupied)

	before := fixture.IssuePrefix + "-bcall-before"
	after := fixture.IssuePrefix + "-bcall-after"
	_, err := fixture.BatchCreator.CreateBatch(ctx, publicops.CreateBatchRequest{
		Actor:         "batch-writer",
		ForceIDPrefix: true,
		Items: []publicops.BatchCreateItem{
			batchCreatorItem(batchCreatorIssue(before, "lands before the refusal")),
			batchCreatorItem(batchCreatorIssue(occupied, "collides")),
			batchCreatorItem(batchCreatorIssue(after, "never reached")),
		},
	})
	if err == nil {
		t.Fatal("CreateBatch over an occupied id returned no error; an occupied explicit id is ErrAlreadyExists")
	}
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("CreateBatch error = %v, want ErrAlreadyExists: the batch's refusals are Lifecycle.Create's, in the same typed vocabulary", err)
	}
	assertBatchCreatorRowCount(t, ctx, fixture, "issues", before, 0)
	assertBatchCreatorRowCount(t, ctx, fixture, "issues", after, 0)
	// The seeded row is untouched: a refusal that had upserted would report the
	// same error and still have rewritten every column.
	assertBatchCreatorScalar(t, ctx, fixture, "occupied title", occupied,
		"SELECT title FROM issues WHERE id = ?", []any{occupied})
}

// RunBatchCreatorRejectsAnUnusableRequest pins the deterministic
// request-validation refusals (issueops/batchcreator.go:56-67, 143-149): an
// empty Actor, no items at all, and an item with no issue are each
// ErrValidation and each leave persistent state unchanged.
//
// The empty-items clause is the one worth stating twice, because the read
// batches beside this role answer an empty request with an empty answer. A
// WRITE batch that wrote nothing is a caller bug, and answering it with a
// cheerful empty success is how a front door with a filtered-to-nothing list
// silently stops creating anything.
func RunBatchCreatorRejectsAnUnusableRequest(t *testing.T, ctx context.Context, fixture BatchCreatorFixture) {
	t.Helper()
	item := batchCreatorItem(&types.Issue{Title: "unreachable", Priority: 2, IssueType: types.TypeTask})
	for _, test := range []struct {
		name    string
		request publicops.CreateBatchRequest
	}{
		{"no actor", publicops.CreateBatchRequest{Items: []publicops.BatchCreateItem{item}}},
		{"no items", publicops.CreateBatchRequest{Actor: "batch-writer"}},
		{"nil issue", publicops.CreateBatchRequest{Actor: "batch-writer", Items: []publicops.BatchCreateItem{{}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := fixture.BatchCreator.CreateBatch(ctx, test.request)
			if !errors.Is(err, storage.ErrValidation) {
				t.Fatalf("CreateBatch(%s) error = %v, want ErrValidation", test.name, err)
			}
			if len(result.Issues) != 0 {
				t.Errorf("CreateBatch(%s) returned %d issues with an error; result values are unspecified but a refusal creates nothing",
					test.name, len(result.Issues))
			}
		})
	}
}

// RunBatchCreatorLinksAnEarlierItemOfTheSameBatch pins the capability that
// makes the batch more than a loop (issueops/batchcreator.go:36-45): an item's
// edge may name an item created EARLIER in the same request, and the edge is
// written.
//
// One call at a time this is impossible, because the target does not exist when
// the source is created. It is the reason a caller writing a plan file gets a
// graph and not a pile of unlinked issues.
func RunBatchCreatorLinksAnEarlierItemOfTheSameBatch(t *testing.T, ctx context.Context, fixture BatchCreatorFixture) {
	t.Helper()
	first := fixture.IssuePrefix + "-bclink-first"
	second := fixture.IssuePrefix + "-bclink-second"
	result, err := fixture.BatchCreator.CreateBatch(ctx, publicops.CreateBatchRequest{
		Actor:         "batch-writer",
		ForceIDPrefix: true,
		Items: []publicops.BatchCreateItem{
			batchCreatorItem(batchCreatorIssue(first, "the blocker")),
			{
				Issue:        batchCreatorIssue(second, "the blocked"),
				Dependencies: []publicops.CreateDependency{{TargetID: first, Type: types.DepBlocks}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateBatch with an edge onto an earlier item: %v", err)
	}
	if len(result.Issues) != 2 {
		t.Fatalf("CreateBatch returned %d issues, want 2", len(result.Issues))
	}
	assertBatchCreatorEdgeCount(t, ctx, fixture, second, first, 1)
}

// RunBatchCreatorRefusesAnAbsentEdgeTarget pins the edge clause
// (issueops/batchcreator.go:125-131): every requested edge is written or the
// batch refuses, with ErrValidation wrapping ErrNotFound and nothing created.
//
// The target here shares the SOURCE'S PREFIX, which is what makes it a miss
// rather than a foreign reference — see the case below. The sibling item is
// there to assert the refusal is the BATCH'S: an implementation that dropped
// the edge and kept the issues would leave it behind.
func RunBatchCreatorRefusesAnAbsentEdgeTarget(t *testing.T, ctx context.Context, fixture BatchCreatorFixture) {
	t.Helper()
	sibling := fixture.IssuePrefix + "-bcmiss-sibling"
	source := fixture.IssuePrefix + "-bcmiss-source"
	absent := fixture.IssuePrefix + "-bcmiss-absent"

	_, err := fixture.BatchCreator.CreateBatch(ctx, publicops.CreateBatchRequest{
		Actor:         "batch-writer",
		ForceIDPrefix: true,
		Items: []publicops.BatchCreateItem{
			batchCreatorItem(batchCreatorIssue(sibling, "would land")),
			{
				Issue:        batchCreatorIssue(source, "names something absent"),
				Dependencies: []publicops.CreateDependency{{TargetID: absent, Type: types.DepBlocks}},
			},
		},
	})
	if err == nil {
		t.Fatal("CreateBatch with an edge onto an absent target returned no error; " +
			"a create that reported success having dropped an edge is data loss the caller cannot learn about")
	}
	if !errors.Is(err, storage.ErrValidation) || !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("CreateBatch error = %v, want ErrValidation wrapping ErrNotFound", err)
	}
	assertBatchCreatorRowCount(t, ctx, fixture, "issues", sibling, 0)
	assertBatchCreatorRowCount(t, ctx, fixture, "issues", source, 0)
}

// RunBatchCreatorAcceptsAForeignEdgeTarget pins the other half of the same
// clause (issueops/batchcreator.go:132-137): a target this database was never
// going to hold is not a miss.
//
// Both shapes are asserted because they are one rule with two spellings
// (issueops.IsExternalDepTarget): an `external:` reference names something
// outside beads entirely, and an id whose prefix belongs to another repository
// lives in that rig's database. A role that refused either would make a plan
// naming work in a sibling rig uncreatable, which is a thing `bd dep add` has
// always allowed.
func RunBatchCreatorAcceptsAForeignEdgeTarget(t *testing.T, ctx context.Context, fixture BatchCreatorFixture) {
	t.Helper()
	external := fixture.IssuePrefix + "-bcext-external"
	foreign := fixture.IssuePrefix + "-bcext-foreign"
	externalTarget := "external:JIRA-4471"
	foreignTarget := "otherrig-9910"

	result, err := fixture.BatchCreator.CreateBatch(ctx, publicops.CreateBatchRequest{
		Actor:         "batch-writer",
		ForceIDPrefix: true,
		Items: []publicops.BatchCreateItem{
			{
				Issue:        batchCreatorIssue(external, "depends on something outside beads"),
				Dependencies: []publicops.CreateDependency{{TargetID: externalTarget, Type: types.DepBlocks}},
			},
			{
				Issue:        batchCreatorIssue(foreign, "depends on another rig"),
				Dependencies: []publicops.CreateDependency{{TargetID: foreignTarget, Type: types.DepBlocks}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateBatch with foreign edge targets: %v", err)
	}
	if len(result.Issues) != 2 {
		t.Fatalf("CreateBatch returned %d issues, want 2", len(result.Issues))
	}
	assertBatchCreatorEdgeCount(t, ctx, fixture, external, externalTarget, 1)
	assertBatchCreatorEdgeCount(t, ctx, fixture, foreign, foreignTarget, 1)
}

// RunBatchCreatorRecordsOneHistoryEntry pins the durable half of "at most one
// history entry" (issueops/batchcreator.go:117-124): a batch of three durable
// items records ONE entry, not three and not none.
//
// The Provenance half is asserted in the same case rather than its own, because
// the promise is that a label changes how the entry READS and never WHETHER one
// is recorded (issueops/batchcreator.go:68-78). Two batches, one labeled and
// one not, must therefore move the counter by the same amount — and the entry's
// TEXT is deliberately not asserted here: reading it back means ordering two
// commits that can tie on date, which the fixture's own doc warns against.
func RunBatchCreatorRecordsOneHistoryEntry(t *testing.T, ctx context.Context, fixture BatchCreatorFixture) {
	t.Helper()
	if fixture.CountHistory == nil {
		t.Skip("this backend cannot observe history entries")
	}
	for _, test := range []struct {
		name       string
		provenance string
	}{
		{"default label", ""},
		{"caller label", "bd: create 3 issue(s) from plan.md"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := batchCreatorHistoryCount(t, ctx, fixture)
			if _, err := fixture.BatchCreator.CreateBatch(ctx, publicops.CreateBatchRequest{
				Actor:      "batch-writer",
				Provenance: test.provenance,
				Items: []publicops.BatchCreateItem{
					batchCreatorItem(&types.Issue{Title: "one", Priority: 2, IssueType: types.TypeTask}),
					batchCreatorItem(&types.Issue{Title: "two", Priority: 2, IssueType: types.TypeTask}),
					batchCreatorItem(&types.Issue{Title: "three", Priority: 2, IssueType: types.TypeTask}),
				},
			}); err != nil {
				t.Fatalf("CreateBatch(3 durable items): %v", err)
			}
			if delta := batchCreatorHistoryCount(t, ctx, fixture) - before; delta != 1 {
				t.Errorf("history entries += %d for a 3-item batch, want 1: the request is the transaction, so it records one entry", delta)
			}
		})
	}
}

// RunBatchCreatorRecordsNoHistoryForAnEphemeralBatch pins the other half
// (issueops/batchcreator.go:117-124): an all-ephemeral batch records NO durable
// entry, because the wisp tables are dolt-ignored precisely so ephemeral work
// never ships.
//
// It also asserts the wisps ARE there, and that is not padding. The
// unit-of-work backend reads an empty commit message as "roll this attempt
// back", so the obvious way to record no entry — return no message — silently
// discards everything the batch created. Counting the entry without checking
// the rows would call that a pass.
func RunBatchCreatorRecordsNoHistoryForAnEphemeralBatch(t *testing.T, ctx context.Context, fixture BatchCreatorFixture) {
	t.Helper()
	if fixture.CountHistory == nil {
		t.Skip("this backend cannot observe history entries")
	}
	first := fixture.IssuePrefix + "-bcwisp-first"
	second := fixture.IssuePrefix + "-bcwisp-second"

	before := batchCreatorHistoryCount(t, ctx, fixture)
	firstIssue := batchCreatorIssue(first, "ephemeral one")
	firstIssue.Ephemeral = true
	secondIssue := batchCreatorIssue(second, "ephemeral two")
	secondIssue.Ephemeral = true
	result, err := fixture.BatchCreator.CreateBatch(ctx, publicops.CreateBatchRequest{
		Actor:         "batch-writer",
		ForceIDPrefix: true,
		Items:         []publicops.BatchCreateItem{batchCreatorItem(firstIssue), batchCreatorItem(secondIssue)},
	})
	if err != nil {
		t.Fatalf("CreateBatch(2 ephemeral items): %v", err)
	}
	if len(result.Issues) != 2 {
		t.Fatalf("CreateBatch returned %d issues, want 2", len(result.Issues))
	}
	assertBatchCreatorRowCount(t, ctx, fixture, "wisps", first, 1)
	assertBatchCreatorRowCount(t, ctx, fixture, "wisps", second, 1)
	if delta := batchCreatorHistoryCount(t, ctx, fixture) - before; delta != 0 {
		t.Errorf("history entries += %d for an all-ephemeral batch, want 0: a durable entry naming a wisp is the sync artifact "+
			"the ignored wisp tables exist to prevent", delta)
	}
}

// RunBatchCreatorDoesNotMutateTheCallerRequest pins the snapshot clause
// (issueops/batchcreator.go:143-149). The ID is the field that matters: an
// implementation that assigned it in place would leave the caller holding an
// issue that now names a stored row, and the caller's next create with the same
// struct would refuse as an occupied id.
func RunBatchCreatorDoesNotMutateTheCallerRequest(t *testing.T, ctx context.Context, fixture BatchCreatorFixture) {
	t.Helper()
	target := fixture.IssuePrefix + "-bcsnap-target"
	seedBatchCreatorIssue(t, ctx, fixture, target)

	request := publicops.CreateBatchRequest{
		Actor:      "batch-writer",
		Provenance: "bd: create 1 issue(s) from plan.md",
		Items: []publicops.BatchCreateItem{{
			Issue:        &types.Issue{Title: "caller owned", Priority: 2, IssueType: types.TypeTask, Labels: []string{"kept"}},
			Dependencies: []publicops.CreateDependency{{TargetID: target, Type: types.DepBlocks}},
		}},
	}
	snapshot := publicops.CreateBatchRequest{
		Actor:      request.Actor,
		Provenance: request.Provenance,
		Items: []publicops.BatchCreateItem{{
			Issue:        &types.Issue{Title: "caller owned", Priority: 2, IssueType: types.TypeTask, Labels: []string{"kept"}},
			Dependencies: []publicops.CreateDependency{{TargetID: target, Type: types.DepBlocks}},
		}},
	}
	if _, err := fixture.BatchCreator.CreateBatch(ctx, request); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if !reflect.DeepEqual(request, snapshot) {
		t.Errorf("CreateBatch mutated the caller's request:\n got %+v\nwant %+v", *request.Items[0].Issue, *snapshot.Items[0].Issue)
	}
}

// batchCreatorItem is the item every case builds, so a case names only the
// fields it is about.
func batchCreatorItem(issue *types.Issue) publicops.BatchCreateItem {
	return publicops.BatchCreateItem{Issue: issue}
}

// batchCreatorIssue is an issue with an EXPLICIT id, for the cases that have to
// name their rows — the create-only guard and every edge target.
func batchCreatorIssue(id, title string) *types.Issue {
	return &types.Issue{ID: id, Title: title, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
}

// seedBatchCreatorIssue occupies an id through the fixture's own create, which
// is deliberately NOT the role under test: a case that seeded through
// CreateBatch would be asserting the role against itself.
func seedBatchCreatorIssue(t *testing.T, ctx context.Context, fixture BatchCreatorFixture, id string) {
	t.Helper()
	if err := fixture.CreateIssue(ctx, batchCreatorIssue(id, id), "seeder"); err != nil {
		t.Fatalf("seed issue %s: %v", id, err)
	}
}

// assertBatchCreatorRowCount asserts how many rows of a plane carry an id.
func assertBatchCreatorRowCount(t *testing.T, ctx context.Context, fixture BatchCreatorFixture, table, id string, want int) {
	t.Helper()
	var got int
	query := "SELECT COUNT(*) FROM " + table + " WHERE id = ?"
	if err := fixture.QueryScalar(ctx, query, []any{id}, &got); err != nil {
		t.Fatalf("count %s rows for %s: %v", table, id, err)
	}
	if got != want {
		t.Errorf("%s rows for %s = %d, want %d", table, id, got, want)
	}
}

// assertBatchCreatorEdgeCount asserts how many stored edges run from source to
// target, across BOTH dependency tables and all three target columns, because
// which of each the row landed in is a placement detail this role makes no
// promise about.
func assertBatchCreatorEdgeCount(t *testing.T, ctx context.Context, fixture BatchCreatorFixture, source, target string, want int) {
	t.Helper()
	var got int
	const query = `SELECT
		(SELECT COUNT(*) FROM dependencies WHERE issue_id = ?
			AND (depends_on_issue_id = ? OR depends_on_wisp_id = ? OR depends_on_external = ?)) +
		(SELECT COUNT(*) FROM wisp_dependencies WHERE issue_id = ?
			AND (depends_on_issue_id = ? OR depends_on_wisp_id = ? OR depends_on_external = ?))`
	args := []any{source, target, target, target, source, target, target, target}
	if err := fixture.QueryScalar(ctx, query, args, &got); err != nil {
		t.Fatalf("count edges %s -> %s: %v", source, target, err)
	}
	if got != want {
		t.Errorf("edges %s -> %s = %d, want %d", source, target, got, want)
	}
}

// assertBatchCreatorScalar asserts one stored column value.
func assertBatchCreatorScalar(t *testing.T, ctx context.Context, fixture BatchCreatorFixture, what, want, query string, args []any) {
	t.Helper()
	var got string
	if err := fixture.QueryScalar(ctx, query, args, &got); err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	if got != want {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

// batchCreatorHistoryCount reads the fixture's history counter.
func batchCreatorHistoryCount(t *testing.T, ctx context.Context, fixture BatchCreatorFixture) int {
	t.Helper()
	entries, err := fixture.CountHistory(ctx)
	if err != nil {
		t.Fatalf("count history entries: %v", err)
	}
	return entries
}
