package dolt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/config"
)

// The open-retry budget (dolt.open-retry-budget, GH#4379) wraps newServerMode's
// fail-fast pre-dial probe in a bounded retry for servers bd does not manage.
// These tests stub serverDial so they run without a live dolt sql-server.

const (
	externalHost = "dolt.example.internal"
	externalAddr = "dolt.example.internal:3306"
	// A port that is neither a production port nor the auto-start default, so
	// the BEADS_TEST_MODE guards in New leave it alone.
	externalTestPort = 43211
)

// errConnRefused is retryable per isRetryableError ("connection refused").
var errConnRefused = errors.New("dial tcp 10.0.0.9:3306: connect: connection refused")

// errNoSuchHost is NOT retryable: no substring isRetryableError recognizes.
var errNoSuchHost = errors.New("dial tcp: lookup dolt.example.internal: no such host")

type fakeConn struct{ net.Conn }

// dialRecord is what a stubbed dial saw: how many times it was called and the
// per-attempt timeout it was handed. The timeouts are what prove the budget is
// a deadline rather than a sleep bound.
type dialRecord struct {
	attempts int
	timeouts []time.Duration
}

// stubServerDial replaces serverDial with one that fails failures times before
// succeeding. A negative failures count never succeeds. sleep, when positive,
// is how long each attempt blocks before answering, capped at the timeout it
// was given, so a stub can imitate an unresponsive endpoint.
func stubServerDial(t *testing.T, failures int, err error, sleep time.Duration) *dialRecord {
	t.Helper()
	orig := serverDial
	t.Cleanup(func() { serverDial = orig })
	rec := &dialRecord{}
	serverDial = func(ctx context.Context, _, _ string, timeout time.Duration) (net.Conn, error) {
		rec.attempts++
		rec.timeouts = append(rec.timeouts, timeout)
		if sleep > 0 {
			block := sleep
			if timeout > 0 && timeout < block {
				block = timeout
			}
			timer := time.NewTimer(block)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		if failures < 0 || rec.attempts <= failures {
			return nil, err
		}
		return &fakeConn{}, nil
	}
	return rec
}

// stubServerDialErrs replaces serverDial with one that returns errs[i] for
// attempt i, repeating the last entry once the sequence is exhausted. Ending a
// sequence with a NON-retryable error lets a test assert that a retry happened
// without paying for the whole budget.
func stubServerDialErrs(t *testing.T, errs ...error) *dialRecord {
	t.Helper()
	orig := serverDial
	t.Cleanup(func() { serverDial = orig })
	rec := &dialRecord{}
	serverDial = func(_ context.Context, _, _ string, timeout time.Duration) (net.Conn, error) {
		rec.attempts++
		rec.timeouts = append(rec.timeouts, timeout)
		i := rec.attempts - 1
		if i >= len(errs) {
			i = len(errs) - 1
		}
		return nil, errs[i]
	}
	return rec
}

// unmanagedCfg is a config in the mode the budget targets: a server bd neither
// owns nor can start, reached over TCP.
func unmanagedCfg(budget time.Duration) *Config {
	return &Config{
		ServerMode:      true,
		ServerHost:      externalHost,
		ServerPort:      3306,
		OpenRetryBudget: budget,
	}
}

// (a) Budget off: exactly one dial attempt and the error returned unwrapped.
func TestOpenRetryBudgetOff_SingleAttemptErrorUnchanged(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
	}{
		{"budget unset", unmanagedCfg(0)},
		{"budget zero explicitly", unmanagedCfg(0)},
		{"budget negative", unmanagedCfg(-5 * time.Second)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := stubServerDial(t, -1, errConnRefused, 0)
			conn, err := dialServerPreflight(context.Background(), tc.cfg, "tcp", externalAddr, time.Millisecond)
			if conn != nil {
				t.Errorf("expected no connection, got %v", conn)
			}
			if rec.attempts != 1 {
				t.Errorf("expected exactly 1 dial attempt with the budget off, got %d", rec.attempts)
			}
			// Byte-identical: not merely errors.Is-compatible, the very same
			// error value with no wrapping text added.
			if err != errConnRefused {
				t.Errorf("expected the dial error returned unwrapped, got %v", err)
			}
			if strings.Contains(err.Error(), "open-retry-budget") {
				t.Errorf("fail-fast error must not mention the budget: %v", err)
			}
			// The probe timeout is passed through untouched when the budget is
			// off; only an enabled budget may shorten it.
			if len(rec.timeouts) != 1 || rec.timeouts[0] != time.Millisecond {
				t.Errorf("expected the caller's probe timeout unchanged, got %v", rec.timeouts)
			}
		})
	}
}

// (b) Budget on and the server answers on attempt N within the budget: the
// open succeeds and costs exactly N attempts. The count is fixed by the stub,
// not by timing, so it holds under -count=N.
func TestOpenRetryBudgetOn_SucceedsOnLaterAttempt(t *testing.T) {
	for _, wantAttempts := range []int{2, 3} {
		t.Run(fmt.Sprintf("succeeds on attempt %d", wantAttempts), func(t *testing.T) {
			rec := stubServerDial(t, wantAttempts-1, errConnRefused, 0)
			// 30s is far above the worst-case cost of two backoff sleeps
			// (~1.9s), so the budget cannot end the loop early here.
			conn, err := dialServerPreflight(context.Background(), unmanagedCfg(30*time.Second), "tcp", externalAddr, time.Millisecond)
			if err != nil {
				t.Fatalf("expected success within the budget, got %v", err)
			}
			if conn == nil {
				t.Fatal("expected a connection on success")
			}
			if rec.attempts != wantAttempts {
				t.Errorf("expected %d dial attempts, got %d", wantAttempts, rec.attempts)
			}
		})
	}
}

// (c) Budget on and never reachable: the open fails after a BOUNDED number of
// attempts, within roughly the budget, and the error names the budget while
// still wrapping the underlying dial error.
func TestOpenRetryBudgetOn_ExhaustsBudget(t *testing.T) {
	const (
		budget       = 750 * time.Millisecond
		probeTimeout = time.Millisecond
	)
	rec := stubServerDial(t, -1, errConnRefused, 0)
	start := time.Now()
	conn, err := dialServerPreflight(context.Background(), unmanagedCfg(budget), "tcp", externalAddr, probeTimeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected failure once the budget is spent")
	}
	if conn != nil {
		t.Errorf("expected no connection, got %v", conn)
	}
	if !errors.Is(err, errConnRefused) {
		t.Errorf("expected the dial error to remain in the chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "dolt.open-retry-budget=") {
		t.Errorf("expected the error to name the budget, got %v", err)
	}
	if rec.attempts < 2 {
		t.Errorf("expected the budget to buy at least one retry, got %d attempts", rec.attempts)
	}
	// Bounded: the loop must terminate on the budget, not spin. The ceiling is
	// deliberately loose (the backoff is jittered) -- it fails a runaway loop,
	// not a slow machine.
	if rec.attempts > 12 {
		t.Errorf("expected a bounded attempt count, got %d", rec.attempts)
	}
	// The budget is a DEADLINE on the whole probe: at worst it overruns by the
	// last dial's own timeout plus scheduling slack. A stray sleep after
	// exhaustion, or a sleep that crosses the deadline, fails here.
	if ceiling := budget + probeTimeout + 250*time.Millisecond; elapsed > ceiling {
		t.Errorf("expected the probe to end within %s (budget %s), took %s", ceiling, budget, elapsed)
	}
}

// The deadline covers the DIALS, not only the sleeps between them. With an
// endpoint that swallows every connection until its timeout, a budget smaller
// than the probe timeout must still be respected: the loop may not spend a
// full probe timeout per attempt on top of the budget.
func TestOpenRetryBudgetOn_DeadlineCapsEachDial(t *testing.T) {
	const (
		budget       = 200 * time.Millisecond
		probeTimeout = 500 * time.Millisecond
	)
	// Every attempt blocks for as long as it is allowed to.
	rec := stubServerDial(t, -1, errConnRefused, time.Hour)
	start := time.Now()
	_, err := dialServerPreflight(context.Background(), unmanagedCfg(budget), "tcp", externalAddr, probeTimeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected failure once the budget is spent")
	}
	if rec.attempts == 0 {
		t.Fatal("expected at least one dial attempt")
	}
	for i, to := range rec.timeouts {
		if to > probeTimeout {
			t.Errorf("attempt %d got timeout %s, above the caller's %s", i+1, to, probeTimeout)
		}
		if to <= 0 {
			t.Errorf("attempt %d got a non-positive timeout %s, which means NO timeout at all", i+1, to)
		}
	}
	// Without a deadline over the dials this costs at least 2 x 500ms.
	if ceiling := budget + 250*time.Millisecond; elapsed > ceiling {
		t.Errorf("expected the probe to end within %s (budget %s), took %s", ceiling, budget, elapsed)
	}
}

// A non-retryable failure ends the budget immediately and surfaces exactly as
// it would have without the budget. Retrying a DNS failure for 30s would be a
// worse UX than today's fail-fast, not a better one.
func TestOpenRetryBudgetOn_NonRetryableFailsImmediately(t *testing.T) {
	rec := stubServerDial(t, -1, errNoSuchHost, 0)
	_, err := dialServerPreflight(context.Background(), unmanagedCfg(30*time.Second), "tcp", externalAddr, time.Millisecond)
	if rec.attempts != 1 {
		t.Errorf("expected exactly 1 attempt for a non-retryable error, got %d", rec.attempts)
	}
	if err != errNoSuchHost {
		t.Errorf("expected the non-retryable error returned unwrapped, got %v", err)
	}
}

// The maintainer's explicit ask. With the budget CONFIGURED, every mode whose
// remedy is something other than waiting takes exactly one dial attempt and
// returns the fail-fast error.
func TestOpenRetryBudgetDoesNotEngageInManagedModes(t *testing.T) {
	const budget = 30 * time.Second
	cases := []struct {
		name string
		cfg  *Config
	}{
		{
			// Embedded: the store factory routes these to
			// internal/storage/embeddeddolt and they never reach
			// newServerMode. An embedded config's shape -- no configured
			// host, auto-start on, a local data path -- is a bd-managed
			// local server, so the gate is false even if one arrived here.
			name: "embedded shape (no host, auto-start, local path)",
			cfg: &Config{
				ServerHost:      "",
				Path:            "/tmp/does-not-matter/.beads/dolt",
				AutoStart:       true,
				OpenRetryBudget: budget,
			},
		},
		{
			// Localhost-managed / auto-start: EnsureRunningDetailed
			// recovers by starting a server, so waiting is the wrong remedy.
			name: "bd-managed localhost (127.0.0.1, auto-start)",
			cfg: &Config{
				ServerMode:      true,
				ServerHost:      "127.0.0.1",
				ServerPort:      3308,
				Path:            "/tmp/does-not-matter/.beads/dolt",
				AutoStart:       true,
				OpenRetryBudget: budget,
			},
		},
		{
			name: "bd-managed localhost (localhost, auto-start)",
			cfg: &Config{
				ServerMode:      true,
				ServerHost:      "localhost",
				ServerPort:      3308,
				Path:            "/tmp/does-not-matter/.beads/dolt",
				AutoStart:       true,
				OpenRetryBudget: budget,
			},
		},
		{
			// Unix socket: a local server the operator manages directly.
			name: "unix socket",
			cfg: &Config{
				ServerMode:      true,
				ServerSocket:    "/tmp/mysql.sock",
				ServerHost:      externalHost,
				ServerPort:      3306,
				OpenRetryBudget: budget,
			},
		},
		{
			// A proxied server owns its own connection lifecycle.
			name: "proxied server",
			cfg: &Config{
				ServerMode:      true,
				ProxiedServer:   true,
				ServerHost:      externalHost,
				ServerPort:      3306,
				OpenRetryBudget: budget,
			},
		},
		{
			// Strict --readonly and the diagnostic opens suppress every
			// implicit recovery. Waiting is one of them.
			name: "diagnostic open (DisableAutoStart)",
			cfg: &Config{
				ServerMode:       true,
				ServerHost:       externalHost,
				ServerPort:       3306,
				DisableAutoStart: true,
				OpenRetryBudget:  budget,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if openRetryEnabled(tc.cfg) {
				t.Fatal("openRetryEnabled must be false in this mode even with a budget configured")
			}
			rec := stubServerDial(t, -1, errConnRefused, 0)
			start := time.Now()
			_, err := dialServerPreflight(context.Background(), tc.cfg, "tcp", externalAddr, time.Millisecond)
			if rec.attempts != 1 {
				t.Errorf("expected exactly 1 dial attempt (no retry), got %d", rec.attempts)
			}
			if err != errConnRefused {
				t.Errorf("expected the fail-fast error unchanged, got %v", err)
			}
			// A retry would have cost at least one backoff sleep (>=250ms).
			if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
				t.Errorf("expected no backoff sleep, took %s", elapsed)
			}
		})
	}
}

// A server on localhost that bd does NOT manage -- an explicit
// dolt_server_port resolves to ServerModeExternal, which suppresses
// auto-start -- is exactly what the budget is for. Keying the gate on the host
// alone would have excluded it.
func TestOpenRetryBudgetEngagesForExternallyManagedLocalhost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		t.Run(host, func(t *testing.T) {
			cfg := &Config{
				ServerMode:      true,
				ServerHost:      host,
				ServerPort:      3307,
				Path:            "/tmp/does-not-matter/.beads/dolt",
				AutoStart:       false, // resolveAutoStart: mode == ServerModeExternal
				OpenRetryBudget: 750 * time.Millisecond,
			}
			if !openRetryEnabled(cfg) {
				t.Fatal("an externally managed localhost server must honour the budget")
			}
			rec := stubServerDial(t, 1, errConnRefused, 0)
			conn, err := dialServerPreflight(context.Background(), cfg, "tcp", "127.0.0.1:3307", time.Millisecond)
			if err != nil {
				t.Fatalf("expected the retry to succeed, got %v", err)
			}
			if conn == nil {
				t.Fatal("expected a connection on success")
			}
			if rec.attempts != 2 {
				t.Errorf("expected 2 dial attempts, got %d", rec.attempts)
			}
		})
	}
}

// The budget's gate and the auto-start path's gate must be mutually exclusive
// by construction, not by ordering inside newServerMode: no config may satisfy
// both. This is what lets the managed-mode cases above be statements about the
// code rather than about the branch order.
func TestOpenRetryAndAutoStartGatesAreMutuallyExclusive(t *testing.T) {
	hosts := []string{
		"", "localhost", "LOCALHOST", " localhost ", "127.0.0.1", "::1", "[::1]",
		"0.0.0.0", "dolt.example.internal", "10.0.0.9", "hosted.doltdb.com",
	}
	for _, host := range hosts {
		for _, autoStart := range []bool{true, false} {
			for _, path := range []string{"", "/tmp/does-not-matter/.beads/dolt"} {
				cfg := &Config{
					ServerMode:      true,
					ServerHost:      host,
					ServerPort:      3306,
					Path:            path,
					AutoStart:       autoStart,
					OpenRetryBudget: 30 * time.Second,
				}
				if openRetryEnabled(cfg) && serverOpenCanAutoStart(cfg) {
					t.Errorf("host %q (auto-start %v, path %q) enables both the open-retry budget and auto-start",
						host, autoStart, path)
				}
			}
		}
	}
}

// An already-cancelled context buys no dial at all, and the cancellation is
// identifiable with errors.Is rather than only readable in the message.
func TestOpenRetryBudgetOn_CancelledContextDialsNothing(t *testing.T) {
	rec := stubServerDial(t, -1, errConnRefused, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := dialServerPreflight(ctx, unmanagedCfg(30*time.Second), "tcp", externalAddr, time.Millisecond)
	if err == nil {
		t.Fatal("expected an error on a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if rec.attempts != 0 {
		t.Errorf("expected no dial attempt on an already-cancelled context, got %d", rec.attempts)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("expected prompt cancellation, took %s", elapsed)
	}
}

// A context cancelled DURING the loop ends it promptly, and both causes stay
// identifiable: the dial failure that made it retry and the context error that
// ended it.
func TestOpenRetryBudgetOn_CancellationDuringLoopStops(t *testing.T) {
	rec := stubServerDial(t, -1, errConnRefused, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, err := dialServerPreflight(ctx, unmanagedCfg(30*time.Second), "tcp", externalAddr, time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when the context is cancelled mid-loop")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if !errors.Is(err, errConnRefused) {
		t.Errorf("expected the dial error to remain in the chain, got %v", err)
	}
	if rec.attempts == 0 {
		t.Error("expected at least one dial before the cancellation")
	}
	// Far below the 30s budget: the loop stopped on the context, not on it.
	if elapsed > 5*time.Second {
		t.Errorf("expected prompt cancellation, took %s", elapsed)
	}
}

// newOpenRetryBackoff must never hand back an unbounded schedule, whatever it
// is given.
func TestNewOpenRetryBackoffIsAlwaysBounded(t *testing.T) {
	for _, budget := range []time.Duration{-time.Second, 0, time.Millisecond, 45 * time.Second} {
		bo := newOpenRetryBackoff(budget)
		if bo.MaxElapsedTime <= 0 {
			t.Errorf("budget %s produced an unbounded backoff (MaxElapsedTime=%s)", budget, bo.MaxElapsedTime)
		}
		if budget > 0 && bo.MaxElapsedTime != budget {
			t.Errorf("budget %s not applied, got MaxElapsedTime=%s", budget, bo.MaxElapsedTime)
		}
		if budget <= 0 && bo.MaxElapsedTime != serverRetryMaxElapsed {
			t.Errorf("non-positive budget %s should fall back to %s, got %s", budget, serverRetryMaxElapsed, bo.MaxElapsedTime)
		}
	}
}

// ---------------------------------------------------------------------------
// Production wiring: the tests above drive dialServerPreflight directly, which
// cannot see whether the production open path calls it or whether the config
// key is ever read. These drive dolt.New -- the function every CLI command
// reaches through cmd/bd's store factory -- so reverting either
// dialServerPreflight call site to net.DialTimeout, or dropping the config
// resolution from New, turns them red.
// ---------------------------------------------------------------------------

// newBeadsDir writes <dir>/config.yaml with the given dolt.open-retry-budget
// value (empty means no key at all) and returns the .beads directory.
func newBeadsDir(t *testing.T, budget string) string {
	t.Helper()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "dolt:\n  auto-start: false\n"
	if budget != "" {
		body += "  open-retry-budget: " + budget + "\n"
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	return beadsDir
}

// productionCfg is what cmd/bd hands dolt.New for an unmanaged server: a
// beadsDir, a data path under it, an explicit endpoint, and no auto-start.
// OpenRetryBudget is deliberately left unset -- New must resolve it.
func productionCfg(beadsDir string) *Config {
	return &Config{
		ServerMode: true,
		BeadsDir:   beadsDir,
		Path:       filepath.Join(beadsDir, "dolt"),
		Database:   "openretry_probe",
		ServerHost: externalHost,
		ServerPort: externalTestPort,
	}
}

// isolateOpenEnv keeps New's guards and the config singleton away from any
// real server or user config.
func isolateOpenEnv(t *testing.T) {
	t.Helper()
	// Disables the circuit breaker (which would otherwise write state files)
	// and the auto-start machinery. The port above is not a production port,
	// so New's test-mode port panic does not fire.
	t.Setenv("BEADS_TEST_MODE", "1")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_SOCKET", "")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "")
	config.ResetForTesting()
	t.Cleanup(config.ResetForTesting)
}

// (a) Through New: the key in the opened project's config.yaml enables the
// budget, and the open retries.
func TestNew_ReadsOpenRetryBudgetFromProjectConfig(t *testing.T) {
	isolateOpenEnv(t)
	beadsDir := newBeadsDir(t, "1200ms")
	rec := stubServerDial(t, -1, errConnRefused, 0)

	_, err := New(context.Background(), productionCfg(beadsDir))
	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	if !errors.Is(err, errConnRefused) {
		t.Errorf("expected the dial error in the chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "dolt.open-retry-budget=") {
		t.Errorf("expected the error to name the exhausted budget, got %v", err)
	}
	if rec.attempts < 2 {
		t.Errorf("expected New to retry the probe, got %d dial attempts", rec.attempts)
	}
}

// (b) Through New with no key configured: exactly one dial, and the error is
// the fail-fast one -- no mention of a budget anywhere in the chain.
func TestNew_WithoutOpenRetryBudgetDialsOnce(t *testing.T) {
	isolateOpenEnv(t)
	beadsDir := newBeadsDir(t, "")
	rec := stubServerDial(t, -1, errConnRefused, 0)

	start := time.Now()
	_, err := New(context.Background(), productionCfg(beadsDir))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	if !errors.Is(err, errConnRefused) {
		t.Errorf("expected the dial error in the chain, got %v", err)
	}
	if strings.Contains(err.Error(), "open-retry-budget") {
		t.Errorf("the default open must not mention the budget: %v", err)
	}
	if rec.attempts != 1 {
		t.Errorf("expected exactly 1 dial attempt by default, got %d", rec.attempts)
	}
	if elapsed > time.Second {
		t.Errorf("expected a fail-fast open, took %s", elapsed)
	}
}

// (c) Precedence: an ambient budget from the config singleton (initialized
// from a DIFFERENT workspace, via BEADS_DIR) must not override an explicit
// "0" in the directory actually being opened.
func TestNew_ProjectConfigZeroOverridesAmbientBudget(t *testing.T) {
	isolateOpenEnv(t)
	otherWorkspace := newBeadsDir(t, "30s")
	t.Setenv("BEADS_DIR", otherWorkspace)
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
	if got := config.GetString("dolt.open-retry-budget"); got != "30s" {
		t.Fatalf("precondition: ambient config should carry 30s, got %q", got)
	}

	target := newBeadsDir(t, "0")
	rec := stubServerDial(t, -1, errConnRefused, 0)

	_, err := New(context.Background(), productionCfg(target))
	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	if rec.attempts != 1 {
		t.Errorf("an explicit 0 in the opened project must disable the budget, got %d dial attempts", rec.attempts)
	}
	if strings.Contains(err.Error(), "open-retry-budget") {
		t.Errorf("a disabled budget must not appear in the error: %v", err)
	}
}

// The mirror of (c): with nothing configured in the opened project, the
// ambient budget still applies, so the directory read is a precedence rule and
// not a replacement for the global default.
func TestNew_AmbientBudgetAppliesWhenProjectIsSilent(t *testing.T) {
	isolateOpenEnv(t)
	otherWorkspace := newBeadsDir(t, "30s")
	t.Setenv("BEADS_DIR", otherWorkspace)
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}

	target := newBeadsDir(t, "")
	// The second attempt fails non-retryably, so the loop ends there instead
	// of spending the ambient 30s budget.
	rec := stubServerDialErrs(t, errConnRefused, errNoSuchHost)

	_, err := New(context.Background(), productionCfg(target))
	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	if !errors.Is(err, errNoSuchHost) {
		t.Errorf("expected the second attempt's error to surface, got %v", err)
	}
	if rec.attempts != 2 {
		t.Errorf("expected the ambient budget to buy exactly one retry, got %d dial attempts", rec.attempts)
	}
}

// An explicit cfg.OpenRetryBudget outranks every config file.
func TestResolveOpenRetryBudgetPrecedence(t *testing.T) {
	isolateOpenEnv(t)
	dirWith30s := newBeadsDir(t, "30s")
	dirWithZero := newBeadsDir(t, "0")
	dirSilent := newBeadsDir(t, "")

	cases := []struct {
		name     string
		cfg      *Config
		beadsDir string
		want     time.Duration
	}{
		{"caller override wins over the directory", &Config{OpenRetryBudget: 5 * time.Second}, dirWith30s, 5 * time.Second},
		{"caller override wins over a disabling directory", &Config{OpenRetryBudget: 5 * time.Second}, dirWithZero, 5 * time.Second},
		{"directory value is used", &Config{}, dirWith30s, 30 * time.Second},
		{"directory zero disables", &Config{}, dirWithZero, 0},
		{"silent directory falls through", &Config{}, dirSilent, 0},
		{"bare seconds are accepted", &Config{}, newBeadsDir(t, "45"), 45 * time.Second},
		{"unparseable reads as off", &Config{}, newBeadsDir(t, "\"soon\""), 0},
		{"no directory at all", &Config{}, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveOpenRetryBudget(tc.cfg, tc.beadsDir); got != tc.want {
				t.Errorf("resolveOpenRetryBudget = %s, want %s", got, tc.want)
			}
		})
	}
}
