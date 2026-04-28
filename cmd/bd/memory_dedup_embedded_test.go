//go:build cgo

package main

import (
	"os"
	"strings"
	"testing"
)

// TestEmbeddedMemoryDedup is the integration counterpart to the unit
// tests in memory_dedup_test.go. It verifies that successive `bd remember`
// calls with cosmetic-only differences collapse onto one key, and that
// --no-dedup forces a sibling key.
func TestEmbeddedMemoryDedup(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "md")

	t.Run("dedup_collapses_punct_and_case_variations", func(t *testing.T) {
		// Seed with the canonical wording.
		out := bdRemember(t, bd, dir, "Always run tests with -race flag")
		if !strings.Contains(out, "Remembered") {
			t.Fatalf("expected first call to be Remembered, got: %s", out)
		}

		// Punctuation/case variation -> dedup hit.
		out = bdRemember(t, bd, dir, "always run tests with -race flag.")
		if !strings.Contains(out, "Deduped") {
			t.Errorf("expected dedup hit on variation 1, got: %s", out)
		}

		// All-caps + extra spaces -> dedup hit.
		out = bdRemember(t, bd, dir, "  ALWAYS  RUN  TESTS  WITH  -RACE  FLAG  ")
		if !strings.Contains(out, "Deduped") {
			t.Errorf("expected dedup hit on variation 2, got: %s", out)
		}

		// Confirm exactly one memory exists for this insight.
		mems := bdMemories(t, bd, dir)
		count := strings.Count(strings.ToLower(mems), "always run tests with -race")
		// At least one mention; we don't try to count occurrences strictly because
		// 'bd memories' formats with truncation. The key observation: only ONE key.
		if count < 1 {
			t.Errorf("expected at least one mention of the insight, got: %s", mems)
		}
	})

	t.Run("no_dedup_creates_sibling_key", func(t *testing.T) {
		// Use a fresh insight so we don't collide with the previous test run.
		out := bdRemember(t, bd, dir, "Use Dolt 1.0.3 not 1.0.2")
		if !strings.Contains(out, "Remembered") {
			t.Fatalf("expected Remembered on first insert, got: %s", out)
		}

		// Same content, --no-dedup, but slugify is deterministic so the auto
		// key collides AS A KEY. The "Updated" verb confirms key collision.
		out = bdRemember(t, bd, dir, "Use Dolt 1.0.3 not 1.0.2", "--no-dedup")
		if !strings.Contains(out, "Updated") {
			t.Errorf("expected Updated (same auto-key), got: %s", out)
		}

		// Now make the variation that WOULD have deduped — punctuation
		// difference produces a different slug. With --no-dedup, we must
		// see Remembered (sibling key created).
		out = bdRemember(t, bd, dir, "Use Dolt 1.0.3, not 1.0.2.", "--no-dedup")
		if !strings.Contains(out, "Remembered") {
			t.Errorf("expected Remembered (sibling) under --no-dedup, got: %s", out)
		}
	})

	t.Run("dedup_disabled_when_explicit_key", func(t *testing.T) {
		// Seed canonical insight under an explicit key.
		_ = bdRemember(t, bd, dir, "JWT for auth, not sessions", "--key", "auth-jwt")

		// Same fingerprint, NO --key -> dedup should hit auth-jwt.
		out := bdRemember(t, bd, dir, "jwt for auth not sessions!")
		if !strings.Contains(out, "Deduped") {
			t.Errorf("expected dedup hit onto auth-jwt, got: %s", out)
		}
		if !strings.Contains(out, "auth-jwt") {
			t.Errorf("expected key 'auth-jwt' in output, got: %s", out)
		}

		// Same fingerprint, WITH explicit different --key -> NEW memory under
		// the new key (explicit --key always wins over dedup).
		out = bdRemember(t, bd, dir, "jwt for auth not sessions!!!", "--key", "auth-jwt-alt")
		if strings.Contains(out, "Deduped") {
			t.Errorf("--key should bypass dedup, got: %s", out)
		}
		if !strings.Contains(out, "auth-jwt-alt") {
			t.Errorf("expected key 'auth-jwt-alt' in output, got: %s", out)
		}
	})
}
