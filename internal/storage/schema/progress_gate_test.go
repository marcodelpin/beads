package schema

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// The tests below pin sys-fzjf03: a migration pass slow enough to look like a
// hang must stop being silent on a non-terminal stderr, without making any
// fast pass noisy. Both halves matter — dropping the gate entirely would
// regress the machine-parsed-output contract defaultStderr exists to keep.

func TestDelayedWriterSilentBeforeDeadline(t *testing.T) {
	var buf bytes.Buffer
	d := &delayedWriter{w: &buf, after: time.Now().Add(time.Hour), header: "HEADER\n"}

	n, err := d.Write([]byte("Applying migration 0038: drop_hop_columns\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want := len("Applying migration 0038: drop_hop_columns\n"); n != want {
		t.Errorf("Write returned n=%d, want %d (a swallowed write must still report a full write)", n, want)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %q before the deadline; want nothing", buf.String())
	}
}

func TestDelayedWriterOpensAfterDeadlineWithHeaderOnce(t *testing.T) {
	var buf bytes.Buffer
	d := &delayedWriter{w: &buf, after: time.Now().Add(-time.Second), header: "HEADER\n"}

	if _, err := d.Write([]byte("  done (71.4s)\n")); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := d.Write([]byte("Applying migration 0039: drop_child_counters_fk\n")); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	got := buf.String()
	if n := strings.Count(got, "HEADER"); n != 1 {
		t.Errorf("header written %d times, want exactly 1; got %q", n, got)
	}
	if !strings.Contains(got, "  done (71.4s)") || !strings.Contains(got, "Applying migration 0039") {
		t.Errorf("both post-deadline lines should pass through; got %q", got)
	}
	if !strings.HasPrefix(got, "HEADER\n") {
		t.Errorf("header must precede the first passed-through line; got %q", got)
	}
}

func TestPassProgressWriterPreservesInjectedWriter(t *testing.T) {
	var buf bytes.Buffer
	orig := stderr
	stderr = &buf
	defer func() { stderr = orig }()

	// A terminal stderr, and a test's injected buffer, are already the right
	// destination: the gate must not wrap them or a TTY user would lose the
	// first nonTTYProgressDelay of progress they can see today.
	if got := passProgressWriter(time.Now()); got != io.Writer(&buf) {
		t.Errorf("passProgressWriter wrapped a non-discard stderr (%T); want it returned as-is", got)
	}
}

func TestPassProgressWriterGatesDiscardStderr(t *testing.T) {
	orig := stderr
	stderr = io.Discard
	defer func() { stderr = orig }()

	start := time.Now()
	got, ok := passProgressWriter(start).(*delayedWriter)
	if !ok {
		t.Fatalf("passProgressWriter returned %T; want *delayedWriter so a slow pass becomes audible", passProgressWriter(start))
	}
	if want := start.Add(nonTTYProgressDelay); !got.after.Equal(want) {
		t.Errorf("deadline = %v, want %v (pass start + nonTTYProgressDelay)", got.after, want)
	}
	if got.header == "" {
		t.Error("gated writer has no header; a lone trailing \"done\" line reads as corrupted output")
	}
}
