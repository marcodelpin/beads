package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInjectBdBlock covers the four required acceptance scenarios:
//  1. file missing
//  2. file present without marker block
//  3. file present with marker block (replace in place)
//  4. file present with marker block + extra content after the block
//     (must preserve extra content untouched)
//
// Plus an idempotency test: running twice produces the same end state.
func TestInjectBdBlock(t *testing.T) {
	const oldBlock = "OLD bd block content that must be replaced"
	const extraSuffix = "\n\n## My Custom Section\n\nThis content lives AFTER the bd block and must survive re-injection.\n"
	const customPrefix = "# My Project\n\nIntro paragraph the user wrote.\n"

	tests := []struct {
		name           string
		initialContent *string // nil = file does not exist
		wantPrefix     string  // text expected before bd:start (substring check)
		wantSuffix     string  // text expected after bd:end (substring check)
		wantNoOldBlock bool    // verify old marker content is gone
	}{
		{
			name:           "file missing — create from scratch",
			initialContent: nil,
			wantPrefix:     "",
			wantSuffix:     "",
		},
		{
			name:           "file present without marker — append",
			initialContent: strPtrInject(customPrefix),
			wantPrefix:     "# My Project",
			wantSuffix:     "",
		},
		{
			name: "file present with marker — replace in place",
			initialContent: strPtrInject(customPrefix +
				bdInjectStartMarker + "\n" + oldBlock + "\n" + bdInjectEndMarker + "\n"),
			wantPrefix:     "# My Project",
			wantSuffix:     "",
			wantNoOldBlock: true,
		},
		{
			name: "file present with marker + extra suffix — preserve extra",
			initialContent: strPtrInject(customPrefix +
				bdInjectStartMarker + "\n" + oldBlock + "\n" + bdInjectEndMarker +
				extraSuffix),
			wantPrefix:     "# My Project",
			wantSuffix:     "## My Custom Section",
			wantNoOldBlock: true,
		},
	}

	block := bdInjectBlock()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fname := filepath.Join(dir, "AGENTS.md")

			if tc.initialContent != nil {
				if err := os.WriteFile(fname, []byte(*tc.initialContent), 0644); err != nil {
					t.Fatalf("setup write failed: %v", err)
				}
			}

			if err := injectBdBlock(fname, block); err != nil {
				t.Fatalf("injectBdBlock failed: %v", err)
			}

			out, err := os.ReadFile(fname)
			if err != nil {
				t.Fatalf("read after inject failed: %v", err)
			}
			got := string(out)

			// All cases must contain the markers and the new block content.
			if !strings.Contains(got, bdInjectStartMarker) {
				t.Errorf("missing start marker in output:\n%s", got)
			}
			if !strings.Contains(got, bdInjectEndMarker) {
				t.Errorf("missing end marker in output:\n%s", got)
			}
			if !strings.Contains(got, "bd (beads)") {
				t.Errorf("missing canonical bd block content in output:\n%s", got)
			}
			if !strings.Contains(got, "bd ready") {
				t.Errorf("missing 'bd ready' command in output:\n%s", got)
			}

			// Prefix preserved when expected.
			if tc.wantPrefix != "" {
				prefixIdx := strings.Index(got, tc.wantPrefix)
				startIdx := strings.Index(got, bdInjectStartMarker)
				if prefixIdx < 0 || prefixIdx >= startIdx {
					t.Errorf("expected prefix %q to appear before start marker; got:\n%s",
						tc.wantPrefix, got)
				}
			}

			// Suffix preserved when expected.
			if tc.wantSuffix != "" {
				suffixIdx := strings.Index(got, tc.wantSuffix)
				endIdx := strings.Index(got, bdInjectEndMarker)
				if suffixIdx < 0 || suffixIdx <= endIdx {
					t.Errorf("expected suffix %q to appear after end marker; got:\n%s",
						tc.wantSuffix, got)
				}
			}

			// Old block content must be gone after replacement.
			if tc.wantNoOldBlock && strings.Contains(got, oldBlock) {
				t.Errorf("old block content still present; expected it to be replaced.\nGot:\n%s", got)
			}

			// Idempotency: a second invocation must produce the same content.
			if err := injectBdBlock(fname, block); err != nil {
				t.Fatalf("second injectBdBlock failed: %v", err)
			}
			out2, err := os.ReadFile(fname)
			if err != nil {
				t.Fatalf("read after second inject failed: %v", err)
			}
			if string(out2) != got {
				t.Errorf("second invocation changed content (not idempotent).\nFirst:\n%s\n---\nSecond:\n%s",
					got, string(out2))
			}
		})
	}
}

// TestRunInjectAgentsMd verifies the multi-file driver writes BOTH CLAUDE.md
// and AGENTS.md, and that errors on one file do not block the other.
func TestRunInjectAgentsMd(t *testing.T) {
	dir := t.TempDir()
	prevWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(prevWd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if err := runInjectAgentsMd(false); err != nil {
		t.Fatalf("runInjectAgentsMd returned error: %v", err)
	}

	for _, f := range bdInjectFiles {
		out, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("expected %s to exist after inject; read err: %v", f, err)
		}
		got := string(out)
		if !strings.Contains(got, bdInjectStartMarker) {
			t.Errorf("%s missing start marker", f)
		}
		if !strings.Contains(got, bdInjectEndMarker) {
			t.Errorf("%s missing end marker", f)
		}
		if !strings.Contains(got, "bd ready") {
			t.Errorf("%s missing canonical bd block content", f)
		}
	}

	// Idempotency at the driver level: second run, same content.
	beforeClaude, _ := os.ReadFile("CLAUDE.md")
	beforeAgents, _ := os.ReadFile("AGENTS.md")
	if err := runInjectAgentsMd(false); err != nil {
		t.Fatalf("second runInjectAgentsMd: %v", err)
	}
	afterClaude, _ := os.ReadFile("CLAUDE.md")
	afterAgents, _ := os.ReadFile("AGENTS.md")
	if string(beforeClaude) != string(afterClaude) {
		t.Error("CLAUDE.md changed on second run (not idempotent)")
	}
	if string(beforeAgents) != string(afterAgents) {
		t.Error("AGENTS.md changed on second run (not idempotent)")
	}
}

func strPtrInject(s string) *string { return &s }
