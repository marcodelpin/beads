INSERT IGNORE INTO issues SELECT * FROM wisps
WHERE issue_type IN ('agent', 'rig', 'role', 'message');

UPDATE issues SET ephemeral = 0
WHERE issue_type IN ('agent', 'rig', 'role', 'message');

INSERT IGNORE INTO labels (issue_id, label)
SELECT issue_id, label FROM wisp_labels wl
WHERE EXISTS (SELECT 1 FROM issues i WHERE i.id = wl.issue_id
              AND i.issue_type IN ('agent', 'rig', 'role', 'message'));

INSERT IGNORE INTO dependencies (issue_id, depends_on_id, type, created_at, created_by, metadata, thread_id)
SELECT issue_id, depends_on_id, type, created_at, created_by, metadata, thread_id FROM wisp_dependencies wd
WHERE EXISTS (SELECT 1 FROM issues i WHERE i.id = wd.issue_id
              AND i.issue_type IN ('agent', 'rig', 'role', 'message'));

INSERT IGNORE INTO events (id, issue_id, event_type, actor, old_value, new_value, comment, created_at)
SELECT id, issue_id, event_type, actor, old_value, new_value, comment, created_at FROM wisp_events we
WHERE EXISTS (SELECT 1 FROM issues i WHERE i.id = we.issue_id
              AND i.issue_type IN ('agent', 'rig', 'role', 'message'));

INSERT IGNORE INTO comments (id, issue_id, author, text, created_at)
SELECT id, issue_id, author, text, created_at FROM wisp_comments wc
WHERE EXISTS (SELECT 1 FROM issues i WHERE i.id = wc.issue_id
              AND i.issue_type IN ('agent', 'rig', 'role', 'message'));

DELETE FROM wisp_comments WHERE issue_id IN (SELECT id FROM issues WHERE issue_type IN ('agent', 'rig', 'role', 'message'));
DELETE FROM wisp_events WHERE issue_id IN (SELECT id FROM issues WHERE issue_type IN ('agent', 'rig', 'role', 'message'));
DELETE FROM wisp_dependencies WHERE issue_id IN (SELECT id FROM issues WHERE issue_type IN ('agent', 'rig', 'role', 'message'));
DELETE FROM wisp_labels WHERE issue_id IN (SELECT id FROM issues WHERE issue_type IN ('agent', 'rig', 'role', 'message'));
DELETE FROM wisps WHERE issue_type IN ('agent', 'rig', 'role', 'message');
