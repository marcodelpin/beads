package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseValidFor(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"1w", 7 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"1y", 365 * 24 * time.Hour, false},
		{"72h", 72 * time.Hour, false},
		{"15m", 15 * time.Minute, false},
		{"30s", 30 * time.Second, false},
		{"", 0, true},
		{"abc", 0, true},
		{"0d", 0, true},
		{"-1d", 0, true},
	}
	for _, c := range cases {
		got, err := parseValidFor(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseValidFor(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseValidFor(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseValidFor(%q): got %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseValidUntil(t *testing.T) {
	t.Run("date_only", func(t *testing.T) {
		got, err := parseValidUntil("2026-12-31")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Year() != 2026 || got.Month() != 12 || got.Day() != 31 {
			t.Errorf("date mismatch: %v", got)
		}
		if got.Location() != time.UTC {
			t.Errorf("expected UTC, got %v", got.Location())
		}
	})
	t.Run("rfc3339", func(t *testing.T) {
		got, err := parseValidUntil("2026-12-31T15:04:05Z")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Hour() != 15 || got.Minute() != 4 {
			t.Errorf("time mismatch: %v", got)
		}
	})
	t.Run("bad_input", func(t *testing.T) {
		if _, err := parseValidUntil("not-a-date"); err == nil {
			t.Error("expected error for bad input")
		}
		if _, err := parseValidUntil(""); err == nil {
			t.Error("expected error for empty input")
		}
	})
}

func TestValidatePolicy(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{"hide", false},
		{"notify", false},
		{"delete", false},
		{"HIDE", true}, // case-sensitive
		{"bogus", true},
	}
	for _, c := range cases {
		err := validatePolicy(c.in)
		if c.wantErr && err == nil {
			t.Errorf("validatePolicy(%q): expected error", c.in)
		}
		if !c.wantErr && err != nil {
			t.Errorf("validatePolicy(%q): unexpected error %v", c.in, err)
		}
	}
}

func TestBuildMemoryEnvelope(t *testing.T) {
	now := time.Date(2026, 4, 8, 9, 0, 0, 0, time.UTC)

	t.Run("valid_for", func(t *testing.T) {
		raw, err := buildMemoryEnvelope("hello world", now, 30*24*time.Hour, time.Time{}, "hide", memoryProvenance{}, nil, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var env memoryEnvelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if env.Version != envelopeVersion {
			t.Errorf("version: got %d, want %d", env.Version, envelopeVersion)
		}
		if env.Content != "hello world" {
			t.Errorf("content: got %q", env.Content)
		}
		if env.ValidUntil != "2026-05-08T09:00:00Z" {
			t.Errorf("valid_until: got %q", env.ValidUntil)
		}
		if env.ExpirePolicy != "hide" {
			t.Errorf("policy: got %q", env.ExpirePolicy)
		}
	})

	t.Run("valid_until_absolute", func(t *testing.T) {
		target := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
		raw, err := buildMemoryEnvelope("fact", now, 0, target, "notify", memoryProvenance{}, nil, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		env := parseStoredMemory(raw)
		if env.ValidUntil != "2026-12-31T00:00:00Z" {
			t.Errorf("valid_until: got %q", env.ValidUntil)
		}
		if env.effectivePolicy() != "notify" {
			t.Errorf("policy: got %q", env.effectivePolicy())
		}
	})

	t.Run("conflict_both_flags", func(t *testing.T) {
		_, err := buildMemoryEnvelope("x", now, 1*time.Hour, now.Add(24*time.Hour), "", memoryProvenance{}, nil, "")
		if err == nil {
			t.Error("expected error when both validFor and validUntil set")
		}
	})

	t.Run("bad_policy", func(t *testing.T) {
		_, err := buildMemoryEnvelope("x", now, 0, time.Time{}, "nuke", memoryProvenance{}, nil, "")
		if err == nil {
			t.Error("expected error for bad policy")
		}
	})

	t.Run("no_validity_still_envelopes", func(t *testing.T) {
		// If caller passes no validity at all but a policy is set, the
		// envelope must still be serializable.
		raw, err := buildMemoryEnvelope("fact", now, 0, time.Time{}, "hide", memoryProvenance{}, nil, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		env := parseStoredMemory(raw)
		if env.Content != "fact" {
			t.Errorf("content: got %q", env.Content)
		}
		if env.ValidUntil != "" {
			t.Errorf("valid_until should be empty, got %q", env.ValidUntil)
		}
	})
}

func TestParseStoredMemoryLegacy(t *testing.T) {
	t.Run("plain_text_legacy", func(t *testing.T) {
		env := parseStoredMemory("always run tests with -race flag")
		if env.Version != 0 {
			t.Errorf("legacy should have version 0, got %d", env.Version)
		}
		if env.Content != "always run tests with -race flag" {
			t.Errorf("content: got %q", env.Content)
		}
		if env.isExpired(time.Now()) {
			t.Error("legacy value should never be expired")
		}
	})

	t.Run("user_json_without_version_tag", func(t *testing.T) {
		// A user who wrote {"foo": "bar"} as their memory must get it back
		// verbatim, not parsed as an envelope.
		raw := `{"foo": "bar"}`
		env := parseStoredMemory(raw)
		if env.Content != raw {
			t.Errorf("expected verbatim, got %q", env.Content)
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		raw := `{not valid json`
		env := parseStoredMemory(raw)
		if env.Content != raw {
			t.Errorf("expected verbatim, got %q", env.Content)
		}
	})

	t.Run("valid_envelope_roundtrip", func(t *testing.T) {
		now := time.Date(2026, 4, 8, 9, 0, 0, 0, time.UTC)
		raw, err := buildMemoryEnvelope("insight", now, 24*time.Hour, time.Time{}, "delete", memoryProvenance{}, nil, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		env := parseStoredMemory(raw)
		if env.Version != envelopeVersion {
			t.Errorf("version: got %d", env.Version)
		}
		if env.Content != "insight" {
			t.Errorf("content: got %q", env.Content)
		}
		if env.effectivePolicy() != "delete" {
			t.Errorf("policy: got %q", env.effectivePolicy())
		}
	})
}

func TestIsExpired(t *testing.T) {
	now := time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC)

	t.Run("no_valid_until_never_expires", func(t *testing.T) {
		env := memoryEnvelope{Version: envelopeVersion, Content: "x"}
		if env.isExpired(now) {
			t.Error("memory without valid_until should never expire")
		}
	})

	t.Run("future_not_expired", func(t *testing.T) {
		env := memoryEnvelope{
			Version:    envelopeVersion,
			Content:    "x",
			ValidUntil: now.Add(1 * time.Hour).Format(time.RFC3339),
		}
		if env.isExpired(now) {
			t.Error("future valid_until should not be expired")
		}
	})

	t.Run("past_expired", func(t *testing.T) {
		env := memoryEnvelope{
			Version:    envelopeVersion,
			Content:    "x",
			ValidUntil: now.Add(-1 * time.Hour).Format(time.RFC3339),
		}
		if !env.isExpired(now) {
			t.Error("past valid_until should be expired")
		}
	})

	t.Run("corrupted_fail_open", func(t *testing.T) {
		// Corrupted timestamp: caller should not lose the memory silently.
		env := memoryEnvelope{
			Version:    envelopeVersion,
			Content:    "x",
			ValidUntil: "not-a-timestamp",
		}
		if env.isExpired(now) {
			t.Error("corrupted timestamp should fail open (not expired)")
		}
	})
}

func TestEffectivePolicy(t *testing.T) {
	cases := map[string]string{
		"":       policyHide,
		"hide":   policyHide,
		"notify": policyNotify,
		"delete": policyDelete,
	}
	for in, want := range cases {
		env := memoryEnvelope{ExpirePolicy: in}
		if got := env.effectivePolicy(); got != want {
			t.Errorf("effectivePolicy(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestEnvelopeEndToEndSerialization(t *testing.T) {
	// Build an envelope, serialize, parse back, check round-trip fidelity.
	now := time.Date(2026, 6, 15, 10, 30, 0, 0, time.UTC)
	raw, err := buildMemoryEnvelope(
		"multi\nline\ncontent with \"quotes\" and /slashes/",
		now,
		0,
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		"notify",
		memoryProvenance{},
		nil,
		"",
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(raw, `"_bd_mem":1`) {
		t.Errorf("serialized envelope missing version tag: %s", raw)
	}
	env := parseStoredMemory(raw)
	if env.Content != "multi\nline\ncontent with \"quotes\" and /slashes/" {
		t.Errorf("round-trip content lost: %q", env.Content)
	}
	if env.ValidUntil != "2027-01-01T00:00:00Z" {
		t.Errorf("valid_until: %q", env.ValidUntil)
	}
	if env.CreatedAt != "2026-06-15T10:30:00Z" {
		t.Errorf("created_at: %q", env.CreatedAt)
	}
	if env.effectivePolicy() != policyNotify {
		t.Errorf("policy: %q", env.effectivePolicy())
	}
}
