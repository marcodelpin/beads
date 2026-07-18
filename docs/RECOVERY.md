# Recovery Playbooks

This playbook moved to [docs/recovery/](recovery/index.md), rendered on the
docs site at [https://beads.gascity.com/recovery/](https://beads.gascity.com/recovery/). Everything that lived on
this page is now in [recovery/init-safety.md](recovery/init-safety.md)
([https://beads.gascity.com/recovery/init-safety](https://beads.gascity.com/recovery/init-safety)).

> **Why this file exists:** released `bd` binaries print URLs into this file —
> v1.1.0's Dolt merge-refusal guidance (`printAncestorPKMismatchGuidance`,
> `cmd/bd/dolt.go` at tag `v1.1.0`) prints
> `docs/RECOVERY.md#pk-fork-refused`. This stub keeps those printed links
> landing on a working page with the same anchors. Do not delete it while
> released binaries still print it; it is deliberately exempt from the
> no-pointer-stubs rule (see `test/docsync`).

## init-force-refused

Moved: [recovery/init-safety.md § init-force-refused](recovery/init-safety.md#init-force-refused) · [https://beads.gascity.com/recovery/init-safety#init-force-refused](https://beads.gascity.com/recovery/init-safety#init-force-refused)

## init-token-missing

Moved: [recovery/init-safety.md § init-token-missing](recovery/init-safety.md#init-token-missing) · [https://beads.gascity.com/recovery/init-safety#init-token-missing](https://beads.gascity.com/recovery/init-safety#init-token-missing)

## init-local-exists

Moved: [recovery/init-safety.md § init-local-exists](recovery/init-safety.md#init-local-exists) · [https://beads.gascity.com/recovery/init-safety#init-local-exists](https://beads.gascity.com/recovery/init-safety#init-local-exists)

## pk-fork-refused

Moved: [recovery/init-safety.md § pk-fork-refused](recovery/init-safety.md#pk-fork-refused) · [https://beads.gascity.com/recovery/init-safety#pk-fork-refused](https://beads.gascity.com/recovery/init-safety#pk-fork-refused)
