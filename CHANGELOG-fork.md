# CHANGELOG-fork — marcodelpin/beads fork-only deltas

Operational mandate from **bda-116** (fork-first strategy, 2026-04-28): track every fork-only
commit so reconciles can protect the deltas and an eventual upstream re-engagement knows
exactly what we carry.

- **Baseline**: upstream = `gastownhall/beads` `main`. Fork-only set = `git log gastownhall/main..HEAD --no-merges`.
- **As of 2026-06-12** (HEAD `de69cef478`): **55 fork-only non-merge commits** (+52 upstream-sync merge commits).
- **Maintenance protocol**: any session landing a fork-only commit MUST add/extend the matching
  section here in the same push. Regenerate the raw list with the command above; every non-merge
  sha must be accounted for in exactly one section.

Status legend: **active** = live fork delta · **partially-superseded** = upstream later shipped an
equivalent for part of it (residual delta noted) · **housekeeping** = no behavioral delta.

---

## 1. Windows zero-flash: GUI-subsystem bd.exe + console-less git spawns — `active`

**Issues**: chg-8hnz, bda-3co, bda-5u7

| sha | date | subject |
|-----|------|---------|
| `66de45b8dd` | 2026-05-11 | chg-8hnz: build bd.exe as -H=windowsgui + AttachConsole bridge (fork only) |
| `3b077fc090` | 2026-06-07 | bda-3co: hide console window on all git spawns (CREATE_NO_WINDOW) |
| `4e44477c68` | 2026-06-07 | bda-5u7: gate -H=windowsgui on GOOS_EFFECTIVE (build target), not OS (host) |

Eliminates every conhost.exe window flash when bd runs on Windows from non-interactive callers
(Claude Code hooks, statusline, /bdloop, /go scans). The binary links as GUI-subsystem
(`-H=windowsgui`) with a kernel32 `AttachConsole(ATTACH_PARENT_PROCESS)` bridge in
`cmd/bd/console_windows.go` so interactive pwsh use keeps stdio; all 98 git.exe spawn sites
(34 files) route through new leaf package `internal/execx` setting HideWindow +
CREATE_NO_WINDOW. bda-5u7 fixes the Makefile to key the GUI link on the build TARGET
(`go env GOOS`) instead of the host OS, so Linux cross-compiles also emit a GUI bd.exe instead
of silently regressing to console subsystem. Explicitly NOT for upstreaming (upstream keeps the
console-subsystem build by design).

Key surfaces: Makefile `-H=windowsgui` gated on `GOOS_EFFECTIVE`; `cmd/bd/console_windows.go`
(`attachParentConsole()` first in `main()`); `cmd/bd/console_other.go` stub;
`internal/execx.GitCommand/GitCommandContext` drop-ins for `exec.Command("git", ...)`.

## 2. Remote Dolt sql-server hardening (shared-server topology) — `active`

**Issues**: bda-4sl, bda-68f, sys-9np6d, sys-t9tlx, sys-c8066, bda-i69

| sha | date | subject |
|-----|------|---------|
| `fbae05ee84` | 2026-05-11 | bda-68f: fix isEmbeddedMode rename to !usesSQLServer (upstream renamed + inverted semantics) |
| `ef3e8e1a79` | 2026-05-12 | bda-4sl: skip local Dolt spawn when --server-host is remote |
| `8b50b9f2b1` | 2026-05-14 | sys-9np6d: expose BEADS_DOLT_READ_TIMEOUT env var (default 10s preserved) |
| `e7b5b10d19` | 2026-05-27 | sys-c8066: skip Dolt-native backup for remote sql-server (stops /var/lib/dolt/S: leak) |
| `d0047b27b2` | 2026-06-05 | sys-t9tlx: bd dolt push client-side timeout + retry/backoff |
| `7d2c7c0419` | 2026-06-07 | bda-i69: default remote Dolt server port to DefaultSharedServerPort (3308) when unresolved |
| (tip) | 2026-06-12 | bda-53z: tolerate dolthub/dolt#11131 schema-encoding drift in aux row re-key — skip drifted table + warn instead of crashing migration 0051 mid-pass (incident sys-duwt6: drift on 140/142 fleet DBs; on a single shared sql-server the skipped cross-clone PK convergence is moot) |

Hardens bd for the fleet topology: a single shared remote Dolt sql-server
(forgejo-mdp.mdp:3308) serving many client machines, including Windows CGO=0 builds with no
local dolt binary. `bd init --server --server-host=<remote>` no longer spawns a local dolt
(bda-4sl); `DoltStore.BackupDatabase` is a no-op against a remote server, stopping the
server-side `/var/lib/dolt/S:/...` garbage tree that grew 15G on LXC 258 (sys-c8066); fresh
clones missing the gitignored `.beads/dolt-server.port` default to 3308 instead of dialing `:0`
(bda-i69). Operational timeouts: `BEADS_DOLT_READ_TIMEOUT` env var (read path, sys-9np6d) and
bounded timeout + exponential-backoff retry for `bd dolt push` via `dolt.push-timeout`
(default 90s) / `dolt.push-retries` (default 2) config keys (sys-t9tlx). bda-68f adapts fork
code in gc.go to upstream's `isEmbeddedMode`→`usesSQLServer` rename. Upstream assumes
local/embedded Dolt — these remain fork-only.

## 3. Offline write-spool: durable replay queue when Dolt is unreachable — `active`

**Issues**: bda-11t (epic): bda-0fb, bda-k4x, bda-14k, bda-0oe, bda-9bq, bda-9gx

| sha | date | subject |
|-----|------|---------|
| `25dfec080e` | 2026-05-13 | bda-0fb: add internal/spool/ package — entry/spool/append/canonical + tests (kb-spool port) |
| `28db74a2a9` | 2026-05-13 | bda-k4x: add internal/spool/lock — cross-platform file locking + stress test |
| `c30b835f8e` | 2026-05-13 | bda-14k: add internal/spool/replay — Drain/MaybeDrain/SeenSet (kb-spool port) |
| `8d8b54edd0` | 2026-05-13 | fix(spool): advance cursor only past processed entries on permanent error |
| `e14fe3043c` | 2026-05-15 | bda-0oe: wire offline write-spool — 4 callsites + admin subcommand + MaybeDrain hook |
| `bcc13d6fb3` | 2026-05-15 | bda-9bq: integration tests for offline write-spool |
| `a5d2da5f65` | 2026-05-15 | bda-9gx: docs + AGENT_INSTRUCTIONS for offline write-spool |

Ports the fleet's kb-spool primitive into bd: when a write command (create/update/note/close)
hits a transient Dolt failure (network timeout, 5xx, Dolt i/o timeout — classified by
`spool.IsTransientErr()`; SQL-constraint/4xx errors still surface), the operation is durably
queued as a BLAKE3-keyed JSONL entry in `.beads/spool/` (100MB cap, atomic temp-rename writes)
and replayed automatically (opportunistic non-blocking `MaybeDrain` in rootCmd
PersistentPreRun) or manually via `bd spool status|drain|clear --confirm`. Cross-platform
flock/LockFileEx locking, crash-safe Drain/inflight/SeenSet dedup. Rationale: the fleet runs bd
against one remote Dolt server (dolt-remote-only convention) — server outages would otherwise
lose writes; upstream has no offline-write fallback. Operator doc: `docs/spool.md`.

## 4. bd gc safety net + per-item consent flow + memory prune — `active`

**Issues**: upstream GH#3543, sys-8t7vx, sys-2a3rh, bda-td3, bda-3vg, bda-mxa

| sha | date | subject |
|-----|------|---------|
| `d6a784c43d` | 2026-04-28 | feat(gc): fork-only safety net for upstream gastownhall/beads#3543 |
| `f2515a655c` | 2026-04-28 | test(gc): adapt 3 existing tests to fork safety floor |
| `3ac5f7ca85` | 2026-04-28 | feat(gc): bd gc decay phase prunes expired memories with policy=delete |
| `0d500d7dbb` | 2026-04-28 | test(gc-memory-prune): log bd gc output on failure for debugging |
| `20bb162212` | 2026-04-28 | feat(gc): consent flow — --plan emits JSON candidates, --only filters delete |
| `0ff7510ca0` | 2026-04-28 | feat(memories): consent flow on --gc — --gc-plan + --gc-only mirror bd gc |
| `d9a6d17f24` | 2026-04-28 | feat(memory-gc): symmetric backup for bd memories --gc (bda-3vg) |
| `9551c17dca` | 2026-04-28 | fix(fmt): gofmt cmd/bd/gc.go + memory_dedup_test.go |
| `234a460a05` | 2026-04-28 | feat(gc): bd gc --plan-summary human-readable view (bda-mxa) |

Hardens `bd gc` against the unresolved upstream regression GH#3543 (`gc --older-than 30`
deleted 82 same-day-closed beads): 7-day `--older-than` safety floor (override
`--allow-recent`), skip-and-warn on nil/zero closed_at candidates, fsync'd pre-delete JSONL
backups (`.beads/.gc-backup-<unix>.jsonl`, `.beads/.gc-memory-backup-<unix>.jsonl`,
restorable via `bd import`). Per-item consent flow per user mandate sys-2a3rh ("bd must not
delete old things without asking"): `bd gc --plan` (read-only JSON plan), `--plan-summary`
(human table), `--force --only=IDs,keys` (allowlist-curated delete); `bd memories --gc` gets
mirrored `--gc-plan`/`--gc-only`. Expired-memory pruning (policy=delete) integrated into the
gc decay phase (opt-out `--skip-memory-prune`). Upstream has neither the #3543 fix nor
consent-gated gc.

## 5. Memory: fact validity windows (mempalace pattern) — `active`

**Issues**: sys-4f28 (filed upstream as PR #4 + issue #3539, both stalled → fork-first)

| sha | date | subject |
|-----|------|---------|
| `45d5a1a088` | 2026-04-08 | feat(memory): fact validity windows (mempalace pattern) |
| `aa6b8e1275` | 2026-04-11 | fix: correct misspellings flagged by golangci-lint |
| `fff9ce23c1` | 2026-04-22 | fix(memory_envelope): address CodeRabbit findings from PR #4 |
| `8974115c48` | 2026-04-22 | fix(memory_validity_test): tolerate auto-import bootstrap chatter |

Optional per-memory expiration: `bd remember --valid-for=<dur>` / `--valid-until=<date>` +
`--expire-policy=hide|notify|delete`, stored in a backward-compatible `_bd_mem` JSON envelope
inside the existing opaque memory value — no Dolt schema change; legacy plain-text memories
stay non-expiring. `bd memories` gains `--include-expired` and `--gc`; `bd recall`/`bd prime`
unwrap the envelope and prime drops expired hide/delete facts from session priming (notify
memories injected with an `[EXPIRED]` marker). Follow-ups harden the envelope parser (int64
overflow guard, negative-duration rejection) per CodeRabbit review on fork PR #4.

## 6. Memory: content dedup, provenance capture, tags + scope filtering — `active`

**Issues**: bda-97j, bda-5to (+ auto-import resurrection fix)

| sha | date | subject |
|-----|------|---------|
| `c54b1164b2` | 2026-04-28 | feat(memory): fork-only auto-key content dedup on bd remember |
| `69b17c8359` | 2026-04-28 | fix(memory-dedup): whitespace-first normalization + envelope discriminator + no-dedup test |
| `b8a38621b6` | 2026-04-28 | test(memory-dedup): assert on 'Memories (1)' header instead of substring |
| `bc3f1e8aba` | 2026-04-28 | fix(auto-import): also count memories so deletes don't resurrect |
| `fe985a9b28` | 2026-04-28 | feat(memory): provenance capture on bd remember (bda-97j) |
| `9bbb879373` | 2026-04-28 | feat(memory): tags + scope filter (bda-5to) |
| `93e685c5d0` | 2026-04-28 | fix(test+lint): align memory_envelope_test with new buildMemoryEnvelope signature + remove unnecessary int() cast in gc.go |

Three capabilities: (1) auto-key content dedup — `bd remember` without `--key` fingerprints
the insight (lowercase, whitespace-collapsed, punctuation-stripped) and reuses an existing key
on match (verb `Deduped (updated)` / JSON `action='deduped'`; opt-out `--no-dedup`);
(2) provenance capture — every remember stores session_id (`$CLAUDE_SESSION_ID`), git HEAD,
cwd as envelope fields (opt-out `--no-provenance`); (3) repeatable `--tag <k:v>` and
`--scope <machine|project|global>` on remember, with AND-semantic `--tag` and exact-match
`--scope` filters on `bd memories`. Companion fix: `countMemoriesInStore` treats a DB with 0
issues but N memories as populated, so JSONL auto-import no longer resurrects intentionally
deleted memories.

## 7. New commands: bd cohort + bd snapshot — `active`

**Issues**: bda-9pc, bda-c7h, bda-wzl

| sha | date | subject |
|-----|------|---------|
| `ba6c741866` | 2026-04-28 | feat(cohort): bd cohort <id> graph traversal (bda-9pc) |
| `2039e71072` | 2026-04-28 | feat(snapshot): bd snapshot — quick dashboard (bda-c7h) |
| `87d6c6f71c` | 2026-05-13 | bda-wzl: filter fork-only commands (cohort, snapshot) from doc-flags check |

`bd cohort <id>` (cmd/bd/cohort.go, `--depth/-d` default 5, `--json`) shows everything related
to an issue in 4 sections: ANCESTORS (parent chain), DESCENDANTS (BFS), SIBLINGS (shared
labels), REFERENCED BY (descriptions/notes), with cycle guard. `bd snapshot`
(cmd/bd/snapshot.go, `--window-hours/-w` default 24, `--cap/-n` default 10, `--json`) is a
single-screen work-state dashboard (IN PROGRESS / RECENT CLOSED / RECENT CREATED / STATS).
bda-wzl patches `scripts/check-doc-flags.sh` so fork-only commands don't fail the
upstream-aligned doc-freshness CI (`BEADS_FORK_ONLY_CMDS`, default `cohort snapshot`, empty =
upstream-strict).

## 8. nocgo dbname extraction + `bd gate check gh:pr` fix (gh CLI ≥2.89) — `partially-superseded`

**Issues**: upstream GH#3402, GH#3411 (related: GH#2142, GH#3231)

| sha | date | subject |
|-----|------|---------|
| `59428e9e6f` | 2026-04-23 | fix(nocgo): move sanitizeDBName to non-tagged file (GH#3402) |
| `ccc7e45b06` | 2026-04-23 | fix(gate): bd gate check gh:pr broken on gh CLI >=2.89 (GH#3411) |
| `1cdf53d307` | 2026-04-27 | fix(nocgo): remove dbname.go superseded by upstream database_name.go (GH#3402 fixed upstream) |
| `0af848f08c` | 2026-04-27 | fix(nocgo): remove dbname_test.go superseded by upstream database_name_test.go |

Two urgent fixes carried ahead of upstream. (1) `sanitizeDBName` lived in a cgo-tagged file but
was called unconditionally, breaking `CGO_ENABLED=0 GOOS=windows` builds (GH#3402) — the only
clean path to the pure-Go Windows bd.exe this fork ships; the fork extracted it, upstream fixed
it independently days later, fork copy removed 2026-04-27 (**superseded**). (2) gh CLI v2.89
removed the `merged` boolean from `gh pr view --json`, breaking every
`bd gate check --type=gh:pr`; the fork derives merge state from `state==MERGED`. Upstream later
shipped its own variant; the fork still carries the `ghPRJSONFields` const refactor in
cmd/bd/gate.go + `gate_ghpr_test.go` regression tests (**residual delta**).

## 9. Misc fork fixes — `partially-superseded`

**Issues**: bda-y3o, bda-r2r, bda-9a5, bda-v3n (related: bda-3vk, upstream PR #3691)

| sha | date | subject |
|-----|------|---------|
| `2958d24b01` | 2026-04-29 | fix(test): warm up cobra/pflag lazy persistent-flag merge before parallel tests (bda-v3n) |
| `877ddad085` | 2026-05-10 | bda-r2r: bd init --inject-agents-md flag for auto-writing rules block |
| `11a38b03b7` | 2026-05-10 | bda-9a5: scope fork countMemoriesInStore guard to embedded path only |
| `fd48488330` | 2026-05-13 | bda-y3o: rewrite migration 0035 INSERT to enumerate columns (avoid SELECT * mismatch) |

(1) bda-v3n: TestMain warmup walking the cobra command tree calling `InheritedFlags()`
single-threaded — fixes a real GHA `-race` failure from cobra v1.10.x lazy persistent-flag
merges racing across parallel tests (**active**). (2) bda-r2r: `bd init --inject-agents-md`
idempotently writes a marker-delimited `<!-- bd:start --> ... <!-- bd:end -->` bd-usage block
into BOTH CLAUDE.md and AGENTS.md (**active**). (3) bda-9a5: scopes the fork's
memory-resurrection guard inside the `jsonlImporter` embedded-path branch so upstream PR
#3691's server-mode fallback passes while embedded protection is retained (**active**).
(4) bda-y3o: migration 0035 `INSERT IGNORE INTO wisps SELECT *` rewritten to enumerate 51
canonical columns for legacy fleet DBs with residual columns — later **superseded** by
upstream's INFORMATION_SCHEMA dynamic column-intersection (current file header credits/replaces
bda-y3o).

## 10. Housekeeping — `housekeeping`

| sha | date | subject |
|-----|------|---------|
| `89a3d99dd4` | 2026-04-23 | flake.lock: Update (#3) |
| `d003442c5a` | 2026-04-27 | ci: trigger workflow |
| `f5ea7e82f6` | 2026-04-27 | ci: re-trigger after fork main sync |
| `3005853f79` | 2026-04-27 | ci: re-trigger after dbname.go duplicate removed from fork main |
| `ada2802ad1` | 2026-04-27 | ci: re-trigger after dbname_test.go duplicate removed |
| `7902161556` | 2026-05-13 | wip: pre-compact autosave 2026-05-13 10:19:05 sid=unknown |
| `a36fbad71c` | 2026-05-13 | wip: pre-compact autosave 2026-05-13 11:03:40 sid=unknown |
| `433eec23d2` | 2026-05-15 | wip: pre-compact autosave 2026-05-15 10:10:15 sid=unknown |

CI re-triggers, Nix flake.lock bump (fork PR #3), and pre-compact session autosaves. No
behavioral delta.
