package workapi

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// `bd list --search` is ListRequest.Search, and the only thing that makes it
// reach the rows is this builder writing it onto types.IssueFilter.Query. The
// flag used to not exist at all; a flag that parsed and landed nowhere would
// return the UNFILTERED listing, which reads as a working search on any small
// database (bda-nldy).

func TestBuildListFilterCarriesSearchOntoTheFilter(t *testing.T) {
	t.Parallel()

	filter, err := BuildListFilter(issueops.ListRequest{Search: "LXC-GUARD"}, ListConfig{})
	if err != nil {
		t.Fatalf("BuildListFilter: %v", err)
	}
	if filter.Query != "LXC-GUARD" {
		t.Errorf("filter.Query = %q, want %q - the term did not reach the filter, so the listing would be unfiltered", filter.Query, "LXC-GUARD")
	}

	// --search and --title are different questions and must not be conflated:
	// --title is a title substring, --search also matches ids.
	if filter.TitleSearch != "" {
		t.Errorf("filter.TitleSearch = %q, want empty - --search must not be routed through the narrower --title predicate", filter.TitleSearch)
	}
}

func TestBuildListFilterLeavesQueryEmptyWithoutSearch(t *testing.T) {
	t.Parallel()

	filter, err := BuildListFilter(issueops.ListRequest{}, ListConfig{})
	if err != nil {
		t.Fatalf("BuildListFilter: %v", err)
	}
	if filter.Query != "" {
		t.Errorf("filter.Query = %q on a request that set no term; every listing in the tree would be narrowed", filter.Query)
	}
}

// TestReadyRefusesSearch: the blocker-aware ready query is reached through
// ReadyFilterFromIssueFilter, which does not carry Query - so honoring
// `--ready --search` would answer with EVERY ready issue while the request read
// correctly. That is the same silent-wrong-population failure --search was
// added to fix, so it is refused and the refusal NAMES the flag.
func TestReadyRefusesSearch(t *testing.T) {
	t.Parallel()

	_, err := BuildListFilter(issueops.ListRequest{ReadyFlag: true, Search: "LXC-GUARD"}, ListConfig{})
	if err == nil {
		t.Fatal("--ready --search was accepted; the ready query drops the term, so the answer would be the unfiltered ready set")
	}
	if !errors.Is(err, issueops.ErrValidation) {
		t.Errorf("error %v is not an ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "--search") {
		t.Errorf("refusal %q does not name --search; the caller cannot tell which part of the request was refused", err)
	}

	// CONTROL: the refusal must be caused by Search and not by ReadyFlag alone,
	// or this test would pass against a build where --search was never wired.
	if _, err := BuildListFilter(issueops.ListRequest{ReadyFlag: true}, ListConfig{}); err != nil {
		t.Fatalf("a plain --ready listing must still be accepted, got: %v", err)
	}
}

// TestReadyProjectionDropsQuery is the measurement the refusal above rests on.
// If a future change teaches the ready projection to carry the term, this goes
// red and the refusal should be reconsidered rather than left in place - a
// stale refusal is a capability withheld for a reason that stopped being true.
//
// It reads the STRUCT, not a behaviour: types.WorkFilter declaring a free-text
// field is the event worth catching, and nothing else in the tree would catch
// it. The control below proves the needle can find a field that IS there, so a
// false negative from a renamed check cannot read as "the term is dropped".
func TestReadyProjectionDropsQuery(t *testing.T) {
	t.Parallel()

	work := reflect.TypeOf(ReadyFilterFromIssueFilter(types.IssueFilter{Query: "LXC-GUARD"}))

	if _, found := work.FieldByName("Labels"); !found {
		t.Fatal("control failed: WorkFilter has no Labels field, so this test's field lookup proves nothing")
	}
	for _, name := range []string{"Query", "Search", "TitleSearch", "TitleContains"} {
		if _, found := work.FieldByName(name); found {
			t.Errorf("types.WorkFilter now declares %s; --ready may be able to carry a free-text term, so revisit issueops.readyScopeFields", name)
		}
	}
}

// TestBareSearchDropsTheDefaultStatusExclusions pins the one list DEFAULT
// --search changes, and why. `bd list` hides closed and pinned rows; `bd search`
// shows them, deliberately, so "was this already filed or fixed?" cannot answer
// a silent no. A --search that inherited the listing's exclusions would answer
// that question wrongly for every issue already closed - the same defect class
// the flag was added to remove, one notch quieter.
func TestBareSearchDropsTheDefaultStatusExclusions(t *testing.T) {
	t.Parallel()

	// CONTROL: a listing with no --search still excludes, or the assertion
	// below would hold on a build that never excluded anything.
	plain, err := BuildListFilter(issueops.ListRequest{}, ListConfig{})
	if err != nil {
		t.Fatalf("BuildListFilter: %v", err)
	}
	if len(plain.ExcludeStatus) == 0 {
		t.Fatal("control failed: a plain listing excludes nothing, so this test cannot distinguish the two")
	}

	searched, err := BuildListFilter(issueops.ListRequest{Search: "LXC-GUARD"}, ListConfig{})
	if err != nil {
		t.Fatalf("BuildListFilter: %v", err)
	}
	if len(searched.ExcludeStatus) != 0 {
		t.Errorf("a bare --search still excludes %v; a closed match would be withheld from the question the flag exists to answer", searched.ExcludeStatus)
	}
	if searched.Status != nil || len(searched.Statuses) != 0 {
		t.Errorf("a bare --search pinned a status (%v/%v); it must span every status", searched.Status, searched.Statuses)
	}
}

// TestExplicitStatusStillWinsOverSearch: widening is the DEFAULT, not a
// policy. A caller that names a status gets exactly that status.
func TestExplicitStatusStillWinsOverSearch(t *testing.T) {
	t.Parallel()

	narrowed, err := BuildListFilter(issueops.ListRequest{Search: "LXC-GUARD", Status: "open"}, ListConfig{})
	if err != nil {
		t.Fatalf("BuildListFilter: %v", err)
	}
	if narrowed.Status == nil || *narrowed.Status != types.StatusOpen {
		t.Errorf("--search --status open did not pin open; got %v", narrowed.Status)
	}
	if narrowed.Query != "LXC-GUARD" {
		t.Errorf("the term was lost when a status was named: %q", narrowed.Query)
	}
}
