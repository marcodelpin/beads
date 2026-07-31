package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/steveyegge/beads/internal/config"
)

// newReadyFlagsCommand clones readyCmd's flag definitions onto a fresh command
// so each case parses from pristine defaults. Cloning beats re-declaring the
// flags here: a hand-copied default would be a second place for `bd ready` to
// drift, which is the very thing gatherReadyInput exists to stop. The clone is
// by value, not AddFlagSet, so setting a flag here cannot leak into readyCmd.
func newReadyFlagsCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "ready"}
	readyCmd.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Value.Type() {
		case "bool":
			cmd.Flags().Bool(f.Name, f.DefValue == "true", f.Usage)
		case "int":
			n, err := strconv.Atoi(f.DefValue)
			if err != nil {
				t.Fatalf("--%s has non-integer default %q: %v", f.Name, f.DefValue, err)
			}
			cmd.Flags().Int(f.Name, n, f.Usage)
		case "string":
			cmd.Flags().String(f.Name, f.DefValue, f.Usage)
		case "stringSlice":
			if f.DefValue != "[]" {
				t.Fatalf("--%s has a non-empty slice default %q, which this clone does not reproduce", f.Name, f.DefValue)
			}
			cmd.Flags().StringSlice(f.Name, nil, f.Usage)
		case "stringArray":
			if f.DefValue != "[]" {
				t.Fatalf("--%s has a non-empty array default %q, which this clone does not reproduce", f.Name, f.DefValue)
			}
			cmd.Flags().StringArray(f.Name, nil, f.Usage)
		default:
			t.Fatalf("--%s has unhandled flag type %q", f.Name, f.Value.Type())
		}
	})
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cmd
}

// configureDirectoryLabel points directory.labels at the test's own working
// directory and returns nothing: GetDirectoryLabels resolves against the cwd,
// so the test has to own both ends.
func configureDirectoryLabel(t *testing.T, label string) {
	t.Helper()

	// A leaf name, not the whole path: GetDirectoryLabels suffix-matches the
	// pattern against the cwd, and os.Getwd may resolve symlinks on the way.
	const leaf = "readydirlabel"
	dir := filepath.Join(t.TempDir(), leaf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	t.Chdir(dir)

	config.ResetForTesting()
	t.Cleanup(config.ResetForTesting)
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
	config.Set("directory.labels", map[string]string{leaf: label})

	if got := config.GetDirectoryLabels(); !slices.Equal(got, []string{label}) {
		t.Fatalf("precondition: GetDirectoryLabels() = %q, want %q", got, []string{label})
	}
}

// TestGatherReadyInputKeepsDirectoryLabelVerbatim pins GH#541's label against
// the collapse into workapi. The configured label is not user input: `bd ready`
// has always put it on the filter exactly as configured, so it must not be
// routed through ReadyParams, whose label sets BuildReadyFilter normalizes.
// The label below is one NormalizeLabels would visibly change, which is what
// makes this a test and not a tautology.
func TestGatherReadyInputKeepsDirectoryLabelVerbatim(t *testing.T) {
	const configured = "  scope:web  "
	configureDirectoryLabel(t, configured)

	in, err := gatherReadyInput(newReadyFlagsCommand(t))
	if err != nil {
		t.Fatalf("gatherReadyInput: %v", err)
	}
	if want := []string{configured}; !slices.Equal(in.filter.LabelsAny, want) {
		t.Errorf("filter.LabelsAny = %q, want %q (the configured value, unnormalized)", in.filter.LabelsAny, want)
	}
}

// TestGatherReadyInputDirectoryLabelDefaultsOnlyWhenNoLabelsGiven pins the two
// halves of the default's gate: an explicit label suppresses it, and a label
// that normalizes away does not.
func TestGatherReadyInputDirectoryLabelDefaultsOnlyWhenNoLabelsGiven(t *testing.T) {
	const configured = "scope:web"

	tests := []struct {
		name          string
		args          []string
		wantLabelsAny []string
	}{
		{"explicit_label_suppresses_default", []string{"--label", "chosen"}, nil},
		{"explicit_label_any_wins_over_default", []string{"--label-any", "chosen"}, []string{"chosen"}},
		{"blank_label_does_not_suppress_default", []string{"--label", "  "}, []string{configured}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			configureDirectoryLabel(t, configured)

			in, err := gatherReadyInput(newReadyFlagsCommand(t, tc.args...))
			if err != nil {
				t.Fatalf("gatherReadyInput: %v", err)
			}
			if got := in.filter.LabelsAny; len(got) != 0 || len(tc.wantLabelsAny) != 0 {
				if !slices.Equal(got, tc.wantLabelsAny) {
					t.Errorf("filter.LabelsAny = %q, want %q", got, tc.wantLabelsAny)
				}
			}
		})
	}
}
