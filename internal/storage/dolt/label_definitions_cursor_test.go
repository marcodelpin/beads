package dolt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// Server-lane coverage for the label_definitions cursor rule (bda-e080,
// decision D-cursor).
//
// The fork-only label_definitions migration has shipped under slots 0066,
// 0067, 0068, 0070, 0073 and now 0075, always with the same bytes after its
// first day. A database migrated by a build that carried it at slot N recorded
// the row (N, <label_definitions hash>). Upstream later took slot N for one of
// its own migrations. If that row counted toward the cursor, a newer build
// would never apply upstream's file N there: issues would gain no
// participation_generation (upstream 0070) or no arrival_seq (upstream 0073).
//
// The rule: such a row does not count when no other row sits above it, so the
// upstream file it hid is an ordinary pending step (consent, plans, per-step
// commit), and that step replaces the row. A row with upstream's own hash, or
// with no hash, counts. A shape the rule cannot read is refused.

const (
	labelDefsMigrationFile = "0075_create_label_definitions.up.sql"
	// The frozen file's SHA-256, pinned independently of the schema package.
	labelDefsMigrationHash = "9c76d11ee4c29c32c226b273642e087458a5d92f31c525885a0794487d70cd00"
	labelDefsSurvivorLabel = "Area:Storage"
	// labelDefsSlot is the slot label_definitions holds in this build, which
	// is also the latest main version.
	labelDefsSlot = 75
)

// renumberSlotCase is one upstream slot the label_definitions migration once
// held, with the column upstream's file at that slot adds to issues.
type renumberSlotCase struct {
	slot   int
	file   string
	column string
}

var renumberSlotCases = []renumberSlotCase{
	{slot: 70, file: "0070_add_participation_generation.up.sql", column: "participation_generation"},
	{slot: 73, file: "0073_add_arrival_seq.up.sql", column: "arrival_seq"},
}

func migrationFileHash(t *testing.T, name string) string {
	t.Helper()
	body, err := schema.MigrationSQL(name)
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func (d *planTestDB) columnPresent(table, column string) bool {
	d.t.Helper()
	var n int
	if err := d.conn.QueryRowContext(d.ctx,
		"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
		table, column).Scan(&n); err != nil {
		d.t.Fatalf("probe %s.%s: %v", table, column, err)
	}
	return n > 0
}

func (d *planTestDB) columnDatetimePrecision(table, column string) int {
	d.t.Helper()
	var p sql.NullInt64
	if err := d.conn.QueryRowContext(d.ctx,
		"SELECT DATETIME_PRECISION FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
		table, column).Scan(&p); err != nil {
		d.t.Fatalf("probe %s.%s precision: %v", table, column, err)
	}
	return int(p.Int64)
}

func (d *planTestDB) count(query string, args ...any) int {
	d.t.Helper()
	var n int
	if err := d.conn.QueryRowContext(d.ctx, query, args...).Scan(&n); err != nil {
		d.t.Fatalf("count %.200q: %v", query, err)
	}
	return n
}

func (d *planTestDB) dirtyTableNames() []string {
	d.t.Helper()
	rows, err := d.conn.QueryContext(d.ctx, "SELECT table_name, staged, status FROM dolt_status ORDER BY table_name")
	if err != nil {
		d.t.Fatalf("read dolt_status: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name, status string
		var staged bool
		if err := rows.Scan(&name, &staged, &status); err != nil {
			d.t.Fatalf("read dolt_status: %v", err)
		}
		names = append(names, fmt.Sprintf("%s(staged=%v,%s)", name, staged, status))
	}
	if err := rows.Err(); err != nil {
		d.t.Fatalf("read dolt_status: %v", err)
	}
	return names
}

func (d *planTestDB) headHash() string {
	d.t.Helper()
	var h string
	if err := d.conn.QueryRowContext(d.ctx, "SELECT DOLT_HASHOF('HEAD')").Scan(&h); err != nil {
		d.t.Fatalf("read HEAD: %v", err)
	}
	return h
}

// cursorVersion reads the cursor the way every gate does, through the rule.
func (d *planTestDB) cursorVersion() int {
	d.t.Helper()
	v, err := schema.CurrentVersion(d.ctx, d.conn)
	if err != nil {
		d.t.Fatalf("schema.CurrentVersion: %v", err)
	}
	return v
}

// createLabelDefinitionsWithRow runs the frozen label_definitions body and
// defines one label, so a test can see that the vocabulary survives.
func (d *planTestDB) createLabelDefinitionsWithRow() {
	d.t.Helper()
	body, err := schema.MigrationSQL(labelDefsMigrationFile)
	if err != nil {
		d.t.Fatalf("read %s: %v", labelDefsMigrationFile, err)
	}
	d.exec(body)
	d.exec("INSERT INTO label_definitions (label, label_folded, description) VALUES (?, ?, ?)",
		labelDefsSurvivorLabel, "area:storage", "defined at the old slot")
}

// forgeLabelDefinitionsAtSlot builds the database a fork build that carried
// label_definitions at slot left behind: upstream's series genuinely applied
// through slot-1, the label_definitions table, and the cursor row
// (slot, hash). The state is committed, as that build's per-step commit left
// it.
func (d *planTestDB) forgeLabelDefinitionsAtSlot(slot int, hash any) {
	d.t.Helper()
	if _, err := schema.MigrateUpTo(d.ctx, d.conn, slot-1); err != nil {
		d.t.Fatalf("migrate main to %d: %v", slot-1, err)
	}
	d.createLabelDefinitionsWithRow()
	d.exec("INSERT INTO schema_migrations (version, content_hash) VALUES (?, ?)", slot, hash)
	d.commit(fmt.Sprintf("fixture: label_definitions recorded at slot %d", slot))
}

// stepCommit returns the hash of the one commit a per-step commit recorded
// for file, failing the test unless exactly one exists.
func (d *planTestDB) stepCommit(file string) string {
	d.t.Helper()
	rows, err := d.conn.QueryContext(d.ctx, "SELECT commit_hash FROM dolt_log WHERE message = ?", "schema: apply migration "+file)
	if err != nil {
		d.t.Fatalf("read dolt_log: %v", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			d.t.Fatalf("read dolt_log: %v", err)
		}
		hashes = append(hashes, h)
	}
	if err := rows.Err(); err != nil {
		d.t.Fatalf("read dolt_log: %v", err)
	}
	if len(hashes) != 1 {
		d.t.Fatalf("dolt_log holds %d commits named %q, want 1: the file must apply as a step of its own", len(hashes), "schema: apply migration "+file)
	}
	return hashes[0]
}

func (d *planTestDB) parentCommit(commit string) string {
	d.t.Helper()
	var p string
	if err := d.conn.QueryRowContext(d.ctx, "SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ? AND parent_index = 0", commit).Scan(&p); err != nil {
		d.t.Fatalf("read parent of %s: %v", commit, err)
	}
	return p
}

func (d *planTestDB) createTableAsOf(table, rev string) string {
	d.t.Helper()
	var name, stmt string
	if err := d.conn.QueryRowContext(d.ctx, "SHOW CREATE TABLE "+table+" AS OF '"+rev+"'").Scan(&name, &stmt); err != nil {
		d.t.Fatalf("SHOW CREATE TABLE %s AS OF %s: %v", table, rev, err)
	}
	return stmt
}

func (d *planTestDB) cursorHashAsOf(version int, rev string) string {
	d.t.Helper()
	var h sql.NullString
	if err := d.conn.QueryRowContext(d.ctx, "SELECT content_hash FROM schema_migrations AS OF '"+rev+"' WHERE version = ?", version).Scan(&h); err != nil {
		d.t.Fatalf("read schema_migrations row %d AS OF %s: %v", version, rev, err)
	}
	return h.String
}

func newLabelCursorTestDB(t *testing.T) *planTestDB {
	t.Helper()
	skipIfNoDolt(t)
	acquireTestSlot()
	t.Cleanup(releaseTestSlot)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	return newPlanTestDB(t, ctx)
}

// TestMigrateUpAppliesTheUpstreamFileALabelDefinitionsRowHid is bda-e080's
// RED/GREEN pair: a database whose row at 70 (or 73) carries the
// label_definitions hash must get upstream's 0070 (or 0073) as an ordinary
// step, committed with the row that records it (codex bda-e080-r1 #1), and a
// second pass applies nothing.
func TestMigrateUpAppliesTheUpstreamFileALabelDefinitionsRowHid(t *testing.T) {
	for _, tc := range renumberSlotCases {
		t.Run(fmt.Sprintf("slot%d", tc.slot), func(t *testing.T) {
			d := newLabelCursorTestDB(t)
			d.forgeLabelDefinitionsAtSlot(tc.slot, labelDefsMigrationHash)
			if d.columnPresent("issues", tc.column) {
				t.Fatalf("fixture already has issues.%s: it must lack upstream %s to reproduce the skip", tc.column, tc.file)
			}
			if got := d.cursorVersion(); got != tc.slot-1 {
				t.Errorf("cursor with the label_definitions hash at %d = %d, want %d: the row must not count", tc.slot, got, tc.slot-1)
			}

			applied, err := schema.MigrateUp(d.ctx, d.conn)
			if err != nil {
				t.Fatalf("MigrateUp: %v", err)
			}
			if want := labelDefsSlot - tc.slot + 1; applied != want {
				t.Errorf("MigrateUp applied %d migrations, want %d (%d through %d)", applied, want, tc.slot, labelDefsSlot)
			}
			if !d.columnPresent("issues", tc.column) {
				t.Errorf("after MigrateUp issues.%s is missing: upstream %s was skipped because schema_migrations row %d carries the label_definitions hash (bda-e080)",
					tc.column, tc.file, tc.slot)
			}
			upstream := migrationFileHash(t, tc.file)
			if got := d.cursorHash("schema_migrations", tc.slot); got != upstream {
				t.Errorf("schema_migrations row %d content_hash = %q, want %s's %q", tc.slot, got, tc.file, upstream)
			}
			if got := d.cursorHash("schema_migrations", labelDefsSlot); got != labelDefsMigrationHash {
				t.Errorf("schema_migrations row %d content_hash = %q, want the label_definitions hash %q", labelDefsSlot, got, labelDefsMigrationHash)
			}
			if got, want := d.cursorVersion(), schema.LatestVersion(); got != want {
				t.Errorf("schema cursor after MigrateUp = %d, want latest %d", got, want)
			}
			if n := d.count("SELECT COUNT(*) FROM label_definitions WHERE label = ?", labelDefsSurvivorLabel); n != 1 {
				t.Errorf("label_definitions rows for %q after MigrateUp = %d, want 1: the vocabulary must survive", labelDefsSurvivorLabel, n)
			}
			// Only the tables these steps write: the fixture was built with
			// MigrateUpTo, which never runs the ignored series, so MigrateUp
			// runs it here and its ignored 0012/0016 leave leases in the
			// working set, on the unfixed tree too.
			if n := d.count("SELECT COUNT(*) FROM dolt_status WHERE table_name IN ('issues', 'schema_migrations', 'label_definitions')"); n != 0 {
				t.Errorf("dolt_status lists %d of issues/schema_migrations/label_definitions after MigrateUp (%v), want 0", n, d.dirtyTableNames())
			}

			// codex #1: the row and the DDL land in ONE commit, the step's own.
			step := d.stepCommit(tc.file)
			parent := d.parentCommit(step)
			if got := d.cursorHashAsOf(tc.slot, step); got != upstream {
				t.Errorf("row %d AS OF the %s step commit = %q, want %q", tc.slot, tc.file, got, upstream)
			}
			if got := d.cursorHashAsOf(tc.slot, parent); got != labelDefsMigrationHash {
				t.Errorf("row %d AS OF the parent of the %s step commit = %q, want the label_definitions hash", tc.slot, tc.file, got)
			}
			if !strings.Contains(d.createTableAsOf("issues", step), "`"+tc.column+"`") {
				t.Errorf("issues AS OF the %s step commit lacks %s", tc.file, tc.column)
			}
			if strings.Contains(d.createTableAsOf("issues", parent), "`"+tc.column+"`") {
				t.Errorf("issues AS OF the parent of the %s step commit already has %s: the DDL is not in the step commit", tc.file, tc.column)
			}

			headBefore := d.headHash()
			applied, err = schema.MigrateUp(d.ctx, d.conn)
			if err != nil {
				t.Fatalf("second MigrateUp: %v", err)
			}
			if applied != 0 {
				t.Errorf("second MigrateUp applied %d migrations, want 0", applied)
			}
			if got := d.headHash(); got != headBefore {
				t.Errorf("second MigrateUp moved HEAD from %s to %s", headBefore, got)
			}
		})
	}
}

// TestMigrateUpAppliesTheHolesUnderALabelDefinitionsRow is codex bda-e080-r1
// #2: fork revision c2d3c8a28 carried upstream 0066 and 0067, then
// label_definitions at 0070, with no 0068 or 0069. A database it migrated has
// rows through 67 and the label_definitions row at 70. The cursor falls to 67,
// so 0068 (issue_versions.attribution_status) and 0069 (change_at widened to
// DATETIME(6)) apply as well as 0070.
func TestMigrateUpAppliesTheHolesUnderALabelDefinitionsRow(t *testing.T) {
	d := newLabelCursorTestDB(t)
	if _, err := schema.MigrateUpTo(d.ctx, d.conn, 67); err != nil {
		t.Fatalf("migrate main to 67: %v", err)
	}
	d.createLabelDefinitionsWithRow()
	d.exec("INSERT INTO schema_migrations (version, content_hash) VALUES (70, ?)", labelDefsMigrationHash)
	d.commit("fixture: fork rev c2d3c8a28, label_definitions at 70 over 68/69 holes")
	if d.columnPresent("issue_versions", "attribution_status") {
		t.Fatalf("fixture already has issue_versions.attribution_status")
	}
	if p := d.columnDatetimePrecision("issue_versions", "change_at"); p == 6 {
		t.Fatalf("fixture issue_versions.change_at is already DATETIME(6)")
	}

	if _, err := schema.MigrateUp(d.ctx, d.conn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	if !d.columnPresent("issue_versions", "attribution_status") {
		t.Errorf("issue_versions.attribution_status is missing: the 0068 hole under the label_definitions row at 70 was skipped")
	}
	if p := d.columnDatetimePrecision("issue_versions", "change_at"); p != 6 {
		t.Errorf("issue_versions.change_at DATETIME_PRECISION = %d, want 6: the 0069 hole was skipped", p)
	}
	if !d.columnPresent("issues", "participation_generation") {
		t.Errorf("issues.participation_generation is missing: 0070 was skipped")
	}
	for _, f := range []struct {
		version int
		file    string
	}{
		{68, "0068_add_attribution_status.up.sql"},
		{69, "0069_widen_issue_versions_datetime_precision.up.sql"},
		{70, "0070_add_participation_generation.up.sql"},
	} {
		if got, want := d.cursorHash("schema_migrations", f.version), migrationFileHash(t, f.file); got != want {
			t.Errorf("schema_migrations row %d content_hash = %q, want %s's %q", f.version, got, f.file, want)
		}
	}
	if got, want := d.cursorVersion(), schema.LatestVersion(); got != want {
		t.Errorf("schema cursor after MigrateUp = %d, want latest %d", got, want)
	}
}

// TestMigrateUpWithoutConsentRefusesALabelDefinitionsRowWithNoWrite is codex
// bda-e080-r1 #5 on the ordinary shape: the upstream file a label_definitions
// row hid is pending work like any other, so without consent the pass refuses
// before any write, and the refusal counts that file.
func TestMigrateUpWithoutConsentRefusesALabelDefinitionsRowWithNoWrite(t *testing.T) {
	d := newLabelCursorTestDB(t)
	tc := renumberSlotCases[0]
	d.forgeLabelDefinitionsAtSlot(tc.slot, labelDefsMigrationHash)
	headBefore := d.headHash()

	schema.SetLocalMigrateConsent(false)
	t.Cleanup(func() { schema.SetLocalMigrateConsent(true) })
	t.Setenv(schema.AllowMigrateEnv, "")
	t.Setenv(schema.AllowRemoteMigrateEnv, "")

	_, err := schema.MigrateUp(d.ctx, d.conn)
	var consent *schema.MigrateConsentError
	if !errors.As(err, &consent) {
		t.Fatalf("MigrateUp without consent = %v, want *schema.MigrateConsentError", err)
	}
	if consent.CurrentVersion != tc.slot-1 {
		t.Errorf("MigrateConsentError.CurrentVersion = %d, want %d: the label_definitions row at %d must not count", consent.CurrentVersion, tc.slot-1, tc.slot)
	}
	if want := labelDefsSlot - tc.slot + 1; consent.Pending != want {
		t.Errorf("MigrateConsentError.Pending = %d, want %d: %s must count as pending", consent.Pending, want, tc.file)
	}
	if got := d.headHash(); got != headBefore {
		t.Errorf("HEAD moved from %s to %s on a refused pass", headBefore, got)
	}
	if n := d.count("SELECT COUNT(*) FROM dolt_status"); n != 0 {
		t.Errorf("dolt_status lists %d tables after a refused pass, want 0", n)
	}
	if got := d.cursorHash("schema_migrations", tc.slot); got != labelDefsMigrationHash {
		t.Errorf("schema_migrations row %d content_hash = %q after a refused pass, want it unchanged", tc.slot, got)
	}
	if d.columnPresent("issues", tc.column) {
		t.Errorf("issues.%s exists after a refused pass", tc.column)
	}
}

// TestMigrateUpReplaysNothingForALabelDefinitionsHashBelowTheLowestSlot is
// codex bda-e080-r1 #8: no build ever recorded label_definitions below 66, so
// a matching hash on a row at 40 (an import, damage) is an ordinary row: the
// converged database reads as converged and nothing is replayed or rewritten.
func TestMigrateUpReplaysNothingForALabelDefinitionsHashBelowTheLowestSlot(t *testing.T) {
	d := newLabelCursorTestDB(t)
	if _, err := schema.MigrateUp(d.ctx, d.conn); err != nil {
		t.Fatalf("converge the fixture with MigrateUp: %v", err)
	}
	d.exec("UPDATE schema_migrations SET content_hash = ? WHERE version = 40", labelDefsMigrationHash)
	d.commit("fixture: label_definitions hash on row 40")
	headBefore := d.headHash()

	applied, err := schema.MigrateUp(d.ctx, d.conn)
	if err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	if applied != 0 {
		t.Errorf("MigrateUp applied %d migrations, want 0: a matching hash at 40 must replay nothing", applied)
	}
	if got := d.headHash(); got != headBefore {
		t.Errorf("HEAD moved from %s to %s", headBefore, got)
	}
	if got := d.cursorHash("schema_migrations", 40); got != labelDefsMigrationHash {
		t.Errorf("schema_migrations row 40 content_hash = %q, want it untouched", got)
	}
	if got, want := d.cursorVersion(), schema.LatestVersion(); got != want {
		t.Errorf("schema cursor = %d, want latest %d", got, want)
	}
}

// TestMigrateUpLeavesAGenuineUpstreamSlotAlone is the negative half: the same
// slots with upstream's own row, or with a row that carries no hash, count
// toward the cursor and their file is never re-run, even with
// label_definitions present. Each fixture drops the column upstream's file
// adds, so a re-run of that file would show as the column coming back.
func TestMigrateUpLeavesAGenuineUpstreamSlotAlone(t *testing.T) {
	type negCase struct {
		name string
		slot renumberSlotCase
		// genuine applies upstream's file at slot; otherwise the slot row is
		// inserted with a NULL content_hash.
		genuine bool
	}
	cases := []negCase{
		{name: "genuine70", slot: renumberSlotCases[0], genuine: true},
		{name: "genuine73", slot: renumberSlotCases[1], genuine: true},
		{name: "nullhash70", slot: renumberSlotCases[0], genuine: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newLabelCursorTestDB(t)
			var wantHash string
			if tc.genuine {
				if _, err := schema.MigrateUpTo(d.ctx, d.conn, tc.slot.slot); err != nil {
					t.Fatalf("migrate main to %d: %v", tc.slot.slot, err)
				}
				d.createLabelDefinitionsWithRow()
				d.exec("ALTER TABLE issues DROP COLUMN " + tc.slot.column)
				d.commit(fmt.Sprintf("fixture: genuine upstream row %d, issues.%s dropped", tc.slot.slot, tc.slot.column))
				wantHash = migrationFileHash(t, tc.slot.file)
			} else {
				d.forgeLabelDefinitionsAtSlot(tc.slot.slot, nil)
			}
			if got := d.cursorHash("schema_migrations", tc.slot.slot); got != wantHash {
				t.Fatalf("fixture schema_migrations row %d content_hash = %q, want %q", tc.slot.slot, got, wantHash)
			}
			if d.columnPresent("issues", tc.slot.column) {
				t.Fatalf("fixture still has issues.%s", tc.slot.column)
			}
			if got := d.cursorVersion(); got != tc.slot.slot {
				t.Errorf("cursor = %d, want %d: a row with upstream's hash or none counts", got, tc.slot.slot)
			}

			if _, err := schema.MigrateUp(d.ctx, d.conn); err != nil {
				t.Fatalf("MigrateUp: %v", err)
			}

			if d.columnPresent("issues", tc.slot.column) {
				t.Errorf("MigrateUp re-ran %s on a row that is not a label_definitions row (content_hash %q): issues.%s came back",
					tc.slot.file, wantHash, tc.slot.column)
			}
			if got := d.cursorHash("schema_migrations", tc.slot.slot); got != wantHash {
				t.Errorf("schema_migrations row %d content_hash = %q after MigrateUp, want it untouched at %q", tc.slot.slot, got, wantHash)
			}
			if got, want := d.cursorVersion(), schema.LatestVersion(); got != want {
				t.Errorf("schema cursor after MigrateUp = %d, want latest %d", got, want)
			}
		})
	}
}

// TestMigrateUpRefusesAHiddenFileOverADirtyTableItAlters pins the dirty-table
// contract: the file a label_definitions row hid is pending, so an uncommitted
// write to a table that file alters stops the pass before anything runs, as it
// would for any pending step. At slot 73 only 0073 alters issues among the
// pending files (0074 and 0075 do not).
func TestMigrateUpRefusesAHiddenFileOverADirtyTableItAlters(t *testing.T) {
	d := newLabelCursorTestDB(t)
	tc := renumberSlotCases[1]
	d.forgeLabelDefinitionsAtSlot(tc.slot, labelDefsMigrationHash)
	d.exec("INSERT INTO issues (id, title, description, design, acceptance_criteria, notes) VALUES ('label-cursor-dirty-1', 'uncommitted', '', '', '', '')")

	_, err := schema.MigrateUp(d.ctx, d.conn)
	var dirty *schema.DirtyTablesError
	if !errors.As(err, &dirty) {
		t.Fatalf("MigrateUp over an uncommitted issues write with %s pending = %v, want *schema.DirtyTablesError", tc.file, err)
	}
	if !slices.Contains(dirty.Tables, "issues") {
		t.Errorf("DirtyTablesError.Tables = %v, want it to name issues (altered by %s)", dirty.Tables, tc.file)
	}
	if d.columnPresent("issues", tc.column) {
		t.Errorf("issues.%s exists after the refused pass", tc.column)
	}
	if got := d.cursorHash("schema_migrations", tc.slot); got != labelDefsMigrationHash {
		t.Errorf("schema_migrations row %d content_hash = %q after the refused pass, want it unchanged", tc.slot, got)
	}
}

// TestMigrateUpAtLatestWritesNothing is the converged negative: a database at
// 75 whose only label_definitions row is this build's own gets no write from
// the cursor rule's extra read, neither a working-set change nor a commit.
func TestMigrateUpAtLatestWritesNothing(t *testing.T) {
	d := newLabelCursorTestDB(t)
	if _, err := schema.MigrateUp(d.ctx, d.conn); err != nil {
		t.Fatalf("first MigrateUp: %v", err)
	}
	if got := d.cursorVersion(); got != labelDefsSlot {
		t.Fatalf("cursor after the first MigrateUp = %d, want %d", got, labelDefsSlot)
	}
	if n := d.count("SELECT COUNT(*) FROM dolt_status"); n != 0 {
		t.Fatalf("dolt_status lists %d tables after the first MigrateUp, want a clean working set to compare against", n)
	}
	headBefore := d.headHash()

	applied, err := schema.MigrateUp(d.ctx, d.conn)
	if err != nil {
		t.Fatalf("MigrateUp at latest: %v", err)
	}
	if applied != 0 {
		t.Errorf("MigrateUp at latest applied %d, want 0", applied)
	}
	if got := d.headHash(); got != headBefore {
		t.Errorf("HEAD moved from %s to %s at latest", headBefore, got)
	}
	if n := d.count("SELECT COUNT(*) FROM dolt_status"); n != 0 {
		t.Errorf("dolt_status lists %d tables after MigrateUp at latest, want 0", n)
	}
}
