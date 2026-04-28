package main

import (
	"testing"
)

// TestMemoryFingerprint covers the normalization rules that drive
// auto-key dedup on bd remember (fork-only).
func TestMemoryFingerprint(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hello", "hello"},
		{"Hello World", "hello world"},
		{"Hello, World!", "hello world"},
		{"  multiple    spaces  ", "multiple spaces"},
		{"Tabs\tand\nnewlines\r\nmix", "tabs and newlines mix"},
		{"", ""},
		{"Always run tests with -race flag", "always run tests with race flag"},
		{"always run tests with -race flag.", "always run tests with race flag"},
		{"!!!ALWAYS run tests, with -race FLAG!!!", "always run tests with race flag"},
		{"Use Dolt 1.0.3 not 1.0.2", "use dolt 103 not 102"},
		// Punctuation-only or whitespace-only collapses to empty.
		{"!!!", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		got := memoryFingerprint(c.in)
		if got != c.want {
			t.Errorf("memoryFingerprint(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// TestMemoryFingerprintEqualOnVariations confirms common cosmetic
// variations of the same insight collapse to one fingerprint.
func TestMemoryFingerprintEqualOnVariations(t *testing.T) {
	canonical := memoryFingerprint("Always run tests with -race flag")
	variants := []string{
		"always run tests with -race flag",
		"Always run tests with -race flag.",
		"ALWAYS RUN TESTS WITH -RACE FLAG",
		"  Always   run  tests  with  -race  flag  ",
		"Always run tests with -race flag!",
	}
	for _, v := range variants {
		if got := memoryFingerprint(v); got != canonical {
			t.Errorf("variant %q -> %q (want %q)", v, got, canonical)
		}
	}
}

// TestFindDuplicateMemoryKey verifies the dedup scan against a
// synthetic config map with a mix of plain-text and envelope-wrapped
// memories.
func TestFindDuplicateMemoryKey(t *testing.T) {
	// Build a config map matching what store.GetAllConfig would return.
	full := func(k string) string { return kvPrefix + memoryPrefix + k }
	all := map[string]string{
		"unrelated.config":    "ignored",
		full("first-fact"):    "Always run tests with -race flag",
		full("second-fact"):   "Use Dolt 1.0.3 not 1.0.2 (regression in v1.0.3)",
		full("envelope-fact"): `{"_bd_mem":1,"content":"auth module uses JWT not sessions","created_at":"2026-04-28T07:00:00Z"}`,
	}

	t.Run("hits_plain_text", func(t *testing.T) {
		key, ok := findDuplicateMemoryKey(all, "always run tests with -race flag")
		if !ok || key != "first-fact" {
			t.Errorf("expected hit on first-fact, got key=%q ok=%v", key, ok)
		}
	})

	t.Run("hits_with_punctuation_variation", func(t *testing.T) {
		key, ok := findDuplicateMemoryKey(all, "Always run tests with -race flag.")
		if !ok || key != "first-fact" {
			t.Errorf("expected hit on first-fact (punct variation), got key=%q ok=%v", key, ok)
		}
	})

	t.Run("hits_envelope_via_unwrap", func(t *testing.T) {
		key, ok := findDuplicateMemoryKey(all, "Auth module uses JWT, not sessions!")
		if !ok || key != "envelope-fact" {
			t.Errorf("expected hit on envelope-fact via unwrap, got key=%q ok=%v", key, ok)
		}
	})

	t.Run("misses_unrelated", func(t *testing.T) {
		key, ok := findDuplicateMemoryKey(all, "Completely different fact about something else entirely")
		if ok {
			t.Errorf("expected no hit, got key=%q", key)
		}
	})

	t.Run("empty_insight_misses", func(t *testing.T) {
		key, ok := findDuplicateMemoryKey(all, "")
		if ok {
			t.Errorf("expected no hit on empty insight, got key=%q", key)
		}
	})

	t.Run("ignores_non_memory_keys", func(t *testing.T) {
		// "ignored" matches by value but is under unrelated.config (not memory).
		// Should be skipped by the prefix filter.
		key, ok := findDuplicateMemoryKey(all, "ignored")
		if ok {
			t.Errorf("expected no hit on non-memory key, got key=%q", key)
		}
	})
}
