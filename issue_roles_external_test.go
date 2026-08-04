package beads_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/beads"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/telemetry"
)

// TestReadyClaimerLayersHooksOutsideTelemetry pins the decorator order the
// claim accessor produces against the order cmd/bd assembles the chain in
// (storage_chain.go: telemetry first, hooks outermost), the same property
// TestIssueLifecycleLayersHooksOutsideTelemetry pins for the write role. A
// blind delegation at either decorator would still satisfy Storage and would
// still compile.
func TestReadyClaimerLayersHooksOutsideTelemetry(t *testing.T) {
	t.Setenv("BD_OTEL_STDOUT", "true")
	instrumented, ok := telemetry.WrapStorage(&dolt.DoltStore{}).(*telemetry.InstrumentedStorage)
	if !ok {
		t.Fatal("WrapStorage() did not create InstrumentedStorage")
	}

	claimer, err := storage.NewHookFiringStore(instrumented, nil).ReadyClaimer()
	if err != nil {
		t.Fatalf("ReadyClaimer() error = %v", err)
	}
	if got := reflect.TypeOf(claimer).String(); got != "*storage.hookReadyClaimer" {
		t.Fatalf("outer layer = %s, want the hook wrapper", got)
	}
}

// TestBatchCloserLayersHooksOutsideTelemetry is the same pin for the
// close-many role.
func TestBatchCloserLayersHooksOutsideTelemetry(t *testing.T) {
	t.Setenv("BD_OTEL_STDOUT", "true")
	instrumented, ok := telemetry.WrapStorage(&dolt.DoltStore{}).(*telemetry.InstrumentedStorage)
	if !ok {
		t.Fatal("WrapStorage() did not create InstrumentedStorage")
	}

	closer, err := storage.NewHookFiringStore(instrumented, nil).BatchCloser()
	if err != nil {
		t.Fatalf("BatchCloser() error = %v", err)
	}
	if got := reflect.TypeOf(closer).String(); got != "*storage.hookBatchCloser" {
		t.Fatalf("outer layer = %s, want the hook wrapper", got)
	}
}

// TestReadyClaimerExposesTypedUnsupportedError pins the typed error a backend
// returns when it cannot serve the role, reachable through the public alias
// without importing internal/storage.
func TestReadyClaimerExposesTypedUnsupportedError(t *testing.T) {
	claimer, err := (*dolt.DoltStore)(nil).ReadyClaimer()
	if claimer != nil {
		t.Fatalf("ReadyClaimer() claimer = %T, want nil", claimer)
	}
	var unsupported *beads.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("ReadyClaimer() error = %v, want *beads.ErrUnsupported", err)
	}
}

func TestBatchCloserExposesTypedUnsupportedError(t *testing.T) {
	closer, err := (*dolt.DoltStore)(nil).BatchCloser()
	if closer != nil {
		t.Fatalf("BatchCloser() closer = %T, want nil", closer)
	}
	var unsupported *beads.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("BatchCloser() error = %v, want *beads.ErrUnsupported", err)
	}
}

// TestDependencyEditorLayersHooksOutsideTelemetry is the same decorator-order
// pin for the edge-write role.
func TestDependencyEditorLayersHooksOutsideTelemetry(t *testing.T) {
	t.Setenv("BD_OTEL_STDOUT", "true")
	instrumented, ok := telemetry.WrapStorage(&dolt.DoltStore{}).(*telemetry.InstrumentedStorage)
	if !ok {
		t.Fatal("WrapStorage() did not create InstrumentedStorage")
	}

	editor, err := storage.NewHookFiringStore(instrumented, nil).DependencyEditor()
	if err != nil {
		t.Fatalf("DependencyEditor() error = %v", err)
	}
	if got := reflect.TypeOf(editor).String(); got != "*storage.hookDependencyEditor" {
		t.Fatalf("outer layer = %s, want the hook wrapper", got)
	}
}

// TestCommenterLayersHooksOutsideTelemetry is the same pin for the comment
// role.
func TestCommenterLayersHooksOutsideTelemetry(t *testing.T) {
	t.Setenv("BD_OTEL_STDOUT", "true")
	instrumented, ok := telemetry.WrapStorage(&dolt.DoltStore{}).(*telemetry.InstrumentedStorage)
	if !ok {
		t.Fatal("WrapStorage() did not create InstrumentedStorage")
	}

	commenter, err := storage.NewHookFiringStore(instrumented, nil).Commenter()
	if err != nil {
		t.Fatalf("Commenter() error = %v", err)
	}
	if got := reflect.TypeOf(commenter).String(); got != "*storage.hookCommenter" {
		t.Fatalf("outer layer = %s, want the hook wrapper", got)
	}
}

// TestIssueRelationsKeepsTelemetryOutermost is the READ role's version of the
// pin, and it is deliberately the other answer: the hook decorator adds no
// layer to a read, so the outermost thing a caller gets is the instrumented
// surface. A wrapper appearing here would mean a hook layer landed on a path
// with nothing to fire.
func TestIssueRelationsKeepsTelemetryOutermost(t *testing.T) {
	t.Setenv("BD_OTEL_STDOUT", "true")
	instrumented, ok := telemetry.WrapStorage(&dolt.DoltStore{}).(*telemetry.InstrumentedStorage)
	if !ok {
		t.Fatal("WrapStorage() did not create InstrumentedStorage")
	}

	relations, err := storage.NewHookFiringStore(instrumented, nil).IssueRelations()
	if err != nil {
		t.Fatalf("IssueRelations() error = %v", err)
	}
	if got := reflect.TypeOf(relations).String(); got != "*telemetry.instrumentedIssueRelations" {
		t.Fatalf("outer layer = %s, want the telemetry wrapper unwrapped by the hook decorator", got)
	}
}

func TestDependencyEditorExposesTypedUnsupportedError(t *testing.T) {
	editor, err := (*dolt.DoltStore)(nil).DependencyEditor()
	if editor != nil {
		t.Fatalf("DependencyEditor() editor = %T, want nil", editor)
	}
	var unsupported *beads.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("DependencyEditor() error = %v, want *beads.ErrUnsupported", err)
	}
}

func TestCommenterExposesTypedUnsupportedError(t *testing.T) {
	commenter, err := (*dolt.DoltStore)(nil).Commenter()
	if commenter != nil {
		t.Fatalf("Commenter() commenter = %T, want nil", commenter)
	}
	var unsupported *beads.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("Commenter() error = %v, want *beads.ErrUnsupported", err)
	}
}

func TestIssueRelationsExposesTypedUnsupportedError(t *testing.T) {
	relations, err := (*dolt.DoltStore)(nil).IssueRelations()
	if relations != nil {
		t.Fatalf("IssueRelations() relations = %T, want nil", relations)
	}
	var unsupported *beads.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("IssueRelations() error = %v, want *beads.ErrUnsupported", err)
	}
}
