package dolt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// Server-lane coverage for the label_definitions renumber heal (bda-e080).
//
// The fork-only label_definitions migration has shipped under slots 0066,
// 0067, 0068, 0069, 0070, 0073 and now 0075, always with the same bytes. A
// database migrated by a build that carried it at slot N recorded the row
// (N, <label_definitions hash>). Upstream later took slot N for one of its own
// migrations, and the runner skips every version at or below MAX(version), so
// a newer build never applies upstream's file N there: issues gains no
// participation_generation (upstream 0070) or no arrival_seq (upstream 0073).
//
// The heal is keyed on the recorded content hash, never on the version or on
// the label_definitions table being present: a row at N that carries upstream's
// own hash (or no hash) is upstream's, and its file is never re-run.

const (
	labelDefsMigrationFile = "0075_create_label_definitions.up.sql"
	// The frozen file's SHA-256, pinned independently of the heal's registry.
	labelDefsMigrationHash = "9c76d11ee4c29c32c226b273642e087458a5d92f31c525885a0794487d70cd00"
	labelDefsSurvivorLabel = "Area:Storage"
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

func (d *planTestDB) count(query string, args ...any) int {
	d.t.Helper()
	var n int
	if err := d.conn.QueryRowContext(d.ctx, query, args...).Scan(&n); err != nil {
		d.t.Fatalf("count %.200q: %v", query, err)
	}
	return n
}

// createLabelDefinitionsWithRow runs the frozen label_definitions body and
// defines one label, so a test can see that the heal keeps both.
func (d *planTestDB) createLabelDefinitionsWithRow() {
	d.t.Helper()
	body, err := schema.MigrationSQL(labelDefsMigrationFile)
	if err != nil {
		d.t.Fatalf("read %s: %v", labelDefsMigrationFile, err)
	}
	d.exec(body)
	d.exec("INSERT INTO label_definitions (label, label_folded, description) VALUES (?, ?, ?)",
		labelDefsSurvivorLabel, "area:storage", "defined before the heal")
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

// TestMigrateUpHealsLabelDefinitionsRecordedAtAnUpstreamSlot is bda-e080's
// RED/GREEN pair: a database whose cursor row at 70 (or 73) carries the
// label_definitions hash must still get upstream's 0070 (or 0073) when a
// build that holds label_definitions at 0075 opens it.
func TestMigrateUpHealsLabelDefinitionsRecordedAtAnUpstreamSlot(t *testing.T) {
	for _, tc := range renumberSlotCases {
		t.Run(fmt.Sprintf("slot%d", tc.slot), func(t *testing.T) {
			skipIfNoDolt(t)
			acquireTestSlot()
			t.Cleanup(releaseTestSlot)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			t.Cleanup(cancel)

			d := newPlanTestDB(t, ctx)
			d.forgeLabelDefinitionsAtSlot(tc.slot, labelDefsMigrationHash)
			if d.columnPresent("issues", tc.column) {
				t.Fatalf("fixture already has issues.%s: it must lack upstream %s to reproduce the skip", tc.column, tc.file)
			}

			if _, err := schema.MigrateUp(ctx, d.conn); err != nil {
				t.Fatalf("MigrateUp: %v", err)
			}

			if !d.columnPresent("issues", tc.column) {
				t.Errorf("after MigrateUp issues.%s is missing: upstream %s was skipped because schema_migrations row %d carries the label_definitions hash (bda-e080)",
					tc.column, tc.file, tc.slot)
			}
			if got, want := d.cursorHash("schema_migrations", tc.slot), migrationFileHash(t, tc.file); got != want {
				t.Errorf("schema_migrations row %d content_hash = %q, want %s's %q", tc.slot, got, tc.file, want)
			}
			if got := d.cursorHash("schema_migrations", 75); got != labelDefsMigrationHash {
				t.Errorf("schema_migrations row 75 content_hash = %q, want the label_definitions hash %q", got, labelDefsMigrationHash)
			}
			if got, want := schemaVersion(t, d), schema.LatestVersion(); got != want {
				t.Errorf("schema cursor after MigrateUp = %d, want latest %d", got, want)
			}
			if n := d.count("SELECT COUNT(*) FROM label_definitions WHERE label = ?", labelDefsSurvivorLabel); n != 1 {
				t.Errorf("label_definitions rows for %q after MigrateUp = %d, want 1: the heal must keep the vocabulary", labelDefsSurvivorLabel, n)
			}
			// The heal rides the step commit of the migration it runs before,
			// so neither its ALTER nor its cursor rewrite stays in the working set.
			if n := d.count("SELECT COUNT(*) FROM dolt_status WHERE table_name IN ('issues', 'schema_migrations')"); n != 0 {
				t.Errorf("dolt_status lists %d of issues/schema_migrations after MigrateUp, want 0: the heal must land in a commit", n)
			}

			applied, err := schema.MigrateUp(ctx, d.conn)
			if err != nil {
				t.Fatalf("second MigrateUp: %v", err)
			}
			if applied != 0 {
				t.Errorf("second MigrateUp applied %d migrations, want 0", applied)
			}
		})
	}
}

// TestMigrateUpRefusesTheHealOverADirtyTableItAlters pins the dirty-table
// contract for the heal: the file it runs in place of a label_definitions row
// counts as pending, so an uncommitted write to a table that file alters stops
// the pass before anything runs, as it would for a normal pending step. At
// slot 73 only the healed 0073 alters issues (0074 and 0075 do not), so
// without that the heal would ALTER issues while the user's write sits in it,
// and the step commit, which leaves pre-existing dirt out, would record the
// cursor rewrite without the ALTER.
func TestMigrateUpRefusesTheHealOverADirtyTableItAlters(t *testing.T) {
	skipIfNoDolt(t)
	acquireTestSlot()
	t.Cleanup(releaseTestSlot)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)

	tc := renumberSlotCases[1]
	d := newPlanTestDB(t, ctx)
	d.forgeLabelDefinitionsAtSlot(tc.slot, labelDefsMigrationHash)
	d.exec("INSERT INTO issues (id, title, description, design, acceptance_criteria, notes) VALUES ('heal-dirty-1', 'uncommitted', '', '', '', '')")

	_, err := schema.MigrateUp(ctx, d.conn)
	var dirty *schema.DirtyTablesError
	if !errors.As(err, &dirty) {
		t.Fatalf("MigrateUp over an uncommitted issues write with %s still to heal = %v, want *schema.DirtyTablesError", tc.file, err)
	}
	if !slices.Contains(dirty.Tables, "issues") {
		t.Errorf("DirtyTablesError.Tables = %v, want it to name issues (altered by the healed %s)", dirty.Tables, tc.file)
	}
	if d.columnPresent("issues", tc.column) {
		t.Errorf("issues.%s exists after the refused pass: the heal ran before the guard", tc.column)
	}
	if got := d.cursorHash("schema_migrations", tc.slot); got != labelDefsMigrationHash {
		t.Errorf("schema_migrations row %d content_hash = %q after the refused pass, want it unchanged", tc.slot, got)
	}
}

// TestMigrateUpLeavesAGenuineUpstreamSlotAlone is the negative half: the same
// slots with upstream's own row, or with a row that carries no hash, are not
// the renumber's and must not be touched, even with label_definitions present.
// Each fixture drops the column upstream's file adds, so a re-run of that file
// would show as the column coming back.
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
			skipIfNoDolt(t)
			acquireTestSlot()
			t.Cleanup(releaseTestSlot)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			t.Cleanup(cancel)

			d := newPlanTestDB(t, ctx)
			var wantHash string
			if tc.genuine {
				if _, err := schema.MigrateUpTo(ctx, d.conn, tc.slot.slot); err != nil {
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

			if _, err := schema.MigrateUp(ctx, d.conn); err != nil {
				t.Fatalf("MigrateUp: %v", err)
			}

			if d.columnPresent("issues", tc.slot.column) {
				t.Errorf("MigrateUp re-ran %s on a row that is not the renumber's (content_hash %q): issues.%s came back",
					tc.slot.file, wantHash, tc.slot.column)
			}
			if got := d.cursorHash("schema_migrations", tc.slot.slot); got != wantHash {
				t.Errorf("schema_migrations row %d content_hash = %q after MigrateUp, want it untouched at %q", tc.slot.slot, got, wantHash)
			}
			if got, want := schemaVersion(t, d), schema.LatestVersion(); got != want {
				t.Errorf("schema cursor after MigrateUp = %d, want latest %d", got, want)
			}
		})
	}
}
