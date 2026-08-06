package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

func TestOutputDeleteProxiedPreviewIsPayloadBlind(t *testing.T) {
	titleMarker := "PROXIED_OUTPUT_TITLE_MARKER"
	descriptionMarker := "PROXIED_OUTPUT_DESCRIPTION_MARKER"
	notesMarker := "PROXIED_OUTPUT_NOTES_MARKER"
	payloadMarker := "PROXIED_OUTPUT_PAYLOAD_MARKER"
	result := deletePreviewResult{
		preview: domain.DeletePreview{
			Issues: map[string]*types.Issue{
				"test-target": {
					ID:          "test-target",
					Title:       titleMarker,
					Description: descriptionMarker,
					Notes:       notesMarker,
					Metadata:    json.RawMessage(`{"marker":"` + payloadMarker + `"}`),
				},
			},
			ConnectedIssues: map[string]*types.Issue{
				"test-connected": {
					ID:          "test-connected",
					Title:       titleMarker,
					Description: descriptionMarker,
					Notes:       notesMarker,
					Metadata:    json.RawMessage(`{"marker":"` + payloadMarker + `"}`),
				},
			},
		},
		res: issueops.DeleteResult{Deleted: 1, Dependencies: 2},
	}
	markers := []string{titleMarker, descriptionMarker, notesMarker, payloadMarker}

	t.Run("quiet dry-run emits nothing", func(t *testing.T) {
		in := &deleteInput{ids: []string{"test-target"}, force: true, dryRun: true, quiet: true}
		out := captureStdout(t, func() error { return outputDeleteProxiedPreview(in, result) })
		if out != "" {
			t.Fatalf("proxied quiet preview produced output: %s", out)
		}
	})

	t.Run("JSON takes precedence without payload", func(t *testing.T) {
		in := &deleteInput{ids: []string{"test-target"}, force: true, dryRun: true, quiet: true, jsonOutput: true}
		out := captureStdout(t, func() error { return outputDeleteProxiedPreview(in, result) })
		for _, marker := range markers {
			if strings.Contains(out, marker) {
				t.Fatalf("proxied JSON preview leaked %q: %s", marker, out)
			}
		}
		start := strings.Index(out, "{")
		if start < 0 {
			t.Fatalf("proxied JSON preview produced no JSON: %s", out)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out[start:]), &got); err != nil {
			t.Fatalf("parse proxied JSON preview: %v\nraw: %s", err, out[start:])
		}
		if _, ok := got["would_delete"]; !ok {
			t.Fatalf("proxied JSON preview missing would_delete: %v", got)
		}
	})
}

func TestOutputDeleteProxiedPreviewExactContracts(t *testing.T) {
	result := deletePreviewResult{
		preview: domain.DeletePreview{
			Issues: map[string]*types.Issue{
				"bd-target": {ID: "bd-target", Title: "Target"},
			},
			ConnectedIssues: map[string]*types.Issue{
				"bd-zulu":  {ID: "bd-zulu", Title: "Zulu"},
				"bd-alpha": {ID: "bd-alpha", Title: "Alpha"},
			},
		},
		res: issueops.DeleteResult{Deleted: 3, Dependencies: 4, Labels: 2, Events: 5},
	}

	t.Run("JSON includes the complete preview contract with sorted connections and takes precedence over quiet", func(t *testing.T) {
		in := &deleteInput{ids: []string{"bd-target"}, force: true, dryRun: true, quiet: true, jsonOutput: true}
		out := captureStdout(t, func() error { return outputDeleteProxiedPreview(in, result) })
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("parse preview JSON: %v\nraw: %s", err, out)
		}
		want := map[string]any{
			"schema_version":       float64(1),
			"would_delete":         float64(3),
			"dependencies_removed": float64(4),
			"labels_removed":       float64(2),
			"events_removed":       float64(5),
			"ids":                  []any{"bd-target"},
			"not_found":            nil,
			"connected":            []any{"bd-alpha", "bd-zulu"},
			"dry_run":              true,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("preview JSON: got %#v, want %#v", got, want)
		}
	})

	t.Run("prose renders preview counts, sorted reference candidates, and dry-run marker", func(t *testing.T) {
		in := &deleteInput{ids: []string{"bd-target"}, dryRun: true}
		out := captureStdout(t, func() error { return outputDeleteProxiedPreview(in, result) })
		for _, required := range []string{
			"Issues to delete (1):", "bd-target: Target", "3 issue(s) total", "4 dependency link(s)",
			"2 label(s)", "5 event(s)", "Connected issues (text references may be rewritten):",
			"bd-alpha: Alpha", "bd-zulu: Zulu", "(Dry-run mode - no changes made)",
		} {
			if !strings.Contains(out, required) {
				t.Errorf("prose preview missing %q:\n%s", required, out)
			}
		}
		if strings.Index(out, "bd-alpha: Alpha") > strings.Index(out, "bd-zulu: Zulu") {
			t.Errorf("prose connected issue order is not sorted:\n%s", out)
		}
	})
}

func TestRenderDeleteProxiedResultExactContracts(t *testing.T) {
	res := issueops.DeleteResult{Deleted: 3, Dependencies: 4, Labels: 2, Events: 5, ReferencesUpdated: 1, Orphaned: []string{"bd-orphan"}}

	t.Run("JSON includes the complete final aggregate", func(t *testing.T) {
		in := &deleteInput{ids: []string{"bd-target", "bd-dependent"}, jsonOutput: true}
		out := captureStdout(t, func() error {
			renderDeleteProxiedResult(in, res)
			return nil
		})
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("parse final JSON: %v\nraw: %s", err, out)
		}
		want := map[string]any{
			"schema_version":       float64(1),
			"deleted":              []any{"bd-target", "bd-dependent"},
			"deleted_count":        float64(3),
			"dependencies_removed": float64(4),
			"labels_removed":       float64(2),
			"events_removed":       float64(5),
			"references_updated":   float64(1),
			// New on this route with the cascade convergence: `--force` used
			// to take the dependents, so there was never anything to orphan.
			"orphaned_issues": []any{"bd-orphan"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("final JSON: got %#v, want %#v", got, want)
		}
	})

	t.Run("prose includes all aggregate counts and reference updates", func(t *testing.T) {
		out := captureStdout(t, func() error {
			renderDeleteProxiedResult(&deleteInput{}, res)
			return nil
		})
		for _, required := range []string{
			"Deleted 3 issue(s)", "Removed 4 dependency link(s)", "Removed 2 label(s)",
			"Removed 5 event(s)", "Updated text references in 1 issue(s)",
			"Orphaned 1 issue(s): bd-orphan",
		} {
			if !strings.Contains(out, required) {
				t.Errorf("final prose missing %q:\n%s", required, out)
			}
		}
	})
}
