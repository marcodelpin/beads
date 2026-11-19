//go:build cgo

package main

// bda-tfok.4.4 (codex round 1 F5): the memory CLI on a remote (HTTP) backend.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/memoryops"
)

// mapMemories is a memory role backed by one map of key -> stored value.
type mapMemories struct {
	memoryops.Memories
	stored     map[string]string
	remembered []string
}

func (m *mapMemories) Remember(_ context.Context, req memoryops.RememberRequest) (memoryops.RememberResult, error) {
	_, replaced := m.stored[req.Key]
	m.stored[req.Key] = req.Content
	m.remembered = append(m.remembered, req.Key)
	return memoryops.RememberResult{Key: req.Key, Value: req.Content, Replaced: replaced}, nil
}

func (m *mapMemories) Recall(_ context.Context, req memoryops.RecallRequest) (memoryops.RecallResult, error) {
	value, found := m.stored[req.Key]
	return memoryops.RecallResult{Key: req.Key, Value: value, Found: found}, nil
}

func (m *mapMemories) List(_ context.Context, req memoryops.ListRequest) (memoryops.ListResult, error) {
	out := make(map[string]string, len(m.stored))
	for k, v := range m.stored {
		if req.Search == "" || strings.Contains(strings.ToLower(k+" "+v), strings.ToLower(req.Search)) {
			out[k] = v
		}
	}
	return memoryops.ListResult{Memories: out}, nil
}

func (m *mapMemories) Forget(_ context.Context, req memoryops.ForgetRequest) (memoryops.ForgetResult, error) {
	value, found := m.stored[req.Key]
	delete(m.stored, req.Key)
	return memoryops.ForgetResult{Key: req.Key, Value: value, Found: found}, nil
}

// remoteMemoryStore models the HTTP client store as the memory CLI sees it:
// a remote backend whose memories are reachable ONLY through its Memories
// accessor. Its settings document carries no kv.memory.* rows and it refuses
// raw config writes, as internal/httpclient does.
type remoteMemoryStore struct {
	storage.DoltStorage
	memories *mapMemories
}

func (*remoteMemoryStore) IsRemoteBackendStore() bool { return true }

func (s *remoteMemoryStore) Memories() (memoryops.Memories, error) { return s.memories, nil }

// CommitConfigUserKVOnly is the HTTP store's: a no-op (internal/httpclient
// store.go). bd remember now runs upstream's RunE on this route too, and that
// RunE commits the memory write (a no-op here, as on the real backend).
func (*remoteMemoryStore) CommitConfigUserKVOnly(context.Context, string) error { return nil }

func (*remoteMemoryStore) GetAllConfig(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (*remoteMemoryStore) GetConfig(context.Context, string) (string, error) { return "", nil }

var errRemoteConfigUnsupported = errors.New("SetConfig: unsupported by the http backend")

func (*remoteMemoryStore) SetConfig(context.Context, string, string) error {
	return errRemoteConfigUnsupported
}

func (*remoteMemoryStore) DeleteConfig(context.Context, string) error {
	return errRemoteConfigUnsupported
}

func installRemoteMemoryStore(t *testing.T, stored map[string]string) *remoteMemoryStore {
	t.Helper()
	t.Setenv("BEADS_DIR", t.TempDir())
	saveAndRestoreGlobals(t)
	saveStorageMode(t)
	serverMode = true
	fake := &remoteMemoryStore{memories: &mapMemories{stored: stored}}
	store = fake
	setStoreActive(true)
	savedKey, savedJSON, savedConfigInt := memoryKeyFlag, jsonOutput, memoryConfigInt
	t.Cleanup(func() { memoryKeyFlag, jsonOutput, memoryConfigInt = savedKey, savedJSON, savedConfigInt })
	memoryKeyFlag, jsonOutput = "", false
	memoryConfigInt = func(string) int { return 0 }
	return fake
}

// F5: every memory command reaches the remote backend's memories through its
// role accessor. At 80ec2c147 they fell through to the direct config-table
// route, where an HTTP store lists nothing and refuses SetConfig.
func TestMemoryCLIOnRemoteBackendUsesTheMemoryRole(t *testing.T) {
	fake := installRemoteMemoryStore(t, map[string]string{"k": "remote value"})

	var runErr error
	stdout, stderr := captureStdio(t, func() { runErr = recallCmd.RunE(recallCmd, []string{"k"}) })
	if runErr != nil || !strings.Contains(stdout, "remote value") {
		t.Errorf("bd recall k on a remote backend: err=%v stdout=%q stderr=%q, want the stored value", runErr, stdout, stderr)
	}

	stdout, stderr = captureStdio(t, func() { runErr = memoriesCmd.RunE(memoriesCmd, nil) })
	if runErr != nil || !strings.Contains(stdout, "remote value") {
		t.Errorf("bd memories on a remote backend: err=%v stdout=%q stderr=%q, want the stored memory listed", runErr, stdout, stderr)
	}

	memoryKeyFlag = "k2"
	stdout, stderr = captureStdio(t, func() { runErr = rememberCmd.RunE(rememberCmd, []string{"a new remote memory"}) })
	memoryKeyFlag = ""
	if runErr != nil || fake.memories.stored["k2"] != "a new remote memory" {
		t.Errorf("bd remember --key k2 on a remote backend: err=%v stdout=%q stderr=%q stored=%v, want the write through the role", runErr, stdout, stderr, fake.memories.stored)
	}

	stdout, stderr = captureStdio(t, func() { runErr = forgetCmd.RunE(forgetCmd, []string{"k"}) })
	if _, still := fake.memories.stored["k"]; runErr != nil || still {
		t.Errorf("bd forget k on a remote backend: err=%v stdout=%q stderr=%q stored=%v, want k deleted through the role", runErr, stdout, stderr, fake.memories.stored)
	}
}

// F5 control: fork-only flags have no role transport, so on a remote backend
// they are refused loudly, exactly as on the proxied route.
func TestMemoryCLIOnRemoteBackendRefusesForkOnlyFlags(t *testing.T) {
	fake := installRemoteMemoryStore(t, map[string]string{})
	savedScope := memoryScopeFlag
	t.Cleanup(func() { memoryScopeFlag = savedScope })
	memoryScopeFlag = "project"
	memoryKeyFlag = "k3"

	var runErr error
	_, stderr := captureStdio(t, func() { runErr = rememberCmd.RunE(rememberCmd, []string{"scoped memory"}) })
	if runErr == nil || len(fake.memories.remembered) != 0 {
		t.Errorf("bd remember --scope on a remote backend: err=%v remembered=%v stderr=%q, want a loud refusal and no write", runErr, fake.memories.remembered, stderr)
	}
}
