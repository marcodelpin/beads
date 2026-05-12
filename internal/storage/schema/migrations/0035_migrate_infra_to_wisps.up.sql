-- bda-y3o: enumerate columns explicitly so this INSERT is robust to schema drift.
-- Earlier `SELECT *` failed on legacy DBs that carry pre-migration Go schema
-- residual columns (e.g. crystallizes, quality_score, no_history) on `issues`
-- but not on `wisps`. Dolt validates the column count at parse time, so the
-- statement fails even when WHERE clause matches 0 rows — blocking 0036+ from
-- running. The list below is the canonical 0020-wisps intersection with 0001-
-- issues (51 columns, identical sets at source level).
INSERT IGNORE INTO wisps (
    id, content_hash, title, description, design, acceptance_criteria, notes,
    status, priority, issue_type, assignee, estimated_minutes, created_at,
    created_by, owner, updated_at, closed_at, closed_by_session, external_ref,
    spec_id, compaction_level, compacted_at, compacted_at_commit, original_size,
    sender, ephemeral, wisp_type, pinned, is_template, mol_type, work_type,
    source_system, metadata, source_repo, close_reason, event_kind, actor,
    target, payload, await_type, await_id, timeout_ns, waiters, hook_bead,
    role_bead, agent_state, last_activity, role_type, rig, due_at, defer_until
)
SELECT
    id, content_hash, title, description, design, acceptance_criteria, notes,
    status, priority, issue_type, assignee, estimated_minutes, created_at,
    created_by, owner, updated_at, closed_at, closed_by_session, external_ref,
    spec_id, compaction_level, compacted_at, compacted_at_commit, original_size,
    sender, ephemeral, wisp_type, pinned, is_template, mol_type, work_type,
    source_system, metadata, source_repo, close_reason, event_kind, actor,
    target, payload, await_type, await_id, timeout_ns, waiters, hook_bead,
    role_bead, agent_state, last_activity, role_type, rig, due_at, defer_until
FROM issues
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
