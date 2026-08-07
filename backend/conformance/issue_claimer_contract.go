package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// This file holds the semantic contract every implementation of
// publicops.Claimer must satisfy — the ROLE reached through
// storage.Storage.IssueClaimer(), not the store's ClaimIssue method.
//
// The suite already pins the store method's sentinels (claim.go: testClaim,
// testClaimIdempotent, testClaimAlreadyClaimed, testClaimOpenForeignAssignee,
// testClaimNotClaimable), and it already pins the typed conflict payload and
// the winning claim's persisted state at the Update seam
// (issue_operations_contract.go). This file exists because neither reaches what
// the role promises:
//
//   - DIFFERENT PRODUCERS. The role's outermost refusal classifiers rebuild the
//     conflict payload from their own in-transaction re-read
//     (internal/storage/issueops/public_claim.go classifyClaimRefusalInTx and
//     internal/storage/uow/issue_claimer.go classifyClaimError). The Update-seam
//     cases execute neither. ClaimResult itself — Changed, and the promise that
//     the snapshot is the BARE row — is structurally invisible to claim.go,
//     whose subject returns a plain error.
//   - DIFFERENT REACHABILITY. The Update-seam cases run only behind
//     IssueOperationsStagingFixture, which demands raw-SQL hooks a partial or
//     remote backend cannot supply. Every case here binds the accessor and the
//     public role API and nothing else, so any backend that can answer
//     IssueClaimer() runs them.
//
// The winning-claim leg of WinsThenReclaimsIdempotently and the whole of
// ForeignHolderRefusalCarriesTheHolder are deliberate re-pins of assertions the
// Update-seam cases also make: same typed error, different seam, different
// producers, and reachable without the staging fixture. The overlap is named
// rather than hidden. Everything else here — the idempotent Changed:false leg,
// the bare-snapshot promise, the unknown-id, wisp-id and empty-actor refusals,
// and the message fragments — has no owning proof anywhere.
//
// WHAT IS DELIBERATELY NOT HERE: the lease the winning claim grants, which
// claim.go already pins on the store method; and two racing claimants producing
// one winner, which is a transaction property of the persistence boundary
// rather than a promised result of the operation.

// ClaimerFixture supplies adapter-specific storage access for the claim-role
// assertions. It is constructible from a factory store alone — the accessor
// plus CreateIssue and GetIssue — which is what lets RunAll compose it with no
// per-backend wiring while a partial backend fills the same fields itself.
type ClaimerFixture struct {
	// IssuePrefix namespaces the ids each assertion seeds, so several of them
	// can share one database.
	IssuePrefix string
	Claimer     publicops.Claimer
	// CreateIssue seeds a durable issue in the issues plane.
	CreateIssue func(context.Context, *types.Issue, string) error
	// CreateWisp seeds an ephemeral issue in the wisps plane. It is a separate
	// field rather than an Ephemeral flag on CreateIssue because adapters reach
	// the two planes through different verbs.
	CreateWisp func(context.Context, *types.Issue, string) error
	// GetIssue reads a row back, for the post-state and unchanged-state halves
	// of every case.
	GetIssue func(context.Context, string) (*types.Issue, error)
	// CountClaimEvents reports how many claim events an issue carries. Nil
	// means the backend cannot observe events: the idempotent leg then drops
	// its "and persisted nothing" check and keeps the rest of its subject.
	CountClaimEvents func(context.Context, string) (int, error)
}

// RunClaimWinsThenReclaimsIdempotently pins what a winning claim ANSWERS and
// what the same actor's second claim does not do (issueops/claimer.go:42-48):
// the win reports a post-state snapshot with Changed true, and the re-claim is
// a success with Changed false that writes nothing.
//
// The snapshot is asserted to be BARE. That is the one place this family
// differs from UpdateResult, CloseResult and ReopenResult, all of which carry
// labels and dependency records, and the reason is that the role answers an
// already-published surface — the v0 claim response and `bd update --claim
// --json` both report the bare row (issueops/claimer.go:14-24). A backend that
// hydrated here would change a shipped body, so the seeded issue carries a
// label the snapshot must not report.
//
// Changed is the only signal a polling caller has that its retry was a no-op,
// and staging nothing for it is what keeps such a caller from minting an empty
// version-control commit per call.
func RunClaimWinsThenReclaimsIdempotently(t *testing.T, ctx context.Context, fixture ClaimerFixture) {
	t.Helper()
	id := fixture.IssuePrefix + "-win"
	seedClaimerIssue(t, ctx, fixture, claimerIssue(id, id+"-label"))

	result, err := fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "worker", IssueID: id})
	if err != nil {
		t.Fatalf("Claim of a claimable issue: %v", err)
	}
	if !result.Changed {
		t.Error("Changed = false on the winning claim, want true")
	}
	assertClaimerSnapshot(t, result, id, "worker", types.StatusInProgress)
	if snapshot := result.Issue; snapshot != nil {
		if len(snapshot.Labels) != 0 || len(snapshot.Dependencies) != 0 || len(snapshot.Comments) != 0 {
			t.Errorf("snapshot carries relations (labels %v, %d dependencies, %d comments), want the bare row",
				snapshot.Labels, len(snapshot.Dependencies), len(snapshot.Comments))
		}
	}
	assertClaimerRowState(t, ctx, fixture, id, types.StatusInProgress, "worker", "after the winning claim")

	var before int
	if fixture.CountClaimEvents != nil {
		before = claimerEventCount(t, ctx, fixture, id)
	}

	again, err := fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "worker", IssueID: id})
	if err != nil {
		t.Fatalf("re-claim by the holder: %v, want an idempotent success", err)
	}
	if again.Changed {
		t.Error("Changed = true on the holder's re-claim, want false — nothing was written")
	}
	assertClaimerSnapshot(t, again, id, "worker", types.StatusInProgress)
	assertClaimerRowState(t, ctx, fixture, id, types.StatusInProgress, "worker", "after the idempotent re-claim")

	if fixture.CountClaimEvents != nil {
		if after := claimerEventCount(t, ctx, fixture, id); after != before {
			t.Errorf("claim events went %d -> %d across the idempotent re-claim, want no change", before, after)
		}
	}
}

// RunClaimForeignHolderRefusalCarriesTheHolder pins the refusal a caller
// classifies on: a claim of an issue someone else holds answers a
// *issueops.ClaimConflictError carrying that holder's assignee and status, read
// inside the transaction that lost (issueops/errors.go:19-27), still matching
// storage.ErrAlreadyClaimed through the wrap, and leaving the row exactly as
// the winner left it.
//
// This re-pins the Update-seam payload assertions at the role seam. The value
// is the seam and the producer: the classifier that builds this payload for the
// role is not the one the Update seam runs, and this case reaches it with
// nothing but the accessor.
func RunClaimForeignHolderRefusalCarriesTheHolder(t *testing.T, ctx context.Context, fixture ClaimerFixture) {
	t.Helper()
	id := fixture.IssuePrefix + "-held"
	seedClaimerIssue(t, ctx, fixture, claimerIssue(id))

	if _, err := fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "worker1", IssueID: id}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	held := claimerRow(t, ctx, fixture, id)

	_, err := fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "worker2", IssueID: id})
	conflict := claimerConflict(t, err, storage.ErrAlreadyClaimed, id)
	if conflict.Assignee != "worker1" {
		t.Errorf("conflict.Assignee = %q, want worker1 — the holder that beat the claim", conflict.Assignee)
	}
	if conflict.Status != types.StatusInProgress {
		t.Errorf("conflict.Status = %q, want %q", conflict.Status, types.StatusInProgress)
	}

	after := claimerRow(t, ctx, fixture, id)
	if after.Assignee != held.Assignee || after.Status != held.Status {
		t.Errorf("the refused claim moved the row: {status %q assignee %q} -> {status %q assignee %q}",
			held.Status, held.Assignee, after.Status, after.Assignee)
	}
	if !claimerTimeEqual(after.StartedAt, held.StartedAt) {
		t.Errorf("the refused claim restamped started_at: %v -> %v", held.StartedAt, after.StartedAt)
	}
}

// RunClaimRefusesIneligibleUnknownAndActorlessRequests pins the three refusals
// the role owns alone, each with the state clause the leaf attaches to it:
// "Refusals and deterministic validation failures leave persistent state
// unchanged" (issueops/claimer.go:57-58).
//
//   - An ineligible status wraps storage.ErrNotClaimable.
//   - An id no row holds answers storage.ErrNotFound.
//   - A WISP id answers storage.ErrNotFound too: "the wisp plane is not
//     claimable through this role" (issueops/claimer.go:63-64). The refusal
//     lands before any write the enclosing transaction would have to roll back,
//     which is only observable from outside as the wisp staying open.
//   - A request with no actor answers storage.ErrValidation. The actor is the
//     audit trail the storage commit carries, so an implementation that
//     defaulted it would write an unattributable claim.
//
// The claimable row seeded for the actorless leg is the point of its state
// half: the request would have won, so an implementation that validated after
// claiming fails on the row rather than on the error.
func RunClaimRefusesIneligibleUnknownAndActorlessRequests(t *testing.T, ctx context.Context, fixture ClaimerFixture) {
	t.Helper()
	closed := fixture.IssuePrefix + "-closed"
	wisp := fixture.IssuePrefix + "-wisp-refuse"
	actorless := fixture.IssuePrefix + "-actorless"

	closedIssue := claimerIssue(closed)
	closedIssue.Status = types.StatusClosed
	seedClaimerIssue(t, ctx, fixture, closedIssue)
	seedClaimerWisp(t, ctx, fixture, claimerIssue(wisp))
	seedClaimerIssue(t, ctx, fixture, claimerIssue(actorless))

	if _, err := fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "worker", IssueID: closed}); !errors.Is(err, storage.ErrNotClaimable) {
		t.Errorf("claim of a closed issue = %v, want ErrNotClaimable", err)
	}
	assertClaimerRowState(t, ctx, fixture, closed, types.StatusClosed, "", "after the refused claim of a closed issue")

	unknown := fixture.IssuePrefix + "-nobody"
	if _, err := fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "worker", IssueID: unknown}); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("claim of an unknown id = %v, want ErrNotFound", err)
	}

	if _, err := fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "worker", IssueID: wisp}); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("claim of a wisp id = %v, want ErrNotFound — the wisp plane is not claimable through this role", err)
	}
	assertClaimerRowState(t, ctx, fixture, wisp, types.StatusOpen, "", "after the refused claim of a wisp")

	if _, err := fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "", IssueID: actorless}); !errors.Is(err, storage.ErrValidation) {
		t.Errorf("claim with no actor = %v, want ErrValidation", err)
	}
	assertClaimerRowState(t, ctx, fixture, actorless, types.StatusOpen, "", "after the actorless request")
}

// RunClaimRefusalMessagesCarryTheirFragments pins the string coupling the
// published parser depends on. beads.ParseClaimConflict rebuilds its marker as
// sentinel.Error()+fragment from storage.ClaimedByFragment and
// storage.NotClaimableStatusFragment (internal/storage/storage.go:56-81) rather
// than hardcoding a literal, so a backend whose refusal drops the fragment
// hands that parser an empty field with no other signal.
//
// Three refusal shapes, three different obligations:
//
//   - an in_progress foreign holder carries " by <assignee>";
//   - an ineligible status carries ": status <status>";
//   - an OPEN issue assigned to someone else DELIBERATELY omits the assignee
//     fragment. That refusal answers holder-steering copy instead, so the
//     assignee is recoverable from the typed field only — and this case pins
//     the omission, because a backend that "fixed" it by appending the fragment
//     would be changing published refusal copy.
//
// The tripwire for this coupling was backend-local while the parser it protects
// is backend-independent, which is why it belongs here.
func RunClaimRefusalMessagesCarryTheirFragments(t *testing.T, ctx context.Context, fixture ClaimerFixture) {
	t.Helper()
	held := fixture.IssuePrefix + "-frag-held"
	closed := fixture.IssuePrefix + "-frag-closed"
	assigned := fixture.IssuePrefix + "-frag-assigned"

	seedClaimerIssue(t, ctx, fixture, claimerIssue(held))
	closedIssue := claimerIssue(closed)
	closedIssue.Status = types.StatusClosed
	seedClaimerIssue(t, ctx, fixture, closedIssue)
	assignedIssue := claimerIssue(assigned)
	assignedIssue.Assignee = "alice"
	seedClaimerIssue(t, ctx, fixture, assignedIssue)

	if _, err := fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "alice", IssueID: held}); err != nil {
		t.Fatalf("first claim of %s: %v", held, err)
	}
	_, err := fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "bob", IssueID: held})
	conflict := claimerConflict(t, err, storage.ErrAlreadyClaimed, held)
	if got := claimerRecoveredTail(err.Error(), storage.ErrAlreadyClaimed, storage.ClaimedByFragment); got != "alice" {
		t.Errorf("assignee recovered from the in_progress conflict = %q, want alice (message %q)", got, err.Error())
	}
	if conflict.Assignee != "alice" {
		t.Errorf("conflict.Assignee = %q, want alice", conflict.Assignee)
	}

	_, err = fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "carol", IssueID: closed})
	conflict = claimerConflict(t, err, storage.ErrNotClaimable, closed)
	if got := claimerRecoveredTail(err.Error(), storage.ErrNotClaimable, storage.NotClaimableStatusFragment); got != string(types.StatusClosed) {
		t.Errorf("status recovered from the not-claimable refusal = %q, want %q (message %q)", got, types.StatusClosed, err.Error())
	}
	if conflict.Status != types.StatusClosed {
		t.Errorf("conflict.Status = %q, want %q", conflict.Status, types.StatusClosed)
	}

	_, err = fixture.Claimer.Claim(ctx, publicops.ClaimRequest{Actor: "bob", IssueID: assigned})
	conflict = claimerConflict(t, err, storage.ErrAlreadyClaimed, assigned)
	if got := claimerRecoveredTail(err.Error(), storage.ErrAlreadyClaimed, storage.ClaimedByFragment); got != "" {
		t.Errorf("the open-but-assigned refusal carried the %q fragment (recovered %q from %q); that copy omits it on purpose",
			storage.ClaimedByFragment, got, err.Error())
	}
	if conflict.Assignee != "alice" {
		t.Errorf("conflict.Assignee = %q, want alice — the holder the prose does not name must still reach the caller typed", conflict.Assignee)
	}
}

// RunClaimerRole runs the whole block against a factory store: the per-block
// entry point RunAll composes, and the one a supported-subset gate adopts when
// its backend answers IssueClaimer(). A backend whose allowlist refuses that
// accessor runs neither this nor the individual runners.
//
// Each case gets its own store, matching how the rest of RunAll works; a
// backend sharing one database across the block wires the exported runners
// directly with a prefix-scoped fixture instead.
func RunClaimerRole(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("WinsThenReclaimsIdempotently", func(t *testing.T) {
		RunClaimWinsThenReclaimsIdempotently(t, ctx(), claimerFixtureFromStore(t, factory(t)))
	})
	t.Run("ForeignHolderRefusalCarriesTheHolder", func(t *testing.T) {
		RunClaimForeignHolderRefusalCarriesTheHolder(t, ctx(), claimerFixtureFromStore(t, factory(t)))
	})
	t.Run("RefusesIneligibleUnknownAndActorlessRequests", func(t *testing.T) {
		RunClaimRefusesIneligibleUnknownAndActorlessRequests(t, ctx(), claimerFixtureFromStore(t, factory(t)))
	})
	t.Run("RefusalMessagesCarryTheirFragments", func(t *testing.T) {
		RunClaimRefusalMessagesCarryTheirFragments(t, ctx(), claimerFixtureFromStore(t, factory(t)))
	})
}

// claimerStorePrefix namespaces the ids this wiring seeds. One value serves
// every case because each gets its own store; a backend sharing one database
// across the block gives its fixture a prefix per case instead.
const claimerStorePrefix = "clr"

func claimerFixtureFromStore(t *testing.T, s storage.DoltStorage) ClaimerFixture {
	t.Helper()
	claimer, err := s.IssueClaimer()
	if err != nil {
		t.Fatalf("IssueClaimer(): %v", err)
	}
	return ClaimerFixture{
		IssuePrefix: claimerStorePrefix,
		Claimer:     claimer,
		CreateIssue: s.CreateIssue,
		CreateWisp: func(ctx context.Context, issue *types.Issue, actor string) error {
			issue.Ephemeral = true
			return s.CreateIssue(ctx, issue, actor)
		},
		GetIssue: s.GetIssue,
		CountClaimEvents: func(ctx context.Context, id string) (int, error) {
			events, err := s.GetEvents(ctx, id, 0)
			if err != nil {
				return 0, err
			}
			n := 0
			for _, e := range events {
				if e.EventType == types.EventClaimed {
					n++
				}
			}
			return n, nil
		},
	}
}

func claimerIssue(id string, labels ...string) *types.Issue {
	return &types.Issue{
		ID:        id,
		Title:     id,
		Status:    types.StatusOpen,
		Priority:  2,
		IssueType: types.TypeTask,
		Labels:    labels,
	}
}

func seedClaimerIssue(t *testing.T, ctx context.Context, fixture ClaimerFixture, issue *types.Issue) {
	t.Helper()
	if err := fixture.CreateIssue(ctx, issue, "seed"); err != nil {
		t.Fatalf("seed issue %s: %v", issue.ID, err)
	}
}

func seedClaimerWisp(t *testing.T, ctx context.Context, fixture ClaimerFixture, issue *types.Issue) {
	t.Helper()
	if err := fixture.CreateWisp(ctx, issue, "seed"); err != nil {
		t.Fatalf("seed wisp %s: %v", issue.ID, err)
	}
}

func claimerRow(t *testing.T, ctx context.Context, fixture ClaimerFixture, id string) *types.Issue {
	t.Helper()
	issue, err := fixture.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("read back %s: %v", id, err)
	}
	if issue == nil {
		t.Fatalf("read back %s: no row", id)
	}
	return issue
}

func assertClaimerRowState(t *testing.T, ctx context.Context, fixture ClaimerFixture, id string, wantStatus types.Status, wantAssignee, when string) {
	t.Helper()
	got := claimerRow(t, ctx, fixture, id)
	if got.Status != wantStatus || got.Assignee != wantAssignee {
		t.Errorf("%s, %s is {status %q assignee %q}, want {status %q assignee %q}",
			when, id, got.Status, got.Assignee, wantStatus, wantAssignee)
	}
}

func assertClaimerSnapshot(t *testing.T, result publicops.ClaimResult, id, wantAssignee string, wantStatus types.Status) {
	t.Helper()
	if result.Issue == nil {
		t.Fatalf("claim of %s answered a nil Issue, want a post-state snapshot", id)
	}
	if result.Issue.ID != id {
		t.Errorf("snapshot id = %q, want %q", result.Issue.ID, id)
	}
	if result.Issue.Assignee != wantAssignee {
		t.Errorf("snapshot assignee = %q, want %q", result.Issue.Assignee, wantAssignee)
	}
	if result.Issue.Status != wantStatus {
		t.Errorf("snapshot status = %q, want %q", result.Issue.Status, wantStatus)
	}
}

func claimerConflict(t *testing.T, err error, sentinel error, wantID string) *publicops.ClaimConflictError {
	t.Helper()
	if err == nil {
		t.Fatalf("claim of %s succeeded, want a refusal wrapping %v", wantID, sentinel)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("refusal for %s = %v, want one wrapping %v", wantID, err, sentinel)
	}
	var conflict *publicops.ClaimConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("refusal for %s is not a *issueops.ClaimConflictError (%v); a caller cannot classify the conflict without parsing prose", wantID, err)
	}
	if conflict.IssueID != wantID {
		t.Errorf("conflict.IssueID = %q, want %q", conflict.IssueID, wantID)
	}
	return conflict
}

func claimerEventCount(t *testing.T, ctx context.Context, fixture ClaimerFixture, id string) int {
	t.Helper()
	n, err := fixture.CountClaimEvents(ctx, id)
	if err != nil {
		t.Fatalf("count claim events for %s: %v", id, err)
	}
	return n
}

func claimerTimeEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// claimerRecoveredTail mirrors beads.ParseClaimConflict's extraction: it
// rebuilds the marker from the storage sentinel plus its exported fragment —
// the single source of truth both ends spell the message with — and returns the
// trailing token, or "" when the marker is absent or the token cannot be
// separated from a wrap. It is local because this package cannot import the
// root beads package without a cycle.
func claimerRecoveredTail(msg string, sentinel error, fragment string) string {
	marker := sentinel.Error() + fragment
	i := strings.LastIndex(msg, marker)
	if i < 0 {
		return ""
	}
	tail := msg[i+len(marker):]
	if strings.ContainsAny(tail, " \t\r\n(") {
		return ""
	}
	return tail
}
