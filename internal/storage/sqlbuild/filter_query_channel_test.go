package sqlbuild

import (
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// The free-text term reaches BuildIssueFilterClauses through two channels:
// `bd search` passes it as the query ARGUMENT, `bd list --search` carries it on
// types.IssueFilter.Query. The tests below pin the property the flag exists for
// (bda-nldy): the two channels must render the SAME predicate, so the flag and
// the verb cannot select different rows.

// filterTables is the table naming the durable plane uses; the wisps plane
// swaps the names and is irrelevant to what the query term renders.
var filterTables = FilterTables{Main: "issues", Labels: "issue_labels", Dependencies: "dependencies"}

func TestQueryArgumentAndFilterQueryRenderTheSameClause(t *testing.T) {
	t.Parallel()

	// Both an ordinary word and an ID-LIKE term, because the matcher branches
	// on LooksLikeIssueID and a test that only exercised one branch would go
	// green on a channel wired to the other.
	for _, term := range []string{"lxc-guard alarm", "bd-5q", "scrape"} {
		term := term
		t.Run(term, func(t *testing.T) {
			t.Parallel()

			argClauses, argArgs, err := BuildIssueFilterClauses(term, types.IssueFilter{}, filterTables)
			if err != nil {
				t.Fatalf("query-argument channel: %v", err)
			}
			filterClauses, filterArgs, err := BuildIssueFilterClauses("", types.IssueFilter{Query: term}, filterTables)
			if err != nil {
				t.Fatalf("filter.Query channel: %v", err)
			}

			// POSITIVE CONTROL: a term must produce a predicate at all. Without
			// this an implementation that rendered NOTHING on both channels
			// would satisfy the equality below - two empty clause lists are
			// equal, and that is precisely the silent-empty defect this flag
			// was added to fix.
			if len(argClauses) == 0 {
				t.Fatalf("the query argument rendered no clause for %q; the comparison below would be vacuous", term)
			}

			if !reflect.DeepEqual(argClauses, filterClauses) {
				t.Errorf("channels render different clauses for %q:\n  argument: %v\n  filter:   %v", term, argClauses, filterClauses)
			}
			if !reflect.DeepEqual(argArgs, filterArgs) {
				t.Errorf("channels render different args for %q:\n  argument: %v\n  filter:   %v", term, argArgs, filterArgs)
			}
		})
	}
}

func TestFilterQueryIsANoOpWhenEmpty(t *testing.T) {
	t.Parallel()

	// An unset Query must add nothing: every caller that passes a filter now
	// passes one carrying this field, and a stray predicate here would narrow
	// every listing in the tree.
	bare, bareArgs, err := BuildIssueFilterClauses("", types.IssueFilter{}, filterTables)
	if err != nil {
		t.Fatalf("bare: %v", err)
	}
	withEmpty, withEmptyArgs, err := BuildIssueFilterClauses("", types.IssueFilter{Query: ""}, filterTables)
	if err != nil {
		t.Fatalf("empty Query: %v", err)
	}
	if !reflect.DeepEqual(bare, withEmpty) || !reflect.DeepEqual(bareArgs, withEmptyArgs) {
		t.Errorf("an empty Query changed the rendering: %v/%v vs %v/%v", bare, bareArgs, withEmpty, withEmptyArgs)
	}
}

func TestBothChannelsSetIntersect(t *testing.T) {
	t.Parallel()

	// types.IssueFilter.Query documents that both channels set is an
	// intersection. No CLI path sets both today, so this pins the promise
	// rather than a behaviour a user can reach - a future caller that does set
	// both must get an AND, never a silently discarded half.
	clauses, args, err := BuildIssueFilterClauses("alpha", types.IssueFilter{Query: "beta"}, filterTables)
	if err != nil {
		t.Fatalf("both channels: %v", err)
	}

	one, oneArgs, err := BuildIssueFilterClauses("alpha", types.IssueFilter{}, filterTables)
	if err != nil {
		t.Fatalf("one channel: %v", err)
	}
	if len(clauses) != len(one)+1 {
		t.Errorf("both channels set rendered %d clauses, want %d (one per term)", len(clauses), len(one)+1)
	}
	if len(args) != 2*len(oneArgs) {
		t.Errorf("both channels set rendered %d args, want %d", len(args), 2*len(oneArgs))
	}
}
