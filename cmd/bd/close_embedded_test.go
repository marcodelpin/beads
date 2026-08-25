//go:build cgo

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/types"
)

// ===== Close-specific test helpers =====

// bdClose runs "bd close" with the given args and returns stdout.
// Retries on flock contention.
func bdClose(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"close"}, args...)
	out, err := bdRunWithFlockRetry(t, bd, dir, fullArgs...)
	if err != nil {
		t.Fatalf("bd close %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// bdCloseFail runs "bd close" expecting failure.
func bdCloseFail(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"close"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd close %s to fail, but it succeeded:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

// bdDepAdd runs "bd dep add" with the given args.
// Retries on flock contention.
func bdDepAdd(t *testing.T, bd, dir string, args ...string) {
	t.Helper()
	fullArgs := append([]string{"dep", "add"}, args...)
	out, err := bdRunWithFlockRetry(t, bd, dir, fullArgs...)
	if err != nil {
		t.Fatalf("bd dep add %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// ===== Close tests =====

func TestEmbeddedClose(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "tc")

	// ===== Basic Close Behavior =====

	t.Run("basic_close", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Close me", "--type", "task")
		bdClose(t, bd, dir, issue.ID)
		got := bdShow(t, bd, dir, issue.ID)
		if got.Status != types.StatusClosed {
			t.Errorf("expected status closed, got %s", got.Status)
		}
		if got.ClosedAt == nil {
			t.Error("expected closed_at to be set")
		}
	})

	// Mixed batch: one already-closed bead + one live bead. Both must appear in
	// the JSON array — the already-closed one for output parity, the live one as a
	// real close.
	t.Run("close_json_mixed_batch_includes_already_closed", func(t *testing.T) {
		already := bdCreate(t, bd, dir, "Mixed already", "--type", "task")
		fresh := bdCreate(t, bd, dir, "Mixed fresh", "--type", "task")
		bdClose(t, bd, dir, already.ID) // pre-close one

		cmd := exec.Command(bd, "close", already.ID, fresh.ID, "--json")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		stdout, stderr, err := runCommandBuffers(t, cmd)
		if err != nil {
			t.Fatalf("bd close --json (mixed batch) failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		s := stdout.String()
		start := strings.Index(s, "[")
		if start < 0 {
			t.Fatalf("expected a JSON array for mixed-batch --json close, got: %s", s)
		}
		var issues []json.RawMessage
		if jsonErr := json.Unmarshal([]byte(s[start:]), &issues); jsonErr != nil {
			t.Fatalf("expected valid JSON array, got: %s (%v)", s[start:], jsonErr)
		}
		if len(issues) != 2 {
			t.Fatalf("expected both issues in JSON (real close + already-closed parity), got %d: %s", len(issues), s[start:])
		}
		if !strings.Contains(s, already.ID) || !strings.Contains(s, fresh.ID) {
			t.Errorf("expected both %s and %s in JSON output, got: %s", already.ID, fresh.ID, s)
		}
	})

	// Proves the S7 delegation: `bd close` on a blocked issue now surfaces the
	// engine's atomic guard (storage.ErrCloseBlocked) rather than a duplicated
	// CLI pre-check. The refusal must be atomic — the issue stays open because the
	// guard and the close share one transaction — and the message must name the
	// blocker and the --force hint. --force then bypasses the engine guard.
	t.Run("close_blocked_delegated_guard", func(t *testing.T) {
		blocker := bdCreate(t, bd, dir, "Deleg blocker", "--type", "task")
		blocked := bdCreate(t, bd, dir, "Deleg blocked", "--type", "task")
		bdDepAdd(t, bd, dir, blocked.ID, blocker.ID)

		out := bdCloseFail(t, bd, dir, blocked.ID)
		if !strings.Contains(out, "cannot close") {
			t.Errorf("expected engine guard message ('cannot close'), got: %s", out)
		}
		if !strings.Contains(out, blocker.ID) {
			t.Errorf("expected guard message to name blocker %s, got: %s", blocker.ID, out)
		}
		if !strings.Contains(out, "--force") {
			t.Errorf("expected guard message to mention --force, got: %s", out)
		}

		// Atomic refuse: the guard ran in-transaction, so the issue must remain open.
		got := bdShow(t, bd, dir, blocked.ID)
		if got.Status == types.StatusClosed {
			t.Error("expected blocked issue to remain open after the guard refused (atomic)")
		}

		// --force bypasses the engine guard.
		bdClose(t, bd, dir, blocked.ID, "--force")
		got = bdShow(t, bd, dir, blocked.ID)
		if got.Status != types.StatusClosed {
			t.Errorf("expected closed with --force, got %s", got.Status)
		}
	})

	t.Run("reclose_by_foreign_actor_is_idempotent", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Foreign reclose", "--type", "task")
		bdUpdate(t, bd, dir, issue.ID, "--actor", "bob", "--claim")
		bdClose(t, bd, dir, issue.ID, "--actor", "bob")

		// Alice re-closes a bead she never held. bdClose t.Fatalf's on a nonzero
		// exit, so this line is the assertion.
		bdClose(t, bd, dir, issue.ID, "--actor", "alice")

		got := bdShow(t, bd, dir, issue.ID)
		if got.Status != types.StatusClosed {
			t.Errorf("status: got %q, want closed", got.Status)
		}
		if got.Assignee != "bob" {
			t.Errorf("assignee: got %q, want bob — a re-close must not rewrite the holder", got.Assignee)
		}
	})

	t.Run("close_epic_open_children_force", func(t *testing.T) {
		epic := bdCreate(t, bd, dir, "Epic force", "--type", "epic")
		child := bdCreate(t, bd, dir, "Epic child force", "--type", "task")
		bdDepAdd(t, bd, dir, child.ID, epic.ID, "--type", "parent-child")

		cmd := exec.Command(bd, "close", epic.ID, "--force")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bd close --force failed: %v\n%s", err, out)
		}
		got := bdShow(t, bd, dir, epic.ID)
		if got.Status != types.StatusClosed {
			t.Errorf("expected epic closed with --force, got %s", got.Status)
		}
		if !strings.Contains(string(out), "warning:") || !strings.Contains(string(out), "open child") {
			t.Errorf("expected warning about open children on --force, got: %s", out)
		}
		_ = child
	})

	t.Run("close_dolt_commit", func(t *testing.T) {
		dataDir := filepath.Join(beadsDir, "embeddeddolt")
		cfg, _ := configfile.Load(beadsDir)
		database := ""
		if cfg != nil {
			database = cfg.GetDoltDatabase()
		}

		countCommits := func() int {
			db, cleanup, err := embeddeddolt.OpenSQL(t.Context(), dataDir, database, "main")
			if err != nil {
				t.Fatalf("OpenSQL: %v", err)
			}
			defer cleanup()
			var count int
			if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM dolt_log").Scan(&count); err != nil {
				t.Fatalf("query dolt_log: %v", err)
			}
			return count
		}

		before := countCommits()
		issue := bdCreate(t, bd, dir, "Dolt commit test", "--type", "task")
		_ = issue
		afterCreate := countCommits()
		bdClose(t, bd, dir, issue.ID)
		afterClose := countCommits()

		if afterClose <= afterCreate {
			t.Errorf("expected Dolt commit count to increase after close: before=%d afterCreate=%d afterClose=%d", before, afterCreate, afterClose)
		}
	})

	// The direct route's mirror of the proxied route's
	// single_transaction_dolt_commit oracle. N ids are ONE request, and the
	// request is the transaction boundary, so they land as one transaction with
	// one Dolt commit whose message names every id that landed. Before `bd close`
	// moved onto the BatchCloser role this route wrote one commit per id, each
	// titled "bd: close issue".
	t.Run("close_multiple_ids_single_dolt_commit", func(t *testing.T) {
		// Isolated store so the commit count is deterministic, mirroring
		// close_already_closed_claim_next.
		sdir, sbeads, _ := bdInit(t, bd, "--prefix", "sb")
		readLog := func() (int, string) {
			dataDir := filepath.Join(sbeads, "embeddeddolt")
			cfg, _ := configfile.Load(sbeads)
			database := ""
			if cfg != nil {
				database = cfg.GetDoltDatabase()
			}
			db, cleanup, err := embeddeddolt.OpenSQL(t.Context(), dataDir, database, "main")
			if err != nil {
				t.Fatalf("OpenSQL: %v", err)
			}
			defer cleanup()
			var count int
			if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM dolt_log").Scan(&count); err != nil {
				t.Fatalf("query dolt_log: %v", err)
			}
			var message string
			if err := db.QueryRowContext(t.Context(), "SELECT message FROM dolt_log ORDER BY date DESC LIMIT 1").Scan(&message); err != nil {
				t.Fatalf("query latest dolt_log message: %v", err)
			}
			return count, message
		}

		a := bdCreate(t, bd, sdir, "Batch commit A", "--type", "task")
		b := bdCreate(t, bd, sdir, "Batch commit B", "--type", "task")
		c := bdCreate(t, bd, sdir, "Batch commit C", "--type", "task")

		before, _ := readLog()
		bdClose(t, bd, sdir, a.ID, b.ID, c.ID)
		after, message := readLog()

		if got := after - before; got != 1 {
			t.Errorf("dolt commits for a 3-id close = %d, want 1: the request is the transaction boundary", got)
		}
		if !strings.HasPrefix(message, "bd: close ") {
			t.Errorf("commit message = %q, want it to start with %q", message, "bd: close ")
		}
		for _, id := range []string{a.ID, b.ID, c.ID} {
			if !strings.Contains(message, id) {
				t.Errorf("commit message %q should name %s: the entry names what landed", message, id)
			}
		}
	})

	t.Run("close_already_closed_continue_advances", func(t *testing.T) {
		// Isolated store so molecule progress and the Dolt commit count are
		// deterministic, mirroring close_already_closed_claim_next.
		cdir, cbeads, _ := bdInit(t, bd, "--prefix", "rk")
		countCommits := func() int {
			dataDir := filepath.Join(cbeads, "embeddeddolt")
			cfg, _ := configfile.Load(cbeads)
			database := ""
			if cfg != nil {
				database = cfg.GetDoltDatabase()
			}
			db, cleanup, err := embeddeddolt.OpenSQL(t.Context(), dataDir, database, "main")
			if err != nil {
				t.Fatalf("OpenSQL: %v", err)
			}
			defer cleanup()
			var count int
			if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM dolt_log").Scan(&count); err != nil {
				t.Fatalf("query dolt_log: %v", err)
			}
			return count
		}

		root := bdCreate(t, bd, cdir, "Reclose continue root", "--type", "epic", "--labels", "template")
		step1 := bdCreate(t, bd, cdir, "Reclose continue step one", "--type", "task", "--parent", root.ID)
		step2 := bdCreate(t, bd, cdir, "Reclose continue step two", "--type", "task", "--parent", root.ID)
		// step2 blocks on step1, so closing step1 makes step2 the next ready step.
		bdDepAdd(t, bd, cdir, step2.ID, step1.ID)

		if _, err := bdRunWithFlockRetry(t, bd, cdir, "update", step1.ID, "--claim"); err != nil {
			t.Fatalf("seed claim failed: %v", err)
		}

		// Close step1 for real WITHOUT --continue — the advancement trigger never ran
		// (models a crash/retry between the status flip and the advance).
		bdClose(t, bd, cdir, step1.ID, "--reason", "first")

		// Retry the close WITH --continue against the now already-closed step. The
		// idempotent re-close must advance the molecule AND persist the advance, not
		// just mutate the in-memory working set.
		beforeCommits := countCommits()
		_ = bdClose(t, bd, cdir, step1.ID, "--reason", "retry", "--continue")

		got, err := os.ReadFile(filepath.Join(cbeads, "last-touched"))
		if err != nil {
			t.Fatalf("read .beads/last-touched: %v", err)
		}
		if gotID := strings.TrimSpace(string(got)); gotID != step2.ID {
			t.Errorf(".beads/last-touched = %q after re-closing already-closed %s --continue, want %q (auto-advanced step)",
				gotID, step1.ID, step2.ID)
		}

		// Persisted-advancement assertions — the retry-safety property the fix
		// guarantees, and the gap the reviewer flagged: last-touched alone proves
		// AdvanceToNextStep ran in the working set, not that the advance was
		// committed. step2 must be persisted as in_progress, and the already-closed
		// re-close (closedCount==0) must still produce a Dolt commit for the advance.
		// The auto-advance moves the step to in_progress via UpdateIssue but, unlike
		// --claim-next's ClaimIssue, does not set an assignee, so we assert status +
		// commit rather than assignee.
		if s2 := bdShow(t, bd, cdir, step2.ID); s2.Status != types.StatusInProgress {
			t.Errorf("expected step2 %s persisted as in_progress after already-closed --continue, got status=%s",
				step2.ID, s2.Status)
		}
		if afterCommits := countCommits(); afterCommits <= beforeCommits {
			t.Errorf("expected a Dolt commit for the --continue advance on an already-closed re-close: before=%d after=%d",
				beforeCommits, afterCommits)
		}
	})

	t.Run("close_already_closed_claim_next", func(t *testing.T) {
		// Isolated store so the ready set is deterministic — the shared store carries
		// open issues from sibling subtests, and --claim-next claims the global
		// highest-priority ready issue.
		cdir, cbeads, _ := bdInit(t, bd, "--prefix", "rc")
		countCommits := func() int {
			dataDir := filepath.Join(cbeads, "embeddeddolt")
			cfg, _ := configfile.Load(cbeads)
			database := ""
			if cfg != nil {
				database = cfg.GetDoltDatabase()
			}
			db, cleanup, err := embeddeddolt.OpenSQL(t.Context(), dataDir, database, "main")
			if err != nil {
				t.Fatalf("OpenSQL: %v", err)
			}
			defer cleanup()
			var count int
			if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM dolt_log").Scan(&count); err != nil {
				t.Fatalf("query dolt_log: %v", err)
			}
			return count
		}

		target := bdCreate(t, bd, cdir, "Reclose claim target", "--type", "task")
		next := bdCreate(t, bd, cdir, "Reclose claim next", "--type", "task")
		bdClose(t, bd, cdir, target.ID) // real close; `next` is now the only ready issue

		// Re-close the already-closed target with --claim-next. The claim does NOT
		// fire: bd-yby99.19 adjudicated that a batch whose items were all already
		// closed mutated nothing, so it earns no claim and mints no commit
		// (issueops/batchcloser.go, "CHANGED IS THE TEST"). This subtest used to
		// assert the opposite as a retry-safety property — a crashed agent
		// re-running `bd close X --claim-next` got its next work item — and that
		// property is what the adjudication traded away; bd-yby99.30 carries it.
		beforeCommits := countCommits()
		_ = bdClose(t, bd, cdir, target.ID, "--claim-next")

		got := bdShow(t, bd, cdir, next.ID)
		if got.Status != types.StatusOpen || got.Assignee != "" {
			t.Errorf("next issue %s = (status=%s assignee=%q) after an already-closed --claim-next, want it untouched: the re-close closed nothing, so the claim was never earned",
				next.ID, got.Status, got.Assignee)
		}
		// Nothing landed, so nothing is committed either — the shape the
		// adjudication names, a commit that changed nothing.
		if afterCommits := countCommits(); afterCommits != beforeCommits {
			t.Errorf("dolt_log went %d -> %d across an already-closed re-close that claimed nothing, want no commit",
				beforeCommits, afterCommits)
		}
	})

	// Regression for the delegated-close change: molecule root auto-close is a
	// state-derived post-close contract, so an already-closed re-close of the final
	// step must re-drive it. Models the crash where the final step's close persisted
	// but its molecule-root auto-close did not — the idempotent retry heals the
	// stranded-open root instead of leaving it open forever.
	t.Run("close_already_closed_replays_molecule_auto_close", func(t *testing.T) {
		// Isolated store so molecule progress is deterministic.
		mdir, _, _ := bdInit(t, bd, "--prefix", "rm")
		root := bdCreate(t, bd, mdir, "Reclose molecule root", "--type", "epic", "--labels", "template")
		step1 := bdCreate(t, bd, mdir, "Reclose molecule step one", "--type", "task", "--parent", root.ID)
		step2 := bdCreate(t, bd, mdir, "Reclose molecule step two", "--type", "task", "--parent", root.ID)

		// Close both steps for real. Closing the final step auto-closes the root.
		bdClose(t, bd, mdir, step1.ID, "--reason", "one")
		bdClose(t, bd, mdir, step2.ID, "--reason", "two")
		if got := bdShow(t, bd, mdir, root.ID); got.Status != types.StatusClosed {
			t.Fatalf("precondition: expected molecule root %s auto-closed after final step, got %s", root.ID, got.Status)
		}

		// Strand the molecule: reopen ONLY the root, leaving both steps closed — the
		// exact state left when a final step's close commits but its root auto-close
		// does not.
		bdReopen(t, bd, mdir, root.ID)
		if got := bdShow(t, bd, mdir, root.ID); got.Status != types.StatusOpen {
			t.Fatalf("precondition: expected molecule root %s reopened, got %s", root.ID, got.Status)
		}
		if got := bdShow(t, bd, mdir, step2.ID); got.Status != types.StatusClosed {
			t.Fatalf("precondition: expected step2 %s to stay closed after reopening only the root, got %s", step2.ID, got.Status)
		}

		// Re-close the already-closed final step. The idempotent re-close must replay
		// molecule auto-close and re-close the stranded-open root.
		_ = bdClose(t, bd, mdir, step2.ID, "--reason", "retry")

		if got := bdShow(t, bd, mdir, root.ID); got.Status != types.StatusClosed {
			t.Errorf("expected stranded-open molecule root %s re-closed by an already-closed re-close of the final step, got %s",
				root.ID, got.Status)
		}
	})

	t.Run("close_suggest_next_multiple_ids_fails", func(t *testing.T) {
		issue1 := bdCreate(t, bd, dir, "Suggest multi 1", "--type", "task")
		issue2 := bdCreate(t, bd, dir, "Suggest multi 2", "--type", "task")
		bdCloseFail(t, bd, dir, issue1.ID, issue2.ID, "--suggest-next")
	})

	// ===== Already-Closed Re-close (GH#4816) =====
	// The storage layer's idempotent close (GH#4025) keeps the first reason and
	// mints no second event; the CLI must report that truthfully instead of
	// printing a false "✓ Closed <id>: <new reason>".

	t.Run("reclose_reports_already_closed_and_preserves_reason", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Reclose truth", "--type", "task")
		bdClose(t, bd, dir, issue.ID, "--reason", "FIRST")

		cmd := exec.Command(bd, "close", issue.ID, "--reason", "SECOND")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		stdout, stderr, err := runCommandBuffers(t, cmd)
		if err != nil {
			t.Fatalf("idempotent re-close must exit 0 (GH#4025): %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if strings.Contains(stdout.String(), "Closed") {
			t.Errorf("re-close printed a success line for a no-op:\n%s", stdout.String())
		}
		if !strings.Contains(stderr.String(), "already closed") {
			t.Errorf("expected stderr to report the issue was already closed, got:\n%s", stderr.String())
		}
		got := bdShow(t, bd, dir, issue.ID)
		if got.CloseReason != "FIRST" {
			t.Errorf("close_reason = %q after re-close, want %q (first close wins)", got.CloseReason, "FIRST")
		}
	})

	t.Run("reclose_json_reports_already_closed", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Reclose JSON", "--type", "task")
		bdClose(t, bd, dir, issue.ID, "--reason", "FIRST")

		cmd := exec.Command(bd, "close", issue.ID, "--reason", "SECOND", "--json")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		stdout, stderr, err := runCommandBuffers(t, cmd)
		if err != nil {
			t.Fatalf("re-close --json failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		// RE-POINTED (bda-4myc root-cause, clause-219 discipline): this test
		// was written against GH#4816's {closed, already_closed} object shape;
		// upstream #4911 replaced the --json contract with array parity (see
		// close_json_mixed_batch_includes_already_closed, which pins it) and
		// the #5191 port kept that. The PROPERTY this test guards survives on
		// different channels: the truthful already-closed signal moved to
		// stderr (both output modes), and first-close-wins stays observable in
		// the payload itself. Asserting the old object shape here would just
		// contradict the sibling parity test.
		s := stdout.String()
		start := strings.Index(s, "[")
		if start < 0 {
			t.Fatalf("no JSON array in output (want #4911 parity shape): %s", s)
		}
		var issues []*types.Issue
		if err := json.Unmarshal([]byte(s[start:]), &issues); err != nil {
			t.Fatalf("parse JSON: %v\n%s", err, s[start:])
		}
		if len(issues) != 1 || issues[0].ID != issue.ID {
			t.Fatalf("issues = %+v, want exactly [%s] (parity: the no-op still reports the issue)", issues, issue.ID)
		}
		if issues[0].CloseReason != "FIRST" {
			t.Errorf("close_reason = %q in payload, want %q (first close wins, SECOND dropped)", issues[0].CloseReason, "FIRST")
		}
		if !strings.Contains(stderr.String(), "already closed") {
			t.Errorf("expected the truthful already-closed notice on stderr in --json mode too, got:\n%s", stderr.String())
		}
	})

	t.Run("reclose_mints_no_second_closed_event", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Reclose events", "--type", "task")
		bdClose(t, bd, dir, issue.ID, "--reason", "FIRST")

		cmd := exec.Command(bd, "close", issue.ID, "--reason", "SECOND")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		_, _, _ = runCommandBuffers(t, cmd)

		dataDir := filepath.Join(beadsDir, "embeddeddolt")
		cfg, _ := configfile.Load(beadsDir)
		database := ""
		if cfg != nil {
			database = cfg.GetDoltDatabase()
		}
		db, cleanup, err := embeddeddolt.OpenSQL(t.Context(), dataDir, database, "main")
		if err != nil {
			t.Fatalf("OpenSQL: %v", err)
		}
		defer cleanup()
		var count int
		if err := db.QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM events WHERE issue_id = ? AND event_type = 'closed'", issue.ID).Scan(&count); err != nil {
			t.Fatalf("count closed events: %v", err)
		}
		if count != 1 {
			t.Errorf("closed events = %d, want 1 (no phantom event on re-close)", count)
		}
	})

	t.Run("reclose_skips_claim_next", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Reclose claim-next", "--type", "task")
		bdClose(t, bd, dir, issue.ID)
		ready := bdCreate(t, bd, dir, "Reclose claim-next target", "--type", "task", "--priority", "0")

		cmd := exec.Command(bd, "close", issue.ID, "--claim-next")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		stdout, stderr, err := runCommandBuffers(t, cmd)
		if err != nil {
			t.Fatalf("idempotent re-close must exit 0: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		got := bdShow(t, bd, dir, ready.ID)
		if got.Status != types.StatusOpen || got.Assignee != "" {
			t.Errorf("no-op re-close must not claim-next: target status=%s assignee=%q", got.Status, got.Assignee)
		}
		bdClose(t, bd, dir, ready.ID) // keep later subtests' ready pool clean
	})

	t.Run("close_mixed_batch_reports_each_id_truthfully", func(t *testing.T) {
		done := bdCreate(t, bd, dir, "Mixed batch done", "--type", "task")
		open := bdCreate(t, bd, dir, "Mixed batch open", "--type", "task")
		bdClose(t, bd, dir, done.ID, "--reason", "FIRST")

		cmd := exec.Command(bd, "close", done.ID, open.ID, "--reason", "shared")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		stdout, stderr, err := runCommandBuffers(t, cmd)
		if err != nil {
			t.Fatalf("mixed batch close failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), done.ID+" already closed") {
			t.Errorf("expected already-closed notice for %s, got stderr:\n%s", done.ID, stderr.String())
		}
		gotDone := bdShow(t, bd, dir, done.ID)
		if gotDone.CloseReason != "FIRST" {
			t.Errorf("%s close_reason = %q, want %q (first close wins)", done.ID, gotDone.CloseReason, "FIRST")
		}
		gotOpen := bdShow(t, bd, dir, open.ID)
		if gotOpen.Status != types.StatusClosed || gotOpen.CloseReason != "shared" {
			t.Errorf("%s status=%s close_reason=%q, want closed/%q", open.ID, gotOpen.Status, gotOpen.CloseReason, "shared")
		}
	})
}

// TestEmbeddedCloseConcurrent exercises create, close, and list operations
// concurrently to verify EmbeddedDoltStore handles concurrent CLI invocations
// without panics, data corruption, or deadlocks.
func TestEmbeddedCloseConcurrent(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "cx")

	const (
		numWorkers      = 10
		issuesPerWorker = 5
	)

	type workerResult struct {
		worker     int
		ids        []string
		listCounts []int
		err        error
	}

	results := make([]workerResult, numWorkers)
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		go func(worker int) {
			defer wg.Done()
			r := workerResult{worker: worker}

			for i := 0; i < issuesPerWorker; i++ {
				// Create an issue.
				title := fmt.Sprintf("w%d-close-%d", worker, i)
				out, err := bdRunWithFlockRetry(t, bd, dir, "create", "--silent", title)
				if err != nil {
					r.err = fmt.Errorf("create %d: %v\n%s", i, err, out)
					results[worker] = r
					return
				}
				id := strings.TrimSpace(string(out))
				if id == "" {
					r.err = fmt.Errorf("create %d: empty ID", i)
					results[worker] = r
					return
				}
				r.ids = append(r.ids, id)

				// Close with a reason.
				reason := fmt.Sprintf("done-by-worker-%d", worker)
				cCmd := exec.Command(bd, "close", id, "--reason", reason)
				cCmd.Dir = dir
				cCmd.Env = bdEnv(dir)
				cOut, err := cCmd.CombinedOutput()
				if err != nil {
					r.err = fmt.Errorf("close %d: %v\n%s", i, err, cOut)
					results[worker] = r
					return
				}

				// List to verify consistency (interleaved with writes).
				listCmd := exec.Command(bd, "list", "--json", "--limit", "0", "--all")
				listCmd.Dir = dir
				listCmd.Env = bdEnv(dir)
				listStdout, listStderr, err := runCommandBuffers(t, listCmd)
				if err != nil {
					r.err = fmt.Errorf("list after close %d: %v\nstdout:\n%s\nstderr:\n%s", i, err, listStdout.String(), listStderr.String())
					results[worker] = r
					return
				}
				s := listStdout.String()
				start := strings.Index(s, "[")
				if start < 0 {
					r.listCounts = append(r.listCounts, 0)
					continue
				}
				var issues []json.RawMessage
				if jsonErr := json.Unmarshal([]byte(s[start:]), &issues); jsonErr != nil {
					r.err = fmt.Errorf("list parse %d: %v\nstdout:\n%s\nstderr:\n%s", i, jsonErr, s, listStderr.String())
					results[worker] = r
					return
				}
				r.listCounts = append(r.listCounts, len(issues))
			}

			results[worker] = r
		}(w)
	}
	wg.Wait()

	// Check for errors and collect IDs.
	allIDs := make(map[string]bool)
	var failures int
	for _, r := range results {
		if r.err != nil {
			if !strings.Contains(r.err.Error(), "one writer at a time") {
				t.Errorf("worker %d failed: %v", r.worker, r.err)
			}
			failures++
			continue
		}
		for _, id := range r.ids {
			if allIDs[id] {
				t.Errorf("duplicate ID %q from worker %d", id, r.worker)
			}
			allIDs[id] = true
		}
	}

	successes := numWorkers - failures
	if successes == 0 {
		t.Fatalf("all %d workers failed; expected at least 1 success", numWorkers)
	}
	t.Logf("%d/%d workers succeeded (flock contention expected)", successes, numWorkers)

	if len(allIDs) == 0 {
		t.Fatal("no IDs collected from successful workers")
	}

	// Verify issues from successful workers exist and are closed.
	store := openStore(t, beadsDir, "cx")
	for id := range allIDs {
		issue, err := store.GetIssue(t.Context(), id)
		if err != nil {
			t.Errorf("GetIssue(%s): %v", id, err)
			continue
		}
		if issue.Status != types.StatusClosed {
			t.Errorf("issue %s: expected status closed, got %s", id, issue.Status)
		}
		if issue.ClosedAt == nil {
			t.Errorf("issue %s: expected closed_at to be set", id)
		}
	}

	// Verify list counts were monotonically non-decreasing per worker.
	for _, r := range results {
		if r.err != nil {
			continue
		}
		for i := 1; i < len(r.listCounts); i++ {
			if r.listCounts[i] < r.listCounts[i-1] {
				t.Errorf("worker %d: list count decreased from %d to %d at step %d",
					r.worker, r.listCounts[i-1], r.listCounts[i], i)
			}
		}
	}

	stats, err := store.GetStatistics(t.Context())
	if err != nil {
		t.Fatalf("GetStatistics: %v", err)
	}

	t.Logf("created and closed %d issues across %d concurrent workers, %d in DB",
		len(allIDs), numWorkers, stats.TotalIssues)
}
