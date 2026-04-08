package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Memory validity windows (mempalace pattern).
//
// Memories are stored as opaque strings in the KV config store. To add validity
// windows without a schema migration, new memories are stored as a JSON
// envelope that embeds the raw content plus validity metadata. Legacy
// plain-text values (written before this feature) are still recognized and
// treated as non-expiring.
//
// Envelope format (envelopeVersion = 1):
//
//	{
//	  "_bd_mem": 1,
//	  "content": "<user insight>",
//	  "created_at": "2026-04-08T09:00:00Z",
//	  "valid_until": "2026-05-08T09:00:00Z",
//	  "expire_policy": "hide"
//	}
//
// Fields:
//   - _bd_mem: version tag, distinguishes envelopes from legacy plain text
//   - content: the actual memory text the user stored
//   - created_at: wall-clock UTC timestamp when the memory was written
//   - valid_until: UTC timestamp after which the memory is "expired"; empty
//     string means no expiration (equivalent to legacy behaviour)
//   - expire_policy: what to do with expired memories on default queries:
//     "hide"   (default) — filtered from `bd memories` unless --include-expired
//     "notify" — shown in `bd memories` but marked EXPIRED
//     "delete" — filtered from `bd memories`, removed by `bd memories --gc`
const envelopeVersion = 1

// expirePolicy values.
const (
	policyHide   = "hide"
	policyNotify = "notify"
	policyDelete = "delete"
)

// memoryEnvelope is the on-disk shape for a memory with validity metadata.
// JSON field names are stable across versions; new optional fields may be
// added in future versions but must remain backward-compatible with v1.
type memoryEnvelope struct {
	Version      int    `json:"_bd_mem"`
	Content      string `json:"content"`
	CreatedAt    string `json:"created_at,omitempty"`
	ValidUntil   string `json:"valid_until,omitempty"`
	ExpirePolicy string `json:"expire_policy,omitempty"`
}

// validPolicies is the set of accepted expire-policy strings.
var validPolicies = map[string]bool{
	policyHide:   true,
	policyNotify: true,
	policyDelete: true,
}

// durationRe parses "<number><unit>" with units s/m/h/d/w/y.
// Examples: "30d", "1w", "2y", "12h".
var durationRe = regexp.MustCompile(`^(\d+)([smhdwy])$`)

// parseValidFor parses a short duration string like "30d", "2w", "1y" and
// returns the equivalent time.Duration. Supported units:
//
//	s seconds, m minutes, h hours, d days, w weeks, y years
//
// This mirrors the style used by `--valid-for=30d` in the CLI. Standard Go
// duration strings ("72h", "15m") are also accepted via time.ParseDuration as
// a fallback.
func parseValidFor(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("duration cannot be empty")
	}

	if m := durationRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, fmt.Errorf("invalid duration number %q: %w", m[1], err)
		}
		if n <= 0 {
			return 0, fmt.Errorf("duration must be positive, got %q", s)
		}
		switch m[2] {
		case "s":
			return time.Duration(n) * time.Second, nil
		case "m":
			return time.Duration(n) * time.Minute, nil
		case "h":
			return time.Duration(n) * time.Hour, nil
		case "d":
			return time.Duration(n) * 24 * time.Hour, nil
		case "w":
			return time.Duration(n) * 7 * 24 * time.Hour, nil
		case "y":
			return time.Duration(n) * 365 * 24 * time.Hour, nil
		}
	}

	// Fall back to Go's native duration parsing for expressions like "72h".
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: expected e.g. 30d, 2w, 1y, 72h", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive, got %q", s)
	}
	return d, nil
}

// parseValidUntil parses an absolute expiration date. Accepts:
//
//	2026-12-31                 (date-only, interpreted as UTC midnight)
//	2026-12-31T15:04:05Z       (RFC3339 UTC)
//	2026-12-31T15:04:05+02:00  (RFC3339 with offset)
//
// Returns the resulting time in UTC.
func parseValidUntil(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("valid-until cannot be empty")
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid valid-until %q: expected YYYY-MM-DD or RFC3339", s)
}

// validatePolicy returns an error if p is not a recognized expire policy.
// An empty string is allowed and means "no policy set" (same as hide by
// default semantics at query time).
func validatePolicy(p string) error {
	if p == "" {
		return nil
	}
	if !validPolicies[p] {
		return fmt.Errorf("invalid expire-policy %q: must be one of hide, notify, delete", p)
	}
	return nil
}

// buildMemoryEnvelope constructs and serializes a memory envelope. It is used
// by `bd remember` when any validity flag is set (or, optionally, always — we
// only create envelopes when needed so legacy values stay plain text).
//
// now is injected to make tests deterministic. Callers should pass time.Now()
// in production code.
func buildMemoryEnvelope(content string, now time.Time, validFor time.Duration, validUntil time.Time, policy string) (string, error) {
	if err := validatePolicy(policy); err != nil {
		return "", err
	}
	env := memoryEnvelope{
		Version:      envelopeVersion,
		Content:      content,
		CreatedAt:    now.UTC().Format(time.RFC3339),
		ExpirePolicy: policy,
	}
	switch {
	case validFor > 0 && !validUntil.IsZero():
		return "", fmt.Errorf("specify either --valid-for or --valid-until, not both")
	case validFor > 0:
		env.ValidUntil = now.UTC().Add(validFor).Format(time.RFC3339)
	case !validUntil.IsZero():
		env.ValidUntil = validUntil.UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("marshalling memory envelope: %w", err)
	}
	return string(data), nil
}

// parseStoredMemory parses a stored memory value. Legacy plain-text values
// return an envelope with only Content set (no validity). JSON envelopes are
// decoded and returned with all metadata populated. Any decode error falls
// back to treating the value as opaque plain text, which is the safe default.
func parseStoredMemory(raw string) memoryEnvelope {
	trimmed := strings.TrimSpace(raw)
	// Fast path: only values starting with "{" can be JSON envelopes. Anything
	// else is legacy plain text and must be returned verbatim.
	if !strings.HasPrefix(trimmed, "{") {
		return memoryEnvelope{Content: raw}
	}

	var env memoryEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		// Not a valid envelope; treat as opaque text.
		return memoryEnvelope{Content: raw}
	}
	// A stored envelope must carry the version tag. Without it we are looking
	// at arbitrary JSON that happens to decode into our struct (e.g. a user
	// who happened to store a JSON object as their memory). Fall back to
	// treating it as plain text.
	if env.Version != envelopeVersion {
		return memoryEnvelope{Content: raw}
	}
	return env
}

// isExpired returns true if the envelope has a valid_until timestamp that is
// in the past relative to now. Envelopes without a valid_until are never
// considered expired.
func (e memoryEnvelope) isExpired(now time.Time) bool {
	if e.ValidUntil == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, e.ValidUntil)
	if err != nil {
		// Corrupted timestamp: fail open (not expired) so the user keeps
		// access to the content instead of losing it silently.
		return false
	}
	return now.After(t)
}

// effectivePolicy returns the expire policy for an envelope. Empty policy
// falls back to policyHide to match the default documented behavior.
func (e memoryEnvelope) effectivePolicy() string {
	if e.ExpirePolicy == "" {
		return policyHide
	}
	return e.ExpirePolicy
}
