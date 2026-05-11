INSERT IGNORE INTO wisps SELECT * FROM issues
WHERE issue_type IN ('agent', 'rig', 'role', 'message');

UPDATE wisps SET ephemeral = 1
WHERE issue_type IN ('agent', 'rig', 'role', 'message');

INSERT IGNORE INTO wisp_labels (issue_id, label)
SELECT l.issue_id, l.label
FROM labels l
JOIN issues i ON i.id = l.issue_id
WHERE i.issue_type IN ('agent', 'rig', 'role', 'message');

INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_id, type, created_at, created_by, metadata, thread_id)
SELECT d.issue_id, d.depends_on_id, d.type, d.created_at, d.created_by, d.metadata, d.thread_id
FROM dependencies d
JOIN issues i ON i.id = d.issue_id
WHERE i.issue_type IN ('agent', 'rig', 'role', 'message');

INSERT IGNORE INTO wisp_events (id, issue_id, event_type, actor, old_value, new_value, comment, created_at)
SELECT e.id, e.issue_id, e.event_type, e.actor, e.old_value, e.new_value, e.comment, e.created_at
FROM events e
JOIN issues i ON i.id = e.issue_id
WHERE i.issue_type IN ('agent', 'rig', 'role', 'message');

INSERT IGNORE INTO wisp_comments (id, issue_id, author, text, created_at)
SELECT c.id, c.issue_id, c.author, c.text, c.created_at
FROM comments c
JOIN issues i ON i.id = c.issue_id
WHERE i.issue_type IN ('agent', 'rig', 'role', 'message');

-- Delete originals, children first (FK-safe order).
DELETE c FROM comments c JOIN issues i ON i.id = c.issue_id
WHERE i.issue_type IN ('agent', 'rig', 'role', 'message');

DELETE e FROM events e JOIN issues i ON i.id = e.issue_id
WHERE i.issue_type IN ('agent', 'rig', 'role', 'message');

DELETE d FROM dependencies d JOIN issues i ON i.id = d.issue_id
WHERE i.issue_type IN ('agent', 'rig', 'role', 'message');

DELETE l FROM labels l JOIN issues i ON i.id = l.issue_id
WHERE i.issue_type IN ('agent', 'rig', 'role', 'message');

DELETE FROM issues
WHERE issue_type IN ('agent', 'rig', 'role', 'message');
