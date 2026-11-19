package schema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The label_definitions renumber heal (bda-e080) is keyed on the recorded
// content hash and due on every main pass whatever the cursor (bda-3xdc).
// These tests pin the facts the key, the out-of-order application and the
// at-latest work term rest on; the database-backed RED/GREEN cases live in
// internal/storage/dolt/label_definitions_renumber_heal_test.go.

// TestLabelDefinitionsHashesCoverTheShippedFile pins labelDefinitionsVersion
// to the one file that creates label_definitions and its bytes to the hash
// set. A renumber that forgets the constant, or an edit of the frozen body,
// fails here instead of turning the heal off.
func TestLabelDefinitionsHashesCoverTheShippedFile(t *testing.T) {
	var found []migrationFile
	for _, mf := range mainSource.list() {
		if strings.HasSuffix(mf.name, "_create_label_definitions.up.sql") {
			found = append(found, mf)
		}
	}
	if len(found) != 1 {
		t.Fatalf("main series holds %d create_label_definitions files (%v), want exactly 1", len(found), found)
	}
	if found[0].version != labelDefinitionsVersion {
		t.Errorf("label_definitions is migration %d, labelDefinitionsVersion = %d: move the constant with the file", found[0].version, labelDefinitionsVersion)
	}
	data, err := mainSource.files.ReadFile(mainSource.dir + "/" + found[0].name)
	if err != nil {
		t.Fatalf("read %s: %v", found[0].name, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !slices.Contains(labelDefinitionsContentHashes, got) {
		t.Errorf("%s hashes to %s, which is not in labelDefinitionsContentHashes %v", found[0].name, got, labelDefinitionsContentHashes)
	}
}

// expectAtLatestUpToRenumberRead mocks migrationWorkNeeded on a database
// whose cursors are at latest, whose content_hash columns exist and whose
// custom tables need no backfill: everything up to its last term, the
// renumber heal's read of schema_migrations.
func expectAtLatestUpToRenumberRead(mock sqlmock.Sqlmock) {
	expectCursorProbe(mock, "schema_migrations", true)
	expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", LatestVersion())
	expectCursorProbe(mock, "ignored_schema_migrations", true)
	expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM ignored_schema_migrations", "version", LatestIgnoredVersion())
	expectIgnoredSentinelProbes(mock, true)
	expectContentHashColumnExists(mock)
	expectContentHashColumnExists(mock)
	expectScalar(mock, "SELECT COUNT(*) FROM custom_types", "count", 1)
	expectScalar(mock, "SELECT COUNT(*) FROM custom_statuses", "count", 1)
}

// TestMigrationWorkNeededSeesALabelDefinitionsRowHiddenBelowTheCursor pins
// bda-3xdc at the short-circuit: a database at the latest version can still
// hide a label_definitions row at an old slot (a heal-less build moved its
// cursor past it), and MigrateUp only reaches the heal if migrationWorkNeeded
// reports that row as work. With no such row the term is one read and the
// answer stays "no work", so a converged open writes nothing.
func TestMigrationWorkNeededSeesALabelDefinitionsRowHiddenBelowTheCursor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hidden []int
		want   bool
	}{
		{name: "row70", hidden: []int{70}, want: true},
		{name: "row73", hidden: []int{73}, want: true},
		{name: "none", hidden: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()

			expectAtLatestUpToRenumberRead(mock)
			rows := sqlmock.NewRows([]string{"version"})
			for _, v := range tc.hidden {
				rows.AddRow(v)
			}
			mock.ExpectQuery(`SELECT version FROM schema_migrations WHERE version < \? AND content_hash IN`).
				WithArgs(labelDefinitionsVersion, labelDefinitionsContentHashes[0], labelDefinitionsContentHashes[1]).
				WillReturnRows(rows)

			needed, err := migrationWorkNeeded(context.Background(), db)
			if err != nil {
				t.Fatalf("migrationWorkNeeded: %v", err)
			}
			if needed != tc.want {
				t.Errorf("migrationWorkNeeded at latest with label_definitions rows hidden at %v = %v, want %v", tc.hidden, needed, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet SQL expectations: %v", err)
			}
		})
	}
}

// TestRenumberHealTargetsNeedNoOtherStepMachinery pins what makes the heal's
// out-of-order application equal to a normal step: every file it can run
// (the slots labelDefinitionsLowestSlot through labelDefinitionsVersion-1)
// exists, carries no execution plan, no updated_at guard and no repair of its
// own, and rewrites no row. A file that needs any of these would need the
// heal to grow the same machinery before it may run there.
func TestRenumberHealTargetsNeedNoOtherStepMachinery(t *testing.T) {
	files := map[int]migrationFile{}
	for _, mf := range mainSource.list() {
		files[mf.version] = mf
	}
	for v := labelDefinitionsLowestSlot; v < labelDefinitionsVersion; v++ {
		key := repairKey{mainSource.cursorTable, v}
		mf, ok := files[v]
		if !ok {
			t.Errorf("main series has no migration %d: the heal could not replace a label_definitions row there", v)
			continue
		}
		if _, ok := migrationBodyPlans[key]; ok {
			t.Errorf("%s has an execution plan the heal does not apply", mf.name)
		}
		if _, ok := updatedAtGuards[key]; ok {
			t.Errorf("%s has an updated_at guard the heal does not apply", mf.name)
		}
		if _, ok := preMigrationRepairs[key]; ok {
			t.Errorf("%s has a pre-migration repair the heal does not run", mf.name)
		}
		data, err := mainSource.files.ReadFile(mainSource.dir + "/" + mf.name)
		if err != nil {
			t.Fatalf("read %s: %v", mf.name, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			code := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(code, "--") {
				continue
			}
			for _, verb := range []string{"UPDATE ", "INSERT ", "DELETE ", "REPLACE "} {
				if strings.Contains(code, verb) {
					t.Errorf("%s rewrites rows (%q): the heal runs it out of order, which is only shown safe for guarded DDL", mf.name, strings.TrimSpace(line))
				}
			}
		}
	}
}
