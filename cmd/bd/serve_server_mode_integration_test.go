//go:build cgo

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
)

// `bd serve` against a SERVER-MODE workspace — `bd init --server` pointed at a
// dolt sql-server this process did not start and does not own.
//
// That topology is the reason this file exists rather than another case in the
// proxied test: a server-mode workspace has no proxied-server sidecar to read a
// provider out of, so PersistentPreRunE builds a DoltStore and no unit-of-work
// provider at all. Everything the HTTP surface answers here is answered through
// a provider serve built itself, from the workspace's Dolt connection settings.

// serverModeProject is a `bd init --server` workspace pointed at the shared
// test Dolt container.
type serverModeProject struct {
	dir      string
	beadsDir string
	database string
	env      []string
}

func newServerModeProject(t *testing.T, bd, prefix string) serverModeProject {
	t.Helper()
	// The shared container IS an externally-managed dolt sql-server: nothing
	// about it is proxied. The gate env var is named for the suite that
	// introduced it, not for the topology.
	port := requireSharedProxiedServer(t)

	dir := t.TempDir()
	initGitRepoAt(t, dir)
	beadsDir := filepath.Join(dir, ".beads")
	database := uniqueProxiedDatabase()
	env := bdEnv(dir)

	// serve fronts the server through a proxy child of its own; without this
	// the child outlives the test by its idle timeout.
	proxyRoot := filepath.Join(beadsDir, "dolt")
	t.Cleanup(func() {
		if err := proxy.Shutdown(proxyRoot); err != nil {
			t.Logf("proxy.Shutdown(%s): %v", proxyRoot, err)
		}
	})

	cmd := exec.Command(bd, "init", "--quiet", "--server",
		"--server-host", "127.0.0.1",
		"--server-port", strconv.Itoa(port),
		"--database", database,
		"--prefix", prefix,
		"--non-interactive", "--skip-agents", "--skip-hooks")
	cmd.Dir = dir
	cmd.Env = env
	stdout, stderr, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd init --server failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	return serverModeProject{dir: dir, beadsDir: beadsDir, database: database, env: env}
}

func (p serverModeProject) run(t *testing.T, bd string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bd, args...)
	cmd.Dir = p.dir
	cmd.Env = p.env
	stdout, stderr, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd %s failed: %v\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func TestServerModeServe(t *testing.T) {
	requireSharedProxiedServer(t)
	t.Parallel()
	bd := buildEmbeddedBD(t)
	p := newServerModeProject(t, bd, "srvm")

	issue := strings.TrimSpace(p.run(t, bd, "create", "--silent", "server mode work"))
	if issue == "" {
		t.Fatal("bd create returned no issue id")
	}

	sp := startServe(t, bd, p.dir, p.env)

	startup := sp.awaitLogLine(t, "event=startup")
	// The mode label is what tells an operator which topology this process
	// attached to, and a server-mode workspace's Dolt server is external to it
	// even when Beads is what started it — serve fronts one, never spawns one.
	for _, want := range []string{`mode="server (external dolt)"`, "database=" + p.database, "beads_dir=" + p.beadsDir} {
		if !strings.Contains(startup, want) {
			t.Errorf("startup line is missing %q:\n%s", want, startup)
		}
	}

	// Pool limits are optional on the provider interface, and the server says so
	// out loud when they cannot be applied. This mode must not be the one where
	// that fires: an unbounded request burst against a SHARED dolt sql-server
	// spends somebody else's max_connections budget.
	limits := sp.awaitLogLine(t, "event=limits")
	if !strings.Contains(limits, "pool_max_open=") {
		t.Errorf("limits line is missing the pool cap:\n%s", limits)
	}
	if strings.Contains(sp.stderr.String(), "pool_limits_unavailable") {
		t.Errorf("the provider built for a server-mode workspace does not take pool limits:\n%s", sp.stderr.String())
	}

	t.Run("healthz", func(t *testing.T) {
		status, body, header := sp.get(t, "/healthz")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if body["status"] != "ok" {
			t.Errorf("body = %v", body)
		}
		if got := header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	})

	t.Run("context", func(t *testing.T) {
		status, body, _ := sp.get(t, "/v0/beads/context")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if body["dolt_mode"] != "server" {
			t.Errorf("dolt_mode = %v, want server", body["dolt_mode"])
		}
		if body["database"] != p.database {
			t.Errorf("database = %v, want %q", body["database"], p.database)
		}
		if body["api_version"] != "v0" {
			t.Errorf("api_version = %v, want v0", body["api_version"])
		}
		caps, ok := body["capabilities"].([]any)
		if !ok {
			t.Fatalf("capabilities = %#v, want an array", body["capabilities"])
		}
		// Same build, same handlers: what a mode changes is where the data lives,
		// never what the handshake advertises.
		if len(caps) != 0 {
			t.Errorf("capabilities = %v, want empty while the read and claim handlers are stubs", caps)
		}
	})

	t.Run("unimplemented operations refuse in vocabulary", func(t *testing.T) {
		status, body, header := sp.get(t, "/v0/beads/ready")
		if status != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", status)
		}
		if got := header.Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
			t.Errorf("Content-Type = %q, want problem+json", got)
		}
		if body["request_id"] == nil {
			t.Error("no request_id to correlate with the log line")
		}
	})

	// The proxy serve fronts the server through must survive every quiet period
	// the process can have. Its only client is serve's pool, which drops its last
	// connection after ConnMaxIdleTime (5m) of no requests; a proxy with a finite
	// idle timeout then exits and takes the OS-assigned port the provider's DSN
	// pinned at construction, permanently, with /healthz still green.
	//
	// Asserted on the spawned child's own command line because the failure is
	// otherwise only visible after five idle minutes, which no test can wait for.
	assertProxyChildNeverIdles(t, filepath.Join(p.beadsDir, "dolt"))

	sp.shutdown(t)

	// The workspace is still the CLI's afterwards. Building a provider against a
	// server-mode workspace runs the unit-of-work schema init on a database the
	// DoltStore also manages; this is the check that the two agree.
	if out := p.run(t, bd, "list"); !strings.Contains(out, issue) {
		t.Errorf("bd list no longer shows %s after bd serve ran:\n%s", issue, out)
	}
}

// assertProxyChildNeverIdles reads the running proxy's pid record and checks the
// process was started with no idle timeout.
func assertProxyChildNeverIdles(t *testing.T, proxyRoot string) {
	t.Helper()

	record, err := os.ReadFile(filepath.Join(proxyRoot, "proxy.pid"))
	if err != nil {
		t.Fatalf("read the proxy pid record: %v", err)
	}
	var pid struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(record, &pid); err != nil {
		t.Fatalf("parse %s: %v", record, err)
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid.PID), "cmdline"))
	if err != nil {
		t.Skipf("cannot read the proxy child's command line on this platform: %v", err)
	}

	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	var idle string
	for i, a := range args {
		if a == "--idle-timeout" && i+1 < len(args) {
			idle = args[i+1]
		}
	}
	if want := proxy.IdleTimeoutNever.String(); idle != want {
		t.Errorf("proxy child --idle-timeout = %q, want %q: a finite timeout lets the proxy exit during a quiet period and strands serve on a dead port\nargv: %v",
			idle, want, args)
	}
}

// TestServerModeServeSkipsPostRunMaintenance is the behavioral half of the
// exclusion. In server mode `bd serve` takes the PersistentPostRunE branch that
// runs auto-commit, backup, export and push — so without the exclusion, SIGTERM
// would fire all of it, attributing a whole process lifetime of requests to the
// shutdown and pushing from inside a signal handler.
//
// The throttle state is deleted before each half, which makes an export
// unconditionally due: absence afterwards is a decision, not a coincidence.
func TestServerModeServeSkipsPostRunMaintenance(t *testing.T) {
	requireSharedProxiedServer(t)
	t.Parallel()
	bd := buildEmbeddedBD(t)
	p := newServerModeProject(t, bd, "srvmm")

	p.run(t, bd, "config", "set", "export.auto", "true")

	jsonl := filepath.Join(p.beadsDir, "issues.jsonl")
	state := filepath.Join(p.beadsDir, "export-state.json")
	clearExportState := func() {
		if err := os.Remove(jsonl); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", jsonl, err)
		}
		if err := os.Remove(state); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", state, err)
		}
	}

	// Control: the same workspace, the same config, a command that does run the
	// maintenance net. Without this the assertion below would also pass on a
	// workspace where auto-export was simply never going to fire.
	clearExportState()
	p.run(t, bd, "create", "--silent", "export control")
	if _, err := os.Stat(jsonl); err != nil {
		t.Fatalf("auto-export did not fire for bd create (%v); the serve assertion would prove nothing", err)
	}

	clearExportState()
	sp := startServe(t, bd, p.dir, p.env)
	if status, _, _ := sp.get(t, "/healthz"); status != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", status)
	}
	sp.shutdown(t)

	if _, err := os.Stat(jsonl); err == nil {
		t.Error("bd serve ran the auto-export; a server must not export and push on the way out of a signal handler")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", jsonl, err)
	}
}
