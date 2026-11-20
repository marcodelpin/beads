// Package schematest holds go-sqlmock expectations for reads the schema
// package issues, for tests that mock a whole schema.MigrateUp, a migration
// gate, or a cursor read statement by statement.
//
// It exists so the read sequence of the main cursor is written once: when the
// label_definitions cursor rule added a read to it (bda-e080), only the schema
// package's private helpers learned about it, and the sqlmock fixtures in
// internal/storage/uow and cmd/bd broke (bda-bmz7). It does not import schema,
// so the schema package's own tests use it too, which pins it to the
// production statements; TestSchematestMirrorsTheCursorRule pins the slot.
package schematest

import (
	"regexp"

	"github.com/DATA-DOG/go-sqlmock"
)

// LabelDefinitionsLowestSlot mirrors the schema package's
// labelDefinitionsLowestSlot: the lowest main-series version a fork
// label_definitions row ever held. From this cursor up, every main cursor read
// is followed by the cursor rule's row scan.
const LabelDefinitionsLowestSlot = 66

// MainCursorMaxQuery is the main cursor read, issued after the cursor table's
// existence probe.
const MainCursorMaxQuery = "SELECT COALESCE(MAX(version), 0) FROM schema_migrations"

// LabelCursorRowsQuery is the cursor rule's read of the main-series rows from
// LabelDefinitionsLowestSlot up.
const LabelCursorRowsQuery = "SELECT * FROM schema_migrations WHERE version >= ? ORDER BY version"

// ExpectMainCursorMax mocks one main cursor read after its existence probe:
// MAX(version) answering version, then the cursor rule's row scan finding no
// label_definitions row, so the cursor reads as version.
func ExpectMainCursorMax(mock sqlmock.Sqlmock, version int) {
	mock.ExpectQuery(regexp.QuoteMeta(MainCursorMaxQuery)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(version))
	ExpectLabelCursorRows(mock, version)
}

// ExpectLabelCursorRows mocks the cursor rule's row scan that follows a main
// MAX(version) read answering version, finding no label_definitions row. It
// mocks nothing below LabelDefinitionsLowestSlot, where the rule reads
// nothing. Tests of the rule itself script rows that match instead.
func ExpectLabelCursorRows(mock sqlmock.Sqlmock, version int) {
	if version < LabelDefinitionsLowestSlot {
		return
	}
	mock.ExpectQuery(regexp.QuoteMeta(LabelCursorRowsQuery)).
		WithArgs(LabelDefinitionsLowestSlot).
		WillReturnRows(sqlmock.NewRows([]string{"version", "content_hash"}))
}
