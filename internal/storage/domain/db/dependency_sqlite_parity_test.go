package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/domain"
	storagesqlite "github.com/steveyegge/beads/internal/storage/sqlite"
	"github.com/steveyegge/beads/internal/storage/sqlitedialect"
	"github.com/steveyegge/beads/internal/types"
)

func TestDependencyUseCaseSQLiteRejectsHierarchyBlockingButAllowsSiblings(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "beads.db")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if err := storagesqlite.InitSchema(ctx, raw); err != nil {
		_ = raw.Close()
		t.Fatalf("init sqlite schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw sqlite: %v", err)
	}

	runner, err := sqlitedialect.Open(dbPath)
	if err != nil {
		t.Fatalf("open translated sqlite: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	issueRepo := NewIssueSQLRepository(runner)
	for _, id := range []string{"parent", "child", "sibling"} {
		if err := issueRepo.Insert(ctx, newTestIssue(id, id), "tester", domain.InsertIssueOpts{}); err != nil {
			t.Fatalf("insert issue %s: %v", id, err)
		}
	}

	depRepo := NewDependencySQLRepository(runner)
	for _, childID := range []string{"child", "sibling"} {
		if err := depRepo.Insert(ctx,
			newDep(childID, "parent", types.DepParentChild),
			"tester", domain.DepInsertOpts{}); err != nil {
			t.Fatalf("insert parent-child %s -> parent: %v", childID, err)
		}
	}

	uc := domain.NewDependencyUseCase(depRepo)
	if _, err := uc.AddDependencies(ctx, []*types.Dependency{
		newDep("sibling", "child", types.DepBlocks),
	}, "tester", domain.BulkAddDepsOpts{}); err != nil {
		t.Fatalf("siblings may carry ordering edges: %v", err)
	}

	tests := []struct {
		name      string
		dep       *types.Dependency
		wantError string
	}{
		{
			name:      "child blocked by ancestor",
			dep:       newDep("child", "parent", types.DepBlocks),
			wantError: "cannot be blocked by its ancestor",
		},
		{
			name:      "conditional child blocked by ancestor",
			dep:       newDep("child", "parent", types.DepConditionalBlocks),
			wantError: "cannot be blocked by its ancestor",
		},
		{
			name:      "parent blocked by descendant",
			dep:       newDep("parent", "child", types.DepBlocks),
			wantError: "cannot be blocked by its descendant",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := uc.AddDependencies(ctx, []*types.Dependency{tt.dep}, "tester", domain.BulkAddDepsOpts{})
			if err == nil {
				t.Fatalf("AddDependencies(%s -> %s) succeeded, want hierarchy rejection", tt.dep.IssueID, tt.dep.DependsOnID)
			}
			if got := err.Error(); !strings.Contains(got, tt.wantError) {
				t.Fatalf("error = %q, want substring %q", got, tt.wantError)
			}
		})
	}
}
