//go:build cgo

package embeddeddolt_test

import (
	"testing"
)

// TestListLabelDefinitionsPreVocabularySchema pins the pre-0070 read path:
// a database whose schema predates the label_definitions table (a read-only
// open skips migrations, and an embedded open can stay on the previous
// schema) has, by construction, zero definitions. Listing must report that
// empty registry, not fail - bd export reads it unconditionally.
func TestListLabelDefinitionsPreVocabularySchema(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	te := newTestEnv(t, "ldpre")
	ctx := t.Context()
	te.exec(t, ctx, "DROP TABLE label_definitions")

	defs, err := te.store.ListLabelDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListLabelDefinitions on a schema without label_definitions: %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("expected no definitions, got %v", defs)
	}
}
