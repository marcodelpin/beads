package conformance

import (
	"context"
	"errors"
	"testing"

	publicops "github.com/steveyegge/beads/issueops"
)

// This file holds the contract every implementation of publicops.Bootstrapper
// and publicops.InitVerifier must satisfy. Each case asserts what
// issueops/bootstrapper.go and issueops/initverifier.go PROMISE, cited by line,
// rather than what any one backend happens to do today; a backend that
// disagrees is parked at its own wiring site with skipKnownDivergence so the
// case still runs on the ones that agree.
//
// THE TWO ROLES SHARE ONE CONTRACT FILE because they share one plane and every
// promise on either of them is a statement about the other. Bootstrap's promise
// is "a later VerifyIdentity answers with this result"; the verifier's promise
// is that absence and unreadability are different answers, which is only
// checkable against a substrate a bootstrap did or did not reach. Splitting
// them would mean each file seeding the state the other one writes.
//
// WHAT THIS CONTRACT IS FOR, given that the decision is elsewhere. All three
// backends validate through workapi.ValidateBootstrapRequest and decide the
// refusal through workapi.RefuseIdentifiedSubstrate, and those two functions'
// whole tables — each required field, the trailing-hyphen normalization, all
// three identified shapes — are pinned without a database in
// internal/workapi/bootstrap_test.go. Repeating them here would be testing one
// pure function three times over three sql-servers. What only a real backend
// can show is the SUBSTRATE half:
//
//   - that the identity is there for the NEXT caller, which on the
//     unit-of-work backend means a different transaction;
//   - that a REFUSAL WROTE NOTHING — a statement about the keys on disk rather
//     than about the error the validator already returned, and the one that
//     matters most, because the whole point of Q8's ruling is that running init
//     twice must not relabel a workspace other rigs are already minting ids
//     against;
//   - that the verifier reports a FAILED READ as an error rather than as an
//     empty identity, which is the difference between "this database is
//     unprovisioned, bootstrap it" and "this connection dropped";
//   - that a bootstrap costs AT MOST ONE version-control entry rather than one
//     per key.
//
// THE IDENTITY IS GLOBAL TO A WORKSPACE and cannot be namespaced the way the
// issue contracts namespace their seeded ids: two keys are the point, and a
// case that wrote them under a per-test name would assert nothing about the
// pair the roles actually read. Every case therefore SEEDS the identity
// explicitly through the fixture's out-of-band hook before it asserts, so the
// cases are order-independent over one shared plane rather than a sequence that
// has to run in order.
//
// There are three wirings and only TWO independent bodies between them: dolt
// and embeddeddolt both run internal/storage/issueops.BootstrapInTx and
// VerifyIdentityInTx and differ only in how they reach a transaction, so they
// are one vote plus an engine check; the unit-of-work provider is the second,
// and it reads, refuses and writes through the domain config use case inside
// one transaction it labels itself.

// BootstrapperFixture supplies adapter-specific storage access for the identity
// assertions.
type BootstrapperFixture struct {
	// Bootstrapper and InitVerifier are the two surfaces under test, taken
	// from the backend's own capability accessors.
	Bootstrapper publicops.Bootstrapper
	InitVerifier publicops.InitVerifier
	// SeedIdentity writes the two identity markers OUT OF BAND, past both
	// roles, and is how every case establishes the state it asserts about. An
	// empty string means "this substrate carries no such marker", which the
	// roles read identically to an absent row (initverifier.go:13-19).
	//
	// It is NOT a roleFixtureKit hook — the kit is frozen, and it can seed a
	// prefix but has no way to UNSEED one, which is the state a bootstrap needs
	// to be reachable at all on a database its own test harness already
	// initialized. Each wiring supplies its own short closure over the config
	// and metadata seams that backend already publishes, the way the version
	// contract supplies RecordMarkers.
	SeedIdentity func(ctx context.Context, prefix, projectID string) error
	// CountHistory reports how many history entries the fixture's branch has.
	// A nil hook means "this backend cannot observe history", and the case that
	// needs it SKIPS with that reason rather than passing quietly.
	CountHistory func(context.Context) (int, error)
}

// RunBootstrapperIdentifiesAFreshSubstrate pins bootstrapper.go's core promise —
// "A LATER VerifyIdentity IS THE PROMISE" — on the state every workspace starts
// in: nothing recorded, and a first bootstrap that both markers and the next
// caller can see.
func RunBootstrapperIdentifiesAFreshSubstrate(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	seedWorkspaceIdentity(t, ctx, fixture, "", "")

	before := verifyWorkspaceIdentity(t, ctx, fixture)
	if before.Prefix != "" || before.ProjectID != "" {
		t.Fatalf("VerifyIdentity() = %+v on an unbootstrapped substrate, want both empty", before)
	}

	result := bootstrap(t, ctx, fixture, publicops.BootstrapRequest{
		Prefix:    "acmefresh",
		ProjectID: "proj-fresh",
	})
	if result.Prefix != "acmefresh" || result.ProjectID != "proj-fresh" {
		t.Fatalf("Bootstrap() = %+v, want the identity it was given", result)
	}
	assertWorkspaceIdentity(t, ctx, fixture, "acmefresh", "proj-fresh")
}

// RunBootstrapperStoresThePrefixWithoutItsTrailingHyphen pins
// bootstrapper.go:24-30: the value a later VerifyIdentity answers with is the
// normalized one, and BootstrapResult.Prefix says so rather than echoing the
// request.
//
// It is here as well as in the pure test because the normalization has to
// survive the WRITE. Two of the three backends pass this key through a settings
// plane that normalizes it again and one does not, which is exactly the way a
// caller ends up with a prefix that depends on which route it initialized from.
func RunBootstrapperStoresThePrefixWithoutItsTrailingHyphen(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	seedWorkspaceIdentity(t, ctx, fixture, "", "")

	result := bootstrap(t, ctx, fixture, publicops.BootstrapRequest{
		Prefix:    "acmetrim-",
		ProjectID: "proj-trim",
	})
	if result.Prefix != "acmetrim" {
		t.Fatalf("Bootstrap() result prefix = %q, want the stored form %q", result.Prefix, "acmetrim")
	}
	assertWorkspaceIdentity(t, ctx, fixture, "acmetrim", "proj-trim")
}

// RunBootstrapperRefusesAnIdentifiedSubstrate is Q8's ruling, asserted from the
// side that matters: not that the error came back, which the pure test already
// pins, but that the identity standing on the substrate is EXACTLY the one that
// was there before.
//
// bootstrapper.go's "THE REFUSAL WRITES NOTHING" is what makes running `bd init`
// twice safe on a database several rigs share. A body that refused after writing
// the prefix would pass an error-only assertion and still have renamed every id
// the other rigs are about to mint.
func RunBootstrapperRefusesAnIdentifiedSubstrate(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	seedWorkspaceIdentity(t, ctx, fixture, "acmeheld", "proj-held")

	_, err := fixture.Bootstrapper.Bootstrap(ctx, publicops.BootstrapRequest{
		Prefix:    "acmeintruder",
		ProjectID: "proj-intruder",
	})
	if !errors.Is(err, publicops.ErrAlreadyIdentified) {
		t.Fatalf("Bootstrap() over an identified substrate error = %v, want ErrAlreadyIdentified", err)
	}
	var refusal *publicops.AlreadyIdentifiedError
	if !errors.As(err, &refusal) {
		t.Fatalf("Bootstrap() error = %v, want an *AlreadyIdentifiedError naming what it found", err)
	}
	if refusal.Prefix != "acmeheld" || refusal.ProjectID != "proj-held" {
		t.Fatalf("refusal = %+v, want it to name the identity already on the substrate", refusal)
	}
	assertWorkspaceIdentity(t, ctx, fixture, "acmeheld", "proj-held")
}

// RunBootstrapperRefusesASubstrateCarryingOnlyAPrefix pins the "EITHER marker"
// half of the refusal.
//
// This is the shape that is NOT a re-init: a database several rigs share, or one
// a provisioning tool stamped with a prefix, carries the prefix without a
// project id. A bootstrap that keyed its refusal on the project id alone would
// overwrite it and rename every id those rigs are about to mint.
func RunBootstrapperRefusesASubstrateCarryingOnlyAPrefix(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	seedWorkspaceIdentity(t, ctx, fixture, "acmeprefixonly", "")

	_, err := fixture.Bootstrapper.Bootstrap(ctx, publicops.BootstrapRequest{
		Prefix:    "acmeintruder",
		ProjectID: "proj-intruder",
	})
	if !errors.Is(err, publicops.ErrAlreadyIdentified) {
		t.Fatalf("Bootstrap() over a prefix-only substrate error = %v, want ErrAlreadyIdentified", err)
	}
	assertWorkspaceIdentity(t, ctx, fixture, "acmeprefixonly", "")
}

// RunBootstrapperRefusesASubstrateCarryingOnlyAProjectID pins the other half.
// It is the state a bootstrap that failed partway leaves behind on a backend
// with no transaction to roll back, and bootstrapper.go's "ATOMICITY IS NOT
// PROMISED ACROSS THE WHOLE WRITE" says it is refused rather than finished —
// the safe direction, because the alternative is a retry that completes an
// identity it did not choose.
func RunBootstrapperRefusesASubstrateCarryingOnlyAProjectID(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	seedWorkspaceIdentity(t, ctx, fixture, "", "proj-idonly")

	_, err := fixture.Bootstrapper.Bootstrap(ctx, publicops.BootstrapRequest{
		Prefix:    "acmeintruder",
		ProjectID: "proj-intruder",
	})
	if !errors.Is(err, publicops.ErrAlreadyIdentified) {
		t.Fatalf("Bootstrap() over a project-id-only substrate error = %v, want ErrAlreadyIdentified", err)
	}
	assertWorkspaceIdentity(t, ctx, fixture, "", "proj-idonly")
}

// RunBootstrapperRefusesAnInvalidRequestWithoutWriting pins bootstrapper.go's
// ErrValidation clauses against the substrate rather than against the error.
//
// Both facts matter and the second is why this case reads the identity back: a
// body that validated AFTER its first write would leave a prefix on a substrate
// the caller was told it had failed to bootstrap, and the next attempt would
// then hit the refusal above.
func RunBootstrapperRefusesAnInvalidRequestWithoutWriting(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	seedWorkspaceIdentity(t, ctx, fixture, "", "")

	_, err := fixture.Bootstrapper.Bootstrap(ctx, publicops.BootstrapRequest{
		Prefix: "acmeinvalid",
	})
	if !errors.Is(err, publicops.ErrValidation) {
		t.Fatalf("Bootstrap() with no project id error = %v, want ErrValidation", err)
	}
	assertWorkspaceIdentity(t, ctx, fixture, "", "")
}

// RunBootstrapperLeavesTheSubstrateUntouchedWhenItCannotComplete pins the case
// that separates a failure from a refusal.
//
// A canceled context is the one failure every backend can be made to have. It is
// not the only one they will meet in the field, but a body that respects
// cancellation on the way in is a body that has not written before it looked —
// and the workspace it leaves behind is one the next attempt can still
// bootstrap, rather than one the refusal above will reject forever.
func RunBootstrapperLeavesTheSubstrateUntouchedWhenItCannotComplete(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	seedWorkspaceIdentity(t, ctx, fixture, "", "")

	canceled, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := fixture.Bootstrapper.Bootstrap(canceled, publicops.BootstrapRequest{
		Prefix:    "acmecanceled",
		ProjectID: "proj-canceled",
	}); err == nil {
		t.Fatal("Bootstrap() on a canceled context = nil error, want the failure reported rather than a silent success")
	}
	assertWorkspaceIdentity(t, ctx, fixture, "", "")
}

// RunBootstrapperRecordsAtMostOneHistoryEntry pins bootstrapper.go's "AT MOST
// ONE VERSION-CONTROL ENTRY".
//
// The promise is deliberately an upper bound rather than a number: the identity
// lands on planes that travel, so a backend that commits at all is right to
// record the bootstrap, and one whose front door commits the whole of `bd init`
// afterwards is right to record none. What it forbids is the shape a caller
// writing these six values through six ordinary setters gets, which is six
// entries.
func RunBootstrapperRecordsAtMostOneHistoryEntry(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	if fixture.CountHistory == nil {
		t.Skip("fixture cannot observe history on this backend")
	}
	seedWorkspaceIdentity(t, ctx, fixture, "", "")

	before, err := fixture.CountHistory(ctx)
	if err != nil {
		t.Fatalf("CountHistory() before: %v", err)
	}
	bootstrap(t, ctx, fixture, publicops.BootstrapRequest{
		Prefix:    "acmehist",
		ProjectID: "proj-hist",
	})
	after, err := fixture.CountHistory(ctx)
	if err != nil {
		t.Fatalf("CountHistory() after: %v", err)
	}
	if after-before > 1 {
		t.Fatalf("history entries %d -> %d, want a bootstrap to record at most one", before, after)
	}
}

// RunInitVerifierAnswersEmptyForAnUnidentifiedSubstrate pins
// initverifier.go:13-19: "" is a NORMAL ANSWER, not an error and not a missing
// row to classify.
//
// It is the answer the one caller that matters acts on — `bd init` deciding
// whether there is anything to adopt — and turning it into ErrNotFound would
// make the ordinary case look like a failure.
func RunInitVerifierAnswersEmptyForAnUnidentifiedSubstrate(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	seedWorkspaceIdentity(t, ctx, fixture, "", "")

	result, err := fixture.InitVerifier.VerifyIdentity(ctx, publicops.VerifyIdentityRequest{})
	if err != nil {
		t.Fatalf("VerifyIdentity() on an unidentified substrate error = %v, want the empty answer with a nil error", err)
	}
	if result.Prefix != "" || result.ProjectID != "" {
		t.Fatalf("VerifyIdentity() = %+v, want both markers empty", result)
	}
}

// RunInitVerifierReportsAPartialIdentityAsItStands pins the verifier's job on
// the state the refusal cases care about: it REPORTS what is there, marker by
// marker, rather than collapsing a half-identified substrate into identified or
// unidentified.
//
// That is what lets its caller tell a database a provisioning tool stamped with
// a prefix apart from a bootstrap that failed partway — two states with the same
// answer to "is this identified?" and different answers to "what should happen
// next?".
func RunInitVerifierReportsAPartialIdentityAsItStands(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	seedWorkspaceIdentity(t, ctx, fixture, "acmepartial", "")

	result := verifyWorkspaceIdentity(t, ctx, fixture)
	if result.Prefix != "acmepartial" || result.ProjectID != "" {
		t.Fatalf("VerifyIdentity() = %+v, want the prefix reported and the project id empty", result)
	}
}

// RunInitVerifierReportsAFailedReadAsAnError is the promise everything else on
// that role is for (initverifier.go:47-56): an ABSENT identity and an
// UNREADABLE one are different answers.
//
// The two states are one line apart at every call site — an unprovisioned
// database gets bootstrapped, a database that merely could not be reached must
// not be — so a body that reported the read failure as two empty strings would
// hand `bd init` a second identity for a workspace that already had one. A
// canceled context is the failure every backend can be made to have.
func RunInitVerifierReportsAFailedReadAsAnError(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	seedWorkspaceIdentity(t, ctx, fixture, "acmeunread", "proj-unread")

	canceled, cancel := context.WithCancel(ctx)
	cancel()

	result, err := fixture.InitVerifier.VerifyIdentity(canceled, publicops.VerifyIdentityRequest{})
	if err == nil {
		t.Fatalf("VerifyIdentity() on a canceled context = %+v with a nil error, want the failure reported rather than an empty identity", result)
	}
}

// RunInitVerifierWritesNothing pins initverifier.go's "IT WRITES NOTHING",
// including the version-control entry: a verifier that repaired or recorded what
// it found would be the bootstrap these two roles exist to keep separate, and a
// read that committed would put an entry in the log of every workspace `bd init`
// merely LOOKED at.
func RunInitVerifierWritesNothing(t *testing.T, ctx context.Context, fixture BootstrapperFixture) {
	t.Helper()
	if fixture.CountHistory == nil {
		t.Skip("fixture cannot observe history on this backend")
	}
	seedWorkspaceIdentity(t, ctx, fixture, "acmereadonly", "proj-readonly")

	before, err := fixture.CountHistory(ctx)
	if err != nil {
		t.Fatalf("CountHistory() before: %v", err)
	}
	verifyWorkspaceIdentity(t, ctx, fixture)
	after, err := fixture.CountHistory(ctx)
	if err != nil {
		t.Fatalf("CountHistory() after: %v", err)
	}
	if after != before {
		t.Fatalf("history entries %d -> %d, want a read to record none", before, after)
	}
	assertWorkspaceIdentity(t, ctx, fixture, "acmereadonly", "proj-readonly")
}

// seedWorkspaceIdentity puts the identity in a known state past both roles.
func seedWorkspaceIdentity(t *testing.T, ctx context.Context, fixture BootstrapperFixture, prefix, projectID string) {
	t.Helper()
	if fixture.SeedIdentity == nil {
		t.Fatal("fixture.SeedIdentity is nil: every case seeds the identity it asserts about")
	}
	if err := fixture.SeedIdentity(ctx, prefix, projectID); err != nil {
		t.Fatalf("seed identity (%q, %q): %v", prefix, projectID, err)
	}
}

// bootstrap runs a bootstrap that is expected to succeed.
func bootstrap(t *testing.T, ctx context.Context, fixture BootstrapperFixture, req publicops.BootstrapRequest) publicops.BootstrapResult {
	t.Helper()
	result, err := fixture.Bootstrapper.Bootstrap(ctx, req)
	if err != nil {
		t.Fatalf("Bootstrap(%+v): %v", req, err)
	}
	return result
}

func verifyWorkspaceIdentity(t *testing.T, ctx context.Context, fixture BootstrapperFixture) publicops.VerifyIdentityResult {
	t.Helper()
	result, err := fixture.InitVerifier.VerifyIdentity(ctx, publicops.VerifyIdentityRequest{})
	if err != nil {
		t.Fatalf("VerifyIdentity(): %v", err)
	}
	return result
}

// assertWorkspaceIdentity reads the pair back THROUGH InitVerifier, which is the
// promise being checked: bootstrapper.go says a later VerifyIdentity is what
// "bootstrapped" means, and on the unit-of-work backend that read is a new
// transaction — the only place a write that never committed shows up.
func assertWorkspaceIdentity(t *testing.T, ctx context.Context, fixture BootstrapperFixture, prefix, projectID string) {
	t.Helper()
	got := verifyWorkspaceIdentity(t, ctx, fixture)
	if got.Prefix != prefix || got.ProjectID != projectID {
		t.Fatalf("VerifyIdentity() = %+v, want {Prefix:%q ProjectID:%q}", got, prefix, projectID)
	}
}
