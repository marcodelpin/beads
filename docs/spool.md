# Offline Write-Spool

## Overview

The write-spool is a local JSONL buffer that keeps `bd` write commands alive
when the Dolt remote is temporarily unreachable. Write ops (create, update,
note, close) that fail due to transient errors are serialised to
`.beads/spool/queue.jsonl` on disk and replayed automatically on the next `bd`
invocation.

**No user action is required for normal operation.** The spool exists so that
work survives network hiccups, a sleeping Dolt process, or a brief remote
outage without losing data or forcing you to retry manually.

Architecture rationale: see [docs/adr/0003-bd-write-spool.md](adr/0003-bd-write-spool.md)
(once authored; the design was planned as ADR-0001 during bda-11t decomposition
but numbered 0003 in the repo sequence).

---

## When the Spool Kicks In

The spool activates automatically when a write op returns a *transient* error.
Transient errors include:

- Network timeouts and connection-refused (Dolt remote temporarily down)
- `context.DeadlineExceeded` / `context.Canceled`
- `io timeout`, `connection reset`, `EOF`, `broken pipe`
- HTTP 5xx responses from a remote Dolt endpoint
- `driver: bad connection` from the Go SQL driver

Permanent errors — SQL constraint violations (`UNIQUE`, `FOREIGN KEY`), 4xx HTTP,
schema errors — are **not** spooled. They surface immediately because replaying
them would just land in the dead-letter queue anyway.

When a write is spooled, `bd` prints to stderr:

```
queued for replay (op_id=<hex>, will retry on next bd command)
```

The command exits successfully so your workflow is not blocked.

---

## Auto-Drain

Every `bd` invocation triggers `spool.MaybeDrain` in `PersistentPreRun` as a
background goroutine. The drain:

1. Acquires a `TryLock` on the spool directory — skips silently if another
   process is already draining (no double-replay).
2. Replays entries from `queue.jsonl` in FIFO order against the live Dolt store.
3. Successfully replayed entries move to `acked/<YYYY-MM-DD>.jsonl`.
4. Entries that fail permanently (e.g. constraint violation after a retried
   create that actually succeeded) move to `dead-letter.jsonl`.

The drain runs **non-blocking** in the background. Drain errors are logged at
debug level and never fail the foreground command.

---

## Inspecting Spool State

```sh
bd spool status
```

Sample output:

```
Spool directory: /your/repo/.beads/spool
  Pending entries:  3
  Queue size:       1842 bytes
  Dead-letter:      0 entries
  Last drain:       2026-05-15T08:00:00Z
```

JSON output for scripting:

```sh
bd spool status --json
# {"queue_entries":3,"queue_bytes":1842,"dead_letter_entries":0,"inflight_oldest_age_sec":0,"last_drain_ts":"2026-05-15T08:00:00Z"}
```

Fields:

| Field | Meaning |
|-------|---------|
| `Pending entries` | Entries in `queue.jsonl` not yet replayed |
| `Queue size` | Raw bytes on disk (cap: 100 MB) |
| `Dead-letter` | Permanently-failed entries (need manual review) |
| `Inflight age` | Seconds since oldest entry currently being drained (>0 = drain in progress) |
| `Last drain` | Timestamp of most recent completed drain cycle |

---

## Manual Drain

Force-drain all pending entries now (synchronous, waits for completion):

```sh
bd spool drain
```

Sample output:

```
Drain complete: 3 replayed, 0 dead-lettered
```

Use this after restoring Dolt connectivity to flush the spool immediately
rather than waiting for the next background drain.

JSON output:

```sh
bd spool drain --json
# {"drained":3,"dead":0}
```

---

## Clearing the Spool

**Last resort only.** This permanently discards pending writes that have not
yet been replayed into Dolt.

```sh
bd spool clear --confirm
```

`--confirm` is required to prevent accidental data loss. The command removes:

- `queue.jsonl` — pending entries
- `inflight.jsonl` — partially-drained batch
- `cursor.json` — drain position marker

It leaves `acked/` and `dead-letter.jsonl` intact as an audit trail.

When to clear:

- Dolt has been wiped and re-initialised (the queued writes reference a now-
  gone database state).
- The spool contains entries from a scrapped experiment you do not want to
  replay.
- You have manually applied the queued operations some other way.

---

## Storage Layout

All spool files live under `.beads/spool/` in the repository root:

```
.beads/
└── spool/
    ├── queue.jsonl        # Pending write entries (append-only producer log)
    ├── inflight.jsonl     # Batch currently being drained (crash-recovery)
    ├── cursor.json        # Drain position + last-drain timestamp
    ├── dead-letter.jsonl  # Permanently-failed entries (for inspection)
    └── acked/
        ├── 2026-05-14.jsonl   # Successfully replayed entries, by UTC date
        └── 2026-05-15.jsonl
```

The queue is capped at **100 MB**. Appends beyond this limit return
`ErrSpoolFull` and the write op surfaces the original Dolt error instead.

Acked files older than the GC retention window are cleaned up automatically.

---

## Troubleshooting

### Spool growing unboundedly

**Symptom:** `bd spool status` shows a rising `Pending entries` count over
multiple `bd` invocations.

**Cause:** Dolt remote is unreachable for an extended period, or the drain is
hitting a persistent transient error.

**Steps:**

1. Check Dolt connectivity: `bd doctor` — look for remote errors.
2. Verify the remote is running: `bd dolt push` (or check your Dolt server).
3. Once connectivity is restored, either wait for the next `bd` auto-drain or
   run `bd spool drain` explicitly.

### Drain fails repeatedly

**Symptom:** `bd spool drain` exits with an error or `bd spool status` shows
non-zero `Dead-letter` after draining.

**Steps:**

1. Run `bd doctor` — check Dolt health, credentials, and remote config.
2. Inspect dead-letter entries:
   ```sh
   cat .beads/spool/dead-letter.jsonl
   ```
   Each line is a JSON `Entry`. The `op`, `payload`, and `ts` fields identify
   what failed and when.
3. If the entries are safe to discard, `bd spool clear --confirm`.
4. If entries must be preserved, manually apply them via `bd create`/`bd update`
   using the data in each `payload` field.

### Inflight age keeps rising

**Symptom:** `bd spool status --json` shows `inflight_oldest_age_sec` growing.

**Cause:** A drain goroutine acquired the lock but crashed or stalled. A stale
`inflight.jsonl` file remains.

**Steps:**

1. Verify no other `bd` process is running (`ps aux | grep bd`).
2. Check whether the lock file is stale:
   ```sh
   ls -la .beads/spool/
   ```
3. If there is no `bd` process and `inflight.jsonl` is old, remove it manually:
   ```sh
   rm .beads/spool/inflight.jsonl
   ```
   The next drain will re-queue the inflight entries from `queue.jsonl`
   (entries are never deleted from queue.jsonl until the cursor advances past
   them).

### Corruption recovery

**Symptom:** Malformed JSON lines in `queue.jsonl`, or `bd spool drain` panics.

**Steps:**

1. The replay engine skips malformed lines automatically (non-fatal). Verify
   with `bd spool status`.
2. If corruption is severe, back up the spool directory first:
   ```sh
   cp -r .beads/spool/ /tmp/spool-backup-$(date +%s)
   ```
3. Then clear:
   ```sh
   bd spool clear --confirm
   ```

---

## See Also

- `bd doctor` — overall health check including Dolt remote reachability
- `bd dolt push` / `bd dolt pull` — explicit Dolt sync
- [docs/DOLT.md](DOLT.md) — Dolt backend overview
- [docs/adr/0001-multi-remote-approach.md](adr/0001-multi-remote-approach.md) — ADR for multi-remote strategy

## Spooled `create` semantics (GH#4378-review follow-up)

A `bd create` whose write gets spooled has **no issue ID yet** -- the ID is
generated server-side when the entry replays. The CLI reports an honest
queued outcome instead of a success with an empty ID: `--json` emits
`{"spooled": true, "op_id": ..., "title": ...}`, `--silent` prints
`QUEUED <op_id>`, and the default output says so explicitly. Dependency
edges from `--parent`/`--deps`/`--waits-for` ride inside the spool payload
(an empty side means "the new issue") and are applied at replay, after the
ID exists. Spooled `close`/`update`/`note` likewise report a queued outcome
and skip their success side effects (audit log, cascades) until replay.

Known residuals:
- **Mixed-version fleet**: an OLD (pre-fix) binary replaying a NEW create
  payload silently ignores the `dependencies` field (Go JSON decoding) --
  the create lands, its edges are dropped. Upgrade all drain sites together
  when rolling this out.
- **Edge failures at replay are warn-only** (stderr), mirroring the live
  path: failing the entry after `CreateIssue` succeeded would re-run the
  create on the next drain and duplicate the issue. A lost edge is
  re-attachable by hand; a duplicated issue is worse.
