package main

import (
	"slices"
	"testing"
)

// Pure-function tests for `bd group` helpers — no store/dolt needed, run in every CI.
// Store-level behavior (park→hide-from-ready→release→unhide, idempotent re-create,
// gate-deferred, no `bd gate` collision) is covered by the E2E in tests/group-e2e.sh.

func TestParseGroupMembers(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"comma", "bd-a,bd-b,bd-c", []string{"bd-a", "bd-b", "bd-c"}},
		{"space", "bd-a bd-b bd-c", []string{"bd-a", "bd-b", "bd-c"}},
		{"mixed", "bd-a, bd-b,bd-c  bd-d", []string{"bd-a", "bd-b", "bd-c", "bd-d"}},
		{"newlines", "bd-a\nbd-b\r\nbd-c\n", []string{"bd-a", "bd-b", "bd-c"}},
		{"trailing+empty-fields", "bd-a,,bd-b, ,", []string{"bd-a", "bd-b"}},
		{"single", "bd-a", []string{"bd-a"}},
		{"empty", "", nil},
		{"whitespace-only", "   \n\t ", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseGroupMembers(c.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("parseGroupMembers(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestGroupGateLabels(t *testing.T) {
	got := groupGateLabels("dvr")
	want := []string{"meta-gate", "focus:dvr"}
	if !slices.Equal(got, want) {
		t.Errorf("groupGateLabels(dvr) = %v, want %v", got, want)
	}
	// Exact-equality matters: the focus label for "test" must differ from "test2"
	// so SearchIssues (AND exact-match) never cross-matches gates.
	if slices.Contains(groupGateLabels("test"), "focus:test2") {
		t.Error("focus:test labels must not contain focus:test2")
	}
	if groupGateLabels("test")[1] == groupGateLabels("test2")[1] {
		t.Error("focus labels for distinct focuses must differ")
	}
}

func TestGroupParkedLabel(t *testing.T) {
	if got := groupParkedLabel("dvr"); got != "focus:dvr-parked" {
		t.Errorf("groupParkedLabel(dvr) = %q, want focus:dvr-parked", got)
	}
	// Distinct focuses → distinct parked labels (no cross-focus release leakage).
	if groupParkedLabel("a") == groupParkedLabel("ab") {
		t.Error("parked labels for distinct focuses must differ")
	}
}
