//go:build cgo

package main

import (
	"os"
	"strings"
	"testing"
)

// TestEmbeddedGCMemoryPrune verifies bd gc decay phase hard-deletes
// expired memories with expire-policy=delete, same as bd memories --gc.
// Fork-only sub-phase (sys-8t7vx, sub-feature 2 of memory roadmap).
func TestEmbeddedGCMemoryPrune(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gp")

	// Seed three memories:
	//   1. expired with policy=delete  -> must be pruned by 'bd gc'
	//   2. expired with policy=hide    -> NOT pruned (only delete policy is purged)
	//   3. valid (not expired)         -> NOT pruned
	//
	// We use --valid-until in the past via a date string the parser
	// accepts (YYYY-MM-DD). Anything before today is expired.
	bdRemember(t, bd, dir,
		"prune target stale fact",
		"--valid-until=2020-01-01",
		"--expire-policy=delete",
		"--key", "prune-target")

	bdRemember(t, bd, dir,
		"hide target stale fact",
		"--valid-until=2020-01-01",
		"--expire-policy=hide",
		"--key", "hide-target")

	bdRemember(t, bd, dir,
		"valid future fact",
		"--valid-until=2099-01-01",
		"--expire-policy=delete",
		"--key", "future-target")

	// Run bd gc (decay phase pruning kicks in). --allow-recent so
	// older-than 0 is accepted, --skip-dolt to keep test fast,
	// --force to skip prompts. Note: closed-issue decay needs no
	// candidates here (we created memories, not issues), so the
	// decay pre-check 'no closed issues' path runs and the memory
	// prune sub-phase ALWAYS runs after it.
	out := bdGC(t, bd, dir, "--force", "--older-than", "0", "--allow-recent", "--skip-dolt")
	t.Logf("bd gc output:\n%s", out)

	if !strings.Contains(out, "Memory prune") {
		t.Errorf("expected 'Memory prune' line in output, got: %s", out)
	}
	if !strings.Contains(out, "deleted 1 expired") {
		t.Errorf("expected 'deleted 1 expired' (only prune-target with policy=delete), got: %s",
			out)
	}

	// Verify 'bd memories --include-expired' still shows hide-target
	// and future-target, but NOT prune-target.
	mem := bdMemories(t, bd, dir, "--include-expired")
	if !strings.Contains(mem, "hide-target") {
		t.Errorf("expected hide-target preserved (policy=hide is not deleted), got: %s", mem)
	}
	if !strings.Contains(mem, "future-target") {
		t.Errorf("expected future-target preserved (not expired), got: %s", mem)
	}
	if strings.Contains(mem, "prune-target") {
		t.Errorf("expected prune-target deleted, but still present: %s", mem)
	}
}

// TestEmbeddedGCMemoryPruneSkipFlag verifies --skip-memory-prune disables
// the memory prune sub-phase.
func TestEmbeddedGCMemoryPruneSkipFlag(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gs")

	bdRemember(t, bd, dir,
		"skip-target stale fact",
		"--valid-until=2020-01-01",
		"--expire-policy=delete",
		"--key", "skip-target")

	out := bdGC(t, bd, dir, "--force", "--older-than", "0", "--allow-recent",
		"--skip-dolt", "--skip-memory-prune")

	if strings.Contains(out, "Memory prune:") {
		t.Errorf("--skip-memory-prune must suppress the Memory prune line, got: %s", out)
	}

	// Memory must still be there.
	mem := bdMemories(t, bd, dir, "--include-expired")
	if !strings.Contains(mem, "skip-target") {
		t.Errorf("--skip-memory-prune must preserve the memory, got: %s", mem)
	}
}
