-- Migration 0052 (down): drop commit_hash column from issues + wisps
--
-- Reverses 0052_add_commit_hash.up.sql. Drops the column from both tables.
-- Data loss is expected: this is a schema revert, not a data migration.

SET @has_col = (
    SELECT IF(COUNT(*) > 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'commit_hash'
);
SET @sql = IF(@has_col = 1,
    'ALTER TABLE issues DROP COLUMN commit_hash',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has_col_w = (
    SELECT IF(COUNT(*) > 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'wisps'
      AND COLUMN_NAME = 'commit_hash'
);
SET @sql = IF(@has_col_w = 1,
    'ALTER TABLE wisps DROP COLUMN commit_hash',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;