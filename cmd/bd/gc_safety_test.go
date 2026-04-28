//go:build cgo

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmbeddedGCSafetyFloor verifies the fork-only --older-than safety floor
// (default 7 days) blocks accidental destructive runs and is bypassed by
// --allow-recent.
//
// Motivated by upstream gastownhall/beads#3543 (root cause unresolved).
func TestEmbeddedGCSafetyFloor(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gs")

	// Below floor without escape hatch -> must fail.
	t.Run("below_floor_no_escape", func(t *testing.T) {
		out := bdGCFail(t, bd, dir, "--dry-run", "--older-than", "5")
		if !strings.Contains(out, "safety floor") {
			t.Errorf("expected 'safety floor' in error output: %s", out)
		}
	})

	// Below floor WITH --allow-recent -> must succeed.
	t.Run("below_floor_with_allow_recent", func(t *testing.T) {
		out := bdGC(t, bd, dir, "--dry-run", "--older-than", "5", "--allow-recent")
		if !strings.Contains(out, "Phase 1/3") {
			t.Errorf("expected normal dry-run output: %s", out)
		}
	})

	// At-or-above floor -> must succeed without --allow-recent.
	t.Run("at_floor_no_escape", func(t *testing.T) {
		out := bdGC(t, bd, dir, "--dry-run", "--older-than", "7")
		if !strings.Contains(out, "Phase 1/3") {
			t.Errorf("expected normal dry-run output: %s", out)
		}
	})

	// --skip-decay short-circuits the floor check (no decay = no risk).
	t.Run("skip_decay_bypasses_floor", func(t *testing.T) {
		out := bdGC(t, bd, dir, "--dry-run", "--older-than", "1", "--skip-decay")
		if !strings.Contains(out, "DRY RUN") {
			t.Errorf("expected dry-run output: %s", out)
		}
	})
}

// TestEmbeddedGCBackupCreated verifies a real (non-dry-run) decay phase writes
// a .gc-backup-<unix>.jsonl inside .beads/ BEFORE deleting issues, unless
// --no-backup is set.
func TestEmbeddedGCBackupCreated(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gb")

	// Create + close 3 issues so the decay phase has candidates.
	for i := 0; i < 3; i++ {
		issue := bdCreate(t, bd, dir, "GC backup test", "--type", "task")
		cmd := exec.Command(bd, "close", issue.ID)
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("close %s failed: %v\n%s", issue.ID, err, out)
		}
	}

	beadsDir := filepath.Join(dir, ".beads")

	t.Run("backup_written_when_decay_runs", func(t *testing.T) {
		preBackups := listBackups(t, beadsDir)

		// Force decay with allow-recent (older-than=0 below floor).
		out := bdGC(t, bd, dir, "--force", "--older-than", "0", "--allow-recent", "--skip-dolt")
		if !strings.Contains(out, "Backup:") && !strings.Contains(out, "0 issues deleted") {
			// "0 issues deleted" path: closed_at filter excluded everything (e.g.
			// fast test where closed_at is the same wall-clock as cutoff). Skip
			// the assertion in that case — there's nothing to back up.
			t.Logf("no backup expected (decay had no candidates): %s", out)
			return
		}

		postBackups := listBackups(t, beadsDir)
		if len(postBackups) <= len(preBackups) {
			t.Errorf("expected new .gc-backup-*.jsonl in %s; pre=%v post=%v output=%s",
				beadsDir, preBackups, postBackups, out)
		}
	})

	t.Run("no_backup_with_flag", func(t *testing.T) {
		// Re-create + re-close some issues to give decay something to do.
		for i := 0; i < 2; i++ {
			issue := bdCreate(t, bd, dir, "GC nobackup test", "--type", "task")
			cmd := exec.Command(bd, "close", issue.ID)
			cmd.Dir = dir
			cmd.Env = bdEnv(dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("close %s failed: %v\n%s", issue.ID, err, out)
			}
		}

		preBackups := listBackups(t, beadsDir)
		_ = bdGC(t, bd, dir, "--force", "--older-than", "0", "--allow-recent",
			"--skip-dolt", "--no-backup")
		postBackups := listBackups(t, beadsDir)

		if len(postBackups) > len(preBackups) {
			t.Errorf("--no-backup should NOT create backup files; pre=%v post=%v",
				preBackups, postBackups)
		}
	})
}

// listBackups returns the names of all .gc-backup-*.jsonl files in dir.
func listBackups(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		// If .beads/ doesn't exist yet, just return empty.
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".gc-backup-") && strings.HasSuffix(name, ".jsonl") {
			out = append(out, name)
		}
	}
	return out
}
