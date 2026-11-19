//go:build cgo

package main

// bda-nchm path (c): the fork seam behind upstream's remember RunE keeps the
// fork route's behaviour (codex nchm-r1 findings 2-5).

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/memoryops"
)

// directMemoryStore is a direct-route store (not proxied, not remote) whose
// memory role is a map.
type directMemoryStore struct {
	storage.DoltStorage
	memories *mapMemories
}

func (s *directMemoryStore) Memories() (memoryops.Memories, error) { return s.memories, nil }

func (*directMemoryStore) CommitConfigUserKVOnly(context.Context, string) error { return nil }

func installDirectMemoryStore(t *testing.T, stored map[string]string) *directMemoryStore {
	t.Helper()
	t.Setenv("BEADS_DIR", t.TempDir())
	saveAndRestoreGlobals(t)
	saveStorageMode(t)
	serverMode = true
	fake := &directMemoryStore{memories: &mapMemories{stored: stored}}
	store = fake
	setStoreActive(true)
	saveForkRememberFlags(t)
	return fake
}

// saveForkRememberFlags restores every remember flag the tests below set.
func saveForkRememberFlags(t *testing.T) {
	t.Helper()
	key, js, cfg := memoryKeyFlag, jsonOutput, memoryConfigInt
	vf, vu, pol, scope := memoryValidForFlag, memoryValidUntilFlag, memoryExpirePolicyFlag, memoryScopeFlag
	t.Cleanup(func() {
		memoryKeyFlag, jsonOutput, memoryConfigInt = key, js, cfg
		memoryValidForFlag, memoryValidUntilFlag, memoryExpirePolicyFlag, memoryScopeFlag = vf, vu, pol, scope
	})
	memoryKeyFlag, jsonOutput = "", false
	memoryConfigInt = func(string) int { return 0 }
}

func runRemember(t *testing.T, insight string) (stdout, stderr string, err error) {
	t.Helper()
	stdout, stderr = captureStdio(t, func() { err = rememberCmd.RunE(rememberCmd, []string{insight}) })
	return stdout, stderr, err
}

// Finding 2: on the direct route a bare-key read follows the content
// fingerprint, as the fork route did: `bd remember cobalt` after
// `bd remember 'Cobalt!' --key paint` recalls paint.
func TestForkRememberBareReadFollowsDedup(t *testing.T) {
	fake := installDirectMemoryStore(t, map[string]string{"paint": "Cobalt!"})
	stdout, stderr, err := runRemember(t, "cobalt")
	if err != nil || !strings.Contains(stdout, "Cobalt!") || !strings.Contains(stderr, `"paint"`) {
		t.Fatalf("bd remember cobalt: err=%v stdout=%q stderr=%q, want paint's memory recalled", err, stdout, stderr)
	}
	if len(fake.memories.remembered) != 0 {
		t.Fatalf("bd remember cobalt wrote %v, want a read only", fake.memories.remembered)
	}
}

// Finding 3: on a role route a bare-key read prints the stored value as
// stored, the way bd recall and upstream do.
func TestForkRememberBareReadIsRawOnRoleRoute(t *testing.T) {
	env := `{"_bd_mem":1,"content":"blue","created_at":"2026-11-19T00:00:00Z"}`
	installRemoteMemoryStore(t, map[string]string{"paint": env})
	stdout, stderr, err := runRemember(t, "paint")
	if err != nil || !strings.Contains(stdout, env) {
		t.Fatalf("bd remember paint on a remote backend: err=%v stdout=%q stderr=%q, want the raw stored value", err, stdout, stderr)
	}
}

// Finding 4: the refusals keep the fork route's precedence.
func TestForkRememberRefusalPrecedence(t *testing.T) {
	t.Run("role route: fork flags before the command guard", func(t *testing.T) {
		installRemoteMemoryStore(t, map[string]string{})
		saveForkRememberFlags(t)
		memoryScopeFlag = "project"
		_, stderr, err := runRemember(t, "list")
		if err == nil || !strings.Contains(stderr, "fork-only remember flags") {
			t.Fatalf("bd remember list --scope: err=%v stderr=%q, want the fork-flag refusal first", err, stderr)
		}
	})
	t.Run("direct: an empty explicit key before a bad duration", func(t *testing.T) {
		installDirectMemoryStore(t, map[string]string{})
		memoryKeyFlag, memoryValidForFlag = " ", "bad"
		_, stderr, err := runRemember(t, "two words")
		if err == nil || strings.Contains(stderr, "invalid --valid-for") {
			t.Fatalf("bd remember --key ' ' --valid-for=bad: err=%v stderr=%q, want the key refusal first", err, stderr)
		}
	})
	t.Run("direct: an underivable key before a bad duration", func(t *testing.T) {
		installDirectMemoryStore(t, map[string]string{})
		memoryValidForFlag = "bad"
		_, stderr, err := runRemember(t, "!!!")
		if err == nil || !strings.Contains(stderr, "could not generate key from content") {
			t.Fatalf("bd remember '!!!' --valid-for=bad: err=%v stderr=%q, want the key refusal first", err, stderr)
		}
	})
}

// Finding 5: a prefix-only row (empty user key) is no dedup match; the write
// lands under the insight's own key, never one derived from the envelope.
func TestForkRememberDedupIgnoresEmptyKeyRow(t *testing.T) {
	fake := installDirectMemoryStore(t, map[string]string{"": "take notes"})
	_, stderr, err := runRemember(t, "take notes")
	if err != nil || len(fake.memories.remembered) != 1 || fake.memories.remembered[0] != "take-notes" {
		t.Fatalf("bd remember 'take notes': err=%v stderr=%q remembered=%q, want one write under take-notes", err, stderr, fake.memories.remembered)
	}
}
