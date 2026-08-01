package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// This file holds the behavior contract every implementation of
// publicops.Lifecycle must satisfy, independent of how it reaches storage.
// There are three of them — the direct store, the embedded store, and the
// unit-of-work backend — and the first two share an execution path the third
// does not. Behavior asserted only against one backend has repeatedly drifted
// on the others, so each of these runs against all three from one spec.

// RunIssueOperationsCreateRoutesInfraTypesToWisps pins the facade create
// against the same infra-type routing the stores' own CreateIssue applies: a
// configured infra type is ephemeral and lives in the wisp tables, never in
// issues.
func RunIssueOperationsCreateRoutesInfraTypesToWisps(t *testing.T, ctx context.Context, fixture IssueOperationsStagingFixture) {
	t.Helper()
	for key, value := range map[string]string{"types.custom": "agent", "types.infra": "agent"} {
		if err := fixture.SetConfig(ctx, key, value); err != nil {
			t.Fatalf("SetConfig(%s): %v", key, err)
		}
	}

	result, err := fixture.Operations.Create(ctx, publicops.CreateRequest{
		Actor: "writer",
		Issue: &types.Issue{Title: "infra bead", Status: types.StatusOpen, Priority: 2, IssueType: types.IssueType("agent")},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !result.Issue.Ephemeral {
		t.Errorf("create result Ephemeral = false, want true for infra type %q", result.Issue.IssueType)
	}
	assertIssueOperationsRowCount(t, ctx, fixture, "wisps", result.Issue.ID, 1)
	assertIssueOperationsRowCount(t, ctx, fixture, "issues", result.Issue.ID, 0)

	// A no-history infra create keeps its no-history retention rather than
	// being upgraded to ephemeral, matching CreateIssue.
	noHistory, err := fixture.Operations.Create(ctx, publicops.CreateRequest{
		Actor: "writer",
		Issue: &types.Issue{Title: "infra no-history", Status: types.StatusOpen, Priority: 2, IssueType: types.IssueType("agent"), NoHistory: true},
	})
	if err != nil {
		t.Fatalf("Create no-history: %v", err)
	}
	if noHistory.Issue.Ephemeral {
		t.Errorf("no-history infra create Ephemeral = true, want false")
	}
	assertIssueOperationsRowCount(t, ctx, fixture, "wisps", noHistory.Issue.ID, 1)
	assertIssueOperationsRowCount(t, ctx, fixture, "issues", noHistory.Issue.ID, 0)

	// A non-infra type is unaffected.
	durable, err := fixture.Operations.Create(ctx, publicops.CreateRequest{
		Actor: "writer",
		Issue: &types.Issue{Title: "durable bead", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
	})
	if err != nil {
		t.Fatalf("Create durable: %v", err)
	}
	if durable.Issue.Ephemeral {
		t.Errorf("durable create Ephemeral = true, want false")
	}
	assertIssueOperationsRowCount(t, ctx, fixture, "issues", durable.Issue.ID, 1)
	assertIssueOperationsRowCount(t, ctx, fixture, "wisps", durable.Issue.ID, 0)
}

// RunIssueOperationsCreateRejectsMissingDependencyTargets pins the facade
// create against reporting success for an issue whose requested relationships
// were never written. The batch engine tolerates a dangling edge so a partial
// import still lands; a guarded single create must refuse the whole request
// with a typed error naming the target, and leave nothing behind.
func RunIssueOperationsCreateRejectsMissingDependencyTargets(t *testing.T, ctx context.Context, fixture IssueOperationsStagingFixture) {
	t.Helper()
	seed := &types.Issue{ID: fixture.IssuePrefix + "-skipdep-seed", Title: "seed", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := fixture.CreateIssue(ctx, seed, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []struct {
		name    string
		id      string
		request publicops.CreateRequest
		target  string
	}{
		{
			name:   "explicit dependency",
			id:     fixture.IssuePrefix + "-skipdep-explicit",
			target: fixture.IssuePrefix + "-skipdep-missing-dep",
			request: publicops.CreateRequest{
				Dependencies: []publicops.CreateDependency{{TargetID: fixture.IssuePrefix + "-skipdep-missing-dep", Type: types.DepBlocks}},
			},
		},
		{
			name:    "parent",
			id:      fixture.IssuePrefix + "-skipdep-parent",
			target:  fixture.IssuePrefix + "-skipdep-missing-parent",
			request: publicops.CreateRequest{ParentID: fixture.IssuePrefix + "-skipdep-missing-parent"},
		},
		{
			name:   "waits-for spawner",
			id:     fixture.IssuePrefix + "-skipdep-waits",
			target: fixture.IssuePrefix + "-skipdep-missing-spawner",
			request: publicops.CreateRequest{
				WaitsFor: &publicops.WaitsFor{SpawnerID: fixture.IssuePrefix + "-skipdep-missing-spawner"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.request
			request.Actor = "writer"
			request.ForceIDPrefix = true
			request.Issue = &types.Issue{ID: tc.id, Title: tc.name, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
			_, err := fixture.Operations.Create(ctx, request)
			if err == nil {
				t.Fatal("Create returned nil error, want a refusal for the missing dependency target")
			}
			if !errors.Is(err, publicops.ErrNotFound) {
				t.Errorf("Create error = %v, want ErrNotFound", err)
			}
			if !errors.Is(err, publicops.ErrValidation) {
				t.Errorf("Create error = %v, want ErrValidation", err)
			}
			if !strings.Contains(err.Error(), tc.target) {
				t.Errorf("Create error = %v, want it to name the missing target %q", err, tc.target)
			}
			assertIssueOperationsRowCount(t, ctx, fixture, "issues", tc.id, 0)
			assertIssueOperationsRowCount(t, ctx, fixture, "wisps", tc.id, 0)
		})
	}

	// A create whose targets all exist is unaffected.
	result, err := fixture.Operations.Create(ctx, publicops.CreateRequest{
		Actor:         "writer",
		ForceIDPrefix: true,
		Issue:         &types.Issue{ID: fixture.IssuePrefix + "-skipdep-ok", Title: "ok", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
		Dependencies:  []publicops.CreateDependency{{TargetID: seed.ID, Type: types.DepBlocks}},
	})
	if err != nil {
		t.Fatalf("Create with existing target: %v", err)
	}
	if len(result.Issue.Dependencies) != 1 || result.Issue.Dependencies[0].DependsOnID != seed.ID {
		t.Fatalf("Create result dependencies = %#v, want one edge to %s", result.Issue.Dependencies, seed.ID)
	}
}

// RunIssueOperationsUpdateFoldsMetadataIntoOneEvent pins a compound update to a
// single event. A guarded update is one atomic mutation, so its history must
// read as one entry; a metadata patch riding along with field edits must not
// write the row twice and fabricate a second event in the stream every history
// consumer sees.
func RunIssueOperationsUpdateFoldsMetadataIntoOneEvent(t *testing.T, ctx context.Context, fixture IssueOperationsStagingFixture) {
	t.Helper()
	issue := &types.Issue{
		ID: fixture.IssuePrefix + "-metadata-event", Title: "metadata event", Status: types.StatusOpen,
		Priority: 2, IssueType: types.TypeTask, Metadata: json.RawMessage(`{"keep":"old"}`),
	}
	if err := fixture.CreateIssue(ctx, issue, "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	events := newIssueOperationsEventCounter(t, ctx, fixture, issue.ID)

	updated, err := fixture.Operations.Update(ctx, publicops.UpdateRequest{Actor: "writer", IssueID: issue.ID, Patch: publicops.IssuePatch{
		Status: publicops.Field[publicops.Status]{Set: true, Value: types.StatusInProgress},
		Metadata: publicops.MetadataPatch{
			Set: map[string]json.RawMessage{"added": json.RawMessage(`"value"`)},
		},
	}})
	if err != nil {
		t.Fatalf("compound update: %v", err)
	}
	if !updated.Changed || updated.Issue.Status != types.StatusInProgress {
		t.Fatalf("compound update result = %#v", updated)
	}
	assertIssueOperationsMetadata(t, "compound update", updated.Issue.Metadata, `{"added":"value","keep":"old"}`)
	events.assert(t, "compound update", 1, map[types.EventType]int{types.EventStatusChanged: 1, types.EventUpdated: 0})

	// A metadata-only patch still records its own single event.
	metadataOnly, err := fixture.Operations.Update(ctx, publicops.UpdateRequest{Actor: "writer", IssueID: issue.ID, Patch: publicops.IssuePatch{
		Metadata: publicops.MetadataPatch{Unset: []string{"keep"}},
	}})
	if err != nil || !metadataOnly.Changed {
		t.Fatalf("metadata-only update = %#v, %v", metadataOnly, err)
	}
	assertIssueOperationsMetadata(t, "metadata-only update", metadataOnly.Issue.Metadata, `{"added":"value"}`)
	events.assert(t, "metadata-only update", 1, map[types.EventType]int{types.EventUpdated: 1})

	// A metadata patch that changes nothing stays elided.
	noOp, err := fixture.Operations.Update(ctx, publicops.UpdateRequest{Actor: "writer", IssueID: issue.ID, Patch: publicops.IssuePatch{
		Metadata: publicops.MetadataPatch{Set: map[string]json.RawMessage{"added": json.RawMessage(`"value"`)}},
	}})
	if err != nil || noOp.Changed {
		t.Fatalf("no-op metadata update = %#v, %v", noOp, err)
	}
	events.assert(t, "no-op metadata update", 0, nil)
}

// RunIssueOperationsUpdateClosePolicy pins what a generic status update does
// when it crosses from a non-done status into the done category: it answers to
// the same policy `bd close` does — the open-children refusal and the
// live-direct-blocker refusal — and ForceClosePolicy is how a caller overrides
// them. Until now the contract had no boundary-crossing case at all, and that
// gap is exactly how two earlier attempts at a shared policy check reached a
// backend that could not satisfy them without any test noticing.
func RunIssueOperationsUpdateClosePolicy(t *testing.T, ctx context.Context, fixture IssueOperationsStagingFixture) {
	t.Helper()

	parentID := fixture.IssuePrefix + "-closepolicy-parent"
	seedClosePolicyIssue(t, ctx, fixture, parentID, publicops.CreateRequest{})
	seedClosePolicyIssue(t, ctx, fixture, fixture.IssuePrefix+"-closepolicy-child", publicops.CreateRequest{ParentID: parentID})

	blockerID := fixture.IssuePrefix + "-closepolicy-blocker"
	blockedID := fixture.IssuePrefix + "-closepolicy-blocked"
	seedClosePolicyIssue(t, ctx, fixture, blockerID, publicops.CreateRequest{})
	seedClosePolicyIssue(t, ctx, fixture, blockedID, publicops.CreateRequest{
		Dependencies: []publicops.CreateDependency{{TargetID: blockerID, Type: types.DepBlocks}},
	})

	// An open child refuses, with the typed error and its count, and writes
	// nothing — not the row, not an event.
	events := newIssueOperationsEventCounter(t, ctx, fixture, parentID)
	var openChildrenErr *publicops.CloseOpenChildrenError
	_, err := fixture.Operations.Update(ctx, closePolicyStatusRequest(parentID, false))
	if !errors.As(err, &openChildrenErr) {
		t.Fatalf("update %s into done with an open child: err = %v, want CloseOpenChildrenError", parentID, err)
	}
	if openChildrenErr.OpenChildren != 1 {
		t.Errorf("refusal reported %d open children, want 1", openChildrenErr.OpenChildren)
	}
	assertClosePolicyStatus(t, ctx, fixture, parentID, types.StatusOpen)
	events.assert(t, "refused crossing", 0, nil)

	// A live direct blocker refuses too.
	_, err = fixture.Operations.Update(ctx, closePolicyStatusRequest(blockedID, false))
	if !errors.Is(err, publicops.ErrCloseBlocked) {
		t.Fatalf("update %s into done with a live blocker: err = %v, want ErrCloseBlocked", blockedID, err)
	}
	assertClosePolicyStatus(t, ctx, fixture, blockedID, types.StatusOpen)

	// Force bypasses close policy and nothing else. A stale ExpectedVersion is
	// an orthogonal precondition, checked ahead of the policy and never waived
	// by it — the same ordering a checked close applies.
	stale := int64(-1)
	staleRequest := closePolicyStatusRequest(parentID, true)
	staleRequest.ExpectedVersion = &stale
	if _, err := fixture.Operations.Update(ctx, staleRequest); !errors.Is(err, publicops.ErrVersionMismatch) {
		t.Fatalf("forced crossing with a stale version: err = %v, want ErrVersionMismatch", err)
	}
	assertClosePolicyStatus(t, ctx, fixture, parentID, types.StatusOpen)

	// ForceClosePolicy bypasses both, and only those.
	for _, id := range []string{parentID, blockedID} {
		forced, err := fixture.Operations.Update(ctx, closePolicyStatusRequest(id, true))
		if err != nil {
			t.Fatalf("forced update %s into done: %v", id, err)
		}
		if !forced.Changed || forced.Issue.Status != types.StatusClosed {
			t.Fatalf("forced update %s into done = %#v, want a committed close", id, forced)
		}
	}

	// A done-to-done restatement is filtered out as a no-op before any policy
	// could observe it, so it needs no force even though the child is still open.
	reclose, err := fixture.Operations.Update(ctx, closePolicyStatusRequest(parentID, false))
	if err != nil {
		t.Fatalf("restate %s as done: %v", parentID, err)
	}
	if reclose.Changed {
		t.Errorf("restating %s as done reported Changed = true, want a no-op", parentID)
	}

	// A status change that does not reach the done category is untouched by any
	// of this, open child or not.
	nonCrossing := closePolicyStatusRequest(parentID, false)
	nonCrossing.Patch.Status.Value = types.StatusInProgress
	if _, err := fixture.Operations.Update(ctx, nonCrossing); err != nil {
		t.Fatalf("non-crossing status update on %s: %v", parentID, err)
	}
	assertClosePolicyStatus(t, ctx, fixture, parentID, types.StatusInProgress)
}

// closePolicyStatusRequest builds the generic status update that crosses into
// the done category — the operation whose policy this case pins.
func closePolicyStatusRequest(id string, force bool) publicops.UpdateRequest {
	return publicops.UpdateRequest{
		Actor:            "writer",
		IssueID:          id,
		ForceClosePolicy: force,
		Patch:            publicops.IssuePatch{Status: publicops.Field[publicops.Status]{Set: true, Value: types.StatusClosed}},
	}
}

func assertClosePolicyStatus(t *testing.T, ctx context.Context, fixture IssueOperationsStagingFixture, id string, want types.Status) {
	t.Helper()
	var got string
	if err := fixture.QueryScalar(ctx, "SELECT status FROM issues WHERE id = ?", []any{id}, &got); err != nil {
		t.Fatalf("read status for %s: %v", id, err)
	}
	if types.Status(got) != want {
		t.Errorf("%s status = %q, want %q", id, got, want)
	}
}

// seedClosePolicyIssue creates one open task at an explicit ID, carrying any
// relationships the close-policy case needs. It goes through Create rather than
// the fixture's raw seed hook so the edges recompute is_blocked exactly as a
// real create would.
func seedClosePolicyIssue(t *testing.T, ctx context.Context, fixture IssueOperationsStagingFixture, id string, request publicops.CreateRequest) {
	t.Helper()
	request.Actor = "seed"
	request.ForceIDPrefix = true
	request.Issue = &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if _, err := fixture.Operations.Create(ctx, request); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func assertIssueOperationsRowCount(t *testing.T, ctx context.Context, fixture IssueOperationsStagingFixture, table, id string, want int) {
	t.Helper()
	var got int
	//nolint:gosec // G201: table is one of the contract's hardcoded table names
	if err := fixture.QueryScalar(ctx, "SELECT COUNT(*) FROM "+table+" WHERE id = ?", []any{id}, &got); err != nil {
		t.Fatalf("count %s rows for %s: %v", table, id, err)
	}
	if got != want {
		t.Errorf("%s rows for %s = %d, want %d", table, id, got, want)
	}
}

func assertIssueOperationsMetadata(t *testing.T, label string, got json.RawMessage, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s metadata %s: %v", label, got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("%s want metadata %s: %v", label, want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s metadata = %s, want %s", label, got, want)
	}
}

// issueOperationsEventCounter reports how many event rows each operation adds
// for one issue.
type issueOperationsEventCounter struct {
	ctx     context.Context
	fixture IssueOperationsStagingFixture
	id      string
	total   int
	byType  map[types.EventType]int
}

func newIssueOperationsEventCounter(t *testing.T, ctx context.Context, fixture IssueOperationsStagingFixture, id string) *issueOperationsEventCounter {
	t.Helper()
	counter := &issueOperationsEventCounter{ctx: ctx, fixture: fixture, id: id, byType: map[types.EventType]int{}}
	counter.total = counter.count(t, "")
	for _, eventType := range []types.EventType{types.EventUpdated, types.EventStatusChanged} {
		counter.byType[eventType] = counter.count(t, eventType)
	}
	return counter
}

func (c *issueOperationsEventCounter) count(t *testing.T, eventType types.EventType) int {
	t.Helper()
	query := "SELECT COUNT(*) FROM events WHERE issue_id = ?"
	args := []any{c.id}
	if eventType != "" {
		query += " AND event_type = ?"
		args = append(args, string(eventType))
	}
	var got int
	if err := c.fixture.QueryScalar(c.ctx, query, args, &got); err != nil {
		t.Fatalf("count events for %s (%q): %v", c.id, eventType, err)
	}
	return got
}

// assert checks the rows added since the previous assert and re-baselines.
func (c *issueOperationsEventCounter) assert(t *testing.T, label string, wantTotal int, wantByType map[types.EventType]int) {
	t.Helper()
	total := c.count(t, "")
	if got := total - c.total; got != wantTotal {
		t.Errorf("%s wrote %d event rows, want %d", label, got, wantTotal)
	}
	c.total = total
	for eventType, want := range wantByType {
		current := c.count(t, eventType)
		if got := current - c.byType[eventType]; got != want {
			t.Errorf("%s wrote %d %q events, want %d", label, got, eventType, want)
		}
	}
	for _, eventType := range []types.EventType{types.EventUpdated, types.EventStatusChanged} {
		c.byType[eventType] = c.count(t, eventType)
	}
}
