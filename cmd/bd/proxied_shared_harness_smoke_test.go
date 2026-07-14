//go:build cgo

package main

import (
	"strings"
	"testing"
)

func TestProxiedSharedHarnessSerial(t *testing.T) {
	bd := buildEmbeddedBD(t)

	for _, prefix := range []string{"aa", "bb", "cc"} {
		t.Run(prefix, func(t *testing.T) {
			p := newSharedProxiedProject(t, bd, prefix)

			created := bdProxiedCreate(t, bd, p.dir, "Issue in "+prefix)
			if !strings.HasPrefix(created.ID, prefix+"-") {
				t.Fatalf("ID %q should have prefix %q-", created.ID, prefix)
			}

			shown := bdProxiedShow(t, bd, p.dir, created.ID)
			if shown.Title != "Issue in "+prefix {
				t.Errorf("title: got %q, want %q", shown.Title, "Issue in "+prefix)
			}

			listed := bdProxiedListJSON(t, bd, p)
			if len(listed) != 1 {
				t.Errorf("database %s should hold exactly 1 issue, got %d", p.database, len(listed))
			}
		})
	}
}

func TestProxiedSharedHarnessParallel(t *testing.T) {
	t.Parallel()
	bd := buildEmbeddedBD(t)

	prefixes := []string{
		"pa", "pb", "pc", "pd", "pe", "pf",
		"pg", "ph", "pi", "pj", "pk", "pl",
	}
	for _, prefix := range prefixes {
		prefix := prefix
		t.Run(prefix, func(t *testing.T) {
			t.Parallel()
			p := newSharedProxiedProject(t, bd, prefix)

			created := bdProxiedCreate(t, bd, p.dir, "Issue in "+prefix)
			if !strings.HasPrefix(created.ID, prefix+"-") {
				t.Fatalf("ID %q should have prefix %q-", created.ID, prefix)
			}

			shown := bdProxiedShow(t, bd, p.dir, created.ID)
			if shown.Title != "Issue in "+prefix {
				t.Errorf("title: got %q, want %q", shown.Title, "Issue in "+prefix)
			}

			listed := bdProxiedListJSON(t, bd, p)
			if len(listed) != 1 {
				t.Errorf("database %s should hold exactly 1 issue, got %d", p.database, len(listed))
			}
		})
	}
}
