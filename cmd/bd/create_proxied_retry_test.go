package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/beads/internal/hooks"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// --- fakes for the create whole-attempt retry test ---

type fakeCreateConfigUC struct {
	domain.ConfigUseCase // unimplemented methods panic; the attempt path must not call them
}

func (f *fakeCreateConfigUC) LoadCreateContext(ctx context.Context) (domain.CreateContext, error) {
	return domain.CreateContext{}, nil
}

type fakeCreateIssueUC struct {
	domain.IssueUseCase
	calls        atomic.Int64
	mu           sync.Mutex
	seenPriority []int
	created      *types.Issue
}

// GetWisp and GetIssue serve the role's post-create hydration read.
func (f *fakeCreateIssueUC) GetWisp(ctx context.Context, id string) (*types.Issue, error) {
	return nil, issueops.ErrNotFound
}

func (f *fakeCreateIssueUC) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created, nil
}

func (f *fakeCreateIssueUC) CreateIssue(ctx context.Context, params domain.CreateIssueParams, actor string) (domain.CreateIssueResult, error) {
	n := f.calls.Add(1)
	f.mu.Lock()
	f.seenPriority = append(f.seenPriority, params.Issue.Priority)
	f.mu.Unlock()
	if n == 1 {
		// Simulate a first attempt that MUTATES the issue it was handed and
		// then fails with a retryable serialization error. This is the hazard
		// under test: a *types.Issue shared across RunTxResult attempts leaks
		// the mutation into the retry (bda-4t4: a create raced by concurrent
		// writers landed with Priority zeroed while every other field held).
		params.Issue.Priority = 0
		params.Issue.ID = "bda-mutated-by-attempt-1"
		return domain.CreateIssueResult{}, serializationFailure()
	}
	f.mu.Lock()
	f.created = params.Issue
	f.mu.Unlock()
	return domain.CreateIssueResult{Issue: params.Issue}, nil
}

// Minimal hydration fakes: the role re-reads the created issue and hydrates
// labels, dependency records and comments inside the same unit of work.
type fakeCreateLabelUC struct{ domain.LabelUseCase }

func (f *fakeCreateLabelUC) GetLabels(ctx context.Context, issueID string) ([]string, error) {
	return nil, nil
}

func (f *fakeCreateLabelUC) GetWispLabels(ctx context.Context, wispID string) ([]string, error) {
	return nil, nil
}

type fakeCreateDepUC struct{ domain.DependencyUseCase }

func (f *fakeCreateDepUC) GetIssueDependencyRecords(ctx context.Context, issueIDs []string) (map[string][]*types.Dependency, error) {
	return map[string][]*types.Dependency{}, nil
}

func (f *fakeCreateDepUC) GetWispDependencyRecords(ctx context.Context, wispIDs []string) (map[string][]*types.Dependency, error) {
	return map[string][]*types.Dependency{}, nil
}

type fakeCreateCommentUC struct{ domain.CommentUseCase }

func (f *fakeCreateCommentUC) GetCommentsForIssue(ctx context.Context, issueID string) ([]*types.Comment, error) {
	return nil, nil
}

func (f *fakeCreateCommentUC) GetCommentsForWisp(ctx context.Context, wispID string) ([]*types.Comment, error) {
	return nil, nil
}

type fakeCreateUOWProvider struct {
	issueUC *fakeCreateIssueUC
}

func (p *fakeCreateUOWProvider) NewUOW(ctx context.Context) (uow.UnitOfWork, error) {
	return &fakeUOW{
		issueUC:  p.issueUC,
		configUC: &fakeCreateConfigUC{},
		commit:   func() error { return nil },
	}, nil
}

func (p *fakeCreateUOWProvider) Close(ctx context.Context) error { return nil }

// IssueLifecycle hands back the REAL role over this fake provider - the same
// accessor doltSQLProvider offers - so the test exercises the role's own
// RunTxResult clone-per-attempt (the bda-4t4 guarantee) against a first
// attempt that mutates its request and fails with a retryable error.
func (p *fakeCreateUOWProvider) IssueLifecycle() (issueops.Lifecycle, error) {
	return uow.NewIssueOperations(p)
}

func withFakeProxiedCreateEnv(t *testing.T, p uow.UnitOfWorkProvider) {
	t.Helper()
	oldProvider := uowProvider
	oldHookRunner := hookRunner
	uowProvider = p
	hookRunner = hooks.NewRunner(t.TempDir()) // no hooks installed: RunSync no-ops
	t.Cleanup(func() {
		uowProvider = oldProvider
		hookRunner = oldHookRunner
	})
}

// TestRunCreateProxiedSingle_RetryDoesNotObserveMutations pins the per-attempt
// isolation contract of the proxied single create: uow.RunTxResult redoes the
// WHOLE closure on a serialization failure, so each attempt must build its
// issue fresh instead of reusing a struct a previous attempt may have mutated.
// Regression guard for bda-4t4.
func TestRunCreateProxiedSingle_RetryDoesNotObserveMutations(t *testing.T) {
	issueUC := &fakeCreateIssueUC{}
	withFakeProxiedCreateEnv(t, &fakeCreateUOWProvider{issueUC: issueUC})

	in := createInput{
		title:       "retry isolation probe",
		description: "attempt 2 must not observe attempt 1 mutations",
		issueType:   "task",
		priority:    3,
		silent:      true,
		// The role validates the request up front: Actor is required.
		createdBy: "retry-test-actor",
	}
	if err := runCreateProxiedSingle(nil, context.Background(), in); err != nil {
		t.Fatalf("runCreateProxiedSingle: %v", err)
	}

	if got := issueUC.calls.Load(); got != 2 {
		t.Fatalf("CreateIssue attempts = %d, want 2 (one failed + one retry)", got)
	}
	issueUC.mu.Lock()
	seen := append([]int(nil), issueUC.seenPriority...)
	issueUC.mu.Unlock()
	if seen[0] != 3 {
		t.Fatalf("attempt 1 saw priority %d, want 3 (test wiring broken)", seen[0])
	}
	if seen[1] != 3 {
		t.Fatalf("retry saw priority %d, want 3 - the retry reused the struct mutated by the failed attempt (bda-4t4 zero-value leak)", seen[1])
	}
}

// fakeUOW is the generic unit-of-work fake the whole-attempt retry tests
// share. It lived beside the update-route fakes until upstream's issueops
// refactor retired those; the create retry test is its remaining consumer.
type fakeUOW struct {
	issueUC  domain.IssueUseCase
	configUC domain.ConfigUseCase // nil for update tests; create tests need LoadCreateContext
	commit   func() error
}

func (f *fakeUOW) Close(ctx context.Context)                                 {}
func (f *fakeUOW) Commit(ctx context.Context, message string) error          { return f.commit() }
func (f *fakeUOW) SwitchDatabase(ctx context.Context, database string) error { return nil }
func (f *fakeUOW) ConfigUseCase() domain.ConfigUseCase                       { return f.configUC }
func (f *fakeUOW) DoltRemoteUseCase() domain.DoltRemoteUseCase               { return nil }
func (f *fakeUOW) EventsJournalUseCase() domain.EventsJournalUseCase         { return nil }
func (f *fakeUOW) IssueUseCase() domain.IssueUseCase                         { return f.issueUC }
func (f *fakeUOW) DependencyUseCase() domain.DependencyUseCase               { return &fakeCreateDepUC{} }
func (f *fakeUOW) LabelUseCase() domain.LabelUseCase                         { return &fakeCreateLabelUC{} }
func (f *fakeUOW) CommentUseCase() domain.CommentUseCase                     { return &fakeCreateCommentUC{} }
func (f *fakeUOW) RawSQLUseCase() domain.RawSQLUseCase                       { return nil }
