package uow

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/storage/schema"
)

func expectVersionQuery(mock sqlmock.Sqlmock, version int) {
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations`).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(version))
}

func newVersionMockDB(t *testing.T) (sqlmock.Sqlmock, schema.DBConn, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return mock, db, func() { _ = db.Close() }
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return buf.String()
}

func TestCheckTeamServerSchema_FreshDB_RefusesWithBtsInit(t *testing.T) {
	mock, db, closeDB := newVersionMockDB(t)
	defer closeDB()
	expectVersionQuery(mock, 0)

	err := checkTeamServerSchema(context.Background(), db, "beads_team")
	if err == nil {
		t.Fatal("checkTeamServerSchema = nil, want refusal for empty (v0) database")
	}
	if !strings.Contains(err.Error(), "bts init") {
		t.Errorf("error %q should point at 'bts init'", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestCheckTeamServerSchema_MissingTable_RefusesWithBtsInit(t *testing.T) {
	mock, db, closeDB := newVersionMockDB(t)
	defer closeDB()
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations`).
		WillReturnError(errors.New("Error 1146 (42S02): Table 'beads_team.schema_migrations' doesn't exist"))

	err := checkTeamServerSchema(context.Background(), db, "beads_team")
	if err == nil {
		t.Fatal("checkTeamServerSchema = nil, want refusal when schema_migrations is absent")
	}
	if !strings.Contains(err.Error(), "bts init") {
		t.Errorf("error %q should point at 'bts init'", err)
	}
}

func TestCheckTeamServerSchema_Behind_RefusesWithBtsMigrate(t *testing.T) {
	mock, db, closeDB := newVersionMockDB(t)
	defer closeDB()
	behind := schema.LatestVersion() - 1
	expectVersionQuery(mock, behind)

	err := checkTeamServerSchema(context.Background(), db, "beads_team")
	if err == nil {
		t.Fatal("checkTeamServerSchema = nil, want refusal for behind database")
	}
	if !strings.Contains(err.Error(), "bts migrate") {
		t.Errorf("error %q should point at 'bts migrate'", err)
	}
	if strings.Contains(err.Error(), "run any bd write command") {
		t.Errorf("error %q must not suggest bd-driven migration on a bts-owned schema", err)
	}
}

func TestCheckTeamServerSchema_Equal_OK(t *testing.T) {
	mock, db, closeDB := newVersionMockDB(t)
	defer closeDB()
	expectVersionQuery(mock, schema.LatestVersion())

	if err := checkTeamServerSchema(context.Background(), db, "beads_team"); err != nil {
		t.Fatalf("checkTeamServerSchema = %v, want nil at matching version", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestCheckTeamServerSchema_Ahead_ReturnsSchemaSkewError(t *testing.T) {
	mock, db, closeDB := newVersionMockDB(t)
	defer closeDB()
	ahead := schema.LatestVersion() + 2
	// The ahead case delegates to CheckForwardDrift, which re-reads the version.
	expectVersionQuery(mock, ahead)
	expectVersionQuery(mock, ahead)

	err := checkTeamServerSchema(context.Background(), db, "beads_team")
	if !schema.IsSchemaSkewError(err) {
		t.Fatalf("checkTeamServerSchema = %v (%T), want *schema.SchemaSkewError", err, err)
	}
}

func TestCheckTeamServerSchema_Behind_EscapeHatchDoesNotBypass(t *testing.T) {
	t.Setenv("BD_IGNORE_SCHEMA_SKEW", "1")
	mock, db, closeDB := newVersionMockDB(t)
	defer closeDB()
	expectVersionQuery(mock, schema.LatestVersion()-1)

	if err := checkTeamServerSchema(context.Background(), db, "beads_team"); err == nil {
		t.Fatal("checkTeamServerSchema = nil; BD_IGNORE_SCHEMA_SKEW must not allow writing against an older bts schema")
	}
}

func TestCheckTeamServerSchema_Ahead_EscapeHatchWarnsInsteadOfRefusing(t *testing.T) {
	t.Setenv("BD_IGNORE_SCHEMA_SKEW", "1")
	mock, db, closeDB := newVersionMockDB(t)
	defer closeDB()
	ahead := schema.LatestVersion() + 1
	expectVersionQuery(mock, ahead)
	expectVersionQuery(mock, ahead)

	var err error
	stderr := captureStderr(t, func() {
		err = checkTeamServerSchema(context.Background(), db, "beads_team")
	})
	if err != nil {
		t.Fatalf("checkTeamServerSchema = %v, want nil with BD_IGNORE_SCHEMA_SKEW=1", err)
	}
	if !strings.Contains(stderr, "schema skew ignored") {
		t.Errorf("stderr = %q, want a schema-skew warning", stderr)
	}
}
