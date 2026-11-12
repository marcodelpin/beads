# Agent Instructions

Contributor instructions for agents developing beads live in
[AGENTS.md](AGENTS.md). Area-specific rules live in nested `AGENTS.md` files
listed in its routing table.

If you are using `bd` to track work in your own project, see the user docs:
[IDE and agent setup](docs/getting-started/ide-setup.md) and
[https://beads.gascity.com/](https://beads.gascity.com/).

## Offline Write-Spool (fork operator notes)

The spool is **automatic**: `bd` buffers writes (create/update/note/close)
during Dolt outages and replays them on the next invocation. No manual
intervention is required for normal operation.

Useful commands:

```sh
bd spool status          # queue depth, disk usage, last drain timestamp
bd spool drain           # force-drain now (synchronous)
bd spool clear --confirm # wipe spool (last resort — discards pending writes)
```

See [docs/spool.md](docs/spool.md) for full operator documentation including
troubleshooting recipes.
