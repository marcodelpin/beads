package schema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The fork-only label_definitions migration changed slot, never bytes after
// its first day: 0066, 0067, 0068 and 0070 on the fork's PR and main lines,
// then 0073 and 0075 during the bda-tfok.4 sync, each time because upstream
// took the slot for a migration of its own. A database migrated by a build
// that held it at slot N recorded (N, <label_definitions hash>). The runner
// skips every version at or below the cursor, so if that row counted, a later
// build would never apply upstream's own file N there: no
// issues.participation_generation for a row at 70, no arrival_seq for a row at
// 73 (bda-e080).
//
// The cursor rule (decision D-cursor, operator 2026-11-19): a schema_migrations
// row in [labelDefinitionsLowestSlot, labelDefinitionsVersion) carrying the
// label_definitions hash does not count toward the cursor when no other row
// sits above it. currentVersion is the one place that rule lives, so the
// consent gate, the shared-store and remote gates, the pending list and the
// dirty-table guard all see the upstream file the row hid as an ordinary
// pending step, applied under consent with its plan, its repair and its own
// per-step commit. That step replaces the row (recordMigrationStep), so the
// upstream file's own hash ends up at its version.
//
// A shape the rule cannot read safely is refused instead, with an error that
// names the row and leaves it intact (RenumberedLabelDefinitionsError).
const (
	// labelDefinitionsVersion is the slot the migration holds in this build.
	labelDefinitionsVersion = 75
	// labelDefinitionsLowestSlot is the lowest slot it ever shipped under. A
	// row below it is never a label_definitions row, whatever its hash: no
	// build recorded the migration there, so a matching hash at 40 is damage
	// or an import, and it counts like any other row (codex bda-e080-r1 #8).
	labelDefinitionsLowestSlot = 66

	// labelDefinitionsHash is the body every slot carried from the day the
	// case-folding constraint landed (2026-08-22): 0066 through 0070, 0073
	// and 0075.
	labelDefinitionsHash = "9c76d11ee4c29c32c226b273642e087458a5d92f31c525885a0794487d70cd00"
	// labelDefinitionsFirstBodyHash is the first 0066 body (fork main
	// 772a42ceb, 2026-08-22), whose table has no label_folded column. The
	// modern file is CREATE TABLE IF NOT EXISTS and would leave that table as
	// it is, so a row carrying this hash is refused (codex bda-e080-r1 #4).
	labelDefinitionsFirstBodyHash = "0aad420facc23a266a99c40764ec10a70acec6d35095ee12504407e354468fd1"
	// forkJournalActorAt67Hash is upstream's events journal actor migration
	// as fork main carried it at 0067, under the label_definitions row at
	// 0066 (fork main until 2026-09-25, e.g. 61037a5ce). Upstream's 0067 is
	// the versioned beads schema, so that shape hides a different file
	// behind a row that is not a label_definitions row.
	forkJournalActorAt67Hash = "b6b01225af1406d3199332d3e42764d0276ed60ec2caa81477e5015b4f5a2295"
)

// labelDefinitionsContentHashes are the SHA-256 values a label_definitions up
// file has ever been recorded with. The cursor rule is keyed on them and on
// nothing else: a row carrying any other hash, or none, is upstream's own row
// and counts.
var labelDefinitionsContentHashes = []string{
	labelDefinitionsHash,
	labelDefinitionsFirstBodyHash,
}

// RenumberedLabelDefinitionsError refuses a schema_migrations history in which
// a fork label_definitions row cannot be read as a plain renumber: a migration
// row sits above it (so the files below that row may never have run, and the
// files above may have run against a schema missing them), or the row carries
// the first 0066 body. The migration pass stops before any write and the row is
// left as it is; repairing such a database is a consented, manual act
// (bda-xdmh).
type RenumberedLabelDefinitionsError struct {
	// Version and Hash identify the label_definitions row.
	Version int
	Hash    string
	// Above is the version of the first row above it that is not a
	// label_definitions row, 0 when the refusal is about the row's own body.
	Above int
	// AboveHash is that row's content_hash, "" when NULL or when Above is 0.
	AboveHash string
}

func (e *RenumberedLabelDefinitionsError) Error() string {
	var why string
	switch {
	case e.Above == 0:
		why = "it carries the first 0066 label_definitions body, whose table has no label_folded column"
	case e.Version == 66 && e.Above == 67 && e.AboveHash == forkJournalActorAt67Hash:
		why = "row 67 above it is the events journal actor as fork main carried it at 0067, so upstream's 0067 never ran"
	default:
		why = fmt.Sprintf("row %d above it is not a label_definitions row, so this build cannot tell which migrations between them ran", e.Above)
	}
	return fmt.Sprintf("schema_migrations row %d carries the fork label_definitions content hash %s, and %s; "+
		"refusing to read the schema version or migrate (the row is left as it is). "+
		"Repairing this database is a manual step (bda-e080)",
		e.Version, shortContentHash(e.Hash), why)
}

// IsRenumberedLabelDefinitionsError reports whether err (or any error it
// wraps) is a *RenumberedLabelDefinitionsError.
func IsRenumberedLabelDefinitionsError(err error) bool {
	var e *RenumberedLabelDefinitionsError
	return errors.As(err, &e)
}

func shortContentHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// schemaMigrationsRow is one row of schema_migrations as the cursor rule reads
// it. hash is "" for a NULL content_hash and for a table without the column.
type schemaMigrationsRow struct {
	version int
	hash    string
}

// isRenumberedLabelDefinitionsRow reports whether r is a fork
// label_definitions row at a slot upstream now owns.
func isRenumberedLabelDefinitionsRow(r schemaMigrationsRow) bool {
	return r.version >= labelDefinitionsLowestSlot && r.version < labelDefinitionsVersion &&
		slices.Contains(labelDefinitionsContentHashes, r.hash)
}

// hiddenLabelDefinitionsFloor applies the cursor rule to rows, every
// schema_migrations row at or above labelDefinitionsLowestSlot in ascending
// order. It returns the lowest label_definitions row that does not count (0
// when every row counts); every row at or above it is then a label_definitions
// row too. It returns a *RenumberedLabelDefinitionsError for a shape it
// refuses.
func hiddenLabelDefinitionsFloor(rows []schemaMigrationsRow) (int, error) {
	lowest := 0
	for i, r := range rows {
		if !isRenumberedLabelDefinitionsRow(r) {
			continue
		}
		if r.hash == labelDefinitionsFirstBodyHash {
			return 0, &RenumberedLabelDefinitionsError{Version: r.version, Hash: r.hash}
		}
		for _, above := range rows[i+1:] {
			if !isRenumberedLabelDefinitionsRow(above) {
				return 0, &RenumberedLabelDefinitionsError{
					Version: r.version, Hash: r.hash, Above: above.version, AboveHash: above.hash,
				}
			}
		}
		if lowest == 0 {
			lowest = r.version
		}
	}
	return lowest, nil
}

// readSchemaMigrationsFrom reads every schema_migrations row at or above from,
// ascending. It selects * so a table that predates the content_hash column
// reads as rows without a hash instead of failing the statement: a failing
// statement pins a pooled Dolt session to its old catalog (be-bv7x).
func readSchemaMigrationsFrom(ctx context.Context, db DBConn, table string, from int) ([]schemaMigrationsRow, error) {
	//nolint:gosec // G202: table is a migration-series constant; from is bound.
	rows, err := db.QueryContext(ctx, "SELECT * FROM "+table+" WHERE version >= ? ORDER BY version", from)
	if err != nil {
		return nil, fmt.Errorf("reading %s rows from %d: %w", table, from, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("reading %s rows from %d: %w", table, from, err)
	}
	versionCol, hashCol := -1, -1
	for i, c := range cols {
		switch strings.ToLower(c) {
		case "version":
			versionCol = i
		case "content_hash":
			hashCol = i
		}
	}
	if versionCol < 0 {
		return nil, fmt.Errorf("reading %s rows from %d: no version column in %v", table, from, cols)
	}
	cells := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range cells {
		dest[i] = &cells[i]
	}
	var out []schemaMigrationsRow
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("reading %s rows from %d: %w", table, from, err)
		}
		v, err := strconv.Atoi(string(cells[versionCol]))
		if err != nil {
			return nil, fmt.Errorf("reading %s rows from %d: version %q: %w", table, from, cells[versionCol], err)
		}
		r := schemaMigrationsRow{version: v}
		if hashCol >= 0 {
			r.hash = string(cells[hashCol])
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s rows from %d: %w", table, from, err)
	}
	return out, nil
}

// applyLabelDefinitionsCursorRule takes the main cursor as MAX(version) read
// it and returns it with the cursor rule applied. It costs one read of the
// rows at or above labelDefinitionsLowestSlot when the cursor reaches that
// slot, and one more MAX read only when a row does not count.
func (m migrationSource) applyLabelDefinitionsCursorRule(ctx context.Context, db DBConn, current int) (int, error) {
	if m.cursorTable != mainSource.cursorTable || current < labelDefinitionsLowestSlot {
		return current, nil
	}
	rows, err := readSchemaMigrationsFrom(ctx, db, m.cursorTable, labelDefinitionsLowestSlot)
	if err != nil {
		return 0, err
	}
	hidden, err := hiddenLabelDefinitionsFloor(rows)
	if err != nil || hidden == 0 {
		return current, err
	}
	//nolint:gosec // G202: cursorTable is a migration-series constant; hidden is bound.
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM "+m.cursorTable+" WHERE version < ?", hidden).Scan(&current); err != nil {
		return 0, fmt.Errorf("reading %s version below the label_definitions row %d: %w", m.cursorTable, hidden, err)
	}
	return current, nil
}

// recordMigrationStep records version in src's cursor table with the applied
// file's hash. On the main source, inside the slots label_definitions once
// held, the step replaces a label_definitions row at its version: that row did
// not count (applyLabelDefinitionsCursorRule), and the upstream file just
// applied there records its own hash. Both statements run before the step's
// commit, so the row and the file's DDL land in one Dolt commit. Every other
// existing row is kept, as INSERT IGNORE always did.
func recordMigrationStep(ctx context.Context, db DBConn, src migrationSource, version int, contentHash string) error {
	if src.cursorTable == mainSource.cursorTable && version >= labelDefinitionsLowestSlot && version < labelDefinitionsVersion {
		args := append([]any{version}, labelDefinitionsHashArgs()...)
		//nolint:gosec // G202: only the placeholder list is concatenated; every value is bound.
		if _, err := db.ExecContext(ctx,
			"DELETE FROM "+src.cursorTable+" WHERE version = ? AND content_hash IN ("+labelDefinitionsHashPlaceholders()+")",
			args...); err != nil {
			return fmt.Errorf("replacing the label_definitions row at %d: %w", version, err)
		}
	}
	//nolint:gosec // G202: cursorTable is a migration-series constant; every value is bound.
	_, err := db.ExecContext(ctx, "INSERT IGNORE INTO "+src.cursorTable+" (version, content_hash) VALUES (?, ?)", version, contentHash)
	return err
}

func labelDefinitionsHashArgs() []any {
	args := make([]any, len(labelDefinitionsContentHashes))
	for i, h := range labelDefinitionsContentHashes {
		args[i] = h
	}
	return args
}

func labelDefinitionsHashPlaceholders() string {
	return strings.TrimSuffix(strings.Repeat("?, ", len(labelDefinitionsContentHashes)), ", ")
}
