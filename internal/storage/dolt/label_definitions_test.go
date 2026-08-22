package dolt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage"
)

// =============================================================================
// Label Vocabulary Registry Tests (bd label define / undefine / defined)
// =============================================================================

func TestDefineLabel_Basic(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := store.DefineLabel(ctx, "backend", "server-side work", "tester"); err != nil {
		t.Fatalf("DefineLabel failed: %v", err)
	}

	defs, err := store.ListLabelDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListLabelDefinitions failed: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 definition, got %d", len(defs))
	}
	if defs[0].Label != "backend" {
		t.Errorf("expected label %q, got %q", "backend", defs[0].Label)
	}
	if defs[0].Description == nil || *defs[0].Description != "server-side work" {
		t.Errorf("expected description %q, got %v", "server-side work", defs[0].Description)
	}
	if defs[0].CreatedBy == nil || *defs[0].CreatedBy != "tester" {
		t.Errorf("expected created_by %q, got %v", "tester", defs[0].CreatedBy)
	}
}

func TestDefineLabel_NoDescription(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := store.DefineLabel(ctx, "frontend", "", "tester"); err != nil {
		t.Fatalf("DefineLabel failed: %v", err)
	}

	defs, err := store.ListLabelDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListLabelDefinitions failed: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 definition, got %d", len(defs))
	}
	if defs[0].Description != nil {
		t.Errorf("expected nil description, got %v", *defs[0].Description)
	}
}

func TestDefineLabel_AlreadyDefinedSameSpelling(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := store.DefineLabel(ctx, "backend", "", "tester"); err != nil {
		t.Fatalf("first DefineLabel failed: %v", err)
	}
	err := store.DefineLabel(ctx, "backend", "", "tester")
	if err == nil {
		t.Fatal("expected an error defining an already-defined label, got nil")
	}
	if !errors.Is(err, storage.ErrValidation) {
		t.Errorf("expected storage.ErrValidation, got %v", err)
	}
	if !strings.Contains(err.Error(), "backend") {
		t.Errorf("expected error to name the label, got %v", err)
	}
}

// TestDefineLabel_CaseInsensitiveCollision pins the creation-time discipline
// the registry exists to add: defining "Backend" when "backend" is already
// defined must be refused, naming the EXISTING spelling in the error.
func TestDefineLabel_CaseInsensitiveCollision(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := store.DefineLabel(ctx, "backend", "", "tester"); err != nil {
		t.Fatalf("DefineLabel(backend) failed: %v", err)
	}
	err := store.DefineLabel(ctx, "Backend", "", "tester")
	if err == nil {
		t.Fatal("expected a case-collision error, got nil")
	}
	if !errors.Is(err, storage.ErrValidation) {
		t.Errorf("expected storage.ErrValidation, got %v", err)
	}
	if !strings.Contains(err.Error(), "backend") {
		t.Errorf("expected error to name the existing spelling %q, got %v", "backend", err)
	}

	// The colliding spelling must not have been inserted: only the original
	// row survives.
	defs, err := store.ListLabelDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListLabelDefinitions failed: %v", err)
	}
	if len(defs) != 1 || defs[0].Label != "backend" {
		t.Errorf("expected exactly [backend], got %v", defs)
	}
}

func TestDefineLabel_Empty(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := store.DefineLabel(ctx, "   ", "", "tester")
	if err == nil {
		t.Fatal("expected an error defining an empty label, got nil")
	}
	if !errors.Is(err, storage.ErrValidation) {
		t.Errorf("expected storage.ErrValidation, got %v", err)
	}
}

func TestUndefineLabel_RoundTrip(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := store.DefineLabel(ctx, "temp", "", "tester"); err != nil {
		t.Fatalf("DefineLabel failed: %v", err)
	}
	if err := store.UndefineLabel(ctx, "temp"); err != nil {
		t.Fatalf("UndefineLabel failed: %v", err)
	}

	defs, err := store.ListLabelDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListLabelDefinitions failed: %v", err)
	}
	if len(defs) != 0 {
		t.Errorf("expected no definitions after undefine, got %v", defs)
	}
}

// TestUndefineLabel_CaseInsensitive confirms undefine matches the one row
// regardless of the case the caller types, since only one spelling of a
// label can ever be defined.
func TestUndefineLabel_CaseInsensitive(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := store.DefineLabel(ctx, "backend", "", "tester"); err != nil {
		t.Fatalf("DefineLabel failed: %v", err)
	}
	if err := store.UndefineLabel(ctx, "BACKEND"); err != nil {
		t.Fatalf("UndefineLabel(BACKEND) failed: %v", err)
	}

	defs, err := store.ListLabelDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListLabelDefinitions failed: %v", err)
	}
	if len(defs) != 0 {
		t.Errorf("expected no definitions after undefine, got %v", defs)
	}
}

func TestUndefineLabel_NotDefined(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := store.UndefineLabel(ctx, "never-defined")
	if err == nil {
		t.Fatal("expected an error undefining a never-defined label, got nil")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected storage.ErrNotFound, got %v", err)
	}
}

func TestListLabelDefinitions_SortedByLabel(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, label := range []string{"zeta", "alpha", "mid"} {
		if err := store.DefineLabel(ctx, label, "", "tester"); err != nil {
			t.Fatalf("DefineLabel(%s) failed: %v", label, err)
		}
	}

	defs, err := store.ListLabelDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListLabelDefinitions failed: %v", err)
	}
	if len(defs) != 3 {
		t.Fatalf("expected 3 definitions, got %d", len(defs))
	}
	got := []string{defs[0].Label, defs[1].Label, defs[2].Label}
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("expected sorted order %v, got %v", want, got)
		}
	}
}

func TestListLabelDefinitions_Empty(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	defs, err := store.ListLabelDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListLabelDefinitions failed: %v", err)
	}
	if len(defs) != 0 {
		t.Errorf("expected no definitions on a fresh store, got %v", defs)
	}
}
