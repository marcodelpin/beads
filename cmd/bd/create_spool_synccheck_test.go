//go:build cgo

// Spooled-create replay fidelity (bda-tfok.4.3, codex round 1 finding F2 on
// sync/main-on-upstream 80ec2c147).
//
// The create role (issueops) owns three things the CLI used to do before the
// spool wrapper: it mints the <parent>.<n> child id, inherits the parent's
// labels and defaults created_by to the request actor. The spool replays a
// queued create through the raw store.CreateIssue, which does none of them.
// These tests pin the seam's answer: the queued copy carries its creator, and
// a create under a parent is never queued (it fails loud instead).
package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads"
	"github.com/steveyegge/beads/internal/spool"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// unreachableCreateOps is a lifecycle whose Create fails the way an
// unreachable Dolt server does. Everything else passes through.
type unreachableCreateOps struct {
	issueops.Lifecycle
}

func (unreachableCreateOps) Create(context.Context, issueops.CreateRequest) (issueops.CreateResult, error) {
	return issueops.CreateResult{}, errors.New("dial tcp 127.0.0.1:3308: connect: connection refused")
}

// withUnreachableCreate makes every write-verb Create fail transiently for the
// rest of the test.
func withUnreachableCreate(t *testing.T) {
	t.Helper()
	saved := newIssueOperations
	t.Cleanup(func() { newIssueOperations = saved })
	newIssueOperations = func(target beads.Storage) (issueops.Lifecycle, error) {
		if decorated, ok := target.(storage.DoltStorage); ok {
			target = storage.UnwrapStore(decorated)
		}
		inner, err := target.IssueLifecycle()
		if err != nil {
			return nil, err
		}
		return unreachableCreateOps{Lifecycle: inner}, nil
	}
}

func createSpoolOf(env *parityEnv) *spool.Spool {
	return spool.NewSpool(filepath.Join(env.beadsDir, "spool"))
}

func createSpoolQueueLines(t *testing.T, sp *spool.Spool) int64 {
	t.Helper()
	n, err := sp.QueueLineCount()
	if err != nil {
		t.Fatalf("QueueLineCount: %v", err)
	}
	return n
}

func issueByTitle(t *testing.T, env *parityEnv, title string) *types.Issue {
	t.Helper()
	issues, err := env.store.inner.SearchIssues(rootCtx, "", types.IssueFilter{})
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	var found *types.Issue
	for _, issue := range issues {
		if issue.Title == title {
			if found != nil {
				t.Fatalf("title %q matches more than one issue (%s, %s)", title, found.ID, issue.ID)
			}
			found = issue
		}
	}
	if found == nil {
		t.Fatalf("no issue titled %q after drain", title)
	}
	return found
}

// TestSyncCheckSpooledCreateKeepsCreator: a create queued on a transient
// failure must, once replayed, record the same creator the live create would
// (the role defaults created_by to the actor; replay does not).
func TestSyncCheckSpooledCreateKeepsCreator(t *testing.T) {
	env := newParityEnv(t)
	withUnreachableCreate(t)
	sp := createSpoolOf(env)

	const title = "Spooled create keeps its creator"
	res := env.run(createCmd, title)
	if res.exitCode != 0 {
		t.Fatalf("create exit = %d, want 0 (queued)\nstdout:\n%s\nstderr:\n%s", res.exitCode, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "Queued for replay") {
		t.Fatalf("create stdout does not report a queued write:\n%s", res.stdout)
	}
	if got := createSpoolQueueLines(t, sp); got != 1 {
		t.Fatalf("queue lines after create = %d, want 1", got)
	}

	setStoreActive(true)
	drained, err := spool.Drain(rootCtx, sp, spoolDispatch(rootCtx))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if drained.Drained != 1 || drained.Dead != 0 {
		t.Fatalf("drain = %+v, want 1 drained and 0 dead", drained)
	}

	created := issueByTitle(t, env, title)
	if created.CreatedBy != "parity-actor" {
		t.Errorf("replayed create created_by = %q, want %q (the actor of the queued create)", created.CreatedBy, "parity-actor")
	}
}

// TestSyncCheckSpooledCreateUnderParentFailsLoud: a create under a parent and
// without an id of its own needs the role to mint <parent>.<n> and inherit the
// parent's labels, which replay cannot do. It must not be queued: the command
// fails and leaves the spool empty. Control in the same test: a plain create
// under the same fault IS queued, so the empty spool is the guard's doing and
// not a broken fault injection.
func TestSyncCheckSpooledCreateUnderParentFailsLoud(t *testing.T) {
	env := newParityEnv(t)
	env.seed("test-epic", "Parent epic", func(i *types.Issue) {
		i.IssueType = types.TypeEpic
		i.Labels = []string{"area:spool"}
	})
	withUnreachableCreate(t)
	sp := createSpoolOf(env)

	env.setFlags(createCmd, map[string]string{"parent": "test-epic"})
	res := env.run(createCmd, "Child created while the server is down")
	if res.exitCode == 0 {
		t.Errorf("create --parent exit = 0, want a failure\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
	}
	if strings.Contains(res.stdout, "Queued for replay") {
		t.Errorf("create --parent reported a queued write:\n%s", res.stdout)
	}
	if got := createSpoolQueueLines(t, sp); got != 0 {
		t.Errorf("queue lines after create --parent = %d, want 0", got)
	}

	// Control: same fault, no parent -> queued.
	resetCommandFlagsToDefaults(createCmd)
	control := env.run(createCmd, "Plain create while the server is down")
	if control.exitCode != 0 || !strings.Contains(control.stdout, "Queued for replay") {
		t.Fatalf("control plain create was not queued: exit=%d\nstdout:\n%s\nstderr:\n%s", control.exitCode, control.stdout, control.stderr)
	}
	before := int64(0)
	if res.exitCode == 0 {
		before = 1 // the parent create was (wrongly) queued above
	}
	if got := createSpoolQueueLines(t, sp); got != before+1 {
		t.Errorf("queue lines after control = %d, want %d", got, before+1)
	}
}
