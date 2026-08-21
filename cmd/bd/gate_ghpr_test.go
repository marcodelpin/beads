package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGHPRJSONFields_NoMergedField is the regression guard for GH#3411.
//
// The gh CLI v2.89 removed the `merged` boolean from `gh pr view --json`.
// Any re-introduction of "merged" here would reproduce the hard failure:
//
//	Unknown JSON field: "merged"
//
// Merge state must be derived from state == "MERGED" instead.
func TestGHPRJSONFields_NoMergedField(t *testing.T) {
	if ghPRJSONFields == "" {
		t.Fatal("ghPRJSONFields is empty")
	}
	for _, field := range strings.Split(ghPRJSONFields, ",") {
		if strings.TrimSpace(field) == "merged" {
			t.Fatalf("ghPRJSONFields includes removed gh CLI field %q (regression of GH#3411); use state instead", "merged")
		}
	}
	// Sanity: state must still be present; we derive resolution from it.
	if !strings.Contains(ghPRJSONFields, "state") {
		t.Fatalf("ghPRJSONFields missing required 'state' field: %q", ghPRJSONFields)
	}
}

// TestGHPRStatus_UnmarshalsModernGHOutput verifies the struct accepts the
// shape produced by gh CLI v2.89+ (no `merged` field). Ignoring unknown
// fields is the default json.Unmarshal behavior, so this also protects
// against future gh output additions.
func TestGHPRStatus_UnmarshalsModernGHOutput(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantState string
		wantTitle string
	}{
		{
			name:      "merged PR",
			payload:   `{"state":"MERGED","title":"feat: add widget"}`,
			wantState: "MERGED",
			wantTitle: "feat: add widget",
		},
		{
			name:      "closed without merge",
			payload:   `{"state":"CLOSED","title":"abandoned"}`,
			wantState: "CLOSED",
			wantTitle: "abandoned",
		},
		{
			name:      "open PR",
			payload:   `{"state":"OPEN","title":"wip: draft"}`,
			wantState: "OPEN",
			wantTitle: "wip: draft",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var status ghPRStatus
			if err := json.Unmarshal([]byte(tc.payload), &status); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if status.State != tc.wantState {
				t.Errorf("State = %q, want %q", status.State, tc.wantState)
			}
			if status.Title != tc.wantTitle {
				t.Errorf("Title = %q, want %q", status.Title, tc.wantTitle)
			}
		})
	}
}
