package schema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	// Every slot label_definitions ever held, each on a dense ledger below it:
	// the range check must cover 66..74 with no gap (codex bda-e080-r2).
	for v := labelDefinitionsLowestSlot; v < labelDefinitionsVersion; v++ {
		cases = append(cases, struct {
			name          string
			rows          []schemaMigrationsRow
			want          int
			refuse, above int
		}{name: fmt.Sprintf("every_slot_%d", v), rows: rowsFrom(append(genuineThrough(v-1), v, L)...), want: v})
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

// TestRenumberedLabelDefinitionsErrorNamesTheShape pins the four refusal
// texts, so `bd doctor` and the failing open say which shape they met.
func TestRenumberedLabelDefinitionsErrorNamesTheShape(t *testing.T) {
	for _, tc := range []struct {
		err  *RenumberedLabelDefinitionsError
		want string
	}{
		{&RenumberedLabelDefinitionsError{Version: 66, Hash: labelDefinitionsFirstBodyHash}, "no label_folded column"},
		{&RenumberedLabelDefinitionsError{Version: 66, Hash: labelDefinitionsHash, Above: 67, AboveHash: forkJournalActorAt67Hash}, "events journal actor"},
		{&RenumberedLabelDefinitionsError{Version: 70, Hash: labelDefinitionsHash, Above: 71, AboveHash: upstreamHash}, "row 71 above it"},
		{&RenumberedLabelDefinitionsError{Version: 70, Hash: labelDefinitionsHash, Sparse: true, Below: 39}, "the highest row below it is 39"},
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
	type stepCase struct {
		name    string
		src     migrationSource
		version int
		replace bool
	}
	cases := []stepCase{
		{name: "main75", src: mainSource, version: 75, replace: false},
		{name: "main40", src: mainSource, version: 40, replace: false},
		{name: "ignored10", src: ignoredSource, version: 10, replace: false},
		// Inside the replacement range, so only the series guard keeps the
		// ignored series' row (codex bda-e080-r2).
		{name: "ignored70", src: ignoredSource, version: 70, replace: false},
	}
	for v := labelDefinitionsLowestSlot; v < labelDefinitionsVersion; v++ {
		cases = append(cases, stepCase{name: fmt.Sprintf("main%d", v), src: mainSource, version: v, replace: true})
	}
	for _, tc := range cases {
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

// TestApplyLabelDefinitionsCursorRuleRefusesASparseLedger is codex
// bda-e080-r2 additional finding 2. Every build that recorded label_definitions
// had applied 0001..0065 first (fork main 772a42ceb), so a hidden
// label_definitions row whose highest counting row below it is under 65 is a
// sparse ledger: a restore, an import or rows deleted by hand. Read as cursor 0
// it is a "fresh database" that every gate lets migrate from 0001 without
// consent; read as 39 a consented pass schedules 0040 and its DOLT_COMMIT -Am.
// The rule refuses it instead. 65 itself is the dense case: the holes from 66
// up are ordinary pending steps (holes_c2d3c8a28).
func TestApplyLabelDefinitionsCursorRuleRefusesASparseLedger(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// below is the highest row under the label_definitions row at 70.
		below  int
		refuse bool
	}{
		{name: "only70", below: 0, refuse: true},
		{name: "through39", below: 39, refuse: true},
		{name: "through64", below: 64, refuse: true},
		{name: "through65", below: 65, refuse: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			expectLabelCursorRowsReturning(mock, rowsFrom(70, labelDefinitionsHash))
			mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations WHERE version < \?`).
				WithArgs(70).WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(tc.below))

			got, err := mainSource.applyLabelDefinitionsCursorRule(ctx, db, 70)
			if !tc.refuse {
				if err != nil || got != tc.below {
					t.Fatalf("applyLabelDefinitionsCursorRule = (%d, %v), want (%d, nil)", got, err, tc.below)
				}
			} else {
				var refusal *RenumberedLabelDefinitionsError
				if !errors.As(err, &refusal) {
					t.Fatalf("applyLabelDefinitionsCursorRule = (%d, %v), want a *RenumberedLabelDefinitionsError for a sparse ledger", got, err)
				}
				if refusal.Version != 70 || refusal.Hash != labelDefinitionsHash || !refusal.Sparse || refusal.Below != tc.below || refusal.Above != 0 {
					t.Errorf("refusal = %+v, want row 70, the label hash, Sparse, Below %d, Above 0", *refusal, tc.below)
				}
				if msg := err.Error(); !strings.Contains(msg, "schema_migrations row 70 ") || !strings.Contains(msg, "row is left as it is") {
					t.Errorf("refusal %q does not name row 70 and say it is left intact", msg)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet SQL expectations: %v", err)
			}
		})
	}
}

// TestLabelDefinitionsCursorRuleSkipsTheIgnoredSeries pins the series guard:
// the ignored series never holds label_definitions, so its cursor is MAX as
// read and a label-hash row at 70 there counts. The mock expects no statement,
// so a rule that read the ignored series' rows fails here (codex bda-e080-r2:
// the ignored10 case sat outside the range and could not see the guard).
func TestLabelDefinitionsCursorRuleSkipsTheIgnoredSeries(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	got, err := ignoredSource.applyLabelDefinitionsCursorRule(context.Background(), db, 70)
	if err != nil || got != 70 {
		t.Fatalf("ignored series applyLabelDefinitionsCursorRule(70) = (%d, %v), want (70, nil): the row counts", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet SQL expectations: %v", err)
	}
}

// refusedLedgerAtLatest is the bda-3xdc ledger at this binary's latest: a
// label_definitions row at 70 under upstream's 71..74, label_definitions at
// its own slot, and upstream's rows above that. MAX(version) is latest, so
// only the rows the cursor rule reads tell it from a converged database.
func refusedLedgerAtLatest() []schemaMigrationsRow {
	pairs := append(genuineThrough(69), 70, labelDefinitionsHash)
	for v := 71; v < labelDefinitionsVersion; v++ {
		pairs = append(pairs, v, upstreamHash)
	}
	pairs = append(pairs, labelDefinitionsVersion, labelDefinitionsHash)
	for v := labelDefinitionsVersion + 1; v <= LatestVersion(); v++ {
		pairs = append(pairs, v, upstreamHash)
	}
	return rowsFrom(pairs...)
}

// TestAlreadyConvergedDeclinesOnALedgerTheCursorRuleRefuses drives the
// converged fast path with the cursor rule's read returning label rows (codex
// bda-e080-r2: the converged mocks returned no rows, so a rule that ran the
// query and ignored its result stayed green). Everything after the main cursor
// answers as a converged database does, so a rule that ignored the rows would
// walk the whole fast path and skip the lock and the refusal.
func TestAlreadyConvergedDeclinesOnALedgerTheCursorRuleRefuses(t *testing.T) {
	conn, mock, cleanup := newMockConn(t)
	defer cleanup()

	expectCurrentDatabase(mock, "testdb")
	expectCursorProbe(mock, "schema_migrations", true)
	expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", LatestVersion())
	expectLabelCursorRowsReturning(mock, refusedLedgerAtLatest())
	expectCursorProbe(mock, "ignored_schema_migrations", true)
	expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM ignored_schema_migrations", "version", LatestIgnoredVersion())
	expectIgnoredSentinelProbes(mock, true)
	expectContentHashColumnExists(mock)
	expectContentHashColumnExists(mock)
	expectScalar(mock, "SELECT COUNT(*) FROM custom_types", "count", 1)
	expectScalar(mock, "SELECT COUNT(*) FROM custom_statuses", "count", 1)
	expectDoltIgnoreRead(mock, unqualifiedDoltIgnore, seededIgnorePatterns(LatestVersion()))
	expectIgnoredCursorGate(mock, "", false, nil, false)
	expectMigrationLockProbe(mock, "testdb", 1)

	converged, err := alreadyConverged(context.Background(), conn, "testdb", nil)
	if err != nil {
		t.Fatalf("alreadyConverged: %v", err)
	}
	if converged {
		t.Fatal("alreadyConverged = true on a ledger the cursor rule refuses: the open would skip the lock and never see the refusal")
	}
}

// TestRemoteMigrateGatesRefuseALedgerTheCursorRuleRefuses drives the remote
// and shared-store gates with the cursor rule's read returning label rows
// (codex bda-e080-r2). The refused bda-3xdc ledger at latest must not read as
// "already current", and the sparse ledger {(70, label hash)} must not read as
// a fresh database, which both gates let migrate (additional finding 2). The
// second cursor read is mocked too, so on the bda-3xdc ledger a rule that
// ignored the rows gets a nil from the gate, not an unexpected-statement error.
func TestRemoteMigrateGatesRefuseALedgerTheCursorRuleRefuses(t *testing.T) {
	t.Setenv(SmartGateEnv, "0")
	gates := map[string]func(context.Context, DBConn) error{
		"remote": CheckRemoteMigrateGate,
		"shared": func(ctx context.Context, db DBConn) error {
			return CheckSharedStoreMigrateGate(ctx, db, "", nil, nil)
		},
	}
	ledgers := []struct {
		name   string
		max    int
		rows   []schemaMigrationsRow
		below  int // the MAX read below the hidden row, -1 when the rule refuses first
		sparse bool
	}{
		{name: "bda3xdc_at_latest", max: LatestVersion(), rows: refusedLedgerAtLatest(), below: -1},
		{name: "sparse_only70", max: 70, rows: rowsFrom(70, labelDefinitionsHash), below: 0, sparse: true},
	}
	for gateName, gate := range gates {
		for _, lg := range ledgers {
			t.Run(gateName+"/"+lg.name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				if err != nil {
					t.Fatalf("sqlmock.New: %v", err)
				}
				defer db.Close()
				for i := 0; i < 2; i++ { // CurrentVersion, then PendingVersions on the mutant path
					expectCursorProbe(mock, "schema_migrations", true)
					expectScalar(mock, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", "version", lg.max)
					expectLabelCursorRowsReturning(mock, lg.rows)
					if lg.below >= 0 {
						mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations WHERE version < \?`).
							WithArgs(70).WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(lg.below))
					}
				}

				err = gate(context.Background(), db)
				var refusal *RenumberedLabelDefinitionsError
				if !errors.As(err, &refusal) {
					t.Fatalf("%s gate = %v, want a *RenumberedLabelDefinitionsError", gateName, err)
				}
				if refusal.Version != 70 || refusal.Sparse != lg.sparse {
					t.Errorf("refusal = %+v, want row 70 sparse=%v", *refusal, lg.sparse)
				}
			})
		}
	}
}
