//go:build cgo

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/dolt"
)

// setupLabelVocabularyTestStore wires a fresh *dolt.DoltStore into the
// package-level store/rootCtx globals checkLabelVocabulary reads, restoring
// both on cleanup -- the same save/restore shape TestIssueIDCompletion uses.
func setupLabelVocabularyTestStore(t *testing.T) (*dolt.DoltStore, context.Context) {
	t.Helper()
	originalStore := store
	originalRootCtx := rootCtx
	t.Cleanup(func() {
		store = originalStore
		rootCtx = originalRootCtx
	})

	tmpDir := t.TempDir()
	testDB := filepath.Join(tmpDir, "test.db")
	s := newTestStoreWithPrefix(t, testDB, "lv")
	store = s

	ctx := context.Background()
	rootCtx = ctx
	return s, ctx
}

// TestCheckLabelVocabulary_OpenModeNoop pins the default: an undefined label
// is accepted silently, no matter what the registry holds, when
// labels.vocabulary is unset (the documented default is "open").
func TestCheckLabelVocabulary_OpenModeNoop(t *testing.T) {
	_, ctx := setupLabelVocabularyTestStore(t)

	out := captureStderr(t, func() {
		if err := checkLabelVocabulary(ctx, []string{"totally-undefined"}); err != nil {
			t.Fatalf("open mode must never refuse a write, got: %v", err)
		}
	})
	if out != "" {
		t.Fatalf("open mode must never warn, got stderr: %q", out)
	}
}

// TestCheckLabelVocabulary_WarnMode pins: the write proceeds (nil error), a
// warning names the undefined label on stderr, and a case-insensitive match
// against a defined label is suggested.
func TestCheckLabelVocabulary_WarnMode(t *testing.T) {
	s, ctx := setupLabelVocabularyTestStore(t)

	if err := s.SetConfig(ctx, labelsVocabularyConfigKey, labelsVocabularyWarn); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := s.DefineLabel(ctx, "backend", "", "tester"); err != nil {
		t.Fatalf("DefineLabel: %v", err)
	}

	out := captureStderr(t, func() {
		if err := checkLabelVocabulary(ctx, []string{"Backend", "frontend"}); err != nil {
			t.Fatalf("warn mode must never refuse a write, got: %v", err)
		}
	})
	if !strings.Contains(out, `"Backend"`) {
		t.Errorf("expected the undefined case-variant named, got: %q", out)
	}
	if !strings.Contains(out, `did you mean "backend"`) {
		t.Errorf("expected a suggestion of the defined spelling, got: %q", out)
	}
	if !strings.Contains(out, `"frontend"`) {
		t.Errorf("expected the never-defined label named, got: %q", out)
	}
	if strings.Contains(out, `did you mean "frontend"`) {
		t.Errorf("must not suggest a spelling for a label with no case-variant match, got: %q", out)
	}
}

// TestCheckLabelVocabulary_WarnMode_DefinedLabelIsSilent confirms a label
// matching the registry EXACTLY produces no warning at all.
func TestCheckLabelVocabulary_WarnMode_DefinedLabelIsSilent(t *testing.T) {
	s, ctx := setupLabelVocabularyTestStore(t)

	if err := s.SetConfig(ctx, labelsVocabularyConfigKey, labelsVocabularyWarn); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := s.DefineLabel(ctx, "backend", "", "tester"); err != nil {
		t.Fatalf("DefineLabel: %v", err)
	}

	out := captureStderr(t, func() {
		if err := checkLabelVocabulary(ctx, []string{"backend"}); err != nil {
			t.Fatalf("a defined label must never be refused, got: %v", err)
		}
	})
	if out != "" {
		t.Fatalf("a defined label must never warn, got: %q", out)
	}
}

// TestCheckLabelVocabulary_EnforceMode pins: the write is refused, and the
// error names the undefined label and the remedy.
func TestCheckLabelVocabulary_EnforceMode(t *testing.T) {
	s, ctx := setupLabelVocabularyTestStore(t)

	if err := s.SetConfig(ctx, labelsVocabularyConfigKey, labelsVocabularyEnforce); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	err := checkLabelVocabulary(ctx, []string{"undefined-label"})
	if err == nil {
		t.Fatal("enforce mode must refuse an undefined label, got nil")
	}
	if !strings.Contains(err.Error(), "undefined-label") {
		t.Errorf("expected the error to name the label, got: %v", err)
	}
	if !strings.Contains(err.Error(), "bd label define") {
		t.Errorf("expected the error to name the remedy, got: %v", err)
	}
}

// TestCheckLabelVocabulary_EnforceMode_DefinedLabelPasses confirms enforce
// mode accepts a label already in the registry.
func TestCheckLabelVocabulary_EnforceMode_DefinedLabelPasses(t *testing.T) {
	s, ctx := setupLabelVocabularyTestStore(t)

	if err := s.SetConfig(ctx, labelsVocabularyConfigKey, labelsVocabularyEnforce); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := s.DefineLabel(ctx, "backend", "", "tester"); err != nil {
		t.Fatalf("DefineLabel: %v", err)
	}

	if err := checkLabelVocabulary(ctx, []string{"backend"}); err != nil {
		t.Fatalf("enforce mode must accept a defined label, got: %v", err)
	}
}

// TestImportBypassesLabelVocabularyEnforcement is the mandated proof (design
// doc Unit B item 4): importing an issue carrying an undefined label
// succeeds untouched even with labels.vocabulary=enforce, because the
// import/JSONL path never calls checkLabelVocabulary at all.
func TestImportBypassesLabelVocabularyEnforcement(t *testing.T) {
	s, ctx := setupLabelVocabularyTestStore(t)

	if err := s.SetConfig(ctx, labelsVocabularyConfigKey, labelsVocabularyEnforce); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	// Deliberately NOT defining "undefined-import-label" -- enforce mode, on
	// any interactive write path, would refuse it.

	tmpDir := t.TempDir()
	jsonlContent := `{"id":"lv-import1","title":"Imported issue","type":"bug","status":"open","priority":2,"labels":["undefined-import-label"],"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z"}
`
	jsonlPath := filepath.Join(tmpDir, "issues.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatalf("write JSONL: %v", err)
	}

	count, err := importFromLocalJSONL(ctx, s, jsonlPath)
	if err != nil {
		t.Fatalf("import must succeed regardless of labels.vocabulary: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 issue imported, got %d", count)
	}

	labels, err := s.GetLabels(ctx, "lv-import1")
	if err != nil {
		t.Fatalf("GetLabels: %v", err)
	}
	found := false
	for _, l := range labels {
		if l == "undefined-import-label" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the undefined label to survive import untouched, got: %v", labels)
	}
}
