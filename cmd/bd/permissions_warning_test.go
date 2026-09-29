//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLoadEnvironmentStderr runs loadEnvironment against a group-writable
// .beads dir with the given jsonOutput value and returns what it wrote to stderr.
func captureLoadEnvironmentStderr(t *testing.T, json bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	// FindBeadsDir only accepts a dir that holds project files.
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DIR", dir)

	oldJSON := jsonOutput
	jsonOutput = json
	defer func() { jsonOutput = oldJSON }()

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	loadEnvironment()
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)
	return buf.String()
}

func TestPermissionsWarning_HumanOutputStillWarns(t *testing.T) {
	t.Setenv("BD_NO_PERMISSIONS_WARNING", "")
	got := captureLoadEnvironmentStderr(t, false)
	if !strings.Contains(got, "has permissions 0775") {
		t.Errorf("expected permissions warning without --json, got %q", got)
	}
}

func TestPermissionsWarning_SilentWithJSON(t *testing.T) {
	t.Setenv("BD_NO_PERMISSIONS_WARNING", "")
	got := captureLoadEnvironmentStderr(t, true)
	if got != "" {
		t.Errorf("expected no stderr with --json, got %q", got)
	}
}

func TestPermissionsWarning_SilentWithEnvVar(t *testing.T) {
	t.Setenv("BD_NO_PERMISSIONS_WARNING", "1")
	got := captureLoadEnvironmentStderr(t, false)
	if got != "" {
		t.Errorf("expected no stderr with BD_NO_PERMISSIONS_WARNING=1, got %q", got)
	}
}
