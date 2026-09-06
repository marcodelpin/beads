package dolt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
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

// errIOTimeout reproduces what net.DialTimeout returns when its own timeout
// expires: an "i/o timeout" whose Is method reports true for
// context.DeadlineExceeded (net's unexported timeoutError, net/net.go). It is a
// SERVER failure -- the endpoint never answered -- that any guard written as an
// errors.Is(err, context.DeadlineExceeded) test would misread as the caller
// giving up.
type errIOTimeout struct{}

func (errIOTimeout) Error() string { return "dial tcp 10.0.0.9:3306: i/o timeout" }

func (errIOTimeout) Timeout() bool { return true }

func (errIOTimeout) Is(target error) bool { return target == context.DeadlineExceeded }

// recordingConn is a net.Conn that records what was done to it before it was
// closed. It exists to tell a bare Close() apart from a
// doltserver.DrainAndCloseProbe: only the latter sets a read deadline and
// reads the handshake greeting, and only the latter closes a probe the way
// dolt sql-server tolerates (gastownhall/beads#4132, #4133).
type recordingConn struct {
	net.Conn
	reads         int
	readDeadlines int
	closes        int
}

func (c *recordingConn) Read(b []byte) (int, error) {
	c.reads++
	return 0, io.EOF
}

func (c *recordingConn) SetReadDeadline(time.Time) error {
	c.readDeadlines++
	return nil
}

func (c *recordingConn) Close() error {
	c.closes++
	return nil
}

// drained reports whether this connection was discarded through
// DrainAndCloseProbe rather than closed bare.
func (c *recordingConn) drained() bool {
	return c.closes > 0 && c.reads > 0 && c.readDeadlines > 0
}

// dialRecord is what a stubbed dial saw: how many times it was called, the
// per-attempt timeout it was handed, and WHICH dial var it came through. The
// timeouts are what prove the budget bounds the retries; the legacy/ctxAware
// split is what proves the budget-off path still runs the original
// net.DialTimeout call and not the context-aware one the retry loop uses.
type dialRecord struct {
	attempts int
	legacy   int
	ctxAware int
	timeouts []time.Duration
	// offsets[i] is how long after the stub was installed attempt i STARTED.
	// offset+timeout is the wall-clock instant an attempt is allowed to run
	// until, which is the only way a test can check a per-attempt timeout
	// against the budget REMAINING at that attempt rather than against the
	// whole budget -- the difference between a real deadline and a
	// min(probeTimeout, budget) cap that overruns it.
	offsets []time.Duration
	t0      time.Time
}

// stubServerDial replaces serverDial with one that fails failures times before
// succeeding. A negative failures count never succeeds. sleep, when positive,
// is how long each attempt blocks before answering, capped at the timeout it
// was given, so a stub can imitate an unresponsive endpoint.
// Both dial vars are stubbed from one record, so an assertion on the attempt
// COUNT cannot be fooled by a call that took the other route, and an
// assertion on rec.legacy / rec.ctxAware says which route it took.
func stubServerDial(t *testing.T, failures int, err error, sleep time.Duration) *dialRecord {
	t.Helper()
	rec := &dialRecord{}
	answer := func(ctx context.Context, timeout time.Duration) (net.Conn, error) {
		rec.attempts++
		rec.timeouts = append(rec.timeouts, timeout)
		if sleep > 0 {
			block := sleep
			if timeout > 0 && timeout < block {
				block = timeout
			}
			timer := time.NewTimer(block)
			defer timer.Stop()
			if ctx != nil {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-timer.C:
				}
			} else {
				<-timer.C
			}
		}
		if failures < 0 || rec.attempts <= failures {
			return nil, err
		}
		return &recordingConn{}, nil
	}
	installDialStubs(t, rec, answer)
	return rec
}

// installDialStubs points BOTH dial vars at answer and tags each call with
// the var it arrived through. answer receives a nil context for the legacy
// dialer, which by construction has none.
func installDialStubs(t *testing.T, rec *dialRecord, answer func(context.Context, time.Duration) (net.Conn, error)) {
	t.Helper()
	origCtx, origLegacy := serverDial, serverDialLegacy
	t.Cleanup(func() { serverDial, serverDialLegacy = origCtx, origLegacy })
	rec.t0 = time.Now()
	serverDial = func(ctx context.Context, _, _ string, timeout time.Duration) (net.Conn, error) {
		rec.ctxAware++
		rec.offsets = append(rec.offsets, time.Since(rec.t0))
		return answer(ctx, timeout)
	}
	serverDialLegacy = func(_, _ string, timeout time.Duration) (net.Conn, error) {
		rec.legacy++
		rec.offsets = append(rec.offsets, time.Since(rec.t0))
		return answer(nil, timeout) //nolint:staticcheck // the legacy dialer has no context, by design
	}
}

// stubServerDialErrs replaces serverDial with one that returns errs[i] for
// attempt i, repeating the last entry once the sequence is exhausted. Ending a
// sequence with a NON-retryable error lets a test assert that a retry happened
// without paying for the whole budget.
func stubServerDialErrs(t *testing.T, errs ...error) *dialRecord {
	t.Helper()
	rec := &dialRecord{}
	installDialStubs(t, rec, func(_ context.Context, timeout time.Duration) (net.Conn, error) {
		rec.attempts++
		rec.timeouts = append(rec.timeouts, timeout)
		i := rec.attempts - 1
		if i >= len(errs) {
			i = len(errs) - 1
		}
		return nil, errs[i]
	})
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
			// ...and through the ORIGINAL dialer. Routing the disabled path
			// through the retry loop's context-aware dial would keep the
			// count at 1 while changing what a cancelled or short-deadline
			// context does to a default open.
			if rec.legacy != 1 || rec.ctxAware != 0 {
				t.Errorf("expected the budget-off probe on the legacy dialer, got legacy=%d ctx-aware=%d", rec.legacy, rec.ctxAware)
			}
		})
	}
}

// The default open must not acquire context semantics it never had. Base
// newServerMode dialled with net.DialTimeout, which ignores context: with the
// budget off, an already-cancelled or already-expired context must still
// produce exactly one dial and the dial's own error, not the context's.
func TestOpenRetryBudgetOff_KeepsLegacyDialSemantics(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	t.Cleanup(cancelExpired)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"already cancelled", cancelled},
		{"deadline already passed", expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &dialRecord{}
			// The context-aware stub HONOURS the context, exactly as
			// net.Dialer.DialContext does. The legacy one cannot see it.
			installDialStubs(t, rec, func(ctx context.Context, timeout time.Duration) (net.Conn, error) {
				rec.attempts++
				rec.timeouts = append(rec.timeouts, timeout)
				if ctx != nil {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				return nil, errConnRefused
			})

			conn, err := dialServerPreflight(tc.ctx, unmanagedCfg(0), "tcp", externalAddr, 250*time.Millisecond)

			if conn != nil {
				t.Errorf("expected no connection, got %v", conn)
			}
			if rec.attempts != 1 {
				t.Fatalf("expected exactly 1 dial with the budget off, got %d", rec.attempts)
			}
			if rec.legacy != 1 || rec.ctxAware != 0 {
				t.Errorf("expected the legacy dialer, got legacy=%d ctx-aware=%d", rec.legacy, rec.ctxAware)
			}
			// The load-bearing assertion: base returned the DIAL error here.
			if err != errConnRefused {
				t.Errorf("expected the dial error unchanged by the context, got %v", err)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("the default open must not surface a context error, got %v", err)
			}
			if len(rec.timeouts) != 1 || rec.timeouts[0] != 250*time.Millisecond {
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
		// Comfortably above the backoff's first interval, which is jittered
		// over [250ms, 750ms]: a budget equal to the TOP of that range lets a
		// long first wait end the loop after one attempt, which the "at least
		// one retry" assertion below then fails. Same jitter hazard as
		// TestOpenRetryBudgetEngagesForExternallyManagedLocalhost.
		budget       = 2500 * time.Millisecond
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
	// The ENABLED path is the context-aware one: an opt-in wait a caller
	// cannot cancel would be worse than the fail-fast open it replaces.
	if rec.ctxAware != rec.attempts || rec.legacy != 0 {
		t.Errorf("expected every retry-loop dial on the context-aware dialer, got legacy=%d ctx-aware=%d of %d",
			rec.legacy, rec.ctxAware, rec.attempts)
	}
	// The budget is a DEADLINE on the whole probe: at worst it overruns by the
	// last dial's own timeout plus scheduling slack. A stray sleep after
	// exhaustion, or a sleep that crosses the deadline, fails here.
	if ceiling := budget + probeTimeout + 250*time.Millisecond; elapsed > ceiling {
		t.Errorf("expected the probe to end within %s (budget %s), took %s", ceiling, budget, elapsed)
	}
}

// The deadline rule, stated once and asserted here: the budget bounds the
// RETRIES. The first probe keeps the caller's full timeout; every dial after
// it is capped by what is left of the budget.
//
// A probe timeout LARGER than the whole budget makes both halves observable
// without depending on the jittered backoff: the first dial must be handed the
// full timeout, and every retry must be handed strictly less.
func TestOpenRetryBudgetOn_FirstProbeFullThenRetriesCapped(t *testing.T) {
	const (
		budget       = 900 * time.Millisecond
		probeTimeout = 2 * time.Second
	)
	// Instant failures: the elapsed time below is spent in backoff sleeps
	// alone, so it measures the budget and nothing else.
	rec := stubServerDial(t, -1, errConnRefused, 0)
	start := time.Now()
	_, err := dialServerPreflight(context.Background(), unmanagedCfg(budget), "tcp", externalAddr, probeTimeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected failure once the budget is spent")
	}
	if rec.attempts < 2 {
		t.Fatalf("expected the budget to buy at least one retry, got %d attempts", rec.attempts)
	}
	// Half one: the first probe is the fail-fast probe, untouched.
	if rec.timeouts[0] != probeTimeout {
		t.Errorf("expected the first probe to keep the caller's full %s timeout, got %s", probeTimeout, rec.timeouts[0])
	}
	// Half two: every RETRY is bounded by what is left of the budget AT THAT
	// ATTEMPT, not by the budget as a whole. Checking start-offset + timeout
	// against the deadline is what separates a real remaining-time cap from a
	// min(probeTimeout, budget) cap: the latter hands every retry the full
	// budget however late it starts, so it overruns the deadline while still
	// producing timeouts that are individually "within the budget".
	const slack = 75 * time.Millisecond
	for i, to := range rec.timeouts[1:] {
		attempt := i + 1
		if to <= 0 {
			t.Errorf("retry %d got a non-positive timeout %s, which means NO timeout at all", attempt, to)
		}
		if to >= probeTimeout {
			t.Errorf("retry %d got timeout %s, not capped at all (caller's probe timeout is %s)", attempt, to, probeTimeout)
		}
		if end := rec.offsets[attempt] + to; end > budget+slack {
			t.Errorf("retry %d started at %s with a %s timeout, so it may run until %s -- past the %s budget",
				attempt, rec.offsets[attempt], to, end, budget)
		}
	}
	if ceiling := budget + 250*time.Millisecond; elapsed > ceiling {
		t.Errorf("expected the retries to end within %s (budget %s), took %s", ceiling, budget, elapsed)
	}
}

// The other end of the same rule: a budget too small to buy anything degrades
// to EXACTLY the fail-fast open -- one probe with its full timeout, zero
// retries -- rather than to an open that never contacts the server. An enabled
// budget must never be less patient than the default it replaces.
func TestOpenRetryBudgetOn_TinyBudgetIsOneFullProbeZeroRetries(t *testing.T) {
	const probeTimeout = 500 * time.Millisecond
	// The endpoint swallows the connection until the timeout it was given, so
	// the elapsed time below reports how long the first probe was allowed.
	rec := stubServerDial(t, -1, errConnRefused, time.Hour)
	start := time.Now()
	_, err := dialServerPreflight(context.Background(), unmanagedCfg(time.Nanosecond), "tcp", externalAddr, probeTimeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the probe to fail")
	}
	if !errors.Is(err, errConnRefused) {
		t.Errorf("expected the dial error to remain in the chain, got %v", err)
	}
	// Exactly one probe: a whole-probe deadline would have made this ZERO.
	if rec.attempts != 1 {
		t.Fatalf("expected exactly 1 probe and no retry, got %d attempts", rec.attempts)
	}
	if rec.timeouts[0] != probeTimeout {
		t.Errorf("expected the probe to keep its full %s timeout, got %s", probeTimeout, rec.timeouts[0])
	}
	// ...and it really got that long: a capped first dial would return in
	// nanoseconds against this endpoint.
	if elapsed < probeTimeout/2 {
		t.Errorf("expected the probe to be allowed its full %s, it returned after %s", probeTimeout, elapsed)
	}
	if ceiling := probeTimeout + 250*time.Millisecond; elapsed > ceiling {
		t.Errorf("expected the probe to end within %s, took %s", ceiling, elapsed)
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
			// Belt and braces for embedded mode, NOT the routing claim: an
			// embedded open never reaches newServerMode at all, and that is
			// a fact about cmd/bd's store factory, asserted there by
			// TestNewDoltStore_EmbeddedRoutingNeverReachesTheOpenRetryPath.
			// What this case says is narrower and still worth saying: if a
			// config with an embedded SHAPE -- no configured host, auto-start
			// on, a local data path -- did arrive here, the gate would still
			// be false.
			name: "embedded-shaped config (no host, auto-start, local path)",
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
			if rec.legacy != 1 || rec.ctxAware != 0 {
				t.Errorf("expected the excluded mode on the legacy dialer, got legacy=%d ctx-aware=%d", rec.legacy, rec.ctxAware)
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
				ServerMode: true,
				ServerHost: host,
				ServerPort: 3307,
				Path:       "/tmp/does-not-matter/.beads/dolt",
				AutoStart:  false, // resolveAutoStart: mode == ServerModeExternal
				// Comfortably above the backoff's first interval, which is
				// JITTERED over [250ms, 750ms]: a budget of 750ms lets a wait
				// at the top of that range end the loop before the retry this
				// test is about, which is a flake at roughly 1 run in 100
				// (caught by -count=5). The budget is not what this test
				// measures -- the gate is -- so give it room.
				OpenRetryBudget: 5 * time.Second,
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

// A dial that lands after the context was cancelled must not report success --
// and the connection it produced must be discarded the way every other probe
// close in this package is: through doltserver.DrainAndCloseProbe. A bare
// Close() on a probe that never read the handshake greeting makes the OS send
// RST instead of FIN, which dolt sql-server can die on when it happens often
// enough (gastownhall/beads#4132, #4133) -- and a retry loop is precisely the
// repeated-probe shape that documents that risk.
func TestOpenRetryBudgetOn_CancellationAfterSuccessDrainsProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	conn := &recordingConn{}
	orig := serverDial
	t.Cleanup(func() { serverDial = orig })
	dials := 0
	serverDial = func(context.Context, string, string, time.Duration) (net.Conn, error) {
		dials++
		// The open is cancelled while this dial is in flight, so the loop
		// finds a live connection and a cancelled context together.
		cancel()
		return conn, nil
	}

	got, err := dialServerPreflight(ctx, unmanagedCfg(30*time.Second), "tcp", externalAddr, time.Millisecond)

	if dials != 1 {
		t.Fatalf("expected exactly 1 dial, got %d", dials)
	}
	if got != nil {
		t.Errorf("a cancelled open must not hand back a connection, got %v", got)
	}
	if err == nil {
		t.Fatal("expected an error when the context is cancelled after a successful dial")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if conn.closes != 1 {
		t.Errorf("expected the connection closed exactly once, got %d closes", conn.closes)
	}
	// The load-bearing assertion: a bare Close() leaves reads == 0 and
	// readDeadlines == 0, so reverting to one turns this red.
	if !conn.drained() {
		t.Errorf("expected the probe drained before close (DrainAndCloseProbe), got %d reads / %d read deadlines / %d closes",
			conn.reads, conn.readDeadlines, conn.closes)
	}
}

// The PRODUCTION dialer must honour the caller's context. Every other
// cancellation test here stubs serverDial, so all of them survive replacing the
// real closure's ctx with context.Background() -- which would make an in-flight
// probe ignore an open its caller has abandoned. This one calls the real var.
func TestOpenRetryProductionDialHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	// A 30s dial timeout against a black-hole address: only the context can
	// end this quickly. The address is never actually contacted -- DialContext
	// returns on an already-cancelled context before touching the network.
	conn, err := serverDial(ctx, "tcp", "10.255.255.1:9", 30*time.Second)
	elapsed := time.Since(start)

	if conn != nil {
		_ = conn.Close()
		t.Fatal("expected no connection from a cancelled dial")
	}
	if err == nil {
		t.Fatal("expected an error from a cancelled dial")
	}
	// The load-bearing assertion: only a context-aware dial can produce this.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled) from the production dialer, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected the cancelled dial to return promptly, took %s", elapsed)
	}
}

// ...and the legacy dialer must NOT honour it: that is what keeps the
// budget-off open byte-for-byte what it was. Asserted on the real closure for
// the same reason as above.
func TestOpenRetryLegacyDialIgnoresCancellation(t *testing.T) {
	// A closed local port refuses immediately, so this costs nothing and the
	// error is the dial's own rather than a timeout.
	_, err := serverDialLegacy("tcp", "127.0.0.1:9", 250*time.Millisecond)
	if err == nil {
		t.Skip("something is listening on 127.0.0.1:9; cannot assert the refusal")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("the legacy dialer has no context and must never report cancellation, got %v", err)
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

// newBeadsDirFlat is newBeadsDir with the key written in the FLAT dotted form
// ("dolt.open-retry-budget: 0") instead of the nested one. config.yaml is
// hand-edited and YAML accepts both, so an operator turning the budget off has
// two correct spellings and neither may be ignored.
func newBeadsDirFlat(t *testing.T, budget string) string {
	t.Helper()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "dolt:\n  auto-start: false\n"
	if budget != "" {
		body += "dolt.open-retry-budget: " + budget + "\n"
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	return beadsDir
}

// writeLocalBudget writes <beadsDir>/config.local.yaml with a budget. Local
// config is the machine-specific override config.Initialize merges OVER
// config.yaml, so a directory-scoped read that skips it reports a value the
// same process's merged configuration would have overridden.
func writeLocalBudget(t *testing.T, beadsDir, budget string) {
	t.Helper()
	body := "dolt:\n  open-retry-budget: " + budget + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.local.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.local.yaml: %v", err)
	}
}

// withUserGlobalBudget points HOME (and the native user-config dir) at a fresh
// tree carrying a user-level config.yaml with the given budget. That file is
// the machine-wide default a silent workspace is entitled to inherit.
func withUserGlobalBudget(t *testing.T, budget string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "bd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir user config dir: %v", err)
	}
	body := "dolt:\n  open-retry-budget: " + budget + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write user config.yaml: %v", err)
	}
	redirectUserConfigHome(t, home)
}

// withoutUserGlobalBudget points the user-config lookup at an EMPTY tree. An
// arm that must observe NO machine-wide default has to install that absence:
// without it the arm would be reading whatever the host running the test
// happens to keep in its user config, and a pass would say nothing about the
// fixture.
func withoutUserGlobalBudget(t *testing.T) {
	t.Helper()
	redirectUserConfigHome(t, t.TempDir())
}

// redirectUserConfigHome points BOTH resolvers the user-level config lookup
// goes through -- os.UserHomeDir and os.UserConfigDir -- at home.
//
// HOME and XDG_CONFIG_HOME alone are a Linux/macOS answer: on Windows those
// two read USERPROFILE and APPDATA, so a fixture that sets only the POSIX pair
// installs its config where nothing looks for it and leaves the real user's
// config visible. The failure is asymmetric -- the "inherits the default" arms
// go red, the "sees no default" arms go green for the wrong reason -- so it
// cannot be caught by reading the arms. Same pattern as
// internal/config/user_config_path_test.go.
func redirectUserConfigHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, ".config"))
}

// newSharedServerDoltPath builds the data path a shared-server open carries:
// <home>/.beads/shared-server/dolt (doltserver.SharedDoltDir). Its parent is
// ~/.beads/shared-server, NOT the project's .beads -- which is the whole point
// of the fixture.
func newSharedServerDoltPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".beads", "shared-server", "dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir shared dolt dir: %v", err)
	}
	return dir
}

// newCustomDoltDataPath builds an absolute dolt_data_dir on another
// filesystem, which configfile.DatabasePath returns verbatim. Nothing about
// this path names a .beads directory.
func newCustomDoltDataPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fastfs", "beads-dolt-data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir custom dolt data dir: %v", err)
	}
	return dir
}

// cfgWithDataPath is productionCfg with the data path decoupled from the
// project directory, the way every non-default layout has it.
func cfgWithDataPath(beadsDir, dataPath string) *Config {
	cfg := productionCfg(beadsDir)
	cfg.Path = dataPath
	return cfg
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

// isolateBreakerEnv is isolateOpenEnv for a test that needs the circuit
// breaker LIVE: same isolation, minus BEADS_TEST_MODE=1, which is what
// disables the breaker outright. BEADS_TEST_CIRCUIT_DIR redirects both the
// current and the legacy breaker state paths, so no test here reads or writes
// the machine's real breaker files.
func isolateBreakerEnv(t *testing.T) string {
	t.Helper()
	circuitDir := t.TempDir()
	t.Setenv(testCircuitBreakerDirEnv, circuitDir)
	t.Setenv("BEADS_TEST_MODE", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_SOCKET", "")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "")
	config.ResetForTesting()
	t.Cleanup(config.ResetForTesting)
	return circuitDir
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
func TestNew_OpenRetryProjectConfigZeroOverridesAmbient(t *testing.T) {
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

// (c2) The same precedence with the target's key written FLAT. This is the
// spelling config.GetStringFromDir cannot see, and reading it as absent would
// hand the open the other workspace's 30s.
func TestNew_OpenRetryFlatProjectConfigZeroOverridesAmbient(t *testing.T) {
	isolateOpenEnv(t)
	otherWorkspace := newBeadsDir(t, "30s")
	t.Setenv("BEADS_DIR", otherWorkspace)
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
	if got := config.GetString("dolt.open-retry-budget"); got != "30s" {
		t.Fatalf("precondition: ambient config should carry 30s, got %q", got)
	}

	target := newBeadsDirFlat(t, "0")
	rec := stubServerDial(t, -1, errConnRefused, 0)

	_, err := New(context.Background(), productionCfg(target))
	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	if rec.attempts != 1 {
		t.Errorf("a flat explicit 0 in the opened project must disable the budget, got %d dial attempts", rec.attempts)
	}
	if strings.Contains(err.Error(), "open-retry-budget") {
		t.Errorf("a disabled budget must not appear in the error: %v", err)
	}
}

// (c3) And the flat spelling ENABLES too, so the fix is a reader change and
// not a special case for the value 0.
func TestNew_OpenRetryFlatProjectConfigEnablesBudget(t *testing.T) {
	isolateOpenEnv(t)
	target := newBeadsDirFlat(t, "1200ms")
	// The second attempt fails non-retryably, so the loop ends there instead
	// of spending the whole budget.
	rec := stubServerDialErrs(t, errConnRefused, errNoSuchHost)

	_, err := New(context.Background(), productionCfg(target))
	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	if !errors.Is(err, errNoSuchHost) {
		t.Errorf("expected the second attempt's error to surface, got %v", err)
	}
	if rec.attempts != 2 {
		t.Errorf("expected a flat budget to buy exactly one retry, got %d dial attempts", rec.attempts)
	}
}

// (d) The mirror of (c), and the part that is NOT a mirror: a silent project
// inherits a machine-wide default, but must NOT inherit another PROJECT's
// setting. The process-wide merged configuration carries the project settings
// of whichever workspace initialized it first, so a library or multi-workspace
// process that opened A with 30s would otherwise give B -- which says nothing,
// and whose operator configured nothing -- a retrying open.
func TestNew_OpenRetryOtherWorkspaceBudgetDoesNotLeak(t *testing.T) {
	isolateOpenEnv(t)
	otherWorkspace := newBeadsDir(t, "30s")
	t.Setenv("BEADS_DIR", otherWorkspace)
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
	// Precondition: the merged view really does carry the other workspace's
	// value, so the zero below is a scoping decision and not an empty config.
	if got := config.GetString("dolt.open-retry-budget"); got != "30s" {
		t.Fatalf("precondition: the merged config should carry the other workspace's 30s, got %q", got)
	}

	target := newBeadsDir(t, "")
	rec := stubServerDial(t, -1, errConnRefused, 0)

	_, err := New(context.Background(), productionCfg(target))
	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	if rec.attempts != 1 {
		t.Errorf("another workspace's budget must not reach this open, got %d dial attempts", rec.attempts)
	}
	if strings.Contains(err.Error(), "open-retry-budget") {
		t.Errorf("a budget that was never configured for this project must not appear in the error: %v", err)
	}
}

// (e) ...and the genuine machine-wide default IS inherited: a user-level
// config.yaml applies to a project that says nothing. Without this the case
// above would read as "the fallback was removed" rather than "the fallback was
// scoped".
func TestNew_OpenRetryUserGlobalBudgetAppliesWhenProjectIsSilent(t *testing.T) {
	isolateOpenEnv(t)
	withUserGlobalBudget(t, "1200ms")

	target := newBeadsDir(t, "")
	// The second attempt fails non-retryably, so the loop ends there instead
	// of spending the whole budget.
	rec := stubServerDialErrs(t, errConnRefused, errNoSuchHost)

	_, err := New(context.Background(), productionCfg(target))
	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	if !errors.Is(err, errNoSuchHost) {
		t.Errorf("expected the second attempt's error to surface, got %v", err)
	}
	if rec.attempts != 2 {
		t.Errorf("expected the user-global budget to buy exactly one retry, got %d dial attempts", rec.attempts)
	}
}

// (f) config.local.yaml is the machine-specific override config.Initialize
// merges OVER config.yaml. A directory-scoped read that stops at config.yaml
// reports a value the same process's merged configuration would have
// overridden -- and this one bites the ordinary single-workspace command, not
// only the multi-workspace case.
func TestNew_OpenRetryLocalYamlZeroOverridesProjectYaml(t *testing.T) {
	isolateOpenEnv(t)
	target := newBeadsDir(t, "30s")
	writeLocalBudget(t, target, "0")
	rec := stubServerDial(t, -1, errConnRefused, 0)

	_, err := New(context.Background(), productionCfg(target))
	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	if rec.attempts != 1 {
		t.Errorf("config.local.yaml must be able to disable the budget, got %d dial attempts", rec.attempts)
	}
	if strings.Contains(err.Error(), "open-retry-budget") {
		t.Errorf("a disabled budget must not appear in the error: %v", err)
	}
}

// newManagedBeadsDir writes a .beads/config.yaml for a bd-MANAGED localhost
// server: auto-start on, and a budget configured. The budget being present is
// the point -- the assertion below is that a CONFIGURED budget still buys no
// retry in this mode.
func newManagedBeadsDir(t *testing.T, budget string) string {
	t.Helper()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "dolt:\n  auto-start: true\n  open-retry-budget: " + budget + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	return beadsDir
}

// The maintainer's mode-exclusion ask, taken through the REAL managed-open
// path rather than through a config shaped like one: a budget-configured
// localhost open that reaches EnsureRunningDetailed must make exactly the two
// bare dials it has always made -- the fail-fast preflight and the
// post-auto-start retry -- and no retry-loop dial at all.
//
// ensureRunningDetailed is stubbed (as port_provenance_test.go does) so no
// dolt sql-server is spawned.
func TestNew_OpenRetryManagedLocalhostThroughEnsureRunning(t *testing.T) {
	isolateOpenEnv(t)
	beadsDir := newManagedBeadsDir(t, "30s")

	cfg := &Config{
		ServerMode: true,
		BeadsDir:   beadsDir,
		Path:       filepath.Join(beadsDir, "dolt"),
		Database:   "openretry_probe",
		ServerHost: "127.0.0.1",
		ServerPort: externalTestPort,
		AutoStart:  true,
	}
	// Same port back: no retarget branch, so this open walks the ordinary
	// auto-start recovery.
	stubEnsureRunningDetailed(t, externalTestPort, false, nil)
	rec := stubServerDial(t, -1, errConnRefused, 0)

	start := time.Now()
	_, err := New(context.Background(), cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the open to fail: nothing is listening")
	}
	// Positive control. Without this the whole test would pass on a budget
	// that was never configured, which is the failure mode it exists to rule
	// out: New resolves the key onto cfg, so a non-zero value here proves the
	// budget WAS live for this open.
	if got := effectiveOpenRetryBudget(cfg); got != 30*time.Second {
		t.Fatalf("precondition: New must have resolved the configured budget, got %s", got)
	}
	if openRetryEnabled(cfg) {
		t.Error("a bd-managed localhost open must not enable the budget")
	}
	// The preflight probe and the post-auto-start retry: two bare dials, both
	// on the legacy dialer, and nothing from the retry loop.
	if rec.ctxAware != 0 {
		t.Errorf("expected no retry-loop dial in a managed open, got %d", rec.ctxAware)
	}
	if rec.legacy != 2 {
		t.Errorf("expected exactly 2 bare dials (preflight + post-auto-start), got %d", rec.legacy)
	}
	if !strings.Contains(err.Error(), "auto-started but still unreachable") {
		t.Errorf("expected the post-auto-start failure, got %v", err)
	}
	// A single backoff sleep would be >=250ms.
	if elapsed > 100*time.Millisecond {
		t.Errorf("expected no backoff sleep in a managed open, took %s", elapsed)
	}
}

// The circuit breaker counts OPENS, not dials. An exhausted budget is one
// failed open, so it must record exactly one failure however many attempts it
// made -- otherwise a single 30s open trips a breaker whose threshold is five
// failures, and the budget an operator configured to ride out a restart would
// instead fail-fast every command after it.
//
// The other production-path tests here run with BEADS_TEST_MODE=1, which
// disables the breaker outright, so this invariant had no coverage at all.
// This one turns the breaker ON and reads the count back off its own state
// file.
func TestNew_OpenRetryExhaustedBudgetRecordsOneCircuitFailure(t *testing.T) {
	circuitDir := isolateBreakerEnv(t)

	beadsDir := newBeadsDir(t, "1200ms")
	rec := stubServerDial(t, -1, errConnRefused, 0)

	_, err := New(context.Background(), productionCfg(beadsDir))
	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	// Positive control: the budget really was live, so the attempt count
	// below is a retry count and not a fail-fast open.
	if rec.attempts < 2 {
		t.Fatalf("precondition: expected the budget to retry, got %d dial attempts", rec.attempts)
	}

	// The invariant. Recording inside the retry loop instead would put
	// rec.attempts here -- and at five, trip the breaker.
	if failures := circuitFailuresRecorded(t, circuitDir); failures != 1 {
		t.Errorf("expected exactly 1 recorded circuit failure for one exhausted open, got %d (after %d dial attempts)",
			failures, rec.attempts)
	}
	if state := circuitStateRecorded(t, circuitDir); state != circuitClosed {
		t.Errorf("one failed open must not trip the breaker, got state %q", state)
	}
}

// circuitFailuresRecorded reads the failure count the breaker persisted in dir.
// No state file at all means nothing was ever recorded, which is a count of 0
// rather than a test error -- that is exactly the outcome one of the callers
// asserts.
func circuitFailuresRecorded(t *testing.T, dir string) int {
	t.Helper()
	state, ok := readCircuitState(t, dir)
	if !ok {
		return 0
	}
	return state.Failures
}

func circuitStateRecorded(t *testing.T, dir string) string {
	t.Helper()
	state, ok := readCircuitState(t, dir)
	if !ok {
		return circuitClosed
	}
	return state.State
}

func readCircuitState(t *testing.T, dir string) (circuitState, bool) {
	t.Helper()
	entries, globErr := filepath.Glob(filepath.Join(dir, "*.json"))
	if globErr != nil {
		t.Fatalf("glob circuit state: %v", globErr)
	}
	if len(entries) == 0 {
		return circuitState{}, false
	}
	if len(entries) != 1 {
		t.Fatalf("expected at most 1 circuit-breaker state file, got %d: %v", len(entries), entries)
	}
	raw, readErr := os.ReadFile(entries[0])
	if readErr != nil {
		t.Fatalf("read circuit state: %v", readErr)
	}
	var state circuitState
	if jsonErr := json.Unmarshal(raw, &state); jsonErr != nil {
		t.Fatalf("parse circuit state %s: %v", raw, jsonErr)
	}
	return state, true
}

// ...and the mirror of that invariant: an open the CALLER cancelled says
// nothing about the server, so it must not count towards the breaker at all.
// The budget introduced this hazard -- before it, the pre-dial probe ignored
// context and could not return a cancellation error -- and five cancelled
// opens inside the failure window would trip the breaker against a healthy
// server, failing every open after them for the cooldown.
func TestNew_OpenRetryCallerCancelledOpenRecordsNoCircuitFailure(t *testing.T) {
	circuitDir := isolateBreakerEnv(t)

	beadsDir := newBeadsDir(t, "30s")
	rec := stubServerDial(t, -1, errConnRefused, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(ctx, productionCfg(beadsDir))
	if err == nil {
		t.Fatal("expected a cancelled open to fail")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancellation to stay identifiable, got %v", err)
	}
	if rec.attempts != 0 {
		t.Fatalf("expected no dial on an already-cancelled open, got %d", rec.attempts)
	}
	if failures := circuitFailuresRecorded(t, circuitDir); failures != 0 {
		t.Errorf("a caller-cancelled open must not count towards the breaker, got %d recorded failures", failures)
	}

	// Positive control, in the same breaker directory: a genuine unreachable
	// server on the SAME config does record one. Without it, the zero above
	// would be indistinguishable from a breaker that was never live.
	if _, err := New(context.Background(), productionCfg(beadsDir)); err == nil {
		t.Fatal("expected the uncancelled open to fail against an unreachable server")
	}
	if failures := circuitFailuresRecorded(t, circuitDir); failures != 1 {
		t.Errorf("control: an uncancelled failed open must record exactly 1 failure, got %d", failures)
	}
}

// A SERVER timeout must still count towards the breaker with the budget off.
// The guard that keeps caller-cancelled opens out of the breaker cannot be an
// error-identity test: net.DialTimeout's own timeout returns net's
// timeoutError, whose Is method reports true for context.DeadlineExceeded, so
// an errors.Is guard silently stops counting ordinary server timeouts on the
// DEFAULT path -- the exact inverse of what the guard is for.
func TestNew_OpenRetryServerTimeoutStillCountsTowardsBreaker(t *testing.T) {
	circuitDir := isolateBreakerEnv(t)

	// No budget configured: this is the DEFAULT open, on the legacy dialer.
	beadsDir := newBeadsDir(t, "")
	rec := stubServerDial(t, -1, errIOTimeout{}, 0)

	_, err := New(context.Background(), productionCfg(beadsDir))
	if err == nil {
		t.Fatal("expected the open to fail against a server that never answers")
	}
	if rec.attempts != 1 {
		t.Fatalf("precondition: the default open makes exactly 1 probe, got %d", rec.attempts)
	}
	// Precondition, and the whole reason this test exists: the error really
	// does satisfy the identity test a naive guard would have used.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("precondition: a net dial timeout satisfies errors.Is(err, context.DeadlineExceeded); got %v", err)
	}
	if failures := circuitFailuresRecorded(t, circuitDir); failures != 1 {
		t.Errorf("a server timeout must count towards the breaker, got %d recorded failures", failures)
	}
}

// The cancellation exemption must be confined to the ENABLED path. With the
// budget off the probe is net.DialTimeout, which cannot see the context at all,
// so its failure is server evidence whatever state the caller's context is in.
// Exempting it would change base breaker accounting on the default open.
func TestNew_OpenRetryCancelledContextStillCountsWithBudgetOff(t *testing.T) {
	circuitDir := isolateBreakerEnv(t)

	beadsDir := newBeadsDir(t, "") // no budget: the default open
	rec := stubServerDial(t, -1, errConnRefused, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(ctx, productionCfg(beadsDir))
	if err == nil {
		t.Fatal("expected the open to fail against an unreachable server")
	}
	// Precondition: the legacy dialer ran and ignored the context, exactly as
	// net.DialTimeout does, so this really is a server refusal.
	if rec.legacy != 1 || rec.ctxAware != 0 {
		t.Fatalf("precondition: the default open dials once on the legacy dialer, got legacy=%d ctx-aware=%d", rec.legacy, rec.ctxAware)
	}
	if !errors.Is(err, errConnRefused) {
		t.Fatalf("precondition: expected the refusal to surface, got %v", err)
	}
	if failures := circuitFailuresRecorded(t, circuitDir); failures != 1 {
		t.Errorf("a server refusal on the default open must count towards the breaker regardless of the caller's context, got %d recorded failures", failures)
	}
}

// A Config reused across opens must not carry the first open's RESOLVED budget
// into the second as if a caller had set it. Retargeting one Config at another
// project -- or reopening after that project's config.yaml changed -- has to
// honour the project it is now pointing at.
func TestNew_OpenRetryResolvedBudgetIsNotACallerOverride(t *testing.T) {
	isolateOpenEnv(t)
	retrying := newBeadsDir(t, "1200ms")
	optedOut := newBeadsDir(t, "0")

	// Both opens go through applyResolvedConfig, which is what a caller
	// reusing one Config across two NewFromConfigWithOptions calls does. The
	// first call is not decoration: a field filled only when empty looks
	// correct on the first open and stale on every one after it, so a test
	// that skips the first call cannot see the difference.
	cfg := productionCfg(retrying)
	fileCfg := &configfile.Config{Backend: configfile.BackendDolt}
	if err := applyResolvedConfig(context.Background(), retrying, fileCfg, cfg); err != nil {
		t.Fatalf("applyResolvedConfig: %v", err)
	}
	rec := stubServerDialErrs(t, errConnRefused, errNoSuchHost)
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("expected the first open to fail")
	}
	if rec.attempts != 2 {
		t.Fatalf("precondition: the first project's budget must retry, got %d dial attempts", rec.attempts)
	}
	if cfg.OpenRetryBudget != 0 {
		t.Errorf("New must not write its resolved budget into the caller's field, got %s", cfg.OpenRetryBudget)
	}

	// Same Config object, retargeted at a project that opted out -- through
	// the function that actually retargets one on a second
	// NewFromConfigWithOptions call. It rewrites cfg.Path and
	// cfg.OpenRetryConfigDir on EVERY open but fills cfg.BeadsDir just once,
	// when empty, so the stale first-project value is still sitting in
	// BeadsDir: an open that resolved its budget from BeadsDir, or from a
	// OpenRetryConfigDir that were also only filled when empty, would read the
	// FIRST project's config.yaml and retry against a project that said 0.
	if err := applyResolvedConfig(context.Background(), optedOut, fileCfg, cfg); err != nil {
		t.Fatalf("applyResolvedConfig: %v", err)
	}
	if cfg.BeadsDir != retrying {
		t.Fatalf("precondition: cfg.BeadsDir must still hold the FIRST project, got %q", cfg.BeadsDir)
	}
	if cfg.Path != filepath.Join(optedOut, "dolt") {
		t.Fatalf("precondition: cfg.Path must be retargeted, got %q", cfg.Path)
	}
	rec2 := stubServerDial(t, -1, errConnRefused, 0)
	_, err := New(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected the second open to fail")
	}
	if rec2.attempts != 1 {
		t.Errorf("the retargeted open must honour the new project's explicit 0, got %d dial attempts", rec2.attempts)
	}
	if strings.Contains(err.Error(), "open-retry-budget") {
		t.Errorf("a disabled budget must not appear in the error: %v", err)
	}
}

// The project beats the machine-wide default, in every spelling. Without a
// POSITIVE user-global fixture beside each project zero, inverting the lookup
// order -- user-global first -- would survive the suite: the other precedence
// tests pair a project zero with another PROJECT's config, which this
// implementation ignores by design.
func TestResolveOpenRetryBudgetProjectZeroBeatsUserGlobal(t *testing.T) {
	isolateOpenEnv(t)
	withUserGlobalBudget(t, "30s")

	nestedZero := newBeadsDir(t, "0")
	flatZero := newBeadsDirFlat(t, "0")
	localZero := newBeadsDir(t, "30s")
	writeLocalBudget(t, localZero, "0")
	silent := newBeadsDir(t, "")

	cases := []struct {
		name     string
		beadsDir string
		want     time.Duration
	}{
		// The control: the user-global default is real and reachable, so the
		// zeros below are a precedence result and not an empty config.
		{"silent project inherits the user-global default", silent, 30 * time.Second},
		{"nested project zero beats the user-global default", nestedZero, 0},
		{"flat project zero beats the user-global default", flatZero, 0},
		{"local-file project zero beats the user-global default", localZero, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveOpenRetryBudget(&Config{}, tc.beadsDir); got != tc.want {
				t.Errorf("resolveOpenRetryBudget = %s, want %s", got, tc.want)
			}
		})
	}
}

// An explicit cfg.OpenRetryBudget outranks every config file.
func TestResolveOpenRetryBudgetPrecedence(t *testing.T) {
	isolateOpenEnv(t)
	dirWith30s := newBeadsDir(t, "30s")
	dirWithZero := newBeadsDir(t, "0")
	dirSilent := newBeadsDir(t, "")
	dirWith30sLocalZero := newBeadsDir(t, "30s")
	writeLocalBudget(t, dirWith30sLocalZero, "0")
	dirWithZeroLocal30s := newBeadsDir(t, "0")
	writeLocalBudget(t, dirWithZeroLocal30s, "30s")

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
		// The flat dotted spelling is as valid as the nested one, and the
		// disabling case is the one that must not be missed: reading it as
		// absent lets another workspace's budget win.
		{"flat directory value is used", &Config{}, newBeadsDirFlat(t, "30s"), 30 * time.Second},
		{"flat directory zero disables", &Config{}, newBeadsDirFlat(t, "0"), 0},
		{"flat directory off disables", &Config{}, newBeadsDirFlat(t, "off"), 0},
		{"bare seconds are accepted", &Config{}, newBeadsDir(t, "45"), 45 * time.Second},
		{"unparseable reads as off", &Config{}, newBeadsDir(t, "\"soon\""), 0},
		{"no directory at all", &Config{}, "", 0},
		{"local yaml zero disables a project 30s", &Config{}, dirWith30sLocalZero, 0},
		{"local yaml value outranks a project zero", &Config{}, dirWithZeroLocal30s, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveOpenRetryBudget(tc.cfg, tc.beadsDir); got != tc.want {
				t.Errorf("resolveOpenRetryBudget = %s, want %s", got, tc.want)
			}
		})
	}
}

// The data path is not the project directory in two supported layouts, and in
// both of them deriving the config directory from the data path reads someone
// else's settings:
//
//   - shared-server mode, where doltserver.ResolveDoltDir returns
//     ~/.beads/shared-server/dolt for every project on the machine;
//   - a custom absolute dolt_data_dir, which configfile.DatabasePath returns
//     verbatim (the WSL "put the data on ext4" case).
//
// Both directions are asserted, because the two failure modes are opposite and
// only one of them is loud: a project's explicit "0" silently skipped while a
// machine-wide default enables retries it opted out of, and a project's own
// budget silently ignored so the feature never engages for a project that
// asked for it.

// sharedServerDataPathIsNotAProjectPath is the tie between the fixtures below
// and production: it asserts that what doltserver actually returns in
// shared-server mode has the shape the fixtures imitate, so the fixtures are
// not an invented path shape that happens to fail isProjectDoltDataPath.
func TestOpenRetrySharedServerDataPathIsNotAProjectPath(t *testing.T) {
	isolateOpenEnv(t)
	// SharedServerPath resolves through the user home, so this fixture needs
	// the same redirect the user-config ones do -- on every platform.
	redirectUserConfigHome(t, t.TempDir())
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")

	project := newBeadsDir(t, "0")
	shared := doltserver.ResolveDoltDir(project)

	// Positive control: the per-project layout MUST pass, or a passing
	// negative below would only be saying isProjectDoltDataPath rejects
	// everything.
	if !isProjectDoltDataPath(filepath.Join(project, "dolt")) {
		t.Fatalf("control: the default layout %q must read as a project data path",
			filepath.Join(project, "dolt"))
	}
	if shared == filepath.Join(project, "dolt") {
		t.Fatalf("precondition: shared-server mode must not resolve to the project dir, got %q", shared)
	}
	if isProjectDoltDataPath(shared) {
		t.Errorf("the shared-server data path %q must not read as a project data path", shared)
	}
	if filepath.Dir(shared) == project {
		t.Errorf("the shared-server data path's parent is not the project .beads, got %q", filepath.Dir(shared))
	}
}

// The shared-server data path must not decide which project's config.yaml the
// budget comes from.
func TestNew_OpenRetrySharedServerDataPathUsesProjectConfig(t *testing.T) {
	t.Run("project zero is honoured against a positive user-global default", func(t *testing.T) {
		isolateOpenEnv(t)
		withUserGlobalBudget(t, "30s")
		project := newBeadsDir(t, "0")
		rec := stubServerDial(t, -1, errConnRefused, 0)

		_, err := New(context.Background(), cfgWithDataPath(project, newSharedServerDoltPath(t)))
		if err == nil {
			t.Fatal("expected the open to fail against an unreachable server")
		}
		if rec.attempts != 1 {
			t.Errorf("the opened project's explicit 0 must disable the budget, got %d dial attempts", rec.attempts)
		}
		if strings.Contains(err.Error(), "open-retry-budget") {
			t.Errorf("a disabled budget must not appear in the error: %v", err)
		}
	})

	t.Run("project-only budget still enables the retry", func(t *testing.T) {
		isolateOpenEnv(t)
		withoutUserGlobalBudget(t)
		project := newBeadsDir(t, "1200ms")
		rec := stubServerDialErrs(t, errConnRefused, errNoSuchHost)

		if _, err := New(context.Background(), cfgWithDataPath(project, newSharedServerDoltPath(t))); err == nil {
			t.Fatal("expected the open to fail against an unreachable server")
		}
		if rec.attempts != 2 {
			t.Errorf("the opened project's own budget must enable the retry, got %d dial attempts", rec.attempts)
		}
	})
}

// A custom absolute dolt_data_dir must not decide it either.
func TestNew_OpenRetryCustomDataDirUsesProjectConfig(t *testing.T) {
	t.Run("project zero is honoured against a positive user-global default", func(t *testing.T) {
		isolateOpenEnv(t)
		withUserGlobalBudget(t, "30s")
		project := newBeadsDir(t, "0")
		rec := stubServerDial(t, -1, errConnRefused, 0)

		_, err := New(context.Background(), cfgWithDataPath(project, newCustomDoltDataPath(t)))
		if err == nil {
			t.Fatal("expected the open to fail against an unreachable server")
		}
		if rec.attempts != 1 {
			t.Errorf("the opened project's explicit 0 must disable the budget, got %d dial attempts", rec.attempts)
		}
	})

	t.Run("project-only budget still enables the retry", func(t *testing.T) {
		isolateOpenEnv(t)
		withoutUserGlobalBudget(t)
		project := newBeadsDir(t, "1200ms")
		rec := stubServerDialErrs(t, errConnRefused, errNoSuchHost)

		if _, err := New(context.Background(), cfgWithDataPath(project, newCustomDoltDataPath(t))); err == nil {
			t.Fatal("expected the open to fail against an unreachable server")
		}
		if rec.attempts != 2 {
			t.Errorf("the opened project's own budget must enable the retry, got %d dial attempts", rec.attempts)
		}
	})
}

// The resolution order itself, at the unit, including the case the two layouts
// above cannot reach: a Config carrying NEITHER directory field, which is what
// bd doctor's federation checks and the ADO config reader hand dolt.New. For a
// non-project data path there the honest answer is "no target directory", so
// the budget falls back on the user-level default and never on the settings of
// whatever project happens to own that path's parent.
func TestOpenRetryBeadsDirPrefersTheDirectoryThisOpenResolvedFrom(t *testing.T) {
	project := filepath.Join("/w", ".beads")
	stale := filepath.Join("/other", ".beads")

	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil config", nil, ""},
		{"empty config", &Config{}, ""},
		{"default layout, path only", &Config{Path: filepath.Join(project, "dolt")}, project},
		{"shared-server path only", &Config{Path: "/home/u/.beads/shared-server/dolt"}, ""},
		{"custom absolute data dir path only", &Config{Path: "/mnt/fast/beads-dolt-data"}, ""},
		{"embedded store path only", &Config{Path: filepath.Join(project, "embeddeddolt")}, project},
		{"relative custom data dir path only", &Config{Path: filepath.Join(project, "fastdata")}, project},
		{"BeadsDir beats a shared-server path", &Config{BeadsDir: project, Path: "/home/u/.beads/shared-server/dolt"}, project},
		{"BeadsDir beats a custom data dir", &Config{BeadsDir: project, Path: "/mnt/fast/beads-dolt-data"}, project},
		{"OpenRetryConfigDir beats a stale BeadsDir", &Config{OpenRetryConfigDir: project, BeadsDir: stale, Path: filepath.Join(stale, "dolt")}, project},
		{"OpenRetryConfigDir beats the path", &Config{OpenRetryConfigDir: project, Path: "/mnt/fast/beads-dolt-data"}, project},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openRetryBeadsDir(tc.cfg); got != tc.want {
				t.Errorf("openRetryBeadsDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// applyResolvedConfig must set OpenRetryConfigDir on EVERY call -- the whole
// point of a field that tracks the open -- while leaving the documented
// cfg.BeadsDir caller override alone.
func TestApplyResolvedConfigOpenRetryDirTracksEveryOpen(t *testing.T) {
	first := filepath.Join(t.TempDir(), ".beads")
	second := filepath.Join(t.TempDir(), ".beads")
	for _, d := range []string{first, second} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	fileCfg := &configfile.Config{Backend: configfile.BackendDolt}
	cfg := &Config{}

	if err := applyResolvedConfig(context.Background(), first, fileCfg, cfg); err != nil {
		t.Fatalf("applyResolvedConfig: %v", err)
	}
	if cfg.OpenRetryConfigDir != first {
		t.Fatalf("first open: OpenRetryConfigDir = %q, want %q", cfg.OpenRetryConfigDir, first)
	}
	if cfg.BeadsDir != first {
		t.Fatalf("first open: BeadsDir = %q, want %q", cfg.BeadsDir, first)
	}

	if err := applyResolvedConfig(context.Background(), second, fileCfg, cfg); err != nil {
		t.Fatalf("applyResolvedConfig: %v", err)
	}
	if cfg.OpenRetryConfigDir != second {
		t.Errorf("second open: OpenRetryConfigDir = %q, want %q -- it must be rewritten every time", cfg.OpenRetryConfigDir, second)
	}
	if cfg.BeadsDir != first {
		t.Errorf("second open: BeadsDir = %q, want the caller override %q left alone", cfg.BeadsDir, first)
	}
}

// The breaker guard, exercised on the path it actually guards.
//
// The guard is `!(openRetryEnabled(cfg) && openEndedByCaller(ctx))`, so with
// the budget OFF it short-circuits before openEndedByCaller is reached: the
// budget-off server-timeout test above pins the DEFAULT path and says nothing
// about how "who ended this open" is decided. These two arms turn the budget
// ON and separate the two answers, which is where an errors.Is guard and a
// context guard disagree:
//
//   - the server never answers (errIOTimeout on every dial, which satisfies
//     errors.Is(err, context.DeadlineExceeded)) while the caller's context is
//     perfectly healthy -- server evidence, ONE recorded failure;
//   - the caller's own deadline expires mid-dial -- caller evidence, ZERO.
//
// An errors.Is guard reports "the caller ended it" for both, so it silently
// stops counting a dead server towards the breaker on the enabled path.
func TestNew_OpenRetryEnabledServerTimeoutCountsTowardsBreaker(t *testing.T) {
	circuitDir := isolateBreakerEnv(t)

	beadsDir := newBeadsDir(t, "1200ms")
	rec := stubServerDial(t, -1, errIOTimeout{}, 0)

	cfg := productionCfg(beadsDir)
	ctx := context.Background()
	_, err := New(ctx, cfg)
	if err == nil {
		t.Fatal("expected the open to fail against a server that never answers")
	}

	// Positive control: the budget really was live for this open, so this is
	// the ENABLED path and not the fail-fast one the budget-off test covers.
	if got := effectiveOpenRetryBudget(cfg); got != 1200*time.Millisecond {
		t.Fatalf("precondition: New must have resolved the configured budget, got %s", got)
	}
	if !openRetryEnabled(cfg) {
		t.Fatal("precondition: this open must be on the enabled path")
	}
	if rec.attempts < 2 {
		t.Fatalf("precondition: expected the budget to retry, got %d dial attempts", rec.attempts)
	}
	// The caller is healthy throughout: whatever ended this open, it was not
	// the caller. Asserted rather than assumed, because it is the ONE input
	// the guard is allowed to read.
	if ctx.Err() != nil {
		t.Fatalf("precondition: the caller's context must be healthy, got %v", ctx.Err())
	}
	// ...and the error nonetheless satisfies the identity test a naive guard
	// would have used. Without this the arm would pass against a guard that
	// happens never to fire.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("precondition: a net dial timeout satisfies errors.Is(err, context.DeadlineExceeded); got %v", err)
	}

	if failures := circuitFailuresRecorded(t, circuitDir); failures != 1 {
		t.Errorf("a server that never answers must record exactly 1 failure on the enabled path, got %d (after %d dial attempts)",
			failures, rec.attempts)
	}
}

// The other direction: the caller's deadline expires while a dial is in
// flight. That is evidence about the caller, so nothing may be recorded --
// five such opens inside the failure window would otherwise trip the breaker
// against a healthy server.
func TestNew_OpenRetryCallerDeadlineDuringOpenRecordsNoCircuitFailure(t *testing.T) {
	circuitDir := isolateBreakerEnv(t)

	// A budget far longer than the caller's deadline, so the open is still
	// mid-retry when the deadline lands: the caller wins the race by
	// construction, not by timing luck.
	beadsDir := newBeadsDir(t, "30s")
	// Every dial blocks for the whole probe timeout (the stub caps its sleep
	// at the timeout it was handed), so the 250ms deadline lands INSIDE a
	// dial rather than between two of them.
	rec := stubServerDial(t, -1, errConnRefused, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := New(ctx, productionCfg(beadsDir))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the open to fail once the caller's deadline expired")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the caller's deadline to stay identifiable in the error, got %v", err)
	}
	// Preconditions: the open really reached a dial (so this is not a
	// zero-dial open passing vacuously), and it ended on the CALLER's
	// deadline rather than on the 30s budget.
	if rec.attempts < 1 {
		t.Fatalf("precondition: expected the open to reach a dial, got %d attempts", rec.attempts)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("precondition: the open must end on the caller's deadline, not the budget; took %s", elapsed)
	}

	if failures := circuitFailuresRecorded(t, circuitDir); failures != 0 {
		t.Errorf("an open ended by the caller's deadline must not count towards the breaker, got %d recorded failures", failures)
	}

	// Positive control, same breaker directory and same config: an open with
	// no caller deadline DOES record one. Without it the zero above is
	// indistinguishable from a breaker that was never live.
	if _, err := New(context.Background(), productionCfg(newBeadsDir(t, "600ms"))); err == nil {
		t.Fatal("expected the uncancelled open to fail against an unreachable server")
	}
	if failures := circuitFailuresRecorded(t, circuitDir); failures != 1 {
		t.Errorf("control: an open with a healthy caller must record exactly 1 failure, got %d", failures)
	}
}

// A hand-built Config can be RETARGETED at another project, including the
// directory the budget is read from. The field applyResolvedConfig writes is
// public for exactly this reason: a caller that reuses one Config across two
// opens updates Path, BeadsDir and OpenRetryConfigDir together, on the same
// terms, and the second open must honour the second project's setting.
func TestNew_OpenRetryConfigDirIsRetargetableByHand(t *testing.T) {
	isolateOpenEnv(t)
	withoutUserGlobalBudget(t)
	retrying := newBeadsDir(t, "1200ms")
	optedOut := newBeadsDir(t, "0")

	// First open, through applyResolvedConfig, exactly as a
	// NewFromConfigWithOptions caller reaches it.
	cfg := productionCfg(retrying)
	fileCfg := &configfile.Config{Backend: configfile.BackendDolt}
	if err := applyResolvedConfig(context.Background(), retrying, fileCfg, cfg); err != nil {
		t.Fatalf("applyResolvedConfig: %v", err)
	}
	rec := stubServerDialErrs(t, errConnRefused, errNoSuchHost)
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("expected the first open to fail")
	}
	if rec.attempts != 2 {
		t.Fatalf("precondition: the first project's budget must retry, got %d dial attempts", rec.attempts)
	}
	if cfg.OpenRetryConfigDir != retrying {
		t.Fatalf("precondition: the first open must have recorded its own directory, got %q", cfg.OpenRetryConfigDir)
	}

	// Retargeted BY HAND -- no applyResolvedConfig this time -- and then
	// opened through New directly, which is the caller shape that could not
	// reach this field while it was unexported.
	cfg.Path = filepath.Join(optedOut, "dolt")
	cfg.BeadsDir = optedOut
	cfg.OpenRetryConfigDir = optedOut
	rec2 := stubServerDial(t, -1, errConnRefused, 0)
	_, err := New(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected the second open to fail")
	}
	if rec2.attempts != 1 {
		t.Errorf("the retargeted open must honour the new project's explicit 0, got %d dial attempts", rec2.attempts)
	}
	if strings.Contains(err.Error(), "open-retry-budget") {
		t.Errorf("a disabled budget must not appear in the error: %v", err)
	}

	// ...and clearing the field is always safe: resolution falls through to
	// BeadsDir, which the same retarget updated.
	cfg.OpenRetryConfigDir = ""
	rec3 := stubServerDial(t, -1, errConnRefused, 0)
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("expected the third open to fail")
	}
	if rec3.attempts != 1 {
		t.Errorf("clearing the field must fall through to BeadsDir, got %d dial attempts", rec3.attempts)
	}
}

// blackholeAddr is a TEST-NET-1 address (RFC 5737): routable in form, answered
// by nobody, so a TCP connect to it hangs in SYN retransmit rather than
// failing fast. That is the only shape in which a dial TIMEOUT is observable.
const blackholeAddr = "192.0.2.1:9"

// The production dialer must honour the per-attempt timeout it is handed.
//
// Every budget test stubs both dial vars, and the two tests that do call the
// real ones drive them with an ALREADY-CANCELLED context, which returns before
// the timeout can matter. So the timeout itself had no coverage: removing
// `Timeout: timeout` from the dialer left the whole suite green while
// defeating the budget on exactly the endpoint it exists for -- one that never
// answers, where without a dial timeout the first probe runs to the OS connect
// timeout (minutes) and the budget bounds nothing.
//
// The context here is deliberately uncancelled and deadline-free: the timeout
// is the only thing that can end this dial.
func TestOpenRetryProductionDialHonoursItsTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	// The dial runs off the test goroutine and is WAITED ON with a bound. A
	// dialer that ignores its timeout does not fail this test, it HANGS -- and
	// an unbounded wait would burn the whole package's `go test` timeout and
	// report as a panic in whatever test happened to be running. Bounding it
	// here makes that failure a named assertion instead.
	type dialResult struct {
		conn    net.Conn
		err     error
		elapsed time.Duration
	}
	done := make(chan dialResult, 1)
	go func() {
		start := time.Now()
		conn, err := serverDial(context.Background(), "tcp", blackholeAddr, timeout)
		done <- dialResult{conn: conn, err: err, elapsed: time.Since(start)}
	}()

	var got dialResult
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the dial did not return within 5s against a %s timeout: the production dialer is not honouring it", timeout)
	}
	conn, err, elapsed := got.conn, got.err, got.elapsed
	if conn != nil {
		_ = conn.Close()
		t.Fatalf("precondition: %s must not be connectable", blackholeAddr)
	}
	if err == nil {
		t.Fatalf("precondition: expected a dial error against %s", blackholeAddr)
	}

	// Fixture control. A network that rejects this address outright (no route,
	// a filtering middlebox answering RST) fails in microseconds and cannot
	// exhibit a timeout at all, so the measurement below would be about the
	// network rather than about the dialer. Skip rather than assert, and say
	// what was measured -- a green here must never mean "the address answered
	// too quickly to tell".
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Skipf("this network does not blackhole %s (err after %s: %v), so a dial timeout is not observable here",
			blackholeAddr, elapsed.Round(time.Millisecond), err)
	}

	// The upper bound is the select above -- a dialer that ignored its timeout
	// would still be in SYN retransmit, since the OS connect timeout is on the
	// order of minutes. What is left to check is that it did not return EARLY
	// either, which would mean the timeout is being shortened somewhere and a
	// retry gets less than the budget allows.
	if elapsed < timeout {
		t.Errorf("the dial returned after %s, before its own %s timeout", elapsed.Round(time.Millisecond), timeout)
	}
}

// The public constructors, which no other test here drives.
//
// Every positive retry fixture in this file builds a Config directly and sets
// ServerMode -- the field the CLI's store factory sets and applyResolvedConfig
// does not. So the whole file would survive re-adding `!cfg.ServerMode` to the
// exclusion guard, which is exactly the defect the first review round found:
// it makes the feature unreachable for library and `bd doctor` opens, whose
// ServerMode stays false even against a genuinely remote server.
//
// These arms go in through NewFromConfigWithOptions with an EXTERNAL server in
// metadata.json and no caller overrides at all, so what decides is the
// workspace on disk.
func TestNewFromConfig_OpenRetryHonoursTheOpenedWorkspace(t *testing.T) {
	writeExternalWorkspace := func(t *testing.T, budget string) string {
		t.Helper()
		beadsDir := newBeadsDir(t, budget)
		// dolt_server_host is a NON-localhost name, which is what makes this
		// an externally managed server: bd cannot start it, so waiting is the
		// only remedy it has -- the case the budget exists for.
		meta := &configfile.Config{
			Backend:        configfile.BackendDolt,
			DoltMode:       "server",
			DoltServerHost: externalHost,
			DoltServerPort: externalTestPort,
			DoltDatabase:   "openretry_probe",
		}
		if err := meta.Save(beadsDir); err != nil {
			t.Fatalf("save metadata.json: %v", err)
		}
		return beadsDir
	}

	t.Run("project budget enables the retry", func(t *testing.T) {
		isolateOpenEnv(t)
		withoutUserGlobalBudget(t)
		beadsDir := writeExternalWorkspace(t, "1200ms")
		rec := stubServerDialErrs(t, errConnRefused, errNoSuchHost)

		if _, err := NewFromConfigWithOptions(context.Background(), beadsDir, nil); err == nil {
			t.Fatal("expected the open to fail against an unreachable server")
		}
		// The discriminator: ServerMode is never set on this path, so a guard
		// keyed on it would report exactly one attempt here.
		if rec.attempts != 2 {
			t.Errorf("a public-constructor open on an external workspace must retry, got %d dial attempts", rec.attempts)
		}
	})

	t.Run("project zero disables it against a positive user-global default", func(t *testing.T) {
		isolateOpenEnv(t)
		withUserGlobalBudget(t, "30s")
		beadsDir := writeExternalWorkspace(t, "0")
		rec := stubServerDial(t, -1, errConnRefused, 0)

		if _, err := NewFromConfigWithOptions(context.Background(), beadsDir, nil); err == nil {
			t.Fatal("expected the open to fail against an unreachable server")
		}
		if rec.attempts != 1 {
			t.Errorf("the opened workspace's explicit 0 must disable the budget, got %d dial attempts", rec.attempts)
		}
	})

	t.Run("an embedded workspace never waits", func(t *testing.T) {
		isolateOpenEnv(t)
		withUserGlobalBudget(t, "30s")
		beadsDir := newBeadsDir(t, "30s")
		// An explicitly embedded workspace with a leftover local server
		// endpoint: the version-maintenance probes open one of these with
		// NewFromConfig* and reach the server path. There is no server for
		// waiting to help, so the budget must not engage however it is
		// configured.
		meta := &configfile.Config{
			Backend:      configfile.BackendDolt,
			DoltMode:     "embedded",
			DoltDatabase: "openretry_probe",
		}
		if err := meta.Save(beadsDir); err != nil {
			t.Fatalf("save metadata.json: %v", err)
		}
		rec := stubServerDial(t, -1, errConnRefused, 0)

		// No DisableAutoStart here, deliberately: that flag is its OWN
		// exclusion, so passing it would make this arm pass whether or not
		// the embedded exclusion exists. The workspace's own
		// "dolt.auto-start: false" (newBeadsDir writes it) is what leaves the
		// open reaching the gate with nothing else excluding it.
		cfg := &Config{}
		if _, err := NewFromConfigWithOptions(context.Background(), beadsDir, cfg); err == nil {
			t.Fatal("expected the open to fail")
		}
		if cfg.AutoStart {
			t.Fatalf("precondition: the workspace's auto-start: false must survive, or another exclusion is doing this arm's work")
		}
		if cfg.DisableAutoStart || cfg.ServerSocket != "" || cfg.ProxiedServer {
			t.Fatalf("precondition: no OTHER exclusion may apply here (DisableAutoStart=%v socket=%q proxied=%v)",
				cfg.DisableAutoStart, cfg.ServerSocket, cfg.ProxiedServer)
		}
		if rec.attempts != 1 {
			t.Errorf("an embedded workspace must not wait out a budget, got %d dial attempts", rec.attempts)
		}
	})
}
