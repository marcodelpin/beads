//go:build cgo

package main

// Fork-only seams for upstream's in-process remember tests (bda-nchm path c).
//
// Upstream's tests drive rememberCmd.RunE against fake memory roles and pin
// what the role receives. On the fork's direct route the RunE also reads the
// plane for dedup (List) and stores an envelope when provenance was captured.
// These two seams let those upstream tests run unedited; the fork behaviour
// itself is covered by the embedded tests, which run the real binary.

import (
	"context"

	"github.com/steveyegge/beads/memoryops"
)

func init() {
	// No provenance in this test binary's in-process RunE calls: the
	// provenance fields are machine-specific (commit, cwd, session) and the
	// upstream tests pin the stored content.
	forkMemoryProvenance = func() memoryProvenance { return memoryProvenance{} }
}

// List serves the fork's dedup read from the fake's stored map.
func (m *bareKeyMemories) List(_ context.Context, _ memoryops.ListRequest) (memoryops.ListResult, error) {
	out := make(map[string]string, len(m.stored))
	for k, v := range m.stored {
		out[k] = v
	}
	return memoryops.ListResult{Memories: out}, nil
}
