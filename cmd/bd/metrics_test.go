package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/config"
)

// TestMetricsOnOffWritesUserConfig verifies `bd metrics on/off` persists the
// preference to the user-global config without any manual editing or env var.
func TestMetricsOnOffWritesUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("USERPROFILE", home)

	run := func(c *cobra.Command) {
		t.Helper()
		var buf bytes.Buffer
		c.SetOut(&buf)
		c.SetErr(&buf)
		if err := c.RunE(c, nil); err != nil {
			t.Fatalf("%s.RunE: %v", c.Name(), err)
		}
	}

	run(metricsOffCmd)
	if got := readUserConfigYAML(t); !strings.Contains(got, "disabled: true") {
		t.Errorf("after `metrics off`, user config = %q, want it to contain `disabled: true`", got)
	}

	run(metricsOnCmd)
	if got := readUserConfigYAML(t); !strings.Contains(got, "disabled: false") {
		t.Errorf("after `metrics on`, user config = %q, want it to contain `disabled: false`", got)
	}
}

func readUserConfigYAML(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(config.UserConfigYamlPath())
	if err != nil {
		t.Fatalf("read user config %s: %v", config.UserConfigYamlPath(), err)
	}
	return string(b)
}

// TestMetricsExampleShowsBdCommandEvent verifies `bd metrics example` shows a
// concrete cli_command payload matching the real wire shape, and never claims to
// send anything bd does not actually emit (e.g. Dolt engine events).
func TestMetricsExampleShowsBdCommandEvent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := runMetricsExample(cmd); err != nil {
		t.Fatalf("runMetricsExample: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"cli_command", `"command"`, `"platform"`, `"app_name"`} {
		if !strings.Contains(out, want) {
			t.Errorf("`bd metrics example` output missing %q\n--- output ---\n%s", want, out)
		}
	}
	// We only emit a single cli_command event; never imply otherwise.
	for _, unwanted := range []string{"Dolt engine", "dolt_mode"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("`bd metrics example` should not mention %q (not actually sent)\n--- output ---\n%s", unwanted, out)
		}
	}
	// The example must not be HTML-escaped (no < for the < in the placeholder).
	if strings.Contains(out, `<`) || strings.Contains(out, `&`) {
		t.Errorf("`bd metrics example` output is HTML-escaped; want raw characters\n--- output ---\n%s", out)
	}
}

// TestMetricsFirstRunNoticeSuppressedForMetricsSubcommands ensures the one-time
// notice never fires while the user is already managing metrics.
func TestMetricsFirstRunNoticeSuppressedForMetricsSubcommands(t *testing.T) {
	parent := &cobra.Command{Use: "metrics"}
	for _, name := range []string{"on", "off", "example"} {
		sub := &cobra.Command{Use: name}
		parent.AddCommand(sub)
	}
	// The parent itself and each subcommand must be treated as "metrics" context.
	if got := isMetricsCommandContext(parent); !got {
		t.Errorf("parent metrics command should be metrics context")
	}
	for _, sub := range parent.Commands() {
		if got := isMetricsCommandContext(sub); !got {
			t.Errorf("`metrics %s` should be metrics context", sub.Name())
		}
	}
	other := &cobra.Command{Use: "ready"}
	if isMetricsCommandContext(other) {
		t.Errorf("`ready` should not be metrics context")
	}
}
