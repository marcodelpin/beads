package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func descriptionUsesExternalInput(cmd *cobra.Command) bool {
	if stdinFlag, _ := cmd.Flags().GetBool("stdin"); stdinFlag {
		return true
	}
	if cmd.Flags().Changed("body-file") || cmd.Flags().Changed("description-file") {
		return true
	}
	if cmd.Flags().Changed("description") {
		desc, _ := cmd.Flags().GetString("description")
		if desc == "-" {
			return true
		}
	}
	if cmd.Flags().Changed("body") {
		body, _ := cmd.Flags().GetString("body")
		if body == "-" {
			return true
		}
	}
	if cmd.Flags().Changed("message") {
		message, _ := cmd.Flags().GetString("message")
		if message == "-" {
			return true
		}
	}
	return false
}

// validateDescriptionUpdate refuses to clear a description unless the caller opts in
// with --allow-empty-description.
//
// This guard originally fired only when the empty value came from an EXTERNAL source
// (--stdin, --body-file, --description-file, or the "-" shorthand), on the assumption
// that an INLINE `--description ""` is always deliberate. That assumption does not hold
// for a shell: `--description "$(cmd)"` expands to an inline empty whenever cmd fails,
// so the caller passes an empty value nobody typed and the existing text is replaced at
// exit 0. Widened 2026-08-26 (sys-t26n1h) to cover every source of an empty value; the
// opt-in flag is unchanged, it is merely now required for the inline path too.
func validateDescriptionUpdate(cmd *cobra.Command, description string, descChanged bool) error {
	if !descChanged || description != "" {
		return nil
	}

	allowEmptyDescription, _ := cmd.Flags().GetBool("allow-empty-description")
	if allowEmptyDescription {
		return nil
	}

	if descriptionUsesExternalInput(cmd) {
		return fmt.Errorf("empty description from stdin/file requires --allow-empty-description")
	}

	return fmt.Errorf("refusing to clear the description: an empty --description requires --allow-empty-description " +
		"(a failed $(...) substitution expands to an empty inline value, which would silently replace the existing text)")
}
