package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newUpdateDescriptionGuardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "test",
		Run: func(cmd *cobra.Command, args []string) {},
	}
	registerCommonIssueFlags(cmd)
	cmd.Flags().Bool("allow-empty-description", false, "Allow empty description replacement when reading from stdin or file")
	return cmd
}

func withTestStdin(t *testing.T, content string, fn func()) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}

	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	go func() {
		_, _ = w.WriteString(content)
		_ = w.Close()
	}()

	fn()
}

func TestValidateDescriptionUpdateRejectsEmptyStdinWithoutOptIn(t *testing.T) {
	withTestStdin(t, "", func() {
		cmd := newUpdateDescriptionGuardCmd()
		if err := cmd.ParseFlags([]string{"--stdin"}); err != nil {
			t.Fatalf("failed to parse flags: %v", err)
		}

		description, changed, flagErr := getDescriptionFlag(cmd)
		if flagErr != nil {
			t.Fatalf("unexpected error: %v", flagErr)
		}
		err := validateDescriptionUpdate(cmd, description, changed)
		if err == nil {
			t.Fatal("expected empty stdin description to be rejected")
		}
		if !strings.Contains(err.Error(), "--allow-empty-description") {
			t.Fatalf("expected opt-in guidance in error, got: %v", err)
		}
	})
}

func TestValidateDescriptionUpdateRejectsEmptyDashShorthandWithoutOptIn(t *testing.T) {
	withTestStdin(t, "", func() {
		cmd := newUpdateDescriptionGuardCmd()
		if err := cmd.ParseFlags([]string{"--description", "-"}); err != nil {
			t.Fatalf("failed to parse flags: %v", err)
		}

		description, changed, flagErr := getDescriptionFlag(cmd)
		if flagErr != nil {
			t.Fatalf("unexpected error: %v", flagErr)
		}
		err := validateDescriptionUpdate(cmd, description, changed)
		if err == nil {
			t.Fatal("expected empty dash shorthand description to be rejected")
		}
		if !strings.Contains(err.Error(), "--allow-empty-description") {
			t.Fatalf("expected opt-in guidance in error, got: %v", err)
		}
	})
}

func TestValidateDescriptionUpdateRejectsEmptyBodyFileWithoutOptIn(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "empty.md")
	if err := os.WriteFile(filePath, []byte{}, 0644); err != nil {
		t.Fatalf("failed to write empty file: %v", err)
	}

	cmd := newUpdateDescriptionGuardCmd()
	if err := cmd.ParseFlags([]string{"--body-file", filePath}); err != nil {
		t.Fatalf("failed to parse flags: %v", err)
	}

	description, changed, flagErr := getDescriptionFlag(cmd)
	if flagErr != nil {
		t.Fatalf("unexpected error: %v", flagErr)
	}
	err := validateDescriptionUpdate(cmd, description, changed)
	if err == nil {
		t.Fatal("expected empty body file description to be rejected")
	}
	if !strings.Contains(err.Error(), "--allow-empty-description") {
		t.Fatalf("expected opt-in guidance in error, got: %v", err)
	}
}

func TestValidateDescriptionUpdateAllowsEmptyStdinWithOptIn(t *testing.T) {
	withTestStdin(t, "", func() {
		cmd := newUpdateDescriptionGuardCmd()
		if err := cmd.ParseFlags([]string{"--stdin", "--allow-empty-description"}); err != nil {
			t.Fatalf("failed to parse flags: %v", err)
		}

		description, changed, flagErr := getDescriptionFlag(cmd)
		if flagErr != nil {
			t.Fatalf("unexpected error: %v", flagErr)
		}
		if err := validateDescriptionUpdate(cmd, description, changed); err != nil {
			t.Fatalf("expected opt-in empty stdin to succeed, got: %v", err)
		}
		if description != "" {
			t.Fatalf("expected empty description, got %q", description)
		}
	})
}

func TestValidateDescriptionUpdateAllowsNonEmptyStdinWithoutOptIn(t *testing.T) {
	withTestStdin(t, "Updated from stdin\n", func() {
		cmd := newUpdateDescriptionGuardCmd()
		if err := cmd.ParseFlags([]string{"--stdin"}); err != nil {
			t.Fatalf("failed to parse flags: %v", err)
		}

		description, changed, flagErr := getDescriptionFlag(cmd)
		if flagErr != nil {
			t.Fatalf("unexpected error: %v", flagErr)
		}
		if err := validateDescriptionUpdate(cmd, description, changed); err != nil {
			t.Fatalf("expected non-empty stdin to succeed, got: %v", err)
		}
		if description != "Updated from stdin\n" {
			t.Fatalf("expected stdin content to round-trip, got %q", description)
		}
	})
}

// INVERTED 2026-08-26 (sys-t26n1h). This test previously asserted that an inline
// `--description ""` MUST succeed, on the assumption that an inline empty is always
// deliberate. That assumption is false in practice: `--description "$(cmd)"` expands
// to an inline empty whenever cmd fails, so the unguarded path silently replaced a
// real description with nothing at exit 0. Measured: a 2725-char description lost that
// way, and an isolating contrast on one issue showed --description-file /dev/null
// refused (rc=1) while --description "$(failing)" wiped the field (rc=0).
// The opt-in has not been removed, only made mandatory: see the WithOptIn test below.
func TestValidateDescriptionUpdateRejectsExplicitInlineEmptyWithoutOptIn(t *testing.T) {
	cmd := newUpdateDescriptionGuardCmd()
	if err := cmd.ParseFlags([]string{"--description", ""}); err != nil {
		t.Fatalf("failed to parse flags: %v", err)
	}

	description, changed, flagErr := getDescriptionFlag(cmd)
	if flagErr != nil {
		t.Fatalf("unexpected error: %v", flagErr)
	}
	err := validateDescriptionUpdate(cmd, description, changed)
	if err == nil {
		t.Fatal("expected inline empty description to be rejected without --allow-empty-description")
	}
	if !strings.Contains(err.Error(), "--allow-empty-description") {
		t.Fatalf("expected opt-in guidance in error, got: %v", err)
	}
	// The old error text told the reader to use an inline empty as the escape hatch.
	// That sentence now points at the very hole this guard closes, so assert it is gone.
	if strings.Contains(err.Error(), "explicit inline empty value") {
		t.Fatalf("error still advertises the inline-empty escape hatch: %v", err)
	}
}

func TestValidateDescriptionUpdateAllowsExplicitInlineEmptyWithOptIn(t *testing.T) {
	cmd := newUpdateDescriptionGuardCmd()
	if err := cmd.ParseFlags([]string{"--description", "", "--allow-empty-description"}); err != nil {
		t.Fatalf("failed to parse flags: %v", err)
	}

	description, changed, flagErr := getDescriptionFlag(cmd)
	if flagErr != nil {
		t.Fatalf("unexpected error: %v", flagErr)
	}
	if err := validateDescriptionUpdate(cmd, description, changed); err != nil {
		t.Fatalf("expected opt-in inline empty description to succeed, got: %v", err)
	}
	if description != "" {
		t.Fatalf("expected empty inline description, got %q", description)
	}
}

// A non-empty inline description must stay unaffected by the widened guard.
func TestValidateDescriptionUpdateAllowsNonEmptyInline(t *testing.T) {
	cmd := newUpdateDescriptionGuardCmd()
	if err := cmd.ParseFlags([]string{"--description", "real content"}); err != nil {
		t.Fatalf("failed to parse flags: %v", err)
	}

	description, changed, flagErr := getDescriptionFlag(cmd)
	if flagErr != nil {
		t.Fatalf("unexpected error: %v", flagErr)
	}
	if err := validateDescriptionUpdate(cmd, description, changed); err != nil {
		t.Fatalf("expected non-empty inline description to succeed, got: %v", err)
	}
	if description != "real content" {
		t.Fatalf("expected inline content to round-trip, got %q", description)
	}
}

// The guard must stay silent when --description was never passed at all: an update
// that touches only other fields must not be forced to opt into anything.
func TestValidateDescriptionUpdateIgnoresUnchangedDescription(t *testing.T) {
	cmd := newUpdateDescriptionGuardCmd()
	if err := cmd.ParseFlags([]string{}); err != nil {
		t.Fatalf("failed to parse flags: %v", err)
	}

	description, changed, flagErr := getDescriptionFlag(cmd)
	if flagErr != nil {
		t.Fatalf("unexpected error: %v", flagErr)
	}
	if changed {
		t.Fatal("expected description flag to be unchanged")
	}
	if err := validateDescriptionUpdate(cmd, description, changed); err != nil {
		t.Fatalf("expected unchanged description to succeed, got: %v", err)
	}
}
