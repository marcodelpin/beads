//go:build cgo

package main

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// TestEmbeddedListSearch pins bda-nldy's acceptance property end to end:
// `bd list --search=TERM` answers with the SAME population `bd search TERM`
// answers with.
//
// The defect it replaces was not a wrong population but a missing flag, and the
// report that found it could not tell the two apart: the probe was
// `bd list --search=X 2>/dev/null | grep -c .`, which turns cobra's
// "unknown flag" on stderr into a measured-looking zero. Every assertion below
// therefore carries its own control, so an empty answer can never read as a
// matching one.
func TestEmbeddedListSearch(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "ls")

	guard := bdCreate(t, bd, dir, "LXC-GUARD false positive on scrape", "--type", "bug", "--priority", "1")
	guardToo := bdCreate(t, bd, dir, "retire the LXC-GUARD advisory", "--type", "task", "--priority", "2")
	unrelated := bdCreate(t, bd, dir, "rotate the backup keys", "--type", "chore", "--priority", "3")

	// The verb includes closed rows by default and so must the flag: the
	// dominant use is "was this already filed?", where excluding closed is the
	// false "no" this whole ticket is about.
	closedGuard := bdCreate(t, bd, dir, "LXC-GUARD stale lock, already fixed", "--type", "bug")
	bdClose(t, bd, dir, closedGuard.ID)

	t.Run("the flag and the verb select the same rows", func(t *testing.T) {
		// BOTH SIDES BARE. This is the acceptance sentence bda-nldy was filed
		// on: `bd list --search TERM` answers with the population
		// `bd search TERM` answers with. It is asserted on the DEFAULTS because
		// that is what a caller types - a comparison run under an explicit
		// --status all would agree even if the flag inherited the listing's
		// closed-row exclusion, which is the half that makes an
		// already-filed-and-fixed issue read as absent.
		fromFlag := searchSortedIDs(listIssueIDs(bdListJSON(t, bd, dir, "--search", "LXC-GUARD", "--limit", "0")))
		fromVerb := searchSortedIDs(searchJSONIDs(bdSearchJSON(t, bd, dir, "LXC-GUARD", "--limit", "0")))

		// POSITIVE CONTROL. Two empty slices are equal, which is exactly how a
		// flag that parsed and filtered nothing - or one that did not exist -
		// would pass this comparison.
		if len(fromVerb) == 0 {
			t.Fatalf("control failed: the verb found nothing for LXC-GUARD, so the comparison below is vacuous")
		}
		if !strings.Contains(strings.Join(fromFlag, " "), guard.ID) {
			t.Errorf("--search did not return %s (%q); got %v", guard.ID, "LXC-GUARD false positive on scrape", fromFlag)
		}
		if !sameIDSet(fromFlag, fromVerb) {
			t.Errorf("--search and the search verb disagree:\n  --search: %v\n  verb:     %v", fromFlag, fromVerb)
		}
		// Named explicitly so a regression says WHICH rows moved rather than
		// only that two sets differ.
		for _, want := range []string{guard.ID, guardToo.ID, closedGuard.ID} {
			if !hasID(fromFlag, want) {
				t.Errorf("--search=LXC-GUARD missed %s", want)
			}
		}
		if hasID(fromFlag, unrelated.ID) {
			t.Errorf("--search=LXC-GUARD returned the unrelated issue %s", unrelated.ID)
		}

		// The closed row is named twice on purpose: it is the member a listing
		// hides by default, so its presence is what separates "the flag works"
		// from "the flag works except where it matters most".
		if !hasID(fromFlag, closedGuard.ID) {
			t.Errorf("a bare --search omitted the CLOSED match %s; the anti-duplicate check would answer no for anything already fixed", closedGuard.ID)
		}

		// CONTROL: a plain listing DOES hide it, so the line above is measuring
		// --search's own default and not a database with nothing hidden.
		plain := searchSortedIDs(listIssueIDs(bdListJSON(t, bd, dir, "--limit", "0")))
		if hasID(plain, closedGuard.ID) {
			t.Fatalf("control failed: a plain `bd list` already shows the closed issue, so --search's widened default is untested here")
		}
	})

	t.Run("an explicit --status still narrows the search", func(t *testing.T) {
		open := searchSortedIDs(listIssueIDs(bdListJSON(t, bd, dir, "--search", "LXC-GUARD", "--status", "open", "--limit", "0")))
		if hasID(open, closedGuard.ID) {
			t.Errorf("--status open was ignored: the closed issue %s came back", closedGuard.ID)
		}
		if !hasID(open, guard.ID) {
			t.Errorf("--search --status open lost the open match %s; got %v", guard.ID, open)
		}
	})

	t.Run("an unmatched term returns an empty set and says so by contrast", func(t *testing.T) {
		none := listIssueIDs(bdListJSON(t, bd, dir, "--search", "nonexistentxyz123", "--status", "all", "--limit", "0"))
		if len(none) != 0 {
			t.Errorf("--search on a term nothing carries returned %d rows: %v", len(none), none)
		}
		// CORPUS CONTROL, the third reading the original report used: without
		// it a zero here is indistinguishable from a broken query path.
		all := listIssueIDs(bdListJSON(t, bd, dir, "--status", "all", "--limit", "0"))
		if len(all) == 0 {
			t.Fatal("control failed: the corpus is empty, so the zero above is unmeasured rather than negative")
		}
	})

	t.Run("--search matches ids where --title cannot", func(t *testing.T) {
		// This is the difference that makes --search a flag of its own rather
		// than an alias for --title: the verb's matcher also reaches the ID.
		byID := listIssueIDs(bdListJSON(t, bd, dir, "--search", guard.ID, "--status", "all", "--limit", "0"))
		if !hasID(searchSortedIDs(byID), guard.ID) {
			t.Errorf("--search=%s did not find the issue with that id; got %v", guard.ID, byID)
		}
		// An inclusion check alone passes on a build that returns EVERYTHING -
		// measured: the mutant that dropped the term kept this subtest green
		// until this exclusion was added. An id is unique, so the answer is one
		// row and the unrelated issue must be absent from it.
		if hasID(searchSortedIDs(byID), unrelated.ID) {
			t.Errorf("--search=%s returned the unrelated issue %s; the term is not being applied", guard.ID, unrelated.ID)
		}
		if len(byID) != 1 {
			t.Errorf("--search on an id returned %d rows, want exactly 1: %v", len(byID), byID)
		}
		byTitle := listIssueIDs(bdListJSON(t, bd, dir, "--title", guard.ID, "--status", "all", "--limit", "0"))
		if len(byTitle) != 0 {
			t.Errorf("control failed: --title=%s matched %v, so this case no longer distinguishes the two flags", guard.ID, byTitle)
		}
	})

	t.Run("--search composes with the rest of the list vocabulary", func(t *testing.T) {
		// The reason to add the flag rather than tell callers to use the verb:
		// the listing's filters travel with it.
		bugs := searchSortedIDs(listIssueIDs(bdListJSON(t, bd, dir, "--search", "LXC-GUARD", "--status", "open", "--type", "bug", "--limit", "0")))
		if !hasID(bugs, guard.ID) {
			t.Errorf("--search --type bug --status open missed %s; got %v", guard.ID, bugs)
		}
		if hasID(bugs, guardToo.ID) {
			t.Errorf("--type bug was not applied: %s is a task and came back", guardToo.ID)
		}
		if hasID(bugs, closedGuard.ID) {
			t.Errorf("--status open was not applied: %s is closed and came back", closedGuard.ID)
		}
	})

	t.Run("--ready --search is refused, not silently widened", func(t *testing.T) {
		// The ready query never receives the term, so answering would return
		// every ready issue while the command line read like a search. The
		// refusal must NAME the flag.
		out := bdListFail(t, bd, dir, "--ready", "--search", "LXC-GUARD")
		if !strings.Contains(out, "--search") {
			t.Errorf("the refusal does not name --search, so the caller cannot tell what was refused:\n%s", out)
		}
		// CONTROL: a plain --ready listing is still accepted, so the failure
		// above is attributable to --search and not to --ready.
		bdList(t, bd, dir, "--ready")
	})

	t.Run("an unknown flag still fails loudly", func(t *testing.T) {
		// The original report read an unknown-flag error as a zero-row success
		// because its probe discarded stderr. Pin that bd does fail: the
		// remedy for the NEXT missing flag has to be visible to the caller.
		out := bdListFail(t, bd, dir, "--no-such-flag=x")
		if !strings.Contains(out, "unknown flag") {
			t.Errorf("expected an unknown-flag error, got:\n%s", out)
		}
	})
}

func searchSortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func searchJSONIDs(results []map[string]interface{}) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		if id, ok := r["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func sameIDSet(a, b []string) bool {
	a, b = searchSortedIDs(a), searchSortedIDs(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasID(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
