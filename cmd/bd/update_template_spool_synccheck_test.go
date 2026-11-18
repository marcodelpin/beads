//go:build cgo

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads"
	"github.com/steveyegge/beads/internal/spool"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// transientUpdateOps fails every Update with a transport error the spool
// classifies as transient, as an unreachable server would. Everything else
// passes through.
type transientUpdateOps struct {
	issueops.Lifecycle
}

func (transientUpdateOps) Update(context.Context, issueops.UpdateRequest) (issueops.UpdateResult, error) {
	return issueops.UpdateResult{}, errors.New("dial tcp 127.0.0.1:3308: connect: connection refused")
}

func injectTransientUpdate(t *testing.T) {
	t.Helper()
	prev := newIssueOperations
	newIssueOperations = func(target beads.Storage) (issueops.Lifecycle, error) {
		inner, err := prev(target)
		if err != nil {
			return nil, err
		}
		return transientUpdateOps{Lifecycle: inner}, nil
	}
	t.Cleanup(func() { newIssueOperations = prev })
}

func templateSpoolQueueLines(t *testing.T, env *parityEnv) int64 {
	t.Helper()
	n, err := spool.NewSpool(filepath.Join(env.beadsDir, "spool")).QueueLineCount()
	if err != nil {
		t.Fatalf("QueueLineCount: %v", err)
	}
	return n
}

// drainTemplateSpool replays the queue through the production dispatcher, as
// the next bd command would.
func drainTemplateSpool(t *testing.T, env *parityEnv) spool.DrainResult {
	t.Helper()
	savedActive := isStoreActive()
	setStoreActive(true)
	t.Cleanup(func() { setStoreActive(savedActive) })
	res, err := spool.Drain(rootCtx, spool.NewSpool(filepath.Join(env.beadsDir, "spool")), spoolDispatch(rootCtx))
	if err != nil {
		t.Fatalf("spool.Drain: %v", err)
	}
	return res
}

// TestUpdateTemplateNotSpooled pins bda-tfok.4.2 F3: a bd update on a template
// is refused by the role's template guard (issueops Update), which a transient
// failure never reaches. Queued, the update would replay through
// store.UpdateIssue, which carries no template guard, and edit the read-only
// template. A template update is therefore never spooled: it fails loud.
func TestUpdateTemplateNotSpooled(t *testing.T) {
	env := newParityEnv(t)
	env.seed("test-tpl1", "Template title", func(i *types.Issue) { i.IsTemplate = true })
	injectTransientUpdate(t)

	env.setFlags(updateCmd, map[string]string{"title": "Changed by replay"})
	res := env.run(updateCmd, "test-tpl1")

	if got := templateSpoolQueueLines(t, env); got != 0 {
		t.Errorf("queue lines = %d, want 0: a template update must not be spooled\nstderr:\n%s", got, res.stderr)
	}
	if res.exitCode == 0 {
		t.Errorf("exit = 0, want non-zero: the transient failure must surface\nstderr:\n%s", res.stderr)
	}
	drainTemplateSpool(t, env)
	if got := env.get("test-tpl1").Title; got != "Template title" {
		t.Errorf("template title after drain = %q, want %q (replay edited a read-only template)", got, "Template title")
	}
}

// TestUpdateNonTemplateStillSpooled is the control for the test above: the
// same transient failure on an ordinary issue is queued, and the drain lands
// it, so the template result is the guard and not a broken harness.
func TestUpdateNonTemplateStillSpooled(t *testing.T) {
	env := newParityEnv(t)
	env.seed("test-tpl2", "Plain title", nil)
	injectTransientUpdate(t)

	env.setFlags(updateCmd, map[string]string{"title": "Changed by replay"})
	res := env.run(updateCmd, "test-tpl2")

	if got := templateSpoolQueueLines(t, env); got != 1 {
		t.Fatalf("queue lines = %d, want 1\nexit=%d stderr:\n%s", got, res.exitCode, res.stderr)
	}
	if res.exitCode != 0 {
		t.Errorf("exit = %d, want 0 (queued)\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "queued for replay") {
		t.Errorf("stderr lacks the queued line:\n%s", res.stderr)
	}
	drained := drainTemplateSpool(t, env)
	if got := env.get("test-tpl2").Title; got != "Changed by replay" {
		t.Errorf("title after drain = %q, want %q (drain result %+v)", got, "Changed by replay", drained)
	}
}
