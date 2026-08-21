package main

import (
	"fmt"
	"os"
	"strings"
)

// bda-r2r: --inject-agents-md flag for `bd init`.
// Writes a marker-delimited bd-usage block into BOTH CLAUDE.md and AGENTS.md
// (creating the files if missing). Marker format: <!-- bd:start --> ... <!-- bd:end -->
// Idempotent: re-running replaces the block in place, preserving content
// before and after the markers.
//
// This is intentionally distinct from the existing "BEGIN BEADS INTEGRATION"
// section installed by addAgentsInstructions(): the legacy block is the full
// AGENTS.md template (long-form, profile-aware), while the bd:start/bd:end
// block is a short, opinionated rules summary borrowed from the ghist init
// pattern (KB doc_cd59fe20e79a927b). Both can coexist; --inject-agents-md
// deliberately does not touch the BEADS INTEGRATION block.

const (
	bdInjectStartMarker = "<!-- bd:start -->"
	bdInjectEndMarker   = "<!-- bd:end -->"
)

// bdInjectFiles lists the files the --inject-agents-md flag writes to.
// Both files are touched on every invocation. CLAUDE.md is the Claude Code
// entry point; AGENTS.md is the OpenAI Codex / generic agent entry point.
var bdInjectFiles = []string{"CLAUDE.md", "AGENTS.md"}

// bdInjectBlock returns the canonical bd-usage block (without the surrounding
// markers — those are added by injectBdBlock).
func bdInjectBlock() string {
	return `## bd (beads) — Task Tracking

This project uses **bd (beads)** for issue tracking. Use bd for ALL task
tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists.

### Quick Reference

` + "```bash" + `
bd ready                        # Find available work
bd show <id>                    # View issue details
bd update <id> --status=in_progress   # Claim work
bd note <id> "<RESULT>"         # Append result note
bd close <id>                   # Complete work
bd remember "<fact>" --key <k>  # Store persistent knowledge
` + "```" + `

### Rules

- Use ` + "`bd`" + ` for ALL task tracking — never TodoWrite / TaskCreate / markdown TODO
- Run ` + "`bd prime`" + ` at session start for full command reference
- Use ` + "`bd remember`" + ` for persistent knowledge — do NOT use MEMORY.md files
- Every change must reference the bd issue ID it addresses
- Close issues only with a RESULT note citing the commit hash

Run ` + "`bd init --help`" + ` to see the full set of init flags including
` + "`--inject-agents-md`" + ` (which wrote this block).
`
}

// injectBdBlock writes the marker-delimited bd block into the named file.
// Behavior:
//   - file missing: create with leading-block-only content
//   - file exists, no marker block: append (with leading blank line if needed)
//   - file exists, marker block present: replace block in place, preserving
//     all content before <!-- bd:start --> and after <!-- bd:end -->
//
// The function is idempotent: the post-condition (file content) depends only
// on the file's content before and the block text, not on how many times it
// has been called.
func injectBdBlock(filename string, block string) error {
	wrappedBlock := bdInjectStartMarker + "\n" + strings.TrimRight(block, "\n") + "\n" + bdInjectEndMarker

	//nolint:gosec // G304: filename is one of the trusted entries in bdInjectFiles
	existing, err := os.ReadFile(filename)
	if os.IsNotExist(err) {
		// File missing: create with the wrapped block (with trailing newline
		// so the file ends cleanly).
		// #nosec G306 — markdown needs to be readable
		return os.WriteFile(filename, []byte(wrappedBlock+"\n"), 0644)
	} else if err != nil {
		return fmt.Errorf("failed to read %s: %w", filename, err)
	}

	contentStr := string(existing)
	startIdx := strings.Index(contentStr, bdInjectStartMarker)
	endIdx := strings.Index(contentStr, bdInjectEndMarker)

	if startIdx >= 0 && endIdx > startIdx {
		// Marker block present: replace in place, preserving prefix and suffix.
		// endIdx points to '<' of "<!-- bd:end -->"; advance past the full marker.
		afterEnd := endIdx + len(bdInjectEndMarker)
		prefix := contentStr[:startIdx]
		suffix := contentStr[afterEnd:]
		newContent := prefix + wrappedBlock + suffix
		// #nosec G306 — markdown needs to be readable
		return os.WriteFile(filename, []byte(newContent), 0644)
	}

	// File exists, no marker block: append with a leading blank line so the
	// block is visually separated from existing content.
	newContent := contentStr
	if !strings.HasSuffix(newContent, "\n") {
		newContent += "\n"
	}
	newContent += "\n" + wrappedBlock + "\n"
	// #nosec G306 — markdown needs to be readable
	return os.WriteFile(filename, []byte(newContent), 0644)
}

// runInjectAgentsMd executes the --inject-agents-md flag: writes the bd block
// into both CLAUDE.md and AGENTS.md in the current working directory.
// Errors on individual files are reported but do not stop the loop, so a
// single permission issue does not block the other file.
func runInjectAgentsMd(verbose bool) error {
	block := bdInjectBlock()
	var firstErr error
	for _, f := range bdInjectFiles {
		if err := injectBdBlock(f, block); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			fmt.Fprintf(os.Stderr, "Warning: --inject-agents-md failed for %s: %v\n", f, err)
			continue
		}
		if verbose {
			fmt.Printf("  ✓ Injected bd block into %s\n", f)
		}
	}
	return firstErr
}
