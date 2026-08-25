package main

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
)

// noRemotePushStore is minimalPushStore plus an empty remote listing: the
// server-mode shape bda-wgjr measured, where Push() succeeds as a no-op with
// zero remotes configured.
type noRemotePushStore struct {
	minimalPushStore
}

func (m *noRemotePushStore) ListRemotes(context.Context) ([]storage.RemoteInfo, error) {
	return nil, nil
}

// TestPushWithNoRemoteReportsSkipNotSuccess pins the bda-wgjr contract: a
// push with NO configured destination must say so ("No remote is configured
// - skipping", the established solo-rig guidance) and must never print the
// false "Push complete." nor call Push() at all. Exit stays 0 - the same
// deliberate exit-0 skip the no-remote gate documents - so session-close
// checklists on shared-server rigs keep passing, now with an honest message.
func TestPushWithNoRemoteReportsSkipNotSuccess(t *testing.T) {
	// Cannot be parallel: modifies process-global store and config.
	saveAndRestoreGlobals(t)
	resetCommandContext()

	fake := &noRemotePushStore{}
	store = fake

	// The documented never-adopt opt-out: without it, a cwd with a git origin
	// makes the non-interactive adopt path fail loudly for consent, which is
	// its own (correct) contract - this test is about the branch where
	// adoption is declined and the push proceeds destination-less.
	t.Setenv("BD_NO_REMOTE_ADOPT", "1")

	config.ResetForTesting()
	t.Cleanup(func() { config.ResetForTesting() })
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}

	out := captureStdout(t, func() error {
		return doltPushCmd.RunE(doltPushCmd, nil)
	})

	if fake.pushCalled {
		t.Error("bd dolt push must not call Push() when no remote is configured; Push() was called")
	}
	if strings.Contains(out, "Push complete.") {
		t.Errorf("push with no destination printed the false success line:\n%s", out)
	}
	if !strings.Contains(out, "No remote is configured") {
		t.Errorf("expected the no-remote guidance, got:\n%s", out)
	}
}
