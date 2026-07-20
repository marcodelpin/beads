package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/beads/internal/hooks"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
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
	return domain.CreateIssueResult{Issue: params.Issue}, nil
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
