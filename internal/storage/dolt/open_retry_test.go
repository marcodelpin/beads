package dolt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// The open-retry budget (dolt.open-retry-budget, GH#4379) wraps newServerMode's
// fail-fast pre-dial probe in a bounded retry for EXTERNAL server mode only.
// These tests stub serverDial so they run without a live dolt sql-server.

const (
	externalHost = "dolt.example.internal"
	externalAddr = "dolt.example.internal:3306"
)

// errConnRefused is retryable per isRetryableError ("connection refused").
var errConnRefused = errors.New("dial tcp 10.0.0.9:3306: connect: connection refused")

// errNoSuchHost is NOT retryable: no substring isRetryableError recognizes.
var errNoSuchHost = errors.New("dial tcp: lookup dolt.example.internal: no such host")

type fakeConn struct{ net.Conn }

// stubServerDial replaces serverDial with one that fails failures times before
// succeeding. A negative failures count never succeeds. It returns a pointer to
// the attempt counter.
func stubServerDial(t *testing.T, failures int, err error) *int {
	t.Helper()
	orig := serverDial
	t.Cleanup(func() { serverDial = orig })
	attempts := 0
	serverDial = func(_, _ string, _ time.Duration) (net.Conn, error) {
		attempts++
		if failures < 0 || attempts <= failures {
			return nil, err
		}
		return &fakeConn{}, nil
	}
	return &attempts
}

// externalCfg is a config in the mode the budget targets: server mode, no
// socket, non-local host.
func externalCfg(budget time.Duration) *Config {
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
		{"budget unset", externalCfg(0)},
		{"budget zero explicitly", externalCfg(0)},
		{"budget negative", externalCfg(-5 * time.Second)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempts := stubServerDial(t, -1, errConnRefused)
			conn, err := dialServerPreflight(context.Background(), tc.cfg, "tcp", externalAddr, time.Millisecond)
			if conn != nil {
				t.Errorf("expected no connection, got %v", conn)
			}
			if *attempts != 1 {
				t.Errorf("expected exactly 1 dial attempt with the budget off, got %d", *attempts)
			}
			// Byte-identical: not merely errors.Is-compatible, the very same
			// error value with no wrapping text added.
			if err != errConnRefused {
				t.Errorf("expected the dial error returned unwrapped, got %v", err)
			}
			if strings.Contains(err.Error(), "open-retry-budget") {
				t.Errorf("fail-fast error must not mention the budget: %v", err)
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
			attempts := stubServerDial(t, wantAttempts-1, errConnRefused)
			// 30s is far above the worst-case cost of two backoff sleeps
			// (~1.9s), so the budget cannot end the loop early here.
			conn, err := dialServerPreflight(context.Background(), externalCfg(30*time.Second), "tcp", externalAddr, time.Millisecond)
			if err != nil {
				t.Fatalf("expected success within the budget, got %v", err)
			}
			if conn == nil {
				t.Fatal("expected a connection on success")
			}
			if *attempts != wantAttempts {
				t.Errorf("expected %d dial attempts, got %d", wantAttempts, *attempts)
			}
		})
	}
}

// (c) Budget on and never reachable: the open fails after a BOUNDED number of
// attempts, within roughly the budget, and the error names the budget while
// still wrapping the underlying dial error.
func TestOpenRetryBudgetOn_ExhaustsBudget(t *testing.T) {
	const budget = 750 * time.Millisecond
	attempts := stubServerDial(t, -1, errConnRefused)
	start := time.Now()
	conn, err := dialServerPreflight(context.Background(), externalCfg(budget), "tcp", externalAddr, time.Millisecond)
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
	if *attempts < 2 {
		t.Errorf("expected the budget to buy at least one retry, got %d attempts", *attempts)
	}
	// Bounded: the loop must terminate on the budget, not spin. The ceiling is
	// deliberately loose (the backoff is jittered) -- it fails a runaway loop,
	// not a slow machine.
	if *attempts > 12 {
		t.Errorf("expected a bounded attempt count, got %d", *attempts)
	}
	if elapsed > budget+10*time.Second {
		t.Errorf("expected the loop to end near the budget (%s), took %s", budget, elapsed)
	}
}

// A non-retryable failure ends the budget immediately and surfaces exactly as
// it would have without the budget. Retrying a DNS failure for 30s would be a
// worse UX than today's fail-fast, not a better one.
func TestOpenRetryBudgetOn_NonRetryableFailsImmediately(t *testing.T) {
	attempts := stubServerDial(t, -1, errNoSuchHost)
	_, err := dialServerPreflight(context.Background(), externalCfg(30*time.Second), "tcp", externalAddr, time.Millisecond)
	if *attempts != 1 {
		t.Errorf("expected exactly 1 attempt for a non-retryable error, got %d", *attempts)
	}
	if err != errNoSuchHost {
		t.Errorf("expected the non-retryable error returned unwrapped, got %v", err)
	}
}

// (d) + (e): the maintainer's explicit ask. With the budget CONFIGURED, every
// mode other than external-server takes exactly one dial attempt and returns
// the fail-fast error.
func TestOpenRetryBudgetDoesNotEngageInNonExternalModes(t *testing.T) {
	const budget = 30 * time.Second
	cases := []struct {
		name string
		cfg  *Config
	}{
		{
			// (d) Embedded: the store factory routes these to
			// internal/storage/embeddeddolt and they never reach
			// newServerMode. ServerMode=false makes that inert by
			// construction rather than by routing.
			name: "embedded (ServerMode false)",
			cfg: &Config{
				ServerMode:      false,
				ServerHost:      externalHost,
				ServerPort:      3306,
				OpenRetryBudget: budget,
			},
		},
		{
			// (e) Localhost-managed / auto-start: EnsureRunningDetailed
			// recovers by starting a server, so waiting is the wrong remedy.
			name: "localhost-managed auto-start (127.0.0.1)",
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
			name: "localhost-managed auto-start (localhost)",
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
			name: "localhost-managed, host defaulted empty",
			cfg: &Config{
				ServerMode:      true,
				ServerHost:      "",
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if openRetryEnabled(tc.cfg) {
				t.Fatal("openRetryEnabled must be false in this mode even with a budget configured")
			}
			attempts := stubServerDial(t, -1, errConnRefused)
			start := time.Now()
			_, err := dialServerPreflight(context.Background(), tc.cfg, "tcp", externalAddr, time.Millisecond)
			if *attempts != 1 {
				t.Errorf("expected exactly 1 dial attempt (no retry), got %d", *attempts)
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

// The budget's gate and the auto-start path's gate must be mutually exclusive
// by construction, not by ordering inside newServerMode: no host may satisfy
// both. This is what lets (e) above be a statement about the code rather than
// about the branch order.
func TestOpenRetryAndAutoStartGatesAreMutuallyExclusive(t *testing.T) {
	hosts := []string{
		"", "localhost", "LOCALHOST", " localhost ", "127.0.0.1", "::1", "[::1]",
		"0.0.0.0", "dolt.example.internal", "10.0.0.9", "hosted.doltdb.com",
	}
	for _, host := range hosts {
		cfg := &Config{
			ServerMode:      true,
			ServerHost:      host,
			ServerPort:      3306,
			Path:            "/tmp/does-not-matter/.beads/dolt",
			AutoStart:       true,
			OpenRetryBudget: 30 * time.Second,
		}
		if openRetryEnabled(cfg) && serverOpenCanAutoStart(cfg) {
			t.Errorf("host %q enables both the open-retry budget and auto-start", host)
		}
	}
}

// A cancelled context ends the retry loop promptly instead of burning the
// whole budget.
func TestOpenRetryBudgetOn_ContextCancellationStops(t *testing.T) {
	attempts := stubServerDial(t, -1, errConnRefused)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := dialServerPreflight(ctx, externalCfg(30*time.Second), "tcp", externalAddr, time.Millisecond)
	if err == nil {
		t.Fatal("expected an error on a cancelled context")
	}
	if !errors.Is(err, errConnRefused) {
		t.Errorf("expected the dial error to remain in the chain, got %v", err)
	}
	if *attempts > 2 {
		t.Errorf("expected the cancelled context to stop the loop immediately, got %d attempts", *attempts)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
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
