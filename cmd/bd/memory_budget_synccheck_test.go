//go:build cgo

package main

// bda-tfok.4.4 (codex round 1 F7): the corpus budget on the memory role route
// measures decoded memory content, as the direct route does. mapMemories is
// in memory_remote_synccheck_test.go.

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/memoryops"
)

// memoriesProvider is a proxied-server unit-of-work provider that offers only
// the memory surface.
type memoriesProvider struct {
	uow.UnitOfWorkProvider
	memories *mapMemories
}

func (p *memoriesProvider) Memories() (memoryops.Memories, error) { return p.memories, nil }

// F7: the role route measures the budget on decoded memory CONTENT, the same
// measure as the direct route. One stored envelope (provenance, tags, scope)
// whose raw JSON alone exceeds a budget of 100 must not refuse a new 10-byte
// memory whose projected content corpus is far below it.
func TestProxiedRememberBudgetCountsDecodedContent(t *testing.T) {
	t.Setenv("BEADS_DIR", t.TempDir())
	saveAndRestoreGlobals(t)
	saveStorageMode(t)
	proxiedServerMode = true
	savedProvider := uowProvider
	t.Cleanup(func() { uowProvider = savedProvider })

	envelope, err := buildMemoryEnvelope("short", time.Now(), 0, time.Time{}, "",
		memoryProvenance{SessionID: "11111111-2222-3333-4444-555555555555", Path: "/srv/some/long/workspace/path"},
		[]string{"machine:mdp-home", "project:beads"}, "project")
	if err != nil {
		t.Fatalf("buildMemoryEnvelope: %v", err)
	}
	if len(envelope) <= 100 {
		t.Fatalf("setup: envelope is %d bytes, want > 100 so the raw value alone is over budget", len(envelope))
	}
	mem := &mapMemories{stored: map[string]string{"old": envelope}}
	uowProvider = &memoriesProvider{memories: mem}

	savedKey, savedJSON, savedConfigInt := memoryKeyFlag, jsonOutput, memoryConfigInt
	t.Cleanup(func() { memoryKeyFlag, jsonOutput, memoryConfigInt = savedKey, savedJSON, savedConfigInt })
	memoryKeyFlag, jsonOutput = "nk", false
	memoryConfigInt = func(key string) int {
		if key == "memories.budget-chars" {
			return 100
		}
		return 0
	}

	var runErr error
	stdout, stderr := captureStdio(t, func() { runErr = rememberCmd.RunE(rememberCmd, []string{"0123456789"}) })
	if runErr != nil || mem.stored["nk"] != "0123456789" {
		t.Errorf("bd remember --key nk (10 bytes, budget 100, corpus content 8 bytes): err=%v stdout=%q stderr=%q, want the write accepted", runErr, stdout, stderr)
	}
	if strings.Contains(stderr, "budget") {
		t.Errorf("projected content corpus is 20 of 100 bytes, want no budget line, got stderr=%q", stderr)
	}
}

// F7: the shared corpus helper is the one measure both routes apply.
func TestMemoryContentCorpusUnwrapsEnvelopes(t *testing.T) {
	envelope, err := buildMemoryEnvelope("short", time.Now(), 0, time.Time{}, "", memoryProvenance{}, []string{"a"}, "s")
	if err != nil {
		t.Fatalf("buildMemoryEnvelope: %v", err)
	}
	direct := directMemoryCorpus(map[string]string{kvPrefix + memoryPrefix + "old": envelope, "issue_prefix": "x"})
	if len(direct) != 1 || direct["old"] != "short" {
		t.Errorf("directMemoryCorpus = %v, want only old -> short", direct)
	}
}

// F7: the role route's raw values reduce to the same content measure.
func TestMemoryContentCorpusIsTheRoleRouteMeasure(t *testing.T) {
	envelope, err := buildMemoryEnvelope("short", time.Now(), 0, time.Time{}, "", memoryProvenance{}, []string{"a"}, "s")
	if err != nil {
		t.Fatalf("buildMemoryEnvelope: %v", err)
	}
	got := memoryContentCorpus(map[string]string{"old": envelope, "plain": "legacy text"})
	if len(got) != 2 || got["old"] != "short" || got["plain"] != "legacy text" {
		t.Errorf("memoryContentCorpus = %v, want old -> short and plain verbatim", got)
	}
}
