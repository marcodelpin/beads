package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
)

// countMemoriesInStore returns the number of kv.memory.* entries in the
// store's config. Used by the fork-only auto-import guard to avoid
// resurrecting deleted memories when the issues table is empty.
func countMemoriesInStore(ctx context.Context, s storage.DoltStorage) (int, error) {
	all, err := s.GetAllConfig(ctx)
	if err != nil {
		return 0, err
	}
	prefix := kvPrefix + memoryPrefix
	n := 0
	for k := range all {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n, nil
}

// maybeAutoImportJSONL checks whether the database is empty and a
// issues.jsonl file exists in beadsDir. When both conditions are true it
// auto-imports the JSONL data so users upgrading from pre-0.56 (which used
// .beads/dolt/) to 1.0+ (which uses .beads/embeddeddolt/) don't appear to
// lose their issues.  See GH#2994.
//
// The function is best-effort: failures are logged as warnings but do not
// prevent the store from being used.
func maybeAutoImportJSONL(ctx context.Context, s storage.DoltStorage, beadsDir string) {
	// Quick check: does the JSONL file exist and have content?
	jsonlPath := filepath.Join(beadsDir, "issues.jsonl")
	info, err := os.Stat(jsonlPath)
	if err != nil || info.Size() == 0 {
		return // no JSONL file or empty — nothing to import
	}

	// Check whether the database already has issues.
	stats, err := s.GetStatistics(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: auto-import: failed to check issue count: %v\n", err)
		return
	}
	if stats.TotalIssues > 0 {
		return // database is not empty — nothing to do
	}

	// Fork-only guard: TotalIssues==0 is not enough — a database with 0
	// issues but >=1 memory IS populated, and re-importing the JSONL on
	// every invocation will resurrect memories that were intentionally
	// deleted (e.g. by 'bd memories --gc' or 'bd gc' memory prune).
	if memCount, err := countMemoriesInStore(ctx, s); err == nil && memCount > 0 {
		return
	}

	// Database is empty but JSONL has data — auto-import.
	fmt.Fprintf(os.Stderr, "auto-importing %d bytes from %s into empty database...\n", info.Size(), jsonlPath)

	result, err := importFromLocalJSONLFull(ctx, s, jsonlPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: auto-import from %s failed: %v\n", jsonlPath, err)
		fmt.Fprintf(os.Stderr, "\nYour issues are still safe in %s.\n", jsonlPath)
		fmt.Fprintf(os.Stderr, "Try: bd init --from-jsonl   (re-initialize and import from the JSONL file)\n")
		fmt.Fprintf(os.Stderr, "If this persists, please report at https://github.com/gastownhall/beads/issues\n\n")
		return
	}

	// Commit the imported data to Dolt history.
	commitMsg := fmt.Sprintf("auto-import: %d issues from %s (upgrade recovery, GH#2994)", result.Issues, filepath.Base(jsonlPath))
	if result.Memories > 0 {
		commitMsg = fmt.Sprintf("auto-import: %d issues, %d memories from %s (upgrade recovery, GH#2994)", result.Issues, result.Memories, filepath.Base(jsonlPath))
	}
	if err := s.Commit(ctx, commitMsg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: auto-import: dolt commit failed: %v\n", err)
		return
	}

	if result.Memories > 0 {
		fmt.Fprintf(os.Stderr, "auto-imported %d issues and %d memories from %s\n", result.Issues, result.Memories, jsonlPath)
	} else {
		fmt.Fprintf(os.Stderr, "auto-imported %d issues from %s\n", result.Issues, jsonlPath)
	}
}
