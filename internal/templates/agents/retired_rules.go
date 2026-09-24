package agents

import "strings"

// RetiredMemoryNeedles lists every spelling of the retired "beads replaces the
// harness memory mechanism" rule, lowercased for case-insensitive matching.
//
// The rule was superseded on 2026-08-07: bd remember is tracker-scoped, while
// the harness-managed memory files (MEMORY.md, memory dirs) are a separate live
// mechanism the agent harness writes itself. A bd-owned block that forbids them
// contradicts the host platform it is injected into, and because the block sits
// inside BEGIN/END BEADS markers, every downstream hand-fix is overwritten on
// the next regeneration - so the producer side is the only place a correction
// holds.
//
// The list is keyed on the MECHANISM (any text forbidding harness memory files)
// rather than on one sentence: the bda-5uxn sweep searched for "do NOT use
// MEMORY.md files" and left the Codex template's "do not create ad hoc memory
// files" standing (bda-atiw), because a needle naming one spelling cannot see
// the other. Add a new spelling here when one appears; it is the single
// authority both the agents-package tests and cmd/bd's injection tests consume.
var RetiredMemoryNeedles = []string{
	"not use memory.md",
	"not create memory.md",
	"ad hoc memory file",
}

// FindRetiredMemoryRule reports the first retired spelling carried by text, or
// "" when text carries none. Matching is case-insensitive.
//
// It makes no claim about text being non-degenerate: an empty string yields "",
// so every caller owns a positive control of its own.
func FindRetiredMemoryRule(text string) string {
	lower := strings.ToLower(text)
	for _, needle := range RetiredMemoryNeedles {
		if strings.Contains(lower, needle) {
			return needle
		}
	}
	return ""
}
