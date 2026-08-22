-- label_namespace_locks: a per-(issue_id, namespace) CAS row mirroring the
-- issues/wisps row_lock mechanism (issueops/lease.go freshRowLock), closing
-- the exclusive-label-namespace race the review of upstream PR #4757
-- (bd-7u5ki) found.
--
-- Two concurrent writers adding DIFFERENT labels into the SAME exclusive
-- namespace on the SAME issue insert DIFFERENT primary keys in the labels
-- table (issue_id, label), so Dolt's cell-level merge accepts BOTH as
-- independent new rows instead of conflicting - the application-level
-- check-then-insert in the old checkExclusiveLabelInTx sees no violation for
-- either writer, because neither has committed yet when the other reads.
--
-- Rewriting THIS row's row_lock cell, inside the SAME transaction as the
-- label read and write, forces both writers to touch the SAME cell: one
-- commits, the other collides on it and gets Dolt's 1213/1205 serialization
-- error, which withRetryTx/RunTx replay against a FRESH read that now sees
-- the winner's label - so the retried writer's own exclusivity check
-- correctly rejects it. This is exactly the technique row_lock already uses
-- for claim/close races, scoped to (issue_id, namespace) rather than the
-- whole issue row, so an unrelated status/assignee update never collides
-- with a label add and vice versa (and the reverse: this lock never collides
-- with a claim/close, since it never touches the issues/wisps row itself).
--
-- Dolt-ignored (like leases, 0055): the lock's only job is to WIN OR LOSE a
-- write-write race on the node that granted it, not to record history or
-- replicate. An ignored table still participates in Dolt's cell-level merge
-- conflict detection at SQL-transaction-commit time - leases already relies
-- on exactly this for the heartbeat-vs-reclaim race - it just never becomes
-- part of a dolt_commit.
--
-- No FK to issues(id): issue_id here can also be a wisp id, and wisps live
-- in a separate table this lock does not need to join against. A lock row
-- outliving its issue/wisp is a few stray bytes in a table nothing else
-- reads by issue existence, and it is reused (not re-inserted) the next time
-- the same (issue_id, namespace) pair is written.
REPLACE INTO dolt_ignore VALUES ('label_namespace_locks', true);
CREATE TABLE IF NOT EXISTS label_namespace_locks (
    issue_id VARCHAR(255) NOT NULL,
    namespace VARCHAR(255) NOT NULL,
    row_lock BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (issue_id, namespace)
);
