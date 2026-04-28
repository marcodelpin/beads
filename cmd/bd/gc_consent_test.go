//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// gcPlanShape mirrors the JSON output of `bd gc --plan` for parsing in tests.
type gcPlanShape struct {
	OlderThanDays int `json:"older_than_days"`
	IssueCount    int `json:"issue_count"`
	MemoryCount   int `json:"memory_count"`
	Issues        []struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		ClosedAt string `json:"closed_at"`
		AgeDays  int    `json:"age_days"`
	} `json:"issues"`
	Memories []struct {
		Key          string `json:"key"`
		Content      string `json:"content"`
		ValidUntil   string `json:"valid_until"`
		ExpirePolicy string `json:"expire_policy"`
	} `json:"memories"`
}

// TestEmbeddedGCConsentPlan verifies `bd gc --plan` emits a structured plan
// of candidates without modifying anything (closed issues stay closed but
// undeleted; memories stay).
func TestEmbeddedGCConsentPlan(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "cp")

	// Seed: 3 closed issues + 2 expired memories with policy=delete.
	for i := 0; i < 3; i++ {
		issue := bdCreate(t, bd, dir, "consent plan candidate", "--type", "task")
		cmd := exec.Command(bd, "close", issue.ID)
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("close %s failed: %v\n%s", issue.ID, err, out)
		}
	}
	bdRemember(t, bd, dir, "plan-stale-1",
		"--valid-until=2020-01-01", "--expire-policy=delete", "--key", "stale-1")
	bdRemember(t, bd, dir, "plan-stale-2",
		"--valid-until=2020-01-01", "--expire-policy=delete", "--key", "stale-2")

	out := bdGC(t, bd, dir, "--plan", "--older-than", "0", "--allow-recent")
	t.Logf("plan output:\n%s", out)

	var plan gcPlanShape
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan output is not valid JSON: %v\noutput: %s", err, out)
	}
	if plan.IssueCount != 3 {
		t.Errorf("expected 3 issues in plan, got %d", plan.IssueCount)
	}
	if plan.MemoryCount != 2 {
		t.Errorf("expected 2 memories in plan, got %d", plan.MemoryCount)
	}

	// Verify nothing was modified: stale-1 and stale-2 must still be visible
	// via 'bd memories --include-expired'.
	mem := bdMemories(t, bd, dir, "--include-expired")
	if !strings.Contains(mem, "stale-1") || !strings.Contains(mem, "stale-2") {
		t.Errorf("--plan must not delete memories; got: %s", mem)
	}
}

// TestEmbeddedGCConsentOnly verifies `bd gc --force --only=...` deletes only
// the listed items and leaves the rest untouched.
func TestEmbeddedGCConsentOnly(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "co")

	// Seed: 3 closed issues + 2 expired memories.
	var issueIDs []string
	for i := 0; i < 3; i++ {
		issue := bdCreate(t, bd, dir, "consent only candidate", "--type", "task")
		issueIDs = append(issueIDs, issue.ID)
		cmd := exec.Command(bd, "close", issue.ID)
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("close %s failed: %v\n%s", issue.ID, err, out)
		}
	}
	bdRemember(t, bd, dir, "only-stale-1",
		"--valid-until=2020-01-01", "--expire-policy=delete", "--key", "only-stale-1")
	bdRemember(t, bd, dir, "only-stale-2",
		"--valid-until=2020-01-01", "--expire-policy=delete", "--key", "only-stale-2")

	// Approve only the first issue + only-stale-1.
	approved := issueIDs[0] + ",only-stale-1"
	out := bdGC(t, bd, dir, "--force", "--older-than", "0", "--allow-recent",
		"--skip-dolt", "--only", approved)
	t.Logf("only output:\n%s", out)

	if !strings.Contains(out, "Deleted 1 issue") && !strings.Contains(out, "1 issues deleted") {
		t.Errorf("expected exactly 1 issue deleted (the approved one), got: %s", out)
	}
	if !strings.Contains(out, "Memory prune: deleted 1 expired") {
		t.Errorf("expected exactly 1 memory deleted (only-stale-1), got: %s", out)
	}

	// only-stale-2 must still be present.
	mem := bdMemories(t, bd, dir, "--include-expired")
	if !strings.Contains(mem, "only-stale-2") {
		t.Errorf("only-stale-2 should remain (not in --only list), got: %s", mem)
	}
	if strings.Contains(mem, "only-stale-1") {
		t.Errorf("only-stale-1 should be deleted, got: %s", mem)
	}
}

// TestEmbeddedGCConsentPlanForceMutex verifies --plan + --force is rejected.
func TestEmbeddedGCConsentPlanForceMutex(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "cm")

	out := bdGCFail(t, bd, dir, "--plan", "--force", "--older-than", "30")
	if !strings.Contains(out, "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' error, got: %s", out)
	}
}
