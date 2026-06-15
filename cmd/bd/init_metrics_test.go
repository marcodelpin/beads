//go:build cgo

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/metrics"
)

type metricsEvent struct {
	AppName string `json:"app_name"`
	Events  []struct {
		Name       string `json:"name"`
		Attributes []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"attributes"`
	} `json:"events"`
}

func readInitEvent(t *testing.T, home, expectedCommand string) metricsEvent {
	t.Helper()
	dir := filepath.Join(home, ".beads", "eventsData")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read eventsData dir: %v", err)
	}
	var evtqs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".evtq") {
			evtqs = append(evtqs, e.Name())
		}
	}
	if len(evtqs) == 0 {
		t.Fatalf("expected at least 1 .evtq file, got 0")
	}
	for _, name := range evtqs {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read evtq %s: %v", name, err)
		}
		var got metricsEvent
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal evtq %s: %v\n%s", name, err, body)
		}
		if got.commandAttr() == expectedCommand {
			return got
		}
	}
	t.Fatalf("no .evtq with command=%s among %v", expectedCommand, evtqs)
	return metricsEvent{}
}

func (e metricsEvent) commandAttr() string {
	if len(e.Events) == 0 {
		return ""
	}
	for _, a := range e.Events[0].Attributes {
		if a.Key == "command" {
			return a.Value
		}
	}
	return ""
}

func (e metricsEvent) attr(key string) (string, bool) {
	if len(e.Events) == 0 {
		return "", false
	}
	for _, a := range e.Events[0].Attributes {
		if a.Key == key {
			return a.Value, true
		}
	}
	return "", false
}

func metricsTestEnv(home string, extra ...string) []string {
	base := bdEnv(home)
	out := make([]string, 0, len(base)+len(extra)+1)
	for _, e := range base {
		if strings.HasPrefix(e, "BD_DISABLE_METRICS=") {
			continue
		}
		out = append(out, e)
	}
	out = append(out, "BD_DISABLE_EVENT_FLUSH=1")
	return append(out, extra...)
}

func runBdInitForMetrics(t *testing.T, home string, args ...string) {
	t.Helper()
	bd := buildEmbeddedBD(t)
	repo, err := testTempDir("bd-metrics-repo-*")
	if err != nil {
		t.Fatalf("temp repo: %v", err)
	}
	initGitRepoAt(t, repo)

	full := append([]string{"init", "--non-interactive", "--quiet"}, args...)
	cmd := exec.Command(bd, full...)
	cmd.Dir = repo
	cmd.Env = metricsTestEnv(home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()
}

func TestInitMetricsEmittedPerDoltMode(t *testing.T) {
	cases := []struct {
		name            string
		extraArgs       []string
		extraEnv        []string
		expectedCommand string
	}{
		{
			name:            "embedded_default",
			expectedCommand: "init-embedded",
		},
		{
			name:            "server_via_flag",
			extraArgs:       []string{"--server"},
			expectedCommand: "init-server",
		},
		{
			name:            "shared_server_via_flag",
			extraArgs:       []string{"--shared-server"},
			expectedCommand: "init-shared-server",
		},
		{
			name:            "proxied_server_via_flag",
			extraArgs:       []string{"--proxied-server"},
			expectedCommand: "init-proxied-server",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home, err := testTempDir("bd-metrics-home-*")
			if err != nil {
				t.Fatalf("temp home: %v", err)
			}
			runBdInitForMetrics(t, home, tc.extraArgs...)
			evt := readInitEvent(t, home, tc.expectedCommand)

			if evt.AppName != "beads" {
				t.Errorf("app_name = %q, want %q", evt.AppName, "beads")
			}
			if len(evt.Events) != 1 {
				t.Fatalf("events len = %d, want 1", len(evt.Events))
			}
			if evt.Events[0].Name != "cli_command" {
				t.Errorf("event name = %q, want %q", evt.Events[0].Name, "cli_command")
			}
			if got, _ := evt.attr("command"); got != tc.expectedCommand {
				t.Errorf("command attr = %q, want %q", got, tc.expectedCommand)
			}
			if got, ok := evt.attr("dolt_mode"); ok {
				t.Errorf("dolt_mode attr should no longer be set, got %q", got)
			}
		})
	}
}

func writeUserMetricsDisabled(t *testing.T, home string, disabled bool) {
	t.Helper()
	dir := filepath.Join(home, ".config", "bd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir user config: %v", err)
	}
	body := []byte("metrics.disabled: false\n")
	if disabled {
		body = []byte("metrics.disabled: true\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), body, 0o644); err != nil {
		t.Fatalf("write user config: %v", err)
	}
}

func evtqFilesIn(t *testing.T, home string) []string {
	t.Helper()
	dir := filepath.Join(home, ".beads", "eventsData")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".evtq") {
			out = append(out, e.Name())
		}
	}
	return out
}

func runBdInitWithEnv(t *testing.T, home string, extraEnv []string) {
	t.Helper()
	bd := buildEmbeddedBD(t)
	repo, err := testTempDir("bd-metrics-repo-*")
	if err != nil {
		t.Fatalf("temp repo: %v", err)
	}
	initGitRepoAt(t, repo)

	cmd := exec.Command(bd, "init", "--non-interactive", "--quiet")
	cmd.Dir = repo
	cmd.Env = metricsTestEnv(home, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()
}

func TestInitMetricsEnvConfigPrecedence(t *testing.T) {
	cases := []struct {
		name           string
		setUserConfig  bool
		configDisabled bool
		envVar         []string
		wantEvtq       bool
	}{
		{
			name:     "default_no_config_no_env",
			wantEvtq: true,
		},
		{
			name:           "config_disabled_env_unset",
			setUserConfig:  true,
			configDisabled: true,
			wantEvtq:       false,
		},
		{
			name:           "config_enabled_env_disables",
			setUserConfig:  true,
			configDisabled: false,
			envVar:         []string{"BD_DISABLE_METRICS=1"},
			wantEvtq:       false,
		},
		{
			name:           "config_disabled_env_overrides_to_enabled",
			setUserConfig:  true,
			configDisabled: true,
			envVar:         []string{"BD_DISABLE_METRICS=0"},
			wantEvtq:       true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home, err := testTempDir("bd-metrics-prec-*")
			if err != nil {
				t.Fatalf("temp home: %v", err)
			}
			if tc.setUserConfig {
				writeUserMetricsDisabled(t, home, tc.configDisabled)
			}

			runBdInitWithEnv(t, home, tc.envVar)

			files := evtqFilesIn(t, home)
			if tc.wantEvtq && len(files) == 0 {
				t.Errorf("expected an .evtq file, got none")
			}
			if !tc.wantEvtq && len(files) > 0 {
				t.Errorf("expected no .evtq files, got %v", files)
			}
		})
	}
}

func TestInitBootstrapsUserConfigWhenMissing(t *testing.T) {
	home, err := testTempDir("bd-bootstrap-fresh-*")
	if err != nil {
		t.Fatalf("temp home: %v", err)
	}

	runBdInitWithEnv(t, home, nil)

	path := filepath.Join(home, ".config", "bd", "config.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := string(body)
	if !strings.Contains(got, metrics.DefaultEndpoint) {
		t.Errorf("user config missing metrics endpoint.\nfile: %q", got)
	}
	if !strings.Contains(got, "endpoint:") {
		t.Errorf("user config missing endpoint key.\nfile: %q", got)
	}
}

func TestInitPreservesExistingMetricsValuesAndFillsMissingLeaves(t *testing.T) {
	home, err := testTempDir("bd-bootstrap-existing-*")
	if err != nil {
		t.Fatalf("temp home: %v", err)
	}

	dir := filepath.Join(home, ".config", "bd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "config.yaml")
	original := []byte("metrics:\n  endpoint: https://example.invalid/custom\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	runBdInitWithEnv(t, home, nil)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(got)
	if !strings.Contains(body, "https://example.invalid/custom") {
		t.Errorf("user-set endpoint was clobbered.\nfile: %q", body)
	}
	if !strings.Contains(body, "disabled:") {
		t.Errorf("missing metrics.disabled was not filled in.\nfile: %q", body)
	}
}

func TestInitMetricsDisabledSuppresses(t *testing.T) {
	bd := buildEmbeddedBD(t)
	home, err := testTempDir("bd-metrics-disabled-home-*")
	if err != nil {
		t.Fatalf("temp home: %v", err)
	}
	repo, err := testTempDir("bd-metrics-disabled-repo-*")
	if err != nil {
		t.Fatalf("temp repo: %v", err)
	}
	initGitRepoAt(t, repo)

	cmd := exec.Command(bd, "init", "--non-interactive", "--quiet")
	cmd.Dir = repo
	env := append([]string{}, bdEnv(home)...)
	env = append(env, "BD_DISABLE_METRICS=1")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bd init failed: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}

	dir := filepath.Join(home, ".beads", "eventsData")
	if _, err := os.Stat(dir); err == nil {
		entries, _ := os.ReadDir(dir)
		var evtqs []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".evtq") {
				evtqs = append(evtqs, e.Name())
			}
		}
		if len(evtqs) > 0 {
			t.Errorf("BD_DISABLE_METRICS=1 still produced .evtq files: %v", evtqs)
		}
	}
}
