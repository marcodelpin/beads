# OpenTelemetry Data Model

Complete schema of all telemetry events emitted by Beads. Each event consists of:

1. **Log record** (→ any OTLP v1.x+ backend, defaults to VictoriaLogs) with full structured attributes
2. **Metric counter** (→ any OTLP v1.x+ backend, defaults to VictoriaMetrics) for aggregation

All events automatically carry \`bd.command\`, \`bd.version\`, \`bd.actor\` from command context for correlation.

---

## Event Index

| Event | Category | Status |
|-------|----------|--------|
| \`bd.command\` | CLI | ✅ Main |
| \`storage.*\` | Storage | ✅ Main |
| \`dolt.*\` | Dolt Backend | ✅ Main |
| \`doltserver.*\` | Server Lifecycle | ✅ Main |
| \`hook.exec\` | Hooks | ✅ Main |

---

## 1. Identity Hierarchy

### 1.1 Instance

The outermost grouping. Derived at command startup time from the machine hostname and the working directory.

| Attribute | Type | Description |
|---|---|---|
| \`host\` | string | System hostname |
| \`os\` | string | System OS information |

### 1.2 Command

Each \`bd\` command execution generates a span with full context.

| Attribute | Type | Source |
|---|---|---|
| \`bd.command\` | string | Subcommand name (\`create\`, \`list\`, \`show\`, etc.) |
| \`bd.version\` | string | bd version (e.g., \`"0.9.3"\`) |
| \`bd.args\` | string | Full argument list |
| \`bd.actor\` | string | Actor identity (from git config or env) |

---

## 2. CLI Command Events

### \`bd.command\`

Emitted once per \`bd\` subcommand execution. Anchors all subsequent events for that command.

| Attribute | Type | Description |
|---|---|---|
| \`bd.command\` | string | Subcommand name |
| \`bd.version\` | string | bd version |
| \`bd.args\` | string | Full arguments passed to command |
| \`bd.actor\` | string | Actor identity |
| \`duration_ms\` | float | Wall-clock duration in milliseconds |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

---

## 3. Storage Events

### \`storage.CreateIssue\`

Emitted when an issue is created.

| Attribute | Type | Description |
|---|---|---|
| \`db.operation\` | string | \`"CreateIssue"\` |
| \`bd.issue.id\` | string | Newly created issue ID |
| \`bd.issue.type\` | string | Issue type (\`task\`, \`epic\`, \`merge-request\`, etc.) |
| \`bd.actor\` | string | Actor creating the issue |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`storage.UpdateIssue\`

Emitted when an issue is updated.

| Attribute | Type | Description |
|---|---|---|
| \`db.operation\` | string | \`"UpdateIssue"\` |
| \`bd.issue.id\` | string | Issue ID being updated |
| \`bd.update.count\` | int | Number of fields being updated |
| \`bd.actor\` | string | Actor updating the issue |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`storage.GetIssue\`

Emitted when an issue is retrieved.

| Attribute | Type | Description |
|---|---|---|
| \`db.operation\` | string | \`"GetIssue"\` |
| \`bd.issue.id\` | string | Issue ID being retrieved |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`storage.SearchIssues\`

Emitted when searching for issues.

| Attribute | Type | Description |
|---|---|---|
| \`db.operation\` | string | \`"SearchIssues"\` |
| \`bd.query\` | string | Search query string |
| \`bd.result.count\` | int | Number of results returned |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`storage.GetReadyWork\`

Emitted when querying for ready work.

| Attribute | Type | Description |
|---|---|---|
| \`db.operation\` | string | \`"GetReadyWork"\` |
| \`bd.result.count\` | int | Number of ready issues returned |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`storage.GetBlockedIssues\`

Emitted when querying for blocked issues.

| Attribute | Type | Description |
|---|---|---|
| \`db.operation\` | string | \`"GetBlockedIssues"\` |
| \`bd.result.count\` | int | Number of blocked issues returned |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`storage.RunInTransaction\`

Emitted when executing a transaction.

| Attribute | Type | Description |
|---|---|---|
| \`db.operation\` | string | \`"RunInTransaction"\` |
| \`db.commit_msg\` | string | Commit message |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

---

## 4. Dolt Backend Events

### \`dolt.query\`

Emitted for each SQL query executed against Dolt.

| Attribute | Type | Description |
|---|---|---|
| \`db.operation\` | string | SQL query type (truncated) |
| \`db.table\` | string | Table being queried (when determinable) |
| \`duration_ms\` | float | Query execution time in milliseconds |
| \`rows_affected\` | int | Number of rows affected (for INSERT/UPDATE/DELETE) |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`dolt.lock_wait\`

Emitted when waiting for Dolt access lock.

| Attribute | Type | Description |
|---|---|---|
| \`dolt_lock_type\` | string | Lock type (\`dolt-access.lock\`, \`noms_lock\`) |
| \`wait_ms\` | float | Wait time in milliseconds |
| \`status\` | string | \`"ok"\` · \`"error"\` |

### \`dolt.commit\`

Emitted for DOLT_COMMIT operations.

| Attribute | Type | Description |
|---|---|---|
| \`commit_msg\` | string | Commit message |
| \`duration_ms\` | float | Commit operation duration in milliseconds |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`dolt.push\`

Emitted for DOLT_PUSH operations.

| Attribute | Type | Description |
|---|---|---|
| \`branch\` | string | Branch being pushed |
| \`remote_url\` | string | Remote URL |
| \`changes_count\` | int | Number of commits pushed |
| \`duration_ms\` | float | Push duration in milliseconds |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`dolt.pull\`

Emitted for DOLT_PULL operations.

| Attribute | Type | Description |
|---|---|---|
| \`branch\` | string | Branch being pulled |
| \`remote_url\` | string | Remote URL |
| \`changes_count\` | int | Number of commits pulled |
| \`duration_ms\` | float | Pull duration in milliseconds |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`dolt.merge\`

Emitted for DOLT_MERGE operations.

| Attribute | Type | Description |
|---|---|---|
| \`strategy\` | string | Merge strategy (\`ours\`, \`theirs\`, \`union\`) |
| \`conflict_count\` | int | Number of conflicts encountered |
| \`duration_ms\` | float | Merge duration in milliseconds |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`dolt.branch\`

Emitted for DOLT_BRANCH operations.

| Attribute | Type | Description |
|---|---|---|
| \`branch_name\` | string | Branch name |
| \`start_ref\` | string | Starting reference (when applicable) |
| \`duration_ms\` | float | Branch operation duration in milliseconds |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`dolt.checkout\`

Emitted for DOLT_CHECKOUT operations.

| Attribute | Type | Description |
|---|---|---|
| \`ref\` | string | Reference being checked out |
| \`duration_ms\` | float | Checkout duration in milliseconds |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

---

## 5. Dolt Server Events

### \`doltserver.start\`

Emitted when Dolt sql-server is started.

| Attribute | Type | Description |
|---|---|---|
| \`port\` | int | Port server is listening on |
| \`data_dir\` | string | Path to Dolt data directory |
| \`pid\` | int | Process ID of server |
| \`source\` | string | Port source (\`hash\` derived, \`config\` explicit) |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`doltserver.stop\`

Emitted when Dolt sql-server is stopped.

| Attribute | Type | Description |
|---|---|---|
| \`pid\` | int | Process ID of stopped server |
| \`reason\` | string | Stop reason (\`graceful\`, \`forced\`, \`idle_timeout\`, \`crash\`) |
| \`uptime_ms\` | float | Server uptime in milliseconds (when available) |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`doltserver.port_allocated\`

Emitted when port is allocated for Dolt server.

| Attribute | Type | Description |
|---|---|---|
| \`port\` | int | Allocated port number |
| \`source\` | string | Port source (\`hash\` derived, \`config\` explicit) |
| \`status\` | string | \`"ok"\` · \`"error"\` |

### \`doltserver.port_reclaimed\`

Emitted when adopting or cleaning up orphan Dolt server.

| Attribute | Type | Description |
|---|---|---|
| \`port\` | int | Port being reclaimed |
| \`adopted_pid\` | int | PID of adopted server (0 if none) |
| \`action\` | string | Action taken (\`adopt\`, \`kill_orphan\`) |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

### \`doltserver.idle_timeout\`

Emitted when idle monitor shuts down server.

| Attribute | Type | Description |
|---|---|---|
| \`idle_duration_ms\` | float | Idle time before shutdown |
| \`timeout_config\` | string | Configured timeout value |
| \`status\` | string | \`"ok"\` |

### \`doltserver.restart\`

Emitted when idle monitor restarts crashed server.

| Attribute | Type | Description |
|---|---|---|
| \`crash_detected\` | bool | Whether restart was due to detected crash |
| \`last_activity_age_ms\` | float | Time since last activity |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

---

## 6. Hooks Events

### \`hook.exec\`

Emitted for hook execution.

| Attribute | Type | Description |
|---|---|---|
| \`hook.event\` | string | Event type (\`create\`, \`update\`, \`close\`, \`delete\`, etc.) |
| \`hook.path\` | string | Absolute path to hook script |
| \`bd.issue_id\` | string | ID of triggering issue (when applicable) |
| \`hook.stdout\` | string | Script standard output (truncated to 1024 bytes) |
| \`hook.stderr\` | string | Script error output (truncated to 1024 bytes) |
| \`output\` | string | Output text (\`stdout\` or \`stderr\`) |
| \`bytes\` | int | Original output size before truncation |
| \`duration_ms\` | float | Hook execution duration in milliseconds |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

---

## 7. AI Events

Emitted by the compaction engine (`bd compact`) and duplicate detection (`bd duplicates --ai`). Both use the Anthropic SDK directly via `ANTHROPIC_API_KEY`.

### \`anthropic.messages.new\`

One span per Anthropic API call. The \`bd.ai.operation\` attribute distinguishes the two callers.

| Attribute | Type | Description |
|---|---|---|
| \`bd.ai.model\` | string | Model used (e.g. \`"claude-haiku-4-5"\`) |
| \`bd.ai.operation\` | string | \`"compact"\` or \`"find_duplicates"\` |
| \`bd.ai.input_tokens\` | int | Input tokens consumed |
| \`bd.ai.output_tokens\` | int | Output tokens generated |
| \`bd.ai.attempts\` | int | Number of attempts (including retries) |
| \`bd.ai.batch_size\` | int | Candidate pairs evaluated (find_duplicates only) |
| \`status\` | string | \`"ok"\` · \`"error"\` |
| \`error\` | string | Error message (empty when status=\`"ok"\`) |

**Retry policy**: exponential backoff, up to 3 attempts, on HTTP 429 and 5xx errors.

---

## 8. Metrics Reference

| Metric | Type | Labels | Status |
|--------|------|--------|--------|
| \`bd_storage_operations_total\` | Counter | \`db.operation\`, \`status\` | ✅ Main |
| \`bd_storage_operation_duration_ms\` | Histogram | \`db.operation\` | ✅ Main |
| \`bd_storage_errors_total\` | Counter | \`db.operation\`, \`error_type\` | ✅ Main |
| \`bd_issue_count\` | Gauge | \`status\` | ✅ Main |
| \`bd_db_retry_count_total\` | Counter | — | ✅ Main |
| \`bd_db_lock_wait_ms\` | Histogram | \`dolt_lock_type\` | ✅ Main |
| \`doltserver_start_total\` | Counter | \`status\`, \`source\` | ✅ Main |
| \`doltserver_stop_total\` | Counter | \`reason\`, \`status\` | ✅ Main |
| \`doltserver_uptime_ms\` | Histogram | — | ✅ Main |
| \`hook_exec_duration_ms\` | Histogram | \`hook.event\`, \`status\` | ✅ Main |
| \`hook_exec_total\` | Counter | \`hook.event\`, \`status\` | ✅ Main |
| \`bd.ai.input_tokens\` | Counter | \`bd.ai.model\` | ✅ Main |
| \`bd.ai.output_tokens\` | Counter | \`bd.ai.model\` | ✅ Main |
| \`bd.ai.request.duration\` | Histogram (ms) | \`bd.ai.model\` | ✅ Main |

---

## 9. Recommended Indexed Attributes

\`\`\`
host, os, bd.command, bd.version, bd.actor, db.operation, db.table,
bd.issue.id, bd.issue.type, dolt_lock_type,
hook.event, hook.path, branch_name, bd.ai.model, bd.ai.operation
\`\`\`

---

## 10. Status Field Semantics

All events include a \`status\` field:

| Value | Meaning |
|-------|---------|
| "ok" | Operation completed successfully |
| "error" | Operation failed |

When status is "error", the \`error\` field contains the error message. When status is "ok", \`error\` is an empty string.

---

## 11. Environment Variables

| Variable | Set by | Description |
|-----------|----------|-------------|
| \`BD_OTEL_METRICS_URL\` | Operator | OTLP metrics endpoint URL |
| \`BD_OTEL_LOGS_URL\` | Operator | OTLP logs endpoint URL |
| \`BD_OTEL_STDOUT\` | Operator | Set to \`true\` to enable stdout traces |

\`OTEL_RESOURCE_ATTRIBUTES\` can also be used to set custom resource attributes that will be attached to all spans and metrics.

---

## 12. Backend Compatibility

This data model is **backend-agnostic** — any OTLP v1.x+ compatible backend can consume these events:

- **VictoriaMetrics** — Default for local development. Override with \`BD_OTEL_METRICS_URL\` to use any OTLP-compatible backend.
- **VictoriaLogs** — Reserved for future log export. Override with \`BD_OTEL_LOGS_URL\`.
- **Prometheus** — Via remote_write receiver
- **Grafana Mimir** — Via write endpoint
- **OpenTelemetry Collector** — Universal forwarder to any backend

The schema uses standard OpenTelemetry Protocol (OTLP) with protobuf encoding, which is universally supported.

---

## 13. Dolt-Specific Telemetry Opportunities

### Available Dolt System Tables

Dolt maintains several system tables that can be queried for telemetry:

| Table | Telemetry Use Case |
|--------|-------------------|
| \`dolt_log\` | Commit rate, author analysis, commit frequency |
| \`dolt_status\` | Working set size, uncommitted changes tracking |
| \`dolt_diff\` | Cell-level change analysis, conflict detection |
| \`dolt_branches\` | Branch proliferation monitoring |
| \`dolt_conflicts\` | Merge conflict rate by operation |

### Sample Queries for Dolt Telemetry

**Commit frequency analysis:**
\`\`\`sql
SELECT
    DATE_FORMAT(commit_date, '%Y-%m') as month,
    COUNT(*) as commits
FROM dolt_log
GROUP BY month
ORDER BY month DESC;
\`\`\`

**Working set size tracking:**
\`\`\`sql
SELECT
    COUNT(*) as staged_changes,
    SUM(CASE WHEN staged = 1 THEN 1 ELSE 0 END) as added,
    SUM(CASE WHEN staged = 0 THEN 1 ELSE 0 END) as removed
FROM dolt_status;
\`\`\`

**Branch proliferation detection:**
\`\`\`sql
SELECT
    COUNT(*) as branch_count,
    MIN(commit_date) as oldest,
    MAX(commit_date) as newest
FROM dolt_branches;
\`\`\`

**Conflict analysis:**
\`\`\`sql
SELECT
    COUNT(*) as conflict_count,
    COUNT(DISTINCT table_name) as tables_affected
FROM dolt_conflicts;
\`\`\`

### Future Dolt Telemetry Integration

Consider adding periodic queries to collect metrics from Dolt system tables:

| Metric | Query | Collection Frequency |
|--------|--------|-------------------|
| \`bd_dolt_commits_per_hour\` | \`dolt_log\` GROUP BY hour | Every 5 minutes |
| \`bd_dolt_working_set_size\` | \`dolt_status\` COUNT(*) | Every 1 minute |
| \`bd_dolt_branch_count\` | \`dolt_branches\` COUNT(*) | Every 5 minutes |
| \`bd_dolt_conflicts_per_day\` | \`dolt_conflicts\` COUNT(*) | Every hour |
