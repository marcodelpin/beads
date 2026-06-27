-- Migration 0052: add commit_hash column to issues + wisps tables
--
-- Steal from ghist Task.commit_hash (bd-improvement, sys-kjdc7).
-- Promotes the audit-trail commit reference from notes (parsed-text) to a
-- first-class Issue field, enabling structured queries such as
-- `bd ready --no-commit-link` to find work missing an audit trail.
--
-- VARCHAR(64) matches git SHA-256 (also covers the longer SHA-512 / hash
-- digests used by some forges) and the shorter 40-char SHA-1 we still
-- encounter in legacy refs. NULL = unlinked, matching the convention used
-- by compacted_at_commit (migration 0001).
--
-- Idempotent: guarded on COLUMN_EXISTS so re-running on a database that
-- already has the column is a no-op. Mirrors the safety pattern used in
-- 0037 (UUID primary keys) and 0038 (drop_hop_columns).
--
-- Both `issues` and `wisps` get the column: wisps are short-lived
-- persistent rows that share the same shape (sys-kjdc7 desc says
-- "first-class Issue field" — the wisp twin is the closest equivalent
-- for ephemeral rows; we add it to both to keep the storage layer
-- symmetrical and to keep the scan path single-shape).

SET @has_col = (
    SELECT IF(COUNT(*) > 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'commit_hash'
);
SET @sql = IF(@has_col = 0,
    'ALTER TABLE issues ADD COLUMN commit_hash VARCHAR(64) DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has_col_w = (
    SELECT IF(COUNT(*) > 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'wisps'
      AND COLUMN_NAME = 'commit_hash'
);
SET @sql = IF(@has_col_w = 0,
    'ALTER TABLE wisps ADD COLUMN commit_hash VARCHAR(64) DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
