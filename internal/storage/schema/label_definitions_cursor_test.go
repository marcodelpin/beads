package schema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The label_definitions cursor rule (bda-e080, decision D-cursor). These tests
// pin the rule with rows that match it; the database-backed RED/GREEN cases
// live in internal/storage/dolt/label_definitions_renumber_heal_test.go.

// upstreamHash stands in for any content_hash that is not a label_definitions
// hash: upstream's own row.
const upstreamHash = "upstream"

// TestLabelDefinitionsHashesCoverTheShippedFile pins labelDefinitionsVersion
// to the one file that creates label_definitions and its bytes to the hash
// set. A renumber that forgets the constant, or an edit of the frozen body,
// fails here instead of turning the cursor rule off.
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
	if got := hex.EncodeToString(sum[:]); got != labelDefinitionsHash {
		t.Errorf("%s hashes to %s, want labelDefinitionsHash %s", found[0].name, got, labelDefinitionsHash)
	}
	if !slices.Contains(labelDefinitionsContentHashes, labelDefinitionsHash) || !slices.Contains(labelDefinitionsContentHashes, labelDefinitionsFirstBodyHash) {
		t.Errorf("labelDefinitionsContentHashes = %v, want both the modern and the first 0066 body", labelDefinitionsContentHashes)
	}
	// Every slot the rule can hide must exist in this build, or the hidden
	// row would never be replaced by a step.
	files := map[int]bool{}
	for _, mf := range mainSource.list() {
		files[mf.version] = true
	}
	for v := labelDefinitionsLowestSlot; v < labelDefinitionsVersion; v++ {
		if !files[v] {
			t.Errorf("main series has no migration %d: a label_definitions row there would never be replaced", v)
		}
	}
}

func rowsFrom(pairs ...any) []schemaMigrationsRow {
	var rows []schemaMigrationsRow
	for i := 0; i < len(pairs); i += 2 {
		rows = append(rows, schemaMigrationsRow{version: pairs[i].(int), hash: pairs[i+1].(string)})
	}
	return rows
}

// genuineThrough is the rows a build left for upstream's own migrations from
// labelDefinitionsLowestSlot through last.
func genuineThrough(last int) []any {
	var pairs []any
	for v := labelDefinitionsLowestSlot; v <= last; v++ {
		pairs = append(pairs, v, upstreamHash)
	}
	return pairs
}

// TestHiddenLabelDefinitionsFloor pins the rule on the row shapes the fork's
// history produced, and on the ones it refuses.
func TestHiddenLabelDefinitionsFloor(t *testing.T) {
	L, old := labelDefinitionsHash, labelDefinitionsFirstBodyHash
	cases := []struct {
		name string
		rows []schemaMigrationsRow
		want int
		// refuse is the refused row, 0 for no refusal; above is the row
		// above it named by the refusal.
		refuse, above int
	}{
		{name: "slot70", rows: rowsFrom(append(genuineThrough(69), 70, L)...), want: 70},
		{name: "slot73", rows: rowsFrom(append(genuineThrough(72), 73, L)...), want: 73},
		// codex bda-e080-r1 #2: fork rev c2d3c8a28 had no 0068/0069, so the
		// row at 70 sits on a hole; the cursor falls to 67 and both holes
		// become pending.
		{name: "holes_c2d3c8a28", rows: rowsFrom(66, upstreamHash, 67, upstreamHash, 70, L), want: 70},
		// Successive builds at 67 and 68 with nothing of upstream's between.
		{name: "stacked_label_rows", rows: rowsFrom(66, upstreamHash, 67, L, 68, L), want: 67},
		{name: "converged_75", rows: rowsFrom(append(genuineThrough(74), 75, L)...), want: 0},
		{name: "nullhash70_counts", rows: rowsFrom(append(genuineThrough(69), 70, "")...), want: 0},
		// codex #8: a matching hash below the lowest slot is not a
		// label_definitions row, whatever comes above it.
		{name: "row40_counts", rows: rowsFrom(40, L, 41, upstreamHash, 66, upstreamHash), want: 0},
		{name: "row75_label_is_genuine_above", rows: rowsFrom(append(genuineThrough(69), 70, L, 75, L)...), refuse: 70, above: 75},
		// bda-3xdc: a heal-less 0075 build moved the cursor past the row.
		{name: "bda3xdc_genuine_above", rows: rowsFrom(append(append(genuineThrough(69), 70, L), 71, upstreamHash, 72, upstreamHash, 73, upstreamHash, 74, upstreamHash, 75, L)...), refuse: 70, above: 71},
		// codex #3: label rows at 67 and 68 under a genuine 69.
		{name: "codex3_genuine69", rows: rowsFrom(66, upstreamHash, 67, L, 68, L, 69, upstreamHash, 70, L), refuse: 67, above: 69},
		// A NULL-hash row above a label row counts as a row above it.
		{name: "nullhash_above", rows: rowsFrom(66, upstreamHash, 67, L, 68, ""), refuse: 67, above: 68},
		{name: "first_0066_body", rows: rowsFrom(66, old), refuse: 66},
		{name: "fork_0066_0067", rows: rowsFrom(66, L, 67, forkJournalActorAt67Hash), refuse: 66, above: 67},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hiddenLabelDefinitionsFloor(tc.rows)
			if tc.refuse == 0 {
				if err != nil {
					t.Fatalf("hiddenLabelDefinitionsFloor = %v, want no refusal", err)
				}
				if got != tc.want {
					t.Errorf("hiddenLabelDefinitionsFloor = %d, want %d", got, tc.want)
				}
				return
			}
			var refusal *RenumberedLabelDefinitionsError
			if !errors.As(err, &refusal) {
				t.Fatalf("hiddenLabelDefinitionsFloor = (%d, %v), want a *RenumberedLabelDefinitionsError", got, err)
			}
			if refusal.Version != tc.refuse || refusal.Above != tc.above {
				t.Errorf("refusal names row %d above %d, want row %d above %d", refusal.Version, refusal.Above, tc.refuse, tc.above)
			}
			if !strings.Contains(err.Error(), "schema_migrations row ") {
				t.Errorf("refusal %q does not name the row", err)
			}
		})
	}
}

// TestRenumberedLabelDefinitionsErrorNamesTheShape pins the three refusal
// texts, so `bd doctor` and the failing open say which shape they met.
func TestRenumberedLabelDefinitionsErrorNamesTheShape(t *testing.T) {
	for _, tc := range []struct {
		err  *RenumberedLabelDefinitionsError
		want string
	}{
		{&RenumberedLabelDefinitionsError{Version: 66, Hash: labelDefinitionsFirstBodyHash}, "no label_folded column"},
		{&RenumberedLabelDefinitionsError{Version: 66, Hash: labelDefinitionsHash, Above: 67, AboveHash: forkJournalActorAt67Hash}, "events journal actor"},
		{&RenumberedLabelDefinitionsError{Version: 70, Hash: labelDefinitionsHash, Above: 71, AboveHash: upstreamHash}, "row 71 above it"},
	} {
		msg := tc.err.Error()
		if !strings.Contains(msg, tc.want) || !strings.Contains(msg, "row is left as it is") {
			t.Errorf("Error() = %q, want it to contain %q and say the row is left intact", msg, tc.want)
		}
		if !IsRenumberedLabelDefinitionsError(errors.Join(errors.New("wrapped"), tc.err)) {
			t.Errorf("IsRenumberedLabelDefinitionsError does not see a wrapped %T", tc.err)
		}
	}
}

// expectLabelCursorRowsReturning mocks the cursor rule's read with these rows.
func expectLabelCursorRowsReturning(mock sqlmock.Sqlmock, rows []schemaMigrationsRow) {
	r := sqlmock.NewRows([]string{"version", "content_hash"})
	for _, row := range rows {
		var hash any = row.hash
		if row.hash == "" {
			hash = nil
		}
		r.AddRow(row.version, hash)
	}
	mock.ExpectQuery(regexp.QuoteMeta(labelCursorRowsQuery)).WithArgs(labelDefinitionsLowestSlot).WillReturnRows(r)
}

// TestCurrentVersionAppliesTheLabelDefinitionsCursorRule drives the rule
// through currentVersion and pendingVersions, the reads the consent gate, the
// remote gates and the dirty-table guard derive from (codex #5, #9).
func TestCurrentVersionAppliesTheLabelDefinitionsCursorRule(t *testing.T) {
	ctx := context.Background()

	t.Run("hidden row 70 reads as 69 and leaves 70 pending", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer db.Close()
		rows := rowsFrom(append(genuineThrough(69), 70, labelDefinitionsHash)...)
		for i := 0; i < 2; i++ { // currentVersion, then pendingVersions
			expectCursorProbe(mock, "schema_migrations", true)
			expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", 70)
			expectLabelCursorRowsReturning(mock, rows)
			mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations WHERE version < \?`).
				WithArgs(70).WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(69))
		}

		current, err := CurrentVersion(ctx, db)
		if err != nil {
			t.Fatalf("CurrentVersion: %v", err)
		}
		if current != 69 {
			t.Errorf("CurrentVersion with the label_definitions hash at 70 = %d, want 69", current)
		}
		pending, err := PendingVersions(ctx, db)
		if err != nil {
			t.Fatalf("PendingVersions: %v", err)
		}
		if len(pending) == 0 || pending[0] != 70 {
			t.Errorf("PendingVersions = %v, want it to start at 70", pending)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet SQL expectations: %v", err)
		}
	})

	t.Run("genuine row above refuses", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer db.Close()
		expectCursorProbe(mock, "schema_migrations", true)
		expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", 75)
		expectLabelCursorRowsReturning(mock, rowsFrom(append(append(genuineThrough(69), 70, labelDefinitionsHash), 71, upstreamHash, 75, labelDefinitionsHash)...))

		_, err = CurrentVersion(ctx, db)
		if !IsRenumberedLabelDefinitionsError(err) {
			t.Fatalf("CurrentVersion = %v, want a *RenumberedLabelDefinitionsError", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet SQL expectations: %v", err)
		}
	})

	t.Run("NULL hash at 70 counts", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer db.Close()
		expectCursorProbe(mock, "schema_migrations", true)
		expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", 70)
		expectLabelCursorRowsReturning(mock, rowsFrom(append(genuineThrough(69), 70, "")...))

		current, err := CurrentVersion(ctx, db)
		if err != nil {
			t.Fatalf("CurrentVersion: %v", err)
		}
		if current != 70 {
			t.Errorf("CurrentVersion with a NULL-hash row at 70 = %d, want 70", current)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet SQL expectations: %v", err)
		}
	})

	t.Run("cursor below the lowest slot reads no rows", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer db.Close()
		expectCursorProbe(mock, "schema_migrations", true)
		expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", 40)

		current, err := CurrentVersion(ctx, db)
		if err != nil {
			t.Fatalf("CurrentVersion: %v", err)
		}
		if current != 40 {
			t.Errorf("CurrentVersion = %d, want 40", current)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet SQL expectations: %v", err)
		}
	})
}

// TestRecordMigrationStepReplacesOnlyALabelDefinitionsRow pins the step
// insert: inside the slots label_definitions held, the main series deletes a
// label_definitions row at the step's version before INSERT IGNORE records the
// applied file; everywhere else INSERT IGNORE alone keeps any existing row.
func TestRecordMigrationStepReplacesOnlyALabelDefinitionsRow(t *testing.T) {
	ctx := context.Background()
	insert := regexp.QuoteMeta("INSERT IGNORE INTO ")
	for _, tc := range []struct {
		name    string
		src     migrationSource
		version int
		replace bool
	}{
		{name: "main70", src: mainSource, version: 70, replace: true},
		{name: "main66", src: mainSource, version: 66, replace: true},
		{name: "main75", src: mainSource, version: 75, replace: false},
		{name: "main40", src: mainSource, version: 40, replace: false},
		{name: "ignored10", src: ignoredSource, version: 10, replace: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			if tc.replace {
				mock.ExpectExec(regexp.QuoteMeta("DELETE FROM schema_migrations WHERE version = ? AND content_hash IN (?, ?)")).
					WithArgs(tc.version, labelDefinitionsHash, labelDefinitionsFirstBodyHash).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectExec(insert+tc.src.cursorTable).
				WithArgs(tc.version, "filehash").
				WillReturnResult(sqlmock.NewResult(0, 1))

			if err := recordMigrationStep(ctx, db, tc.src, tc.version, "filehash"); err != nil {
				t.Fatalf("recordMigrationStep: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet SQL expectations: %v", err)
			}
		})
	}
}
