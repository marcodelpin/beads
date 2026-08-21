//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// memoryGCPlanShape mirrors the JSON shape of `bd memories --gc-plan`.
type memoryGCPlanShape struct {
	Now         string `json:"now"`
	MemoryCount int    `json:"memory_count"`
	Memories    []struct {
		Key          string `json:"key"`
		Content      string `json:"content"`
		ValidUntil   string `json:"valid_until"`
		ExpirePolicy string `json:"expire_policy"`
	} `json:"memories"`
	HintNextStep string `json:"hint_next_step"`
}

// TestEmbeddedMemoriesGCConsentPlan verifies `bd memories --gc-plan` emits a
// structured plan and modifies nothing.
func TestEmbeddedMemoriesGCConsentPlan(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "mp")

	// Anchor issue so auto-import doesn't resurrect deletes.
	bdCreate(t, bd, dir, "anchor", "--type", "task")

	bdRemember(t, bd, dir, "plan-stale-A",
		"--valid-until=2020-01-01", "--expire-policy=delete", "--key", "plan-A")
	bdRemember(t, bd, dir, "plan-stale-B",
		"--valid-until=2020-01-01", "--expire-policy=delete", "--key", "plan-B")

	out := bdMemories(t, bd, dir, "--gc-plan", "--json")
	t.Logf("plan output:\n%s", out)

	// JSON output may be wrapped in stderr noise; isolate the JSON object.
	jsonStart := strings.Index(out, "{")
	if jsonStart < 0 {
		t.Fatalf("no JSON object in plan output: %s", out)
	}
	var plan memoryGCPlanShape
	if err := json.Unmarshal([]byte(out[jsonStart:]), &plan); err != nil {
		t.Fatalf("plan output is not valid JSON: %v\noutput: %s", err, out)
	}
	if plan.MemoryCount != 2 {
		t.Errorf("expected 2 candidates in plan, got %d", plan.MemoryCount)
	}

	// Verify nothing was deleted.
	mem := bdMemories(t, bd, dir, "--include-expired")
	if !strings.Contains(mem, "plan-A") || !strings.Contains(mem, "plan-B") {
		t.Errorf("--gc-plan must not delete; got: %s", mem)
	}
}

// TestEmbeddedMemoriesGCConsentOnly verifies --gc --gc-only=CSV deletes only
// the listed keys.
func TestEmbeddedMemoriesGCConsentOnly(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "mo")

	// Anchor issue.
	bdCreate(t, bd, dir, "anchor", "--type", "task")

	bdRemember(t, bd, dir, "only-A",
		"--valid-until=2020-01-01", "--expire-policy=delete", "--key", "only-A")
	bdRemember(t, bd, dir, "only-B",
		"--valid-until=2020-01-01", "--expire-policy=delete", "--key", "only-B")
	bdRemember(t, bd, dir, "only-C",
		"--valid-until=2020-01-01", "--expire-policy=delete", "--key", "only-C")

	// Approve only A and C.
	_ = bdMemories(t, bd, dir, "--gc", "--gc-only=only-A,only-C")

	mem := bdMemories(t, bd, dir, "--include-expired")
	if strings.Contains(mem, "only-A") {
		t.Errorf("only-A should be deleted (in --gc-only), still present: %s", mem)
	}
	if strings.Contains(mem, "only-C") {
		t.Errorf("only-C should be deleted (in --gc-only), still present: %s", mem)
	}
	if !strings.Contains(mem, "only-B") {
		t.Errorf("only-B should remain (not in --gc-only), got: %s", mem)
	}
}

// TestEmbeddedMemoriesGCConsentMutex verifies --gc-plan + --gc is rejected.
func TestEmbeddedMemoriesGCConsentMutex(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "mm")

	// Inline exec because we expect failure (bdMemories helper t.Fatal's on err).
	cmd := exec.Command(bd, "memories", "--gc-plan", "--gc")
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected --gc-plan + --gc to fail, but succeeded: %s", out)
	}
	if !strings.Contains(string(out), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' error, got: %s", out)
	}
}
