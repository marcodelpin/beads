package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// DefineLabelInTx inserts a new row into the opt-in label vocabulary registry
// (bd label define) within an existing transaction.
//
// It is a CREATE, not an upsert: defining a label that already exists --
// under the SAME spelling -- is rejected, so changing a description means
// undefine then define. Defining a label that collides with an EXISTING row
// only under a DIFFERENT case ("Backend" when "backend" is defined) is also
// rejected, naming the existing spelling in the error, so this table can
// never hold two case-variant spellings of the same word: the creation-time
// discipline the vocabulary registry exists to add, without folding any
// label already stored on an issue (that stays `bd label rename`'s job).
func DefineLabelInTx(ctx context.Context, tx DBTX, label, description, actor string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("%w: label cannot be empty", storage.ErrValidation)
	}
	if err := types.CheckFieldLen("label", label); err != nil {
		return err
	}
	if err := types.CheckTextLen("description", description); err != nil {
		return err
	}
	if err := rejectLabelCollisionInTx(ctx, tx, label); err != nil {
		return err
	}

	descArg := sql.NullString{String: description, Valid: description != ""}
	actorArg := sql.NullString{String: actor, Valid: actor != ""}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO label_definitions (label, description, created_by) VALUES (?, ?, ?)`,
		label, descArg, actorArg,
	); err != nil {
		return fmt.Errorf("define label: %w", err)
	}
	return nil
}

// rejectLabelCollisionInTx returns an error naming the existing spelling when
// label already has a case-insensitive match in label_definitions, and a
// distinct error when the match is the exact same spelling (already defined).
// A clean sql.ErrNoRows (no match at all) returns nil.
func rejectLabelCollisionInTx(ctx context.Context, tx DBTX, label string) error {
	var existing string
	err := tx.QueryRowContext(ctx,
		`SELECT label FROM label_definitions WHERE LOWER(label) = LOWER(?)`, label,
	).Scan(&existing)
	switch {
	case err == nil:
		if existing == label {
			return fmt.Errorf("%w: label %q is already defined", storage.ErrValidation, label)
		}
		return fmt.Errorf("%w: label %q collides with the existing definition %q (case-insensitive); use %q or undefine it first",
			storage.ErrValidation, label, existing, existing)
	case errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("define label: checking for collision: %w", err)
	}
}

// UndefineLabelInTx removes a row from the label vocabulary registry (bd
// label undefine) within an existing transaction. The match is
// case-insensitive, matching the case-collision discipline DefineLabelInTx
// enforces on the way in: only one spelling of a label can ever be defined,
// so there is only ever one row an undefine of any of its case-variants
// could mean.
//
// Undefining a label that was never defined is an error, not a silent no-op:
// unlike RemoveLabelInTx (which repairs a label mistakenly written on an
// issue, where idempotence matters more than catching a typo), undefine acts
// on a registry the caller is expected to already know the contents of, so a
// typo is far more likely than a race with a concurrent undefine of the same
// row.
func UndefineLabelInTx(ctx context.Context, tx DBTX, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("%w: label cannot be empty", storage.ErrValidation)
	}
	result, err := tx.ExecContext(ctx,
		`DELETE FROM label_definitions WHERE LOWER(label) = LOWER(?)`, label,
	)
	if err != nil {
		return fmt.Errorf("undefine label: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("undefine label: rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("%w: label %q is not defined", storage.ErrNotFound, label)
	}
	return nil
}

// ListLabelDefinitionsInTx returns every row in the label vocabulary
// registry, sorted by label, within an existing transaction.
func ListLabelDefinitionsInTx(ctx context.Context, tx DBTX) ([]types.LabelDefinition, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT label, description, created_at, created_by FROM label_definitions ORDER BY label`,
	)
	if err != nil {
		return nil, fmt.Errorf("list label definitions: %w", err)
	}
	defer rows.Close()

	var defs []types.LabelDefinition
	for rows.Next() {
		var (
			d         types.LabelDefinition
			desc      sql.NullString
			createdBy sql.NullString
		)
		if err := rows.Scan(&d.Label, &desc, &d.CreatedAt, &createdBy); err != nil {
			return nil, fmt.Errorf("list label definitions: scan: %w", err)
		}
		if desc.Valid {
			d.Description = &desc.String
		}
		if createdBy.Valid {
			d.CreatedBy = &createdBy.String
		}
		defs = append(defs, d)
	}
	return defs, rows.Err()
}

// LabelVocabularySet builds a case-insensitive lookup of every defined label,
// keyed on its lowercase form, for the write-path vocabulary check
// (checkLabelVocabulary in cmd/bd). The map value is the defined spelling, so
// a caller can suggest it when the input differs only in case.
func LabelVocabularySet(defs []types.LabelDefinition) map[string]string {
	set := make(map[string]string, len(defs))
	for _, d := range defs {
		set[strings.ToLower(d.Label)] = d.Label
	}
	return set
}
