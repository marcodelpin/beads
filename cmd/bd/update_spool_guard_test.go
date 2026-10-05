package main

import (
	"testing"

	"github.com/steveyegge/beads/issueops"
)

// TestUpdateIsGuarded pins which updates bypass the offline write-spool: each
// precondition on its own makes the update guarded, and only a bare field
// update stays spoolable. A replayed spool entry carries no guard, so a guard
// that slipped through here would be dropped silently on replay.
func TestUpdateIsGuarded(t *testing.T) {
	assignee := "alice"
	status := issueops.Status("open")
	revision := int64(7)
	cases := []struct {
		name       string
		claim      bool
		ifAssignee *string
		status     *issueops.Status
		revision   *int64
		want       bool
	}{
		{name: "bare field update", want: false},
		{name: "claim", claim: true, want: true},
		{name: "if-assignee", ifAssignee: &assignee, want: true},
		{name: "if-status", status: &status, want: true},
		{name: "if-revision", revision: &revision, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := updateIsGuarded(tc.claim, tc.ifAssignee, tc.status, tc.revision); got != tc.want {
				t.Fatalf("updateIsGuarded = %v, want %v", got, tc.want)
			}
		})
	}
}
