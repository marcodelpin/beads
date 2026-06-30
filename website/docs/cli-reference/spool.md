---
id: spool
title: bd spool
slug: /cli-reference/spool
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc spool`

## bd spool

Manage the offline write-spool (.beads/spool/).

The spool buffers bd write commands (create/update/note/close) when Dolt is
temporarily unreachable. Entries are replayed automatically at the start of
the next bd command (MaybeDrain), or you can trigger a drain manually.

Subcommands:
  status   Show queue depth, oldest entry, and disk usage
  drain    Force-drain the spool now (replay all pending entries)
  clear    Wipe the spool (requires --confirm)

```
bd spool [flags]
```

### bd spool clear

Clear all pending spool entries. This permanently discards any queued writes
that have not yet been replayed into Dolt. Use with caution.

You must pass --confirm to proceed.

```
bd spool clear [flags]
```

**Flags:**

```
      --confirm   Required: confirms you want to permanently discard pending spool entries
```

### bd spool drain

Force-drain the spool now (replay all pending entries into Dolt)

```
bd spool drain [flags]
```

### bd spool status

Show spool queue depth, oldest entry timestamp, and disk usage

```
bd spool status [flags]
```
