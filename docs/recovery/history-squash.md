---
title: History Bloat
description: Shed reachable Dolt history that dolt gc cannot reclaim
---

Every bead write mints a Dolt commit, and Dolt keeps every byte a reachable
commit references — `dolt gc` only reclaims what nothing points to. A
workspace that has accumulated months of high-frequency writes can grow far
past its live data (gigabytes of storage for a few thousand beads) while
`dolt gc` reclaims nothing, because the entire chain is still reachable from
your branch. This runbook squashes that chain to a single baseline commit so
the old history becomes collectable, without touching your live data.

<Warning>
This procedure rewrites history. Every other clone of the database becomes
unmergeable and must re-clone, and remotes and backups must be re-pointed.
Run it in a fenced window: all writers stopped, backup verified first.
</Warning>

## Symptoms

- The Dolt data directory (or its remote/backup) is large and growing while
  `bd stats` shows a modest number of beads
- `dolt gc` and `dolt gc --full` reclaim little or nothing
- Cloning or pulling the database is slow far out of proportion to its content

## Diagnosis

Run these inside the Dolt data directory — `.beads/embeddeddolt/<database>/`
in embedded mode, `.beads/dolt/<database>/` in server mode:

```bash
# How big is the store?
du -sh .

# How deep is the history? Thousands of commits with a small live
# dataset means the history, not the data, is the bloat.
dolt log --oneline | wc -l

# Confirm gc has nothing unreachable to collect
dolt gc --full
```

If `dolt gc --full` frees the space, you are done — no squash needed.

## Solution

**Step 1:** Fence and back up. Stop every writer (agents, background
services); in server mode also stop the server after backing up. The backup
is dolt-native and keeps the full history, so it remains your rollback.

```bash
bd backup sync
bd dolt stop
```

**Step 2:** Squash to a single baseline. From the Dolt data directory,
re-commit the current tree directly on top of the root commit:

```bash
root=$(dolt log --oneline | tail -1 | cut -d' ' -f1)
dolt reset --soft "$root"
dolt add -A
dolt commit -m "history squash: baseline $(date +%F)"
```

**Step 3:** Drop the other refs and collect. Anything still pointing at the
old chain keeps it alive:

```bash
dolt branch          # delete stale branches: dolt branch -D <name>
dolt tag             # delete stale tags:     dolt tag -d <name>
dolt gc --full
```

**Step 4:** Re-point remotes and backups. The new history is unrelated to
the old, so the first publish must replace it:

```bash
bd dolt push --force
bd backup remove && bd backup init <path>   # fresh destination, then:
bd backup sync
```

**Step 5:** Verify, then re-clone everywhere else. On this machine:

```bash
bd doctor
bd list -n 5
```

Every other clone of this database must be re-created from the squashed
remote (old clones can no longer pull — the histories no longer share a
root). Only then unfence your writers.

## Prevention

High-frequency coordination state (claim leases, wisps) lives in unversioned
tables precisely so routine agent traffic does not mint history; bloat at
this scale usually means something is writing versioned tables in a tight
loop. Find and fix that writer, watch data-directory growth over time, and
run `dolt gc` periodically so unreachable garbage never accumulates on top
of reachable history.
