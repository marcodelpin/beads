-- Reverse migration 0064: drop the clone-local events journal tables.
-- Both are dolt_ignored, so this only touches the working set.
DROP TABLE IF EXISTS bd_events_journal;
DROP TABLE IF EXISTS bd_events_seq;
