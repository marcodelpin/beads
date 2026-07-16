package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
)

func TestBootstrapRejectsRemovedBackendsBeforeWorkspaceWrites(t *testing.T) {
	bd := buildBDForInitTests(t)

	for _, backend := range []string{configfile.BackendPostgres, configfile.BackendMySQL} {
		backend := backend
		for _, args := range [][]string{{"bootstrap", "--dry-run"}, {"bootstrap", "--yes"}} {
			args := args
			mode := "execute"
			if len(args) > 1 && args[1] == "--dry-run" {
				mode = "dry-run"
			}

			t.Run(backend+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				beadsDir := filepath.Join(root, ".beads")
				if err := os.MkdirAll(beadsDir, 0o700); err != nil {
					t.Fatalf("create .beads: %v", err)
				}

				metadata := []byte(fmt.Sprintf("{\n  \"database\": \"legacy.db\",\n  \"backend\": %q,\n  \"project_id\": \"legacy-project\"\n}\n", backend))
				metadataPath := filepath.Join(beadsDir, configfile.ConfigFileName)
				if err := os.WriteFile(metadataPath, metadata, 0o600); err != nil {
					t.Fatalf("write metadata.json: %v", err)
				}

				cmd := exec.Command(bd, args...)
				cmd.Dir = root
				cmd.Env = bootstrapBackendGuardEnv(root, beadsDir)
				out, err := cmd.CombinedOutput()
				if err == nil {
					t.Errorf("bd %s unexpectedly succeeded for removed backend %q:\n%s", strings.Join(args, " "), backend, out)
				}

				message := strings.ToLower(string(out))
				for _, want := range []string{
					"no longer supported",
					"general-purpose server databases",
					"simple and resource-light",
					"was not opened",
					"export",
					"dolt",
					"sqlite",
				} {
					if !strings.Contains(message, want) {
						t.Errorf("bd %s error for %q missing %q:\n%s", strings.Join(args, " "), backend, want, message)
					}
				}

				after, readErr := os.ReadFile(metadataPath)
				if readErr != nil {
					t.Fatalf("read metadata.json after rejected bootstrap: %v", readErr)
				}
				if !bytes.Equal(after, metadata) {
					t.Errorf("bd %s rewrote metadata.json for removed backend %q:\nbefore:\n%s\nafter:\n%s", strings.Join(args, " "), backend, metadata, after)
				}
				assertNoBootstrapStorageArtifacts(t, beadsDir)
			})
		}
	}
}

func TestBootstrapRejectsUnknownBackendBeforeWorkspaceWrites(t *testing.T) {
	bd := buildBDForInitTests(t)

	for _, args := range [][]string{{"bootstrap", "--dry-run"}, {"bootstrap", "--yes"}} {
		mode := "execute"
		if args[1] == "--dry-run" {
			mode = "dry-run"
		}

		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			beadsDir := filepath.Join(root, ".beads")
			if err := os.MkdirAll(beadsDir, 0o700); err != nil {
				t.Fatalf("create .beads: %v", err)
			}

			metadata := []byte("{\n  \"backend\": \"mystery\",\n  \"project_id\": \"legacy-project\"\n}\n")
			metadataPath := filepath.Join(beadsDir, configfile.ConfigFileName)
			if err := os.WriteFile(metadataPath, metadata, 0o600); err != nil {
				t.Fatalf("write metadata.json: %v", err)
			}

			cmd := exec.Command(bd, args...)
			cmd.Dir = root
			cmd.Env = bootstrapBackendGuardEnv(root, beadsDir)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Errorf("bd %s unexpectedly accepted an unknown backend:\n%s", strings.Join(args, " "), out)
			}
			message := string(out)
			for _, want := range []string{"not recognized", "no storage database was opened or modified", "dolt", "sqlite"} {
				if !strings.Contains(message, want) {
					t.Errorf("bd %s error missing %q:\n%s", strings.Join(args, " "), want, message)
				}
			}

			after, readErr := os.ReadFile(metadataPath)
			if readErr != nil {
				t.Fatalf("read metadata.json after rejected bootstrap: %v", readErr)
			}
			if !bytes.Equal(after, metadata) {
				t.Errorf("bd %s rewrote unknown-backend metadata:\nbefore:\n%s\nafter:\n%s", strings.Join(args, " "), metadata, after)
			}
			assertNoBootstrapStorageArtifacts(t, beadsDir)
		})
	}
}

func TestBootstrapRejectsCorruptMetadataBeforeWorkspaceWrites(t *testing.T) {
	bd := buildBDForInitTests(t)

	for _, args := range [][]string{{"bootstrap", "--dry-run"}, {"bootstrap", "--yes"}} {
		mode := "execute"
		if args[1] == "--dry-run" {
			mode = "dry-run"
		}

		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			beadsDir := filepath.Join(root, ".beads")
			if err := os.MkdirAll(beadsDir, 0o700); err != nil {
				t.Fatalf("create .beads: %v", err)
			}

			metadata := []byte("{\n")
			metadataPath := filepath.Join(beadsDir, configfile.ConfigFileName)
			if err := os.WriteFile(metadataPath, metadata, 0o600); err != nil {
				t.Fatalf("write corrupt metadata.json: %v", err)
			}

			cmd := exec.Command(bd, args...)
			cmd.Dir = root
			cmd.Env = bootstrapBackendGuardEnv(root, beadsDir)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("bd %s unexpectedly ignored corrupt metadata:\n%s", strings.Join(args, " "), out)
			}
			message := strings.ToLower(string(out))
			for _, want := range []string{"metadata.json", "no storage database was opened or modified"} {
				if !strings.Contains(message, want) {
					t.Errorf("bd %s error missing %q:\n%s", strings.Join(args, " "), want, out)
				}
			}

			after, readErr := os.ReadFile(metadataPath)
			if readErr != nil {
				t.Fatalf("read corrupt metadata after rejected bootstrap: %v", readErr)
			}
			if !bytes.Equal(after, metadata) {
				t.Errorf("bd %s rewrote corrupt metadata:\nbefore: %q\nafter:  %q", strings.Join(args, " "), metadata, after)
			}
			assertNoBootstrapStorageArtifacts(t, beadsDir)
		})
	}
}

func TestBootstrapDoesNotConvertExistingSQLiteWorkspace(t *testing.T) {
	bd := buildBDForInitTests(t)

	for _, args := range [][]string{{"bootstrap", "--dry-run"}, {"bootstrap", "--yes"}} {
		args := args
		mode := "execute"
		if len(args) > 1 && args[1] == "--dry-run" {
			mode = "dry-run"
		}

		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			for _, gitArgs := range [][]string{
				{"init", "-q"},
				{"config", "user.email", "test@test.com"},
				{"config", "user.name", "Test"},
				{"config", "core.hooksPath", ".git/hooks"},
			} {
				runGitForBootstrapTest(t, root, gitArgs...)
			}
			cmd := exec.Command(bd, "init", "--quiet", "--backend=sqlite", "--prefix=sqliteguard")
			cmd.Dir = root
			cmd.Env = bootstrapBackendGuardEnv(root, filepath.Join(root, ".beads"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("initialize SQLite workspace: %v\n%s", err, out)
			}

			beadsDir := filepath.Join(root, ".beads")
			metadataPath := filepath.Join(beadsDir, configfile.ConfigFileName)
			databasePath := filepath.Join(beadsDir, "beads.db")
			metadataBefore := readBootstrapGuardFile(t, metadataPath)
			databaseBefore := sha256.Sum256(readBootstrapGuardFile(t, databasePath))
			localVersionBefore, hadLocalVersion := readOptionalBootstrapGuardFile(t, filepath.Join(beadsDir, ".local_version"))

			cmd = exec.Command(bd, args...)
			cmd.Dir = root
			cmd.Env = bootstrapBackendGuardEnv(root, beadsDir)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Errorf("bd %s unexpectedly accepted an existing SQLite workspace:\n%s", strings.Join(args, " "), out)
			}

			message := strings.ToLower(string(out))
			if !strings.Contains(message, "bootstrap") || !strings.Contains(message, "sqlite") ||
				!(strings.Contains(message, "not support") || strings.Contains(message, "unsupported") ||
					strings.Contains(message, "cannot") || strings.Contains(message, "refus")) {
				t.Errorf("bd %s should fail with explicit SQLite bootstrap guidance:\n%s", strings.Join(args, " "), out)
			}

			metadataAfter := readBootstrapGuardFile(t, metadataPath)
			if !bytes.Equal(metadataAfter, metadataBefore) {
				t.Errorf("bd %s rewrote SQLite metadata.json:\nbefore:\n%s\nafter:\n%s", strings.Join(args, " "), metadataBefore, metadataAfter)
			}
			databaseAfter := sha256.Sum256(readBootstrapGuardFile(t, databasePath))
			if databaseAfter != databaseBefore {
				t.Errorf("bd %s modified the existing SQLite database", strings.Join(args, " "))
			}
			localVersionAfter, hasLocalVersion := readOptionalBootstrapGuardFile(t, filepath.Join(beadsDir, ".local_version"))
			if hasLocalVersion != hadLocalVersion || !bytes.Equal(localVersionAfter, localVersionBefore) {
				t.Errorf("bd %s created or modified SQLite workspace version tracking", strings.Join(args, " "))
			}
			for _, name := range []string{"embeddeddolt", "dolt"} {
				path := filepath.Join(beadsDir, name)
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Errorf("bd %s converted SQLite by creating Dolt state at %s (stat error: %v)", strings.Join(args, " "), path, statErr)
				}
			}
		})
	}
}

func bootstrapBackendGuardEnv(home, beadsDir string) []string {
	env := make([]string, 0, len(os.Environ())+7)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "BEADS_") || strings.HasPrefix(entry, "BD_") || strings.HasPrefix(entry, "HOME=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HOME="+home,
		"BEADS_DIR="+beadsDir,
		"BEADS_DOLT_AUTO_START=0",
		"BEADS_NO_DAEMON=1",
		"BEADS_TEST_IGNORE_REPO_CONFIG=1",
		"BD_DISABLE_METRICS=1",
		"BD_DISABLE_EVENT_FLUSH=1",
		"BD_NON_INTERACTIVE=1",
	)
}

func assertNoBootstrapStorageArtifacts(t *testing.T, beadsDir string) {
	t.Helper()
	for _, name := range []string{"embeddeddolt", "dolt", "beads.db", ".local_version"} {
		path := filepath.Join(beadsDir, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("rejected bootstrap created local state at %s (stat error: %v)", path, err)
		}
	}
}

func readBootstrapGuardFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func readOptionalBootstrapGuardFile(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data, true
}
