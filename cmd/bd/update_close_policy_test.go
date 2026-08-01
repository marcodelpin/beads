//go:build cgo

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// Close policy on the generic status update. `bd close` refuses an issue that
// still has open parent-child children or a live direct blocker; the cases
// below pin what `bd update --status closed` does at that same boundary, on
// each surface that can reach a write funnel carrying a status of its own.
//
// These start as characterizations of today's permissive behavior. They are
// inverted in the commit that moves the policy into the funnels, so the
// behavior change reads as an explicit diff rather than a new file appearing
// beside untouched old assertions.

// seedClosePolicyFixture creates a parent with one open child and a target with
// one live direct blocker, and returns the two IDs a status update is aimed at.
func seedClosePolicyFixture(t *testing.T, env *parityEnv, prefix string) (parentID, blockedID string) {
	t.Helper()
	parentID = prefix + "-parent"
	childID := prefix + "-child"
	blockerID := prefix + "-blocker"
	blockedID = prefix + "-blocked"
	for _, id := range []string{parentID, childID, blockerID, blockedID} {
		env.seed(id, id, nil)
	}
	for _, dep := range []*types.Dependency{
		{IssueID: childID, DependsOnID: parentID, Type: types.DepParentChild},
		{IssueID: blockedID, DependsOnID: blockerID, Type: types.DepBlocks},
	} {
		if err := env.store.inner.AddDependency(rootCtx, dep, "parity-seed"); err != nil {
			t.Fatalf("seed dependency %s -> %s: %v", dep.IssueID, dep.DependsOnID, err)
		}
	}
	if blocked, _, err := env.store.inner.IsBlocked(rootCtx, blockedID); err != nil || !blocked {
		t.Fatalf("%s should be blocked (blocked=%v err=%v)", blockedID, blocked, err)
	}
	env.store.reset()
	return parentID, blockedID
}

// TestUpdateClosePolicyDirectCrossesIntoDone drives the direct (non-proxied)
// `bd update` path, which reaches the embedded write funnel through the
// issue-operations facade.
func TestUpdateClosePolicyDirectCrossesIntoDone(t *testing.T) {
	env := newParityEnv(t)
	parentID, blockedID := seedClosePolicyFixture(t, env, "test-ucp")

	// An open child refuses, names the count, and writes nothing.
	env.setFlags(updateCmd, map[string]string{"status": "closed"})
	res := env.run(updateCmd, parentID)
	if res.exitCode != 1 {
		t.Fatalf("update %s into done: exit = %d, want 1\nstderr:\n%s", parentID, res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "1 open child issue(s)") {
		t.Errorf("stderr lacks the open-children refusal:\n%s", res.stderr)
	}
	if got := env.get(parentID).Status; got != types.StatusOpen {
		t.Errorf("%s status = %q after a refusal, want open", parentID, got)
	}

	// A live direct blocker refuses too.
	res = env.run(updateCmd, blockedID)
	if res.exitCode != 1 {
		t.Fatalf("update %s into done: exit = %d, want 1\nstderr:\n%s", blockedID, res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "blocked by") {
		t.Errorf("stderr lacks the blocker refusal:\n%s", res.stderr)
	}
	if got := env.get(blockedID).Status; got != types.StatusOpen {
		t.Errorf("%s status = %q after a refusal, want open", blockedID, got)
	}

	// --force overrides both.
	env.setFlags(updateCmd, map[string]string{"status": "closed", "force": "true"})
	for _, id := range []string{parentID, blockedID} {
		if res := env.run(updateCmd, id); res.exitCode != 0 {
			t.Fatalf("forced update %s into done: exit = %d\nstderr:\n%s", id, res.exitCode, res.stderr)
		}
		if got := env.get(id).Status; got != types.StatusClosed {
			t.Errorf("%s status = %q after --force, want closed", id, got)
		}
	}

	// A done-to-done restatement is filtered out as a no-op before any policy
	// could observe it, so it needs no force despite the still-open child.
	env.setFlags(updateCmd, map[string]string{"status": "closed"})
	if res := env.run(updateCmd, parentID); res.exitCode != 0 {
		t.Fatalf("restate %s as done: exit = %d, want 0\nstderr:\n%s", parentID, res.exitCode, res.stderr)
	}
}

// TestUpdateClosePolicyDirectRefusalIsInert pins what a refusal costs. Nothing
// about the issue may move — not the row, not its event stream, not a claim
// riding the same request — and no hook may fire, because the parity harness
// counts one facade mutation per hook production would run.
func TestUpdateClosePolicyDirectRefusalIsInert(t *testing.T) {
	env := newParityEnv(t)
	parentID, _ := seedClosePolicyFixture(t, env, "test-ucpi")

	beforeEvents := len(env.eventTypes(parentID))
	before := env.get(parentID)

	// The claim rides the same request, so a refusal must take it down too.
	env.setFlags(updateCmd, map[string]string{"status": "closed", "claim": "true"})
	res := env.run(updateCmd, parentID)
	if res.exitCode != 1 {
		t.Fatalf("exit = %d, want 1\nstderr:\n%s", res.exitCode, res.stderr)
	}

	after := env.get(parentID)
	if after.Status != before.Status || after.Assignee != before.Assignee || after.RowVersion != before.RowVersion {
		t.Errorf("refusal moved the row: %+v -> %+v", before, after)
	}
	if after.ClosedAt != nil {
		t.Error("refusal stamped closed_at")
	}
	if got := len(env.eventTypes(parentID)); got != beforeEvents {
		t.Errorf("refusal wrote %d events, want 0", got-beforeEvents)
	}
	if got := env.store.mutations(); len(got) != 0 {
		t.Errorf("refusal made %v store mutations, want none (each would fire a hook)", got)
	}
}

// TestUpdateClosePolicyDirectForcedCrossingStaysAnUpdate keeps the change
// scoped to policy. A forced crossing is still an update, not a close: it
// records the status-change event stream an update records, and never the
// close verb's own.
func TestUpdateClosePolicyDirectForcedCrossingStaysAnUpdate(t *testing.T) {
	env := newParityEnv(t)
	parentID, _ := seedClosePolicyFixture(t, env, "test-ucpu")
	before := len(env.eventTypes(parentID))

	env.setFlags(updateCmd, map[string]string{"status": "closed", "force": "true"})
	if res := env.run(updateCmd, parentID); res.exitCode != 0 {
		t.Fatalf("forced crossing: exit = %d\nstderr:\n%s", res.exitCode, res.stderr)
	}

	if got := env.store.mutations(); len(got) != 1 || got[0] != "Update" {
		t.Errorf("store mutations = %v, want exactly one Update (never a Close)", got)
	}
	added := env.eventTypes(parentID)[before:]
	if len(added) != 1 {
		t.Fatalf("forced crossing wrote %v, want one event", added)
	}
}

// TestUpdateClosePolicyDirectForceWithoutAssignee pins how the direct path
// treats a bare `--force`. The flag now carries a second override that stands
// on its own, so `--force` with no `-a` is a legitimate request: the assignee
// half is simply not asserted, and the update applies.
func TestUpdateClosePolicyDirectForceWithoutAssignee(t *testing.T) {
	env := newParityEnv(t)
	env.seed("test-ucpf", "Force without assignee", nil)

	env.setFlags(updateCmd, map[string]string{"status": "closed", "force": "true"})
	res := env.run(updateCmd, "test-ucpf")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if strings.Contains(res.stderr, "invalid forced assignee transfer") {
		t.Errorf("--force without -a still asserts the assignee fence:\n%s", res.stderr)
	}
	if got := env.get("test-ucpf").Status; got != types.StatusClosed {
		t.Errorf("status = %q, want closed", got)
	}
}

// TestUpdateClosePolicyDirectForceStillFencesAssigneeTransfer keeps the other
// half of `--force` intact. Conditioning it on an assignee edit must not turn
// it off when there IS one: a transfer away from a live foreign claim is still
// exactly what the flag authorizes.
func TestUpdateClosePolicyDirectForceStillFencesAssigneeTransfer(t *testing.T) {
	env := newParityEnv(t)
	env.seed("test-ucpa", "Held by another actor", func(i *types.Issue) {
		i.Assignee = "someone-else"
		i.Status = types.StatusInProgress
	})

	// Without --force the fence holds.
	env.setFlags(updateCmd, map[string]string{"assignee": "thief"})
	if res := env.run(updateCmd, "test-ucpa"); res.exitCode == 0 {
		t.Fatalf("unforced transfer succeeded; the fence is gone\nstderr:\n%s", res.stderr)
	}
	if got := env.get("test-ucpa").Assignee; got != "someone-else" {
		t.Fatalf("assignee = %q after a refused transfer, want someone-else", got)
	}

	// With it, the transfer is authorized.
	env.setFlags(updateCmd, map[string]string{"assignee": "thief", "force": "true"})
	if res := env.run(updateCmd, "test-ucpa"); res.exitCode != 0 {
		t.Fatalf("forced transfer exit = %d, want 0\nstderr:\n%s", res.exitCode, res.stderr)
	}
	if got := env.get("test-ucpa").Assignee; got != "thief" {
		t.Errorf("assignee = %q, want thief", got)
	}
}

// TestUpdateClosePolicyBatchCrossesIntoDone drives `bd batch update`, whose
// transaction reaches the same embedded write funnel without going through the
// facade at all.
func TestUpdateClosePolicyBatchCrossesIntoDone(t *testing.T) {
	tmpDir := t.TempDir()
	st := newTestStoreWithPrefix(t, filepath.Join(tmpDir, ".beads", "beads.db"), "tbc")
	ctx := context.Background()

	seedBatchTestIssues(t, ctx, st, "tbc-parent", "tbc-child", "tbc-blocker", "tbc-blocked")
	for _, dep := range []*types.Dependency{
		{IssueID: "tbc-child", DependsOnID: "tbc-parent", Type: types.DepParentChild},
		{IssueID: "tbc-blocked", DependsOnID: "tbc-blocker", Type: types.DepBlocks},
	} {
		if err := st.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatalf("seed dependency %s -> %s: %v", dep.IssueID, dep.DependsOnID, err)
		}
	}

	// An unforced crossing refuses — and because the batch is one transaction,
	// it takes the WHOLE batch down, including the priority edit on a line that
	// had nothing to do with the refusal. That is the documented contract.
	script := "update tbc-blocker priority=0\nupdate tbc-parent status=closed\n"
	err := runBatchScriptInTx(t, ctx, st, script)
	if err == nil {
		t.Fatal("batch update into done with an open child succeeded, want a refusal")
	}
	if !errors.Is(err, storage.ErrCloseOpenChildren) {
		t.Errorf("batch error = %v, want ErrCloseOpenChildren", err)
	}
	rolledBack, getErr := st.GetIssue(ctx, "tbc-blocker")
	if getErr != nil {
		t.Fatalf("GetIssue tbc-blocker: %v", getErr)
	}
	if rolledBack.Priority != 2 {
		t.Errorf("tbc-blocker priority = %d; an unforced refusal must roll back the whole batch", rolledBack.Priority)
	}

	if err := runBatchScriptInTx(t, ctx, st, "update tbc-blocked status=closed\n"); !errors.Is(err, storage.ErrCloseBlocked) {
		t.Errorf("batch error = %v, want ErrCloseBlocked", err)
	}

	// force=true overrides both, in the same one transaction.
	forced := "update tbc-parent status=closed force=true\nupdate tbc-blocked status=closed force=true\n"
	if err := runBatchScriptInTx(t, ctx, st, forced); err != nil {
		t.Fatalf("forced batch update into done: %v", err)
	}
	for _, id := range []string{"tbc-parent", "tbc-blocked"} {
		got, err := st.GetIssue(ctx, id)
		if err != nil {
			t.Fatalf("GetIssue %s: %v", id, err)
		}
		if got.Status != types.StatusClosed {
			t.Errorf("%s status = %q, want closed", id, got.Status)
		}
	}
}

// TestUpdateClosePolicyBatchGrammarForceToken pins the batch update grammar's
// spelling of the override, and — the part that matters — pins the allowlist
// that keeps the reserved update-map key from being client-reachable. A script
// asks for force by the grammar's own token; it can never name the transport
// key itself, which is what stops the key from becoming a policy bypass.
func TestUpdateClosePolicyBatchGrammarForceToken(t *testing.T) {
	updates, err := parseUpdateKVs([]string{"status=closed", "force=true"})
	if err != nil {
		t.Fatalf("parseUpdateKVs(force=true): %v", err)
	}
	if got := updates[issueops.OpForceClosePolicy]; got != true {
		t.Errorf("updates[%q] = %v, want true", issueops.OpForceClosePolicy, got)
	}
	if updates["status"] != "closed" {
		t.Errorf("updates[status] = %v, want closed", updates["status"])
	}

	unforced, err := parseUpdateKVs([]string{"status=closed", "force=false"})
	if err != nil {
		t.Fatalf("parseUpdateKVs(force=false): %v", err)
	}
	if got := unforced[issueops.OpForceClosePolicy]; got != false {
		t.Errorf("updates[%q] = %v, want false", issueops.OpForceClosePolicy, got)
	}

	if _, err := parseUpdateKVs([]string{"force=perhaps"}); err == nil {
		t.Error("parseUpdateKVs accepted a non-boolean force value")
	}
	if _, err := parseUpdateKVs([]string{"_force_close_policy=true"}); err == nil {
		t.Error("parseUpdateKVs accepted the reserved update-map key as a client token")
	}
	if _, err := parseUpdateKVs([]string{"description=foo"}); err == nil {
		t.Error("parseUpdateKVs stopped rejecting keys outside its allowlist")
	}
}

// TestUpdateClosePolicyProxiedSpecCarriesForce pins the proxied path's
// translation of `--force`. An earlier attempt was reverted for exactly this
// missing mapping: the proxied caller built a spec that never carried the
// override, so a shared policy check refused the close with no way for the
// user to say otherwise. The spec must carry it, and must not invent it.
func TestUpdateClosePolicyProxiedSpecCarriesForce(t *testing.T) {
	current := &types.Issue{ID: "test-ucpp", Status: types.StatusOpen}

	forced := buildUpdateSpecForIssue(current, &updateInput{
		fields: map[string]any{"status": string(types.StatusClosed)}, force: true,
	})
	if got := forced.Fields[issueops.OpForceClosePolicy]; got != true {
		t.Errorf("spec.Fields[%q] = %v, want true", issueops.OpForceClosePolicy, got)
	}
	if got := forced.Fields["status"]; got != string(types.StatusClosed) {
		t.Errorf("spec.Fields[status] = %v, want closed", got)
	}

	unforced := buildUpdateSpecForIssue(current, &updateInput{
		fields: map[string]any{"status": string(types.StatusClosed)},
	})
	if _, ok := unforced.Fields[issueops.OpForceClosePolicy]; ok {
		t.Errorf("spec.Fields carries %q without --force", issueops.OpForceClosePolicy)
	}
}
