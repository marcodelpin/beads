package main

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/steveyegge/beads/internal/storage"
)

// TestServeFlags pins the flag surface. v0 has two flags and no more: every
// other bound in this server (in-flight limit, connection cap, wait budget,
// deadline, pool caps) is a constant precisely so that it can become a flag
// later, deliberately, rather than arriving as one nobody designed.
func TestServeFlags(t *testing.T) {
	var got []string
	serveCmd.Flags().VisitAll(func(f *pflag.Flag) { got = append(got, f.Name) })
	sort.Strings(got)

	want := []string{"addr", "allow-non-loopback"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("bd serve flags = %v, want %v", got, want)
	}

	addr := serveCmd.Flags().Lookup("addr")
	if addr == nil {
		t.Fatal("no --addr flag")
	}
	// Loopback, and ephemeral: a default port would be a port nobody chose,
	// and picking one blesses a deployment shape the design deliberately does
	// not bless.
	if addr.DefValue != "127.0.0.1:0" {
		t.Errorf("--addr default = %q, want 127.0.0.1:0", addr.DefValue)
	}
	if nonLoopback := serveCmd.Flags().Lookup("allow-non-loopback"); nonLoopback == nil {
		t.Fatal("no --allow-non-loopback flag")
	} else if nonLoopback.DefValue != "false" {
		t.Errorf("--allow-non-loopback default = %q, want false", nonLoopback.DefValue)
	}
}

func TestServeCommandIsRegistered(t *testing.T) {
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "serve" {
			return
		}
	}
	t.Fatal("serve is not registered under the root command")
}

// TestServeRefusalsPromiseNothing is the honesty gate on the mode gate. Both
// refusals are typed so a caller can dispatch on them; what differs is what
// they claim, and getting that wrong sends an operator to do the wrong work.
func TestServeRefusalsPromiseNothing(t *testing.T) {
	t.Run("embedded is permanent", func(t *testing.T) {
		err := errServeEmbedded()

		var unsupported *storage.ErrUnsupported
		if !errors.As(err, &unsupported) {
			t.Fatalf("err = %v, want a typed storage.ErrUnsupported", err)
		}
		if unsupported.Op != "serve" {
			t.Errorf("Op = %q, want serve", unsupported.Op)
		}
		// Backend names a BACKEND. The type documents it that way and it is the
		// embryo of the pluggable-backend error taxonomy, so a topology string
		// here would hand every downstream errors.As a mixed vocabulary.
		if unsupported.Backend != "embedded-dolt" {
			t.Errorf("Backend = %q, want embedded-dolt", unsupported.Backend)
		}

		msg := err.Error()
		if !strings.Contains(msg, "embedded Dolt") {
			t.Errorf("message does not name the workspace's actual backend: %q", msg)
		}
		for _, promise := range []string{"not yet", "coming", "tracked", "will be"} {
			if strings.Contains(strings.ToLower(msg), promise) {
				t.Errorf("permanent refusal hints at future support (%q): %q", promise, msg)
			}
		}
	})

	t.Run("dolt server mode is staged", func(t *testing.T) {
		err := errServeDoltServerMode()

		var unsupported *storage.ErrUnsupported
		if !errors.As(err, &unsupported) {
			t.Fatalf("err = %v, want a typed storage.ErrUnsupported", err)
		}
		if unsupported.Backend != "dolt" {
			t.Errorf("Backend = %q, want dolt (the mode belongs in the message text)", unsupported.Backend)
		}

		msg := err.Error()
		// It must say this is not supported YET, and name what is supported
		// today, so nobody reads it as a permanent architectural limit.
		if !strings.Contains(msg, "not yet supported by bd serve") {
			t.Errorf("staged refusal does not say the support is not yet there: %q", msg)
		}
		if !strings.Contains(msg, "proxied-server") || !strings.Contains(msg, "today") {
			t.Errorf("staged refusal does not name what works today: %q", msg)
		}
		// And it must not claim the mode it is refusing works.
		if strings.Contains(msg, "dolt server mode is supported") {
			t.Errorf("staged refusal claims the refused mode works: %q", msg)
		}
	})
}
