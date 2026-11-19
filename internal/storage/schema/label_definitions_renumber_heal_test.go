package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The label_definitions renumber heal (bda-e080) is keyed on the recorded
// content hash. These tests pin the three facts the key and the out-of-order
// application rest on; the database-backed RED/GREEN pair lives in
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

// TestRenumberHealRunsBeforeEveryPossibleFirstPendingStep pins the
// registration range: a database whose cursor sits on any slot the migration
// once held (labelDefinitionsLowestSlot through labelDefinitionsVersion-1) has
// the next version as its first pending step, and the heal must run before it.
func TestRenumberHealRunsBeforeEveryPossibleFirstPendingStep(t *testing.T) {
	heal := reflect.ValueOf(healRenumberedLabelDefinitionsRows).Pointer()
	for v := labelDefinitionsLowestSlot + 1; v <= labelDefinitionsVersion; v++ {
		repair, ok := preMigrationRepairs[repairKey{mainSource.cursorTable, v}]
		if !ok {
			t.Errorf("no pre-migration repair before main %d: a cursor at %d would skip the label_definitions renumber heal", v, v-1)
			continue
		}
		if reflect.ValueOf(repair).Pointer() != heal {
			t.Errorf("pre-migration repair before main %d is not healRenumberedLabelDefinitionsRows", v)
		}
	}
}

// TestRenumberHealTargetsNeedNoOtherStepMachinery pins what makes the heal's
// out-of-order application equal to a normal step: every file it can run
// (the slots labelDefinitionsLowestSlot through labelDefinitionsVersion-1)
// exists, carries no execution plan, no updated_at guard and no repair of its
// own, and rewrites no row. A file that needs any of these would need the
// heal to grow the same machinery before it may run there.
func TestRenumberHealTargetsNeedNoOtherStepMachinery(t *testing.T) {
	heal := reflect.ValueOf(healRenumberedLabelDefinitionsRows).Pointer()
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
		if repair, ok := preMigrationRepairs[key]; ok && reflect.ValueOf(repair).Pointer() != heal {
			t.Errorf("%s has a pre-migration repair of its own the heal does not run", mf.name)
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
