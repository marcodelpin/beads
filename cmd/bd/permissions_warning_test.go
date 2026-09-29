//go:build cgo && !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/testutil"
)

const permissionsWarningNeedle = "has permissions 0775"

// setupGroupWritableProject builds bd, initializes an embedded-Dolt project in
// a temp dir and makes its .beads dir group-writable (0775), the shape that
// triggers the permissions warning. It returns the binary, the project dir and
// the env to run bd with.
func setupGroupWritableProject(t *testing.T) (string, string, []string) {
	t.Helper()
	testutil.SkipIfShort(t, "builds+spawns the bd binary; skipped in -short")
	bd := buildBDForInitTests(t)
	dir := t.TempDir()
	env := append(envWithout(bdEnv(dir), "BD_NO_PERMISSIONS_WARNING"), "BD_NON_INTERACTIVE=1")

	init := exec.Command(bd, "init", "--prefix", "pw", "--quiet", "--non-interactive", "--skip-hooks", "--skip-agents")
	init.Dir = dir
	init.Env = env
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("bd init failed: %v\n%s", err, out)
	}
	if err := os.Chmod(filepath.Join(dir, ".beads"), 0o775); err != nil {
		t.Fatal(err)
	}
	return bd, dir, env
}

// runBDPermSplit runs bd and returns stdout and stderr separately.
func runBDPermSplit(t *testing.T, bd, dir string, env []string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(bd, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bd %v failed: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// TestPermissionsWarning_SilentForEveryJSONRoute runs the real binary and
// asserts the .beads permissions warning never reaches stderr when JSON output
// is requested, whichever route requests it (edt-donk).
func TestPermissionsWarning_SilentForEveryJSONRoute(t *testing.T) {
	bd, dir, env := setupGroupWritableProject(t)

	cases := []struct {
		name string
		args []string
	}{
		{"where root --json", []string{"where", "--json"}},
		{"list root --json", []string{"list", "--json"}},
		{"version root --json", []string{"version", "--json"}},
		{"config list root --json", []string{"config", "list", "--json"}},
		{"repo list local --json", []string{"repo", "list", "--json"}},
		{"where --format json", []string{"where", "--format", "json"}},
		{"where --format=json", []string{"where", "--format=json"}},
		{"list root --format json", []string{"--format", "json", "list"}},
		{"list local --format json", []string{"list", "--format", "json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := runBDPermSplit(t, bd, dir, env, tc.args...)
			if strings.Contains(stderr, permissionsWarningNeedle) {
				t.Errorf("bd %v: permissions warning on stderr with JSON output:\n%s", tc.args, stderr)
			}
			if s := strings.TrimSpace(stdout); !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") {
				t.Errorf("bd %v: stdout is not JSON:\n%s", tc.args, stdout)
			}
		})
	}
}

// TestPermissionsWarning_SilentWithConfigJSON covers `json: true` set in the
// project config.yaml, with no flag on the command line.
func TestPermissionsWarning_SilentWithConfigJSON(t *testing.T) {
	bd, dir, env := setupGroupWritableProject(t)
	cfg := filepath.Join(dir, ".beads", "config.yaml")
	existing, err := os.ReadFile(cfg)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(existing, []byte("\njson: true\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"where"}, {"list"}} {
		stdout, stderr := runBDPermSplit(t, bd, dir, env, args...)
		if strings.Contains(stderr, permissionsWarningNeedle) {
			t.Errorf("bd %v with config json:true: permissions warning on stderr:\n%s", args, stderr)
		}
		if s := strings.TrimSpace(stdout); !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") {
			t.Errorf("bd %v with config json:true: stdout is not JSON:\n%s", args, stdout)
		}
	}
}

// TestPermissionsWarning_HumanOutputWarnsOnce asserts a human invocation still
// gets the warning, exactly once per invocation.
func TestPermissionsWarning_HumanOutputWarnsOnce(t *testing.T) {
	bd, dir, env := setupGroupWritableProject(t)
	for _, args := range [][]string{{"where"}, {"list"}} {
		_, stderr := runBDPermSplit(t, bd, dir, env, args...)
		if n := strings.Count(stderr, permissionsWarningNeedle); n != 1 {
			t.Errorf("bd %v: want the permissions warning exactly once, got %d:\n%s", args, n, stderr)
		}
	}
}

// TestPermissionsWarning_SilentWithEnvVar asserts BD_NO_PERMISSIONS_WARNING=1
// silences the warning for a human invocation too.
func TestPermissionsWarning_SilentWithEnvVar(t *testing.T) {
	bd, dir, env := setupGroupWritableProject(t)
	env = append(env, "BD_NO_PERMISSIONS_WARNING=1")
	for _, args := range [][]string{{"where"}, {"list"}} {
		_, stderr := runBDPermSplit(t, bd, dir, env, args...)
		if strings.Contains(stderr, permissionsWarningNeedle) {
			t.Errorf("bd %v with BD_NO_PERMISSIONS_WARNING=1: warning on stderr:\n%s", args, stderr)
		}
	}
}
