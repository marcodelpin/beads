-- label_definitions: the opt-in curated label vocabulary a workspace can
-- declare via `bd label define`. Its presence changes nothing about what
-- label a caller may write by itself -- whether a write is checked against it
-- (and whether an undefined label warns or is refused) is controlled by the
-- labels.vocabulary config knob (open|warn|enforce, default open; see
-- docs/core-concepts/labels.md). `bd label add`/`create --labels`/`update
-- --add-label`/`tag`/`quick` consult it; import/replay never does.
--
-- Same shape as custom_statuses/custom_types (0024): a bare name table with
-- no relation to `issues`, so defining or undefining a label never touches a
-- row already carrying that label on an issue.
--
-- Case handling is deliberately narrow and lives entirely in `bd label
-- define`/`undefine`, not in this table's schema: define rejects a
-- case-insensitive collision against an EXISTING row (defining "Backend" when
-- "backend" is already defined is an error naming the existing spelling), so
-- this table never holds two case-variant spellings of the same word itself.
-- It does NOT fold or rewrite labels already stored on issues in a different
-- case -- that is `bd label rename`'s job, a separate capability -- so a
-- label written before its definition existed, in a different case, is only
-- ever reported by `bd doctor` as a case-variant cluster, never silently
-- merged here.
CREATE TABLE IF NOT EXISTS label_definitions (
    label VARCHAR(255) NOT NULL PRIMARY KEY,
    description TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by VARCHAR(255)
);
