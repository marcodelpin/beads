# OpenTelemetry Architecture

## Overview

Beads uses OpenTelemetry (OTel) for structured observability of all database operations, CLI commands, and Dolt version control. Telemetry is emitted via standard OTLP HTTP to any compatible backend (metrics, traces).

**Backend-agnostic design**: The system emits standard OpenTelemetry Protocol (OTLP) — any OTLP v1.x+ compatible backend can consume it. You are **not obligated** to use VictoriaMetrics/VictoriaLogs; these are simply development defaults.

**Best-effort design**: Telemetry initialization errors are returned but do not affect normal `bd` operation. The system remains functional even when telemetry is unavailable.

---

## Implementation Status

### Core Telemetry (Implemented ✅)

| Feature | Status | Notes |
|---------|--------|-------|
| Core OTel initialization | ✅ Implemented | `telemetry.Init()`, providers setup |
| Metrics export (counters) | ✅ Implemented | Storage operations, Dolt operations |
| Metrics export (histograms) | ✅ Implemented | Operation durations, query latency |
| Traces (stdout only) | ✅ Implemented | OTLP traces via stdout (dev mode) |
| Storage layer instrumentation | ✅ Implemented | `InstrumentedStorage` wrapper for all storage ops |
| Command lifecycle tracing | ✅ Implemented | Per-command spans with arguments |
| Dolt version control tracing | ✅ Implemented | Commit, push, pull, merge operations |

### Dolt Backend Telemetry (Implemented ✅)

| Feature | Status | Notes |
|---------|--------|-------|
| SQL query tracing | ✅ Implemented | All Dolt queries wrapped with spans |
| Dolt lock wait timing | ✅ Implemented | `bd_db_lock_wait_ms` histogram |
| Dolt retry counting | ✅ Implemented | `bd_db_retry_count_total` counter |
| Auto-commit tracking | ✅ Implemented | Per-command auto-commit events |
| Working set flush tracking | ✅ Implemented | Flush on shutdown/signal |

### Server Lifecycle Telemetry (Implemented ✅)

| Feature | Status | Notes |
|---------|--------|-------|
| Server start/stop events | ✅ Implemented | Via `doltserver` package spans |
| Server health monitoring | ✅ Partial | Connection tests, port availability |
| Idle monitor tracking | ✅ Implemented | Activity file, idle shutdown |

---

## Roadmap

Current coverage: ~40% of the codebase. Below is a prioritized plan based on operational value vs. implementation effort.

### Tier 1 — High value, moderate effort

#### Tracker integrations (`internal/linear/`, `internal/jira/`, `internal/gitlab/`)

External API calls are currently a black box. No visibility into latency, rate-limiting, or sync volume.

New metrics:
- `bd_tracker_api_calls_total` (Counter) — by `tracker`, `method`, `status`
- `bd_tracker_api_latency_ms` (Histogram) — by `tracker`, `method`
- `bd_tracker_errors_total` (Counter) — by `tracker`, `error_type`
- `bd_tracker_issues_synced_total` (Counter) — by `tracker`, `direction`

New spans: `tracker.<name>.pull_issues`, `tracker.<name>.push_issue`, `tracker.<name>.resolve_state`

#### Git operations (`internal/git/`)

Git push/pull can dominate wall-clock time but is currently invisible.

New metrics:
- `bd_git_operation_duration_ms` (Histogram) — by `operation`, `status`
- `bd_git_errors_total` (Counter) — by `operation`, `error_type`

New spans: `git.clone`, `git.pull`, `git.push`, `git.commit`, `git.merge`

#### Dolt server lifecycle (`internal/doltserver/`)

Server crashes and restarts are silent. No alerting possible.

New metrics:
- `bd_doltserver_status` (Gauge, 1=running/0=stopped)
- `bd_doltserver_startup_ms` (Histogram)
- `bd_doltserver_restarts_total` (Counter)
- `bd_doltserver_errors_total` (Counter) — by `error_type`

New spans: `doltserver.start`, `doltserver.stop`

---

### Tier 2 — Medium value, low effort

#### Query engine (`internal/query/`)

Distinguishes whether slowness is client-side (parsing/compilation) or DB-side.

New spans: `query.parse`, `query.compile`
New metrics: `bd_query_duration_ms` (Histogram), `bd_query_parse_errors_total` (Counter)

#### Validation engine (`internal/validation/`)

Data integrity errors are currently silent until they surface as user-visible failures.

New spans: `validation.check_dependencies`, `validation.check_schema`
New metrics: `bd_validation_errors_total` (Counter) — by `error_type`

#### Dolt system table polling

Periodic SQL queries against Dolt system tables to surface metrics unavailable via OTLP (Dolt has no native OTel export):

| Metric | Source | Frequency |
|--------|--------|-----------|
| `bd_dolt_commits_per_hour` | `dolt_log` GROUP BY hour | 5 min |
| `bd_dolt_working_set_size` | `dolt_status` COUNT(*) | 1 min |
| `bd_dolt_branch_count` | `dolt_branches` COUNT(*) | 5 min |
| `bd_dolt_conflict_count` | `dolt_conflicts` COUNT(*) | 5 min |

---

### Tier 3 — Low priority / future

- **Command-level sub-spans**: Instrument validation vs. DB vs. render breakdown per command (`bd create`, `bd list`, `bd compact`, etc.)
- **Molecules & recipes**: `molecule.create`, `recipe.execute` spans
- **Hook duration metrics**: Currently only spans, no histogram for aggregation
- **OTel test suite**: Integration tests that verify telemetry output (currently none)

---

## Components

### 1. Initialization (`internal/telemetry/telemetry.go`)

The `telemetry.Init()` function sets up OTel providers on process startup:

```go
provider, err := telemetry.Init(ctx, "bd", version)
if err != nil {
    // Log and continue — telemetry is best-effort
}
defer provider.Shutdown(ctx)
```

**Providers:**
- **Metrics**: Any OTLP-compatible metrics backend via `otlpmetrichttp` exporter
- **Traces**: Stdout only (local debug). No remote trace backend in default stack.

**Default endpoints** (when `BD_OTEL_METRICS_URL` is not set):
- Metrics: `http://localhost:8428/opentelemetry/api/v1/push`
- Traces: stdout (via `BD_OTEL_STDOUT=true`)

> **Note**: These defaults target VictoriaMetrics for local development convenience. Beads uses standard OTLP — you can override endpoints to use any OTLP v1.x+ compatible backend (Prometheus, Grafana Mimir, Datadog, New Relic, Grafana Cloud, Loki, OpenTelemetry Collector, etc.).

**OTLP Compatibility**:
- Uses standard OpenTelemetry Protocol (OTLP) over HTTP
- Protobuf encoding (VictoriaMetrics, Prometheus, and others accept this)
- Compatible with any backend that supports OTLP v1.x+

**Resource attributes** (set at init time):
- `service.name`: "bd"
- `service.version`: bd binary version
- `host`: system hostname
- `os`: system OS info

**Custom resource attributes** (via `OTEL_RESOURCE_ATTRIBUTES` env var or `BD_ACTOR`):
- `bd.actor`: Actor identity (from git config or env)
- `bd.command`: Current command name
- `bd.args`: Full arguments passed to command

---

### 2. Storage Instrumentation (`internal/telemetry/storage.go`)

The `InstrumentedStorage` wraps `storage.Storage` with OTel tracing and metrics:
- Every storage method gets a span
- Counters track operation counts
- Histograms track operation duration
- Error counters track failures

```go
func WrapStorage(s storage.Storage) storage.Storage {
    if !Enabled() {
        return s  // Zero overhead when telemetry disabled
    }
    // Wrap with instrumentation
    return &InstrumentedStorage{inner: s, tracer, ops, dur, errs, issueGauge}
}
```

**Instrumented Storage Operations:**
- Issue CRUD: `CreateIssue`, `GetIssue`, `UpdateIssue`, `CloseIssue`, `DeleteIssue`
- Dependencies: `AddDependency`, `RemoveDependency`, `GetDependencies`
- Labels: `AddLabel`, `RemoveLabel`, `GetLabels`
- Queries: `SearchIssues`, `GetReadyWork`, `GetBlockedIssues`
- Statistics: `GetStatistics` (also emits gauge of issue counts by status)
- Transactions: `RunInTransaction`

---

### 3. Dolt Backend Telemetry (`internal/storage/dolt/store.go`)

Dolt storage layer emits metrics for:
- `bd_db_retry_count_total`: SQL retries in server mode
- `bd_db_lock_wait_ms`: Wait time to acquire `dolt-access.lock`
- SQL query spans: Each Dolt query via `queryContext()` wrapper
- Dolt version control spans: `DOLT_COMMIT`, `DOLT_PUSH`, `DOLT_PULL`, `DOLT_MERGE`

**Dolt Lock Wait Tracking:**
```go
func (s *DoltStore) queryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
    start := time.Now()
    // Acquire lock
    err := s.lockFile.FlockExclusive(lockF)
    // Record wait time if metrics enabled
    if s.metrics != nil {
        waitMs := float64(time.Since(start).Milliseconds())
        s.metrics.LockWaitDuration.Record(ctx, waitMs)
    }
    // Execute query
    rows, err := s.db.QueryContext(ctx, query, args...)
    return rows, err
}
```

---

### 4. Dolt Server Management (`internal/doltserver/doltserver.go`)

Server lifecycle operations are traced:
- Server start/stop via `Start()`, `Stop()` functions
- Port allocation and availability checking
- Orphan server detection and cleanup
- Idle monitor lifecycle

**Idle Monitor Telemetry:**
The idle monitor (`RunIdleMonitor`) tracks:
- Activity file timestamp updates
- Idle duration before shutdown
- Server crash detection and restart

**Activity File Tracking:**
```go
func touchActivity(beadsDir string) {
    // Update activity timestamp
    os.WriteFile(activityPath(beadsDir),
        []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
}
```

---

### 5. Dolt Version Control Telemetry (`internal/storage/dolt/versioned.go`)

Version control operations emit spans:
- `History`: Query complete version history for an issue
- `AsOf`: Query state at specific commit or branch
- `Diff`: Cell-level diff between two commits
- `ListBranches`: Enumerate all branches
- `GetCurrentCommit`: Get HEAD commit hash
- `GetConflicts`: Check for merge conflicts

---

## Environment Variables

### Beads-Level Variables

| Variable | Set by | Description |
|-----------|----------|-------------|
| `BD_OTEL_METRICS_URL` | Operator | OTLP metrics endpoint (default: localhost:8428) |
| `BD_OTEL_LOGS_URL` | Operator | OTLP logs endpoint (reserved for future log export) |
| `BD_OTEL_STDOUT` | Operator | **Opt-in**: Write spans and metrics to stderr (dev/debug). Also activates telemetry. |

### Context Variables

| Variable | Source | Used By |
|-----------|--------|----------|
| `BD_ACTOR` | Git config / env var | Actor identity for audit trails |
| `BD_NAME` | Environment | Binary name override (for multi-instance setups) |
| `OTEL_RESOURCE_ATTRIBUTES` | Operator | Custom resource attributes for all spans |

### Dolt-Specific Variables (See DOLT.md)

| Variable | Purpose |
|-----------|----------|
| `BEADS_DOLT_PASSWORD` | Server mode password |
| `BEADS_DOLT_SERVER_MODE` | Enable server mode |
| `BEADS_DOLT_SERVER_HOST` | Server host (default: 127.0.0.1) |
| `BEADS_DOLT_SERVER_PORT` | Server port (default: 3307 or derived) |
| `BEADS_DOLT_SERVER_TLS` | Enable TLS for server connections |
| `BEADS_DOLT_SERVER_USER` | MySQL connection user |
| `DOLT_REMOTE_USER` | Push/pull auth user |
| `DOLT_REMOTE_PASSWORD` | Push/pull auth password |

> **Note**: Dolt-specific configuration variables are documented in [DOLT.md](../../DOLT.md) and are out of scope for OTEL design documentation.

---

## Event Types

### CLI Command Events

| Event | Trigger | Key Attributes |
|-------|---------|----------------|
| `bd.command.<name>` | Each `bd` subcommand execution | `bd.command`, `bd.version`, `bd.args`, `bd.actor` |
| `bd.command.duration_ms` | Command execution time | `bd.command` |

### Storage Events

| Event | Trigger | Key Attributes |
|-------|---------|----------------|
| `storage.CreateIssue` | Issue creation | `bd.issue.id`, `bd.issue.type`, `bd.actor` |
| `storage.UpdateIssue` | Issue update | `bd.issue.id`, `bd.update.count`, `bd.actor` |
| `storage.GetIssue` | Issue lookup | `bd.issue.id` |
| `storage.SearchIssues` | Issue search | `bd.query`, `bd.result.count` |
| `storage.GetReadyWork` | Ready work query | `bd.result.count` |
| `storage.GetBlockedIssues` | Blocked issues query | `bd.result.count` |
| `storage.RunInTransaction` | Transaction execution | `db.commit_msg` |

### Dolt Events

| Event | Trigger | Key Attributes |
|-------|---------|----------------|
| `dolt.query` | Each SQL query | `db.operation` |
| `dolt.lock_wait` | Waiting for dolt-access.lock | `dolt_lock_type` |
| `dolt.commit` | DOLT_COMMIT operation | `commit_msg` |
| `dolt.push` | DOLT_PUSH operation | `remote_url`, `branch` |
| `dolt.pull` | DOLT_PULL operation | `remote_url`, `branch` |
| `dolt.merge` | DOLT_MERGE operation | `strategy`, `conflict_count` |
| `dolt.branch` | DOLT_BRANCH operation | `branch_name` |
| `dolt.checkout` | DOLT_CHECKOUT operation | `ref` |

### Dolt Server Events

| Event | Trigger | Key Attributes |
|-------|---------|----------------|
| `doltserver.start` | Server start | `port`, `data_dir`, `pid` |
| `doltserver.stop` | Server stop (graceful or forced) | `pid`, `reason` |
| `doltserver.port_allocated` | Port assignment (hash-derived or explicit) | `port`, `source` (hash/config) |
| `doltserver.port_reclaimed` | Orphan server cleanup | `adopted_pid`, `port` |
| `doltserver.idle_timeout` | Idle shutdown | `idle_duration_ms`, `timeout_config` |
| `doltserver.restart` | Server restart by idle monitor | `crash_detected` |

### Hooks Events

| Event | Trigger | Key Attributes |
|-------|---------|----------------|
| `hook.exec` | Hook execution | `hook.event`, `hook.path`, `bd.issue_id` |

---

## Monitoring Gaps

### Currently Monitored ✅

| Area | Coverage |
|-------|----------|
| Storage operations | Full (all CRUD, queries, transactions) |
| CLI command lifecycle | Full (all commands with arguments and duration) |
| Dolt SQL queries | Full (all queries via queryContext wrapper) |
| Dolt lock contention | Full (lock wait time histogram) |
| Dolt version control | Full (commit, push, pull, merge, branch) |
| Dolt server lifecycle | Full (start, stop, idle monitor) |

### Not Currently Monitored ❌

| Area | Notes | Operational Impact |
|-------|-------|-------------------|
| **Dolt server metrics** | Dolt has internal metrics but not exposed to OTel | Cannot monitor server health, connection count, query load |
| **Working set size** | Uncommitted changes count unknown | Cannot detect batch mode accumulation |
| **Database size growth** | Dolt database size not tracked | Cannot plan capacity or detect bloat |
| **Branch proliferation** | Branch count not exposed | Cannot detect cleanup needed |
| **Remote sync bandwidth** | Bytes transferred not tracked | Cannot monitor network usage or cost |
| **Query execution plans** | EXPLAIN ANALYZE not captured | Cannot identify slow queries |
| **Conflict rate by operation** | Dolt merge conflicts counted but not categorized | Cannot detect problematic operations |
| **Hook execution time** | Hook spans lack duration metrics | Cannot detect hook regressions |
| **Connection pool utilization** | Active/idle counts not tracked | Cannot tune connection pool sizing |

---

## Queries

### Metrics (Any OTLP-compatible backend)

**Total counts by operation:**
```promql
sum(rate(bd_storage_operations_total[5m])) by (db.operation)
sum(rate(bd_db_retry_count_total[5m]))
```

**Latency distributions:**
```promql
histogram_quantile(0.50, bd_storage_operation_duration_ms) by (db.operation)
histogram_quantile(0.95, bd_storage_operation_duration_ms) by (db.operation)
histogram_quantile(0.99, bd_storage_operation_duration_ms) by (db.operation)
```

**Issue counts by status:**
```promql
bd_issue_count{status="open"}
bd_issue_count{status="in_progress"}
bd_issue_count{status="closed"}
bd_issue_count{status="deferred"}
```

**Dolt lock contention:**
```promql
histogram_quantile(0.95, bd_db_lock_wait_ms)
rate(bd_db_lock_wait_ms_sum[5m]) / rate(bd_db_lock_wait_ms_count[5m])
```

### VictoriaLogs (Structured Logs)

**Find all events for a command:**
```logsql
_msg:bd.command.* | json bd.command = "create"
```

**Error analysis:**
```logsql
_msg:* | json status = "error" | level >= ERROR
```

**Dolt lock wait analysis:**
```logsql
_msg:dolt.lock_wait | histogram_quantile(0.95, wait_ms)
```

---

## Dolt Telemetry Capabilities

### Dolt Internal Metrics

**Important**: Dolt does not provide native OpenTelemetry export. The documentation search confirms there is no Dolt configuration variable or feature to enable OTLP export.

Dolt exposes internal metrics only via:
- `performance_schema` tables (MySQL standard, accessible via SQL queries)
- System tables (`dolt_log`, `dolt_status`, `dolt_diff`, `dolt_branches`, `dolt_conflicts`)

**Beads implementation**:
Beads currently queries Dolt metrics via direct SQL (see `cmd/bd/doctor/perf_dolt.go`) rather than via OTLP. This is intentional — Dolt lacks native OTel support.

**Future Dolt OTel support**:
If Dolt adds native OTLP export, it would likely be configured via:
- Dolt configuration file
- Environment variables
- `dolt config` CLI commands

Track DoltHub releases for updates on OTel capabilities.

### Dolt System Tables for Telemetry

| Table | Purpose |
|--------|-----------|
| `dolt_log` | Commit history (queryable for audit) |
| `dolt_status` | Working set state (uncommitted changes) |
| `dolt_diff` | Cell-level diff between commits |
| `dolt_branches` | Branch metadata |
| `dolt_conflicts` | Merge conflicts (when present) |

**Telemetry integration opportunities:**
1. Query `dolt_log` for commit metrics (commit rate, authors, timestamps)
2. Query `dolt_status` for working set size
3. Query `dolt_diff` for cell-level change analysis
4. Query `dolt_branches` for branch proliferation detection
5. Query `dolt_conflicts` for conflict rate

### Sample Queries for Dolt Telemetry

**Commit frequency analysis:**
```sql
SELECT
    DATE_FORMAT(commit_date, '%Y-%m') as month,
    COUNT(*) as commits
FROM dolt_log
GROUP BY month
ORDER BY month DESC;
```

**Working set size tracking:**
```sql
SELECT
    COUNT(*) as staged_changes,
    SUM(CASE WHEN staged = 1 THEN 1 ELSE 0 END) as added,
    SUM(CASE WHEN staged = 0 THEN 1 ELSE 0 END) as removed
FROM dolt_status;
```

**Branch proliferation detection:**
```sql
SELECT
    COUNT(*) as branch_count,
    MIN(commit_date) as oldest,
    MAX(commit_date) as newest
FROM dolt_branches;
```

**Conflict analysis:**
```sql
SELECT
    COUNT(*) as conflict_count,
    COUNT(DISTINCT table_name) as tables_affected
FROM dolt_conflicts;
```

### Future Dolt Telemetry Integration

Consider adding periodic queries to collect metrics from Dolt system tables:

| Metric | Query | Collection Frequency |
|--------|--------|-------------------|
| `bd_dolt_commits_per_hour` | `dolt_log` GROUP BY hour | Every 5 minutes |
| `bd_dolt_working_set_size` | `dolt_status` COUNT(*) | Every 1 minute |
| `bd_dolt_branch_count` | `dolt_branches` COUNT(*) | Every 5 minutes |
| `bd_dolt_conflicts_per_day` | `dolt_conflicts` COUNT(*) | Every hour |

---

## Related Documentation

- [OTel Data Model](otel-data-model.md) — Complete event schema
- [OBSERVABILITY.md](../../OBSERVABILITY.md) — Quick reference for metrics
- [Dolt Backend](../../DOLT.md) — Dolt configuration and usage
- [Dolt Concurrency](dolt-concurrency.md) — Concurrency model and transactions

## Backends Compatible with OTLP

| Backend | Notes |
|---------|-------|
| **VictoriaMetrics** | Default for metrics (localhost:8428) — open source. Override with `BD_OTEL_METRICS_URL` |
| **VictoriaLogs** | Reserved for future log export. Override with `BD_OTEL_LOGS_URL` |
| **Prometheus** | Supports OTLP via remote_write receiver — open source |
| **Grafana Mimir** | Supports OTLP via write endpoint — open source |
| **Loki** | Requires OTLP bridge (Loki uses different format) — open source |
| **OpenTelemetry Collector** | Universal forwarder to any backend (recommended for production) — open source |

**Production Recommendation**: For production deployments, consider using **OpenTelemetry Collector** as a sidecar. The Collector provides:
- Single agent for all telemetry
- Advanced processing and batching
- Support for multiple backends simultaneously
- Better resource efficiency than per-process exporters
