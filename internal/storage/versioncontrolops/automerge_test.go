package versioncontrolops

import (
	"testing"
	"time"
)

// conflictRowFor builds a rawConflictRow the way dolt_conflicts_<table> reports
// one: base_/our_/their_ columns plus the diff-type metadata. A nil side value
// stands for the NULLs dolt writes when that side has no row.
func conflictRowFor(t *testing.T, cells map[string][3]any) rawConflictRow {
	t.Helper()
	row := rawConflictRow{}
	// Deterministic order: id first, then the rest as given (map order does not
	// matter to the merge rules, but a stable id column keeps failures readable).
	names := []string{"id"}
	for name := range cells {
		if name != "id" {
			names = append(names, name)
		}
	}
	for _, name := range names {
		v, ok := cells[name]
		if !ok {
			continue
		}
		for i, side := range []string{"base", "our", "their"} {
			row.cols = append(row.cols, side+"_"+name)
			row.vals = append(row.vals, v[i])
		}
	}
	row.cols = append(row.cols, "our_diff_type", "their_diff_type")
	row.vals = append(row.vals, "modified", "modified")
	return row
}

// merged reports the value the plan writes for col, and whether it writes one.
func (m issuesRowMerge) merged(col string) (any, bool) {
	for i, c := range m.columns {
		if c == col {
			return m.values[i], true
		}
	}
	return nil, false
}

const (
	tsBase   = "2026-07-10 10:00:00"
	tsOurs   = "2026-07-10 11:00:00"
	tsTheirs = "2026-07-10 12:00:00"
)

// TestMergeIssuesConflictRow_DisjointFieldsBothSurvive is the flagship case:
// both sides edited the SAME issue since the merge base but different fields,
// so the row conflicts only because every mutation stamps updated_at. Neither
// side's edit may be dropped.
func TestMergeIssuesConflictRow_DisjointFieldsBothSurvive(t *testing.T) {
	row := conflictRowFor(t, map[string][3]any{
		"id":         {"bd-1", "bd-1", "bd-1"},
		"status":     {"open", "in_progress", "open"}, // only we changed it
		"assignee":   {"", "", "alice"},               // only they changed it
		"title":      {"same", "same", "same"},        // untouched
		"updated_at": {tsBase, tsOurs, tsTheirs},
	})

	m, ok := mergeIssuesConflictRow(row)
	if !ok {
		t.Fatal("expected disjoint-field conflict to be field-mergeable")
	}
	if _, written := m.merged("status"); written {
		t.Error("our status edit must be kept, not overwritten")
	}
	v, written := m.merged("assignee")
	if !written || v != "alice" {
		t.Errorf("their assignee edit must survive, got %v (written=%v)", v, written)
	}
	if _, written := m.merged("title"); written {
		t.Error("an agreeing column must not be written")
	}
	// updated_at merges to max(ours, theirs): only they moved it past ours.
	v, written = m.merged("updated_at")
	if !written || v != tsTheirs {
		t.Errorf("updated_at must merge to the later timestamp, got %v (written=%v)", v, written)
	}
}

// TestMergeIssuesConflictRow_ContestedCellLWW checks the one genuine conflict
// class: both sides changed the SAME cell, so the later updated_at wins it.
func TestMergeIssuesConflictRow_ContestedCellLWW(t *testing.T) {
	t.Run("theirs newer", func(t *testing.T) {
		row := conflictRowFor(t, map[string][3]any{
			"id":         {"bd-2", "bd-2", "bd-2"},
			"status":     {"open", "in_progress", "closed"},
			"updated_at": {tsBase, tsOurs, tsTheirs},
		})
		m, ok := mergeIssuesConflictRow(row)
		if !ok {
			t.Fatal("expected contested cell with distinct timestamps to be mergeable")
		}
		if v, written := m.merged("status"); !written || v != "closed" {
			t.Errorf("later writer must win the contested cell, got %v (written=%v)", v, written)
		}
	})
	t.Run("ours newer", func(t *testing.T) {
		row := conflictRowFor(t, map[string][3]any{
			"id":         {"bd-3", "bd-3", "bd-3"},
			"status":     {"open", "in_progress", "closed"},
			"updated_at": {tsBase, tsTheirs, tsOurs},
		})
		m, ok := mergeIssuesConflictRow(row)
		if !ok {
			t.Fatal("expected contested cell with distinct timestamps to be mergeable")
		}
		if _, written := m.merged("status"); written {
			t.Error("our newer status must be kept, not overwritten")
		}
		if _, written := m.merged("updated_at"); written {
			t.Error("updated_at must stay at our (later) value")
		}
	})
}

// TestMergeIssuesConflictRow_AmbiguousLeftAlone covers the classes LWW has no
// answer for: equal or unparseable timestamps on a contested cell.
func TestMergeIssuesConflictRow_AmbiguousLeftAlone(t *testing.T) {
	t.Run("equal timestamps", func(t *testing.T) {
		row := conflictRowFor(t, map[string][3]any{
			"id":         {"bd-4", "bd-4", "bd-4"},
			"status":     {"open", "in_progress", "closed"},
			"updated_at": {tsBase, tsOurs, tsOurs},
		})
		if _, ok := mergeIssuesConflictRow(row); ok {
			t.Error("a contested cell with equal updated_at must be left for the operator")
		}
	})
	t.Run("unparseable timestamp", func(t *testing.T) {
		row := conflictRowFor(t, map[string][3]any{
			"id":         {"bd-5", "bd-5", "bd-5"},
			"status":     {"open", "in_progress", "closed"},
			"updated_at": {tsBase, "not-a-time", tsTheirs},
		})
		if _, ok := mergeIssuesConflictRow(row); ok {
			t.Error("a contested cell with an unparseable updated_at must be left for the operator")
		}
	})
	t.Run("equal timestamps but disjoint fields", func(t *testing.T) {
		// No cell is contested, so the tiebreak is never needed: this MUST
		// still merge even though the timestamps are identical.
		row := conflictRowFor(t, map[string][3]any{
			"id":         {"bd-6", "bd-6", "bd-6"},
			"status":     {"open", "in_progress", "open"},
			"assignee":   {"", "", "alice"},
			"updated_at": {tsBase, tsOurs, tsOurs},
		})
		if _, ok := mergeIssuesConflictRow(row); !ok {
			t.Error("disjoint edits must merge regardless of the timestamps")
		}
	})
}

// TestMergeIssuesConflictRow_StructuralConflictsLeftAlone covers add/add and
// delete/modify, which have no field-level answer.
func TestMergeIssuesConflictRow_StructuralConflictsLeftAlone(t *testing.T) {
	t.Run("add/add", func(t *testing.T) {
		row := conflictRowFor(t, map[string][3]any{
			"id":         {nil, "bd-7", "bd-7"},
			"status":     {nil, "open", "closed"},
			"updated_at": {nil, tsOurs, tsTheirs},
		})
		if _, ok := mergeIssuesConflictRow(row); ok {
			t.Error("add/add must be left for the operator")
		}
	})
	t.Run("delete/modify", func(t *testing.T) {
		row := conflictRowFor(t, map[string][3]any{
			"id":         {"bd-8", nil, "bd-8"},
			"status":     {"open", nil, "closed"},
			"updated_at": {tsBase, nil, tsTheirs},
		})
		if _, ok := mergeIssuesConflictRow(row); ok {
			t.Error("delete/modify must be left for the operator")
		}
	})
}

// TestMergeIssuesConflictRow_NullVsEmpty pins that SQL NULL and the empty
// string are distinct values, so an assignee cleared to ” on one side is a
// real edit and not mistaken for the NULL base.
func TestMergeIssuesConflictRow_NullVsEmpty(t *testing.T) {
	row := conflictRowFor(t, map[string][3]any{
		"id":         {"bd-9", "bd-9", "bd-9"},
		"assignee":   {nil, "", nil},
		"updated_at": {tsBase, tsOurs, tsTheirs},
	})
	m, ok := mergeIssuesConflictRow(row)
	if !ok {
		t.Fatal("expected the row to merge: only one side changed assignee")
	}
	if _, written := m.merged("assignee"); written {
		t.Error("our '' edit must be kept: their side still matches the NULL base")
	}
}

// TestMergeIssuesConflictRow_ByteValuesCompareEqual pins that a driver
// returning []byte for one side and string for the other does not read as a
// difference (the normalization conflictCellsEqual relies on).
func TestMergeIssuesConflictRow_ByteValuesCompareEqual(t *testing.T) {
	row := conflictRowFor(t, map[string][3]any{
		"id":         {"bd-10", "bd-10", []byte("bd-10")},
		"status":     {[]byte("open"), "open", []byte("open")},
		"assignee":   {"", "", "alice"},
		"updated_at": {tsBase, tsOurs, tsTheirs},
	})
	m, ok := mergeIssuesConflictRow(row)
	if !ok {
		t.Fatal("expected the row to merge")
	}
	if _, written := m.merged("status"); written {
		t.Error("[]byte and string spellings of the same value must not read as a conflict")
	}
}

// TestParseConflictTimestamp covers the shapes an updated_at cell arrives in.
func TestParseConflictTimestamp(t *testing.T) {
	want := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		in   any
	}{
		{"mysql datetime text", "2026-07-10 12:00:00"},
		{"mysql datetime bytes", []byte("2026-07-10 12:00:00")},
		{"rfc3339", "2026-07-10T12:00:00Z"},
		{"driver time", time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseConflictTimestamp(tc.in)
			if !ok || !got.Equal(want) {
				t.Errorf("parseConflictTimestamp(%v) = %v, %v; want %v, true", tc.in, got, ok, want)
			}
		})
	}
	for _, bad := range []any{nil, "", "yesterday"} {
		if _, ok := parseConflictTimestamp(bad); ok {
			t.Errorf("parseConflictTimestamp(%v) must not parse", bad)
		}
	}
}

// TestUnionConflictKeyColumnsCoverTheUnionTables pins the union-merged table
// set against its resolver, so adding one without its key columns cannot
// silently produce a resolver that deletes nothing.
func TestUnionConflictKeyColumnsCoverTheUnionTables(t *testing.T) {
	for _, table := range []string{"labels", "comments", "events"} {
		if cols := unionConflictKeyColumns[table]; len(cols) == 0 {
			t.Errorf("union table %s has no key columns", table)
		}
	}
}

// TestDataColumnsExcludesMetaAndKey pins the column classification the merge
// rules walk: dolt's diff_type/cardinality metadata is not row data, and the
// key column is never written back.
func TestDataColumnsExcludesMetaAndKey(t *testing.T) {
	row := rawConflictRow{
		cols: []string{
			"base_id", "our_id", "their_id",
			"base_status", "our_status", "their_status",
			"our_diff_type", "their_diff_type",
			"base_cardinality", "our_cardinality", "their_cardinality",
			"our_their_thing", "base_their_thing", "their_their_thing",
		},
		vals: make([]any, 14),
	}
	got := row.dataColumns("id")
	want := map[string]bool{"status": true, "their_thing": true}
	if len(got) != len(want) {
		t.Fatalf("dataColumns = %v, want %v", got, want)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected data column %q", c)
		}
	}
}
