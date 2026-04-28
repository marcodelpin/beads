//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestEmbeddedMemoryValidityWindows exercises the fact-validity-window CLI
// surface end-to-end through a built bd binary. Each sub-test targets a
// specific behaviour from the design doc at docs/design/bd-fact-validity-windows.md.
func TestEmbeddedMemoryValidityWindows(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "tv")

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bd, args...)
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bd %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	runExpectFail := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bd, args...)
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("bd %s should have failed:\n%s", strings.Join(args, " "), out)
		}
		return string(out)
	}

	// stripBootstrap removes auto-import / dolt-commit chatter that the embedded
	// Dolt runtime writes to stdout on first access to an empty DB. Callers that
	// compare full output (exact string, or JSON parse) must drop this preamble.
	stripBootstrap := func(s string) string {
		var kept []string
		for _, line := range strings.Split(s, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "auto-importing ") ||
				strings.HasPrefix(t, "warning: auto-import:") ||
				strings.Contains(t, "Error 1105: nothing to commit") {
				continue
			}
			kept = append(kept, line)
		}
		return strings.Join(kept, "\n")
	}

	// ---- Relative validity via --valid-for ----

	t.Run("remember_valid_for_visible_before_expiry", func(t *testing.T) {
		run("remember", "future fact", "--key", "future-fact", "--valid-for", "1h")
		// A fresh --valid-for=1h memory must appear in `bd memories` with no
		// --include-expired flag because it has not yet expired.
		out := run("memories", "future-fact")
		if !strings.Contains(out, "future-fact") {
			t.Errorf("expected fresh memory in listing, got: %s", out)
		}
		if !strings.Contains(out, "valid until") {
			t.Errorf("expected [valid until ...] marker, got: %s", out)
		}
	})

	t.Run("remember_valid_for_expired_default_hidden", func(t *testing.T) {
		// Store a memory that's already expired by using --valid-for=1s and
		// sleeping past it. 1s is the smallest accepted positive duration so
		// this keeps the test fast.
		run("remember", "stale fact", "--key", "stale-default", "--valid-for", "1s")
		time.Sleep(1500 * time.Millisecond)
		out := run("memories")
		if strings.Contains(out, "stale-default") {
			t.Errorf("expected stale-default to be hidden from default listing, got: %s", out)
		}
		// The hidden-count hint must be printed so the user knows memories
		// exist but are filtered out.
		if !strings.Contains(out, "expired hidden") {
			t.Errorf("expected 'expired hidden' hint in output, got: %s", out)
		}
	})

	t.Run("memories_include_expired_shows_all", func(t *testing.T) {
		out := run("memories", "--include-expired")
		if !strings.Contains(out, "stale-default") {
			t.Errorf("expected --include-expired to show stale-default: %s", out)
		}
		if !strings.Contains(out, "[EXPIRED]") {
			t.Errorf("expected [EXPIRED] marker in include-expired listing: %s", out)
		}
	})

	// ---- Policy: notify (keep visible, marked EXPIRED) ----

	t.Run("policy_notify_visible_after_expiry", func(t *testing.T) {
		run("remember", "notify me", "--key", "notify-key",
			"--valid-for", "1s", "--expire-policy", "notify")
		time.Sleep(1500 * time.Millisecond)
		out := run("memories")
		if !strings.Contains(out, "notify-key") {
			t.Errorf("expected notify-key still visible: %s", out)
		}
		if !strings.Contains(out, "[EXPIRED]") {
			t.Errorf("expected [EXPIRED] marker: %s", out)
		}
	})

	// ---- Policy: delete + --gc ----

	t.Run("policy_delete_gc_removes", func(t *testing.T) {
		run("remember", "tmp workaround", "--key", "gc-me",
			"--valid-for", "1s", "--expire-policy", "delete")
		time.Sleep(1500 * time.Millisecond)

		// Before gc: --include-expired shows it.
		out := run("memories", "--include-expired")
		if !strings.Contains(out, "gc-me") {
			t.Fatalf("expected gc-me to be present before --gc: %s", out)
		}

		// Run gc.
		gcOut := run("memories", "--gc", "--include-expired")
		if !strings.Contains(gcOut, "Garbage-collected") {
			t.Errorf("expected gc summary line, got: %s", gcOut)
		}
		if !strings.Contains(gcOut, "gc-me") {
			t.Errorf("expected gc summary to list gc-me: %s", gcOut)
		}

		// After gc: the memory must be gone for good.
		after := run("memories", "--include-expired")
		if strings.Contains(after, "gc-me") {
			t.Errorf("expected gc-me to be removed after --gc, still present: %s", after)
		}
	})

	// ---- Flag validation ----

	t.Run("reject_both_valid_for_and_valid_until", func(t *testing.T) {
		out := runExpectFail("remember", "x", "--key", "conflict",
			"--valid-for", "1d", "--valid-until", "2026-12-31")
		if !strings.Contains(out, "not both") && !strings.Contains(out, "valid-for") {
			t.Errorf("expected conflict error, got: %s", out)
		}
	})

	t.Run("reject_bad_policy", func(t *testing.T) {
		out := runExpectFail("remember", "x", "--key", "badpolicy",
			"--expire-policy", "nuke")
		if !strings.Contains(out, "invalid") && !strings.Contains(out, "policy") {
			t.Errorf("expected bad-policy error, got: %s", out)
		}
	})

	t.Run("reject_bad_duration", func(t *testing.T) {
		out := runExpectFail("remember", "x", "--key", "baddur",
			"--valid-for", "banana")
		if !strings.Contains(out, "invalid") {
			t.Errorf("expected bad-duration error, got: %s", out)
		}
	})

	// ---- Legacy plain-text round trip ----

	t.Run("legacy_plaintext_never_expires", func(t *testing.T) {
		// A memory stored without any validity flag must remain visible
		// forever and have no EXPIRED marker.
		run("remember", "no-ttl memory", "--key", "legacy-key")
		out := run("memories", "legacy-key")
		if !strings.Contains(out, "legacy-key") {
			t.Fatalf("expected legacy-key in output: %s", out)
		}
		if strings.Contains(out, "[EXPIRED]") {
			t.Errorf("legacy memory should never be marked expired: %s", out)
		}
		if strings.Contains(out, "valid until") {
			t.Errorf("legacy memory should not have valid-until marker: %s", out)
		}
	})

	// ---- recall returns the user content, not the JSON envelope ----

	t.Run("recall_returns_content_not_envelope", func(t *testing.T) {
		run("remember", "my insight", "--key", "recall-key", "--valid-for", "1h")
		out := run("recall", "recall-key")
		out = strings.TrimSpace(stripBootstrap(out))
		if out != "my insight" {
			t.Errorf("expected plain content, got %q", out)
		}
	})

	// ---- JSON output exposes validity metadata ----

	t.Run("json_output_includes_metadata", func(t *testing.T) {
		run("remember", "jsoninsight", "--key", "json-key", "--valid-for", "2d",
			"--expire-policy", "notify")
		out := stripBootstrap(run("memories", "--json", "json-key"))
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("failed to parse JSON output %q: %v", out, err)
		}
		mems, ok := parsed["memories"].([]interface{})
		if !ok || len(mems) == 0 {
			t.Fatalf("expected memories array, got: %v", parsed)
		}
		entry, ok := mems[0].(map[string]interface{})
		if !ok {
			t.Fatalf("expected object entry, got: %v", mems[0])
		}
		if entry["key"] != "json-key" {
			t.Errorf("key: got %v", entry["key"])
		}
		if entry["valid_until"] == nil || entry["valid_until"] == "" {
			t.Errorf("valid_until missing from JSON output: %v", entry)
		}
		if entry["expire_policy"] != "notify" {
			t.Errorf("expire_policy: got %v", entry["expire_policy"])
		}
	})
}
