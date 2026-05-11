-- issues.started_at
SET @needs_add = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'started_at'
);
SET @sql = IF(@needs_add = 1,
    'ALTER TABLE issues ADD COLUMN started_at DATETIME',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- wisps.started_at
SET @needs_add = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'wisps'
      AND COLUMN_NAME = 'started_at'
);
SET @sql = IF(@needs_add = 1,
    'ALTER TABLE wisps ADD COLUMN started_at DATETIME',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
