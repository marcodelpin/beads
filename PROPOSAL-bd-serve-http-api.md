# PROPOSAL: `bd serve` — a loopback HTTP API over one shared work-surface contract

**Status:** design, ready for implementation as sliced beads PRs
**Author:** design merge (five parallel section drafts, reconciled under judge-panel binding corrections)
**Date:** 2026-07-31
**Repo:** `github.com/steveyegge/beads` (worktree `sd-server` @ `b1694a50a`)
**Motivating incidents:** hosted viewer's bare-`IssueFilter{}` leak and `err != nil || issue == nil → 404` bug; 49 hand-duplicated `*_proxied_server.go` command duals in `cmd/bd`; Gas City's subprocess forks of `bd ready` / `bd list` / `bd show` / `bd update --claim`.

---

## Contents

1. [Thesis, non-goals, constraints, and scope boundaries](#1-thesis-non-goals-constraints-and-scope-boundaries)
2. [The shared library contract](#2-the-shared-library-contract) — the public `issueops.Reader` role over `internal/workapi`: filter builders, not-found normalization, detail assembly, claim typing, parity enforcement table
3. [The wire: OpenAPI document, schemas, errors, pagination](#3-the-wire-openapi-document-schemas-errors-pagination) — the six operations, `x-go-type` pinning, problem+json, keyset cursor
4. [Codegen, CLI migration, server lifecycle and security](#4-codegen-cli-migration-server-lifecycle-and-security) — drift gate, extraction slices M1–M4, mode gate, bind posture, concurrency
5. [Test strategy, slice plan, risks, and open questions](#5-test-strategy-slice-plan-risks-and-open-questions)

Appendix A: [Reconciliation with the issueops Lifecycle refactor](#reconciliation-with-the-issueops-lifecycle-refactor) — owner decisions (Reader role + subsume), rationale, strongest objection, merge order, residual risks
Appendix B: [Council review — dispositions](#council-review--dispositions) — per-reviewer verdicts, applied/deferred ledger, post-review consistency check

Design rules cited as "rule N" throughout refer to the numbered list in
[§ 1 Binding constraints (design rules from review)](#binding-constraints-design-rules-from-review);
"decided parameter N" refers to [§ 1 Binding constraints (decided parameters)](#binding-constraints-decided-parameters).

---

## 1. Thesis, non-goals, constraints, and scope boundaries

### Thesis

`bd serve` is a cobra subcommand that runs a loopback-bound HTTP server exposing six operations
(`GET /healthz`, `GET /v0/beads/context`, `GET /v0/beads/ready`, `GET /v0/beads/issues`,
`GET /v0/beads/issues/{id}`, `POST /v0/beads/issues/{id}:claim`) over the existing UOW seam,
described by a hand-written OpenAPI document from which the Go wire types are generated. The
problem it solves is not "beads needs a web server" — it is that beads already has multiple
hand-written surfaces over the same data and they have already drifted in production: `cmd/bd`
contains 49 `*_proxied_server.go` command duals (34 of them on the UOW seam), ready-work defaults
are constructed from scratch twice inside the CLI alone, and the hosted viewer — a third
hand-written surface — shipped both a bare `IssueFilter{}` that leaks gates/wisps/templates/closed
issues that `bd list` hides, and an `err != nil || issue == nil → 404` bug that reports dropped
connections as missing issues. The shared-contract framing is therefore the point: the deliverable
is ONE implementation of filter/default construction, result shaping, detail assembly, not-found
normalization, and the claim commit protocol, which the direct CLI, the proxied CLI, and the HTTP
handlers all consume, so that a fourth surface cannot be written by hand and a field cannot be
silently dropped. The HTTP layer itself is deliberately thin and boring — stdlib mux, one UOW per
request, no new lifecycle machinery — because everything interesting lives at the contract
boundary. That boundary stops at filter and result-shaping construction: query execution is
explicitly NOT abstracted, because both existing seams (`storage.Storage` and
`domain.IssueUseCase` via UOW) already implement every needed query with one-line call sites, and
an execution port would exist only to launder their signature differences. Drift prevention is
enforced by mechanism, not convention: the spec's `Issue` schema is pinned to the canonical
`internal/types` struct via `x-go-type` with a two-way JSON-tag bijection test, CI regenerates and
diffs the generated types, and CLI-vs-HTTP parity oracles compare full item JSON, not ID sets.
Success for v0 is that Gas City can replace its highest-volume `bd` subprocess forks (`ready`,
`list`, `show`, `update --claim`) with HTTP calls whose responses are field-identical to the CLI's
`--json` output — and that after this slice, the duplicated builder code those four commands used
to carry no longer exists to drift. One scoped exception is stated here rather than discovered
downstream: the cache reconciler's full active-universe scan needs the ephemeral tier and
label-free rows (`TierBoth` + `SkipLabels`, served today by `bd list` UNIONED with a second
`bd query "ephemeral=true AND ..."` fork — gascity caching_store_reconcile.go:61-63,
bdstore.go:2262-2264), and v0's list surface deliberately hard-wires `SkipWisps=true` with labels
always hydrated, so that loop stays on subprocess in v0 (§ 3, "what this list surface deliberately
cannot serve"; slice 11).

### Non-goals

Explicitly out of scope for v0. Each is a decision, not an omission.

- **Multi-tenancy.** No `{project}` path segment, no control-plane operations, no per-request
  workspace selection. The server is single-workspace by construction (workspace resolution
  mutates process-global state: `os.Setenv("BEADS_DIR")` at `cmd/bd/main.go:519,748`).
- **Authentication or TLS.** Loopback is the v0 trust boundary — identical to the loopback Dolt
  SQL server and dbproxy the command already fronts. The named future mechanism for non-loopback
  is the existing rotating 0600 shared secret in `internal/storage/dbproxy/identity`
  (`crypto/subtle` compare), reused, not reinvented. Not in-slice.
- **A write surface beyond claim.** No create/update/close/reopen endpoints. The claim endpoint
  exists because Gas City's dispatch loop needs an atomic CAS with a typed conflict; general
  writes wait for the public issueops facade vocabulary (see "What this does NOT solve").
- **Migrating the other ~46 command pairs.** Only `bd ready`, `bd list`, `bd show`,
  `bd update --claim` move onto the shared contract in this slice.
- **An execution port/adapter layer — AMENDED by the issueops reconciliation (owner decision;
  see "Reconciliation with the issueops Lifecycle refactor").** The read surface IS exposed as
  the public `issueops.Reader` role with `IssueReader()` accessors, following the facade's
  role/accessor convention, with filter/default construction subsumed inside the
  implementations. `internal/workapi` remains the single implementation substrate behind every
  Reader, and no OTHER execution abstraction may be introduced — no generic Port abstracting
  `storage.Storage` and the UOW use-cases beyond this one role. The council's original finding
  is recorded as the considered-and-overridden counter-argument: such a port exists only to
  launder `SearchPage` vs `[]*types.Issue`, forces limit+1 over-fetch semantics onto the direct
  path, and costs ~5 adapter implementations over one-line reads.
- **Serving embedded (non-SQL-server) workspaces.** Typed refusal via the existing
  `storage.ErrUnsupported{Op, Backend}` (`internal/storage/errors_unsupported.go:8`).
- **Fuzzy/substring ID resolution over HTTP.** `GET /v0/beads/issues/{id}` is exact-match (with
  the issue→wisp fallback `bd show` has); fuzzy resolution stays a CLI affordance. This is what
  lets Gas City delete its decode-array-index-`[0]` + `ErrIDCollision` guard.
- **Hooks.** `bd serve` fires no hooks. The UOW seam has never constructed a `HookFiringStore`;
  a user-controlled subprocess per HTTP mutation is a latency and process-lifecycle hazard.
- **CLI auto-commit machinery.** The per-command auto-commit globals in cobra's
  `PersistentPostRunE` are never touched; the claim write commits explicitly per request through
  the one existing transactional helper (`uow.RunTxResult`, `internal/storage/uow/tx.go:76`).
- **Server lifecycle machinery.** No flock, no pidfile, no `.beads/serve-info.json` discovery
  file, no idle timeout. dbproxy needs those because clients race to auto-spawn it; `bd serve` is
  operator-invoked and the TCP bind is the mutual exclusion (for a fixed port — see rule 7 and
  § 4 for the honest ephemeral-default caveat).
- **Wire-level result caps.** No clamping or rejection of `limit=0`; no rate limiting, request
  quotas, or body-size ceremony beyond what the six operations need. Any future cap is an
  operator knob, not a wire refusal.
- **Serving the OpenAPI document over HTTP**, dashboards, metrics endpoints, or any operation
  beyond the six. Every route must appear in the spec-parity set-equality test; additions are
  additive later. A structured request log to stderr is NOT covered by this non-goal — it is a
  required floor (§ 4, "Observability floor"): no metrics endpoint is defensible, no log is not.
- **Lite/partial hydration (`skip_labels`).** `IssueFilter.Lite` sets `Issue.IsLitePartial`,
  which is `json:"-"` — a lite row is wire-indistinguishable from an issue with empty fields. A
  future explicit projection parameter can document the omission contract safely.

### Binding constraints (decided parameters)

These are settled. Design and implementation happen WITHIN them; none may be relitigated in a
slice PR.

1. **Clean single-tenant OSS surface; vendor-neutral source.** The hosted product adapts to OSS,
   not the reverse — no commercial or hosted-product names appear in OSS code or spec.
2. **Spec-first.** The hand-written OpenAPI document is the source of truth and Go types are
   generated from it with CI drift enforcement — because a spec reverse-engineered from handlers
   is documentation, not a contract.
3. **Loopback-only by default.** Bind numeric 127.0.0.1; refuse non-loopback without explicit
   opt-in — v0 ships no auth, so the bind policy IS the security model.
4. **`bd serve` cobra subcommand with `--addr`.** A subcommand gets its own flag surface, help,
   and lifecycle; a root-level `--serve` flag would entangle every other command's flag parsing.
5. **The API fronts the UOW implementation only.** One-UOW-per-request is the natural transaction
   shape and the owner's explicit simplification; the server never constructs a
   `storage.Storage`. All three SQL-server modes are in scope for the gate: the exported
   providers already exist on main (`uow.NewDoltServerUOWProvider`,
   `uow.NewExternalDoltServerUOWProvider` — `internal/storage/uow/doltserver_provider.go:16`,
   `external_doltserver_provider.go:18`, both already called by `cmd/bd/uow_factory.go:77,132`).
   On main today, `uowProvider` is wired only in proxied mode (`cmd/bd/main.go:1378-1388`), so
   the serve command must construct the matching provider for server/external-server workspaces
   itself; the delivery plan states which slice wires which mode, and any mode not yet wired gets
   a typed refusal whose message does not promise what the gate refuses.
6. **MVP surface = exactly the six operations listed in the thesis.** Bounded by what Gas City
   verifiably calls today; every additional operation is permanent wire surface.
7. **CLI migration scope = the four backing commands only** (`bd ready`, `bd list`, `bd show`,
   `bd update --claim`) — enough to prove the shared contract kills real drift without a 49-pair
   big bang.
8. **Delivery target is OSS beads main** (`github.com/steveyegge/beads`); the Gas City fork
   rebase and its Postgres conformance are downstream dependencies, not in-slice work.

### Binding constraints (design rules from review)

These override any section or slice that disagrees. Each is grep-verified against the tree.

1. **No second wire struct.** The spec's `Issue`/`IssueDetails`/`IssueWithCounts` schemas are
   pinned to the canonical `internal/types` structs via `x-go-type`; no hand-written or generated
   mirror struct, no Go-to-Go mapping function anywhere. Enforcement is a two-way JSON-tag ↔
   spec-property bijection test — a round-trip fixture test is insufficient (a new `omitempty`
   field absent from the fixture passes both directions).
2. **`limit=0` means unlimited, exactly as in the CLI.** Gas City calls `bd ready ... --limit 0`
   (`gascity internal/beads/bdstore.go:2513`) and `bd list --json --limit 0` (`bdstore.go:1923`,
   the reconciler's forever full active-set scan). A v0 that 400s or clamps `limit=0` fails the
   brief. (The reconciler's own scan stays on subprocess in v0 for tier/labels reasons — § 3,
   "what this list surface deliberately cannot serve" — but its `limit 0` posture is the
   contract every migrated full-set reader inherits, and `bd ready --limit 0` migrates in v0.) Scope (security review): this is a guarantee for the default loopback bind, where the
   reconciler lives; under `--allow-non-loopback` an unlimited fully-buffered read is a
   network-reachable memory-exhaustion primitive and `limit=0` is refused there (§ 3, limit
   semantics).
3. **`/v0/beads/ready` returns `IssueWithCounts` items, not bare `Issue`.** `bd ready --json`
   emits `[]*types.IssueWithCounts` on both CLI paths (`cmd/bd/ready.go:246,539`;
   `cmd/bd/ready_proxied_server.go:441`); the HTTP surface must carry
   `dependency_count`/`dependent_count`/`comment_count`/`parent` or it drifts at the field level
   on day one.
4. **Not-found is normalized inside the shared get/detail function.** On the UOW seam,
   `db.issueSQLRepositoryImpl.Get` returns `(nil, sql.ErrNoRows)`
   (`internal/storage/domain/db/issue.go:394-410`) — never `storage.ErrNotFound`. The CLI's own
   proxied code already treats both sentinels as not-found (`cmd/bd/create_proxied_server.go:497`).
   The shared function normalizes {wrapped `sql.ErrNoRows`, nil-issue-with-nil-error} to the
   `storage.ErrNotFound` sentinel so no future caller can reintroduce the bug; an integration
   test against real Dolt asserts 404-on-unknown-id. Handler-only mapping is insufficient.
5. **One claim retry/commit implementation.** `uow.RunTxResult`
   (`internal/storage/uow/tx.go:76`) — already on main with serialization retry and
   nothing-to-commit tolerance — is consumed by BOTH the proxied CLI and the server. The bespoke
   `cenkalti/backoff` loop in `cmd/bd/update_proxied_server.go:100-116` does not survive as a
   second copy, and no new `ClaimViaUOW` wrapper is lifted.
6. **No execution abstraction other than the `issueops.Reader` role.** AMENDED by owner decision
   — the reconciliation section supersedes this rule's original absolute form in one respect:
   the read surface IS exposed as the public `issueops.Reader` role with `IssueReader()`
   accessors, filter/default construction subsumed inside the implementations, and
   `internal/workapi` remains the single implementation behind every Reader; no OTHER execution
   abstraction may be introduced. The council's original finding is preserved verbatim as the
   considered-and-overridden counter-argument: "The shared contract stops at filter and
   result-shaping construction; both seams already implement every needed query with one-line
   call sites, and a Port would exist only to launder `SearchPage` vs `[]*types.Issue` while
   forcing limit+1 over-fetch semantics onto the direct CLI."
7. **No flock/pidfile/serve-info.json.** The TCP bind is the singleton — which holds only for an
   explicitly fixed port; under the default ephemeral `--addr 127.0.0.1:0` N serves run in
   parallel with no exclusion at all (§ 4 states this honestly and blesses a fixed-port `--addr`
   for deployments). Concurrent serves are data-safe because every claim is a CAS inside a Dolt
   transaction.
8. **Parity oracles compare full item JSON**, modulo an explicit, documented allowlist. An
   ID-set oracle would have passed while the ready endpoint shipped bare `Issue` against the
   CLI's `IssueWithCounts`.
9. **Defaults come from one shared constant per surface pair.** List default limit is 50
   (`cmd/bd/list.go:743`), ready default is 100 (`cmd/bd/ready.go:796`); the HTTP defaults are
   the same constants imported from the shared package, never re-stated literals.
10. **The mode gate does not over-promise.** Gate on SQL-server-ness (`usesSQLServer()`,
    `cmd/bd/store_factory.go:20`), wire the existing exported providers for server and
    external-server modes in the slice that claims them, and make every refusal message describe
    exactly what is refused and why.
11. **Extractions land as separate no-op refactor slices**, each verified against the byte-pinned
    protocol corpus at `cmd/bd/protocol`, so any semantic change is bisectable to one move.
12. **Document only the status codes the six operations need.** Every documented status+code pair
    is permanent wire surface; no 413/429/504/403-read_only for mechanisms v0 does not ship.
13. **Detail assembly is shared.** Labels + dependencies-with-metadata + counts +
    comments-omitted semantics exist once, consumed by `bd show` (both variants) and the HTTP get
    handler — the anti-drift slice must not ship a new copy of the very shaping logic that
    drifted in the hosted viewer.
14. **No invented seam capabilities.** Every mechanism a section leans on must be grep-verified.
    Known false claims from drafts, excluded here: "workspacegate lease acquired with the
    provider" (only `cmd/bd/doctor/gitignore.go` touches `internal/workspacegate` outside the
    package itself); "every Port method maps 1:1 onto both seams"; "UOW GetIssue wraps
    `storage.ErrNotFound`" (it surfaces `sql.ErrNoRows`, see rule 4).
15. **One CLI-side default stays out of the library.** The cwd-derived directory-label
    substitution (`cmd/bd/ready.go:141-145`) is meaningless in a server process; CLI callers
    apply it to the request before invoking the shared builder, and the server never does.

### What this does NOT solve

Three known gaps are adjacent to this design and deliberately not closed by it. For each: what
the gap is, whether this design leaves it on a credible path, and how it is tracked. None of
these is silently "handled".

#### The other ~46 direct/proxied command pairs

`cmd/bd` has 49 `*_proxied_server.go` duals (34 on the UOW seam); this slice migrates the four
backing the MVP operations and leaves ~45 duals in place, still duplicated, still able to drift.
What this design does for them: it establishes the pattern (pure, seam-independent filter/default
builders and shaping functions; per-seam execution as one-line calls; `uow.RunTxResult` as the
single transactional protocol) and proves it on the four commands with the worst verified
duplication — including retiring the one bespoke retry loop (`update_proxied_server.go:100-116`)
as the exemplar for write-shaped duals. The path is credible for any command whose duplication is
filter/shape-shaped (most of the 34 UOW duals), because the extraction is a move plus two
one-line call sites, verifiable against the protocol corpus. The path is NOT yet paved for
commands whose duplication is multi-step transaction orchestration; those are the trigger for
introducing shared execution machinery later — explicitly not before. Tracked: each future
command-family migration is filed as its own bead (one family per issue, dependency-linked to
the extraction slices of this design), and each lands as a separate no-op refactor slice per
rule 11. This design commits to the pattern and the first four instances, not to a schedule for
the rest, and does not claim the pattern is proven beyond what it ships.

#### The gascity/beads fork rebase and its Postgres conformance

Gas City consumes a fork (`gascity/beads @ feat/hosted-multibackend`, pinned via a go.mod
replace), and the fork carries a Postgres backend that implements `storage.Storage` but not the
UOW seam. Two consequences this design does not fix: (1) the fork must rebase over this work —
new shared packages, a deleted `cmd/bd/list_filter.go`, and modified call sites in the touched
`cmd/bd` files; (2) `bd serve` on the fork's Postgres backend will refuse with the typed
`storage.ErrUnsupported` until the fork either grows a Postgres UOW provider or adapts the gate,
because the decided parameter 5 makes the server UOW-only. What this design does to keep the path
credible: extractions are moves rather than rewrites (minimizing rebase conflict surface), every
slice PR lists its deleted/renamed files, and the CLI-vs-HTTP parity oracle is written as a
seam-parametrized scenario table so the fork can add its backend as another row rather than
writing a new harness. Tracked: as downstream-dependency beads on the fork side (rebase; Postgres
UOW conformance decision), referenced from this design's delivery epic — per decided parameter 8
they are dependencies of adoption, not in-slice work, and nothing in this repo blocks on them.

#### The unmerged `feat/public-issueops-facade` branch

The branch (at `/data/projects/beads-public-issueops-simple`) is 35 commits ahead of origin/main
at base `8bb0d36be` — current-main lineage; the earlier "32 ahead / 12 behind @ `53f3424cd`"
figure is stale. It defines a public WRITE vocabulary whose binding contract is
`issueops.Lifecycle` {Create, Update, Close, Reopen} — the rename from `Operations` is binding
per the facade's final design spec (`/var/tmp/w46-final-design.md`), not yet in code at branch
HEAD, so cite the spec, not branch line numbers — with `UpdateRequest.Claim` CAS semantics. The
facade mints NO error sentinels of its own: it relocates the existing 18 storage/domain error
symbols into the leaf `issueops` package and aliases them back from `internal/storage` with the
same pointer, so existing `errors.Is` keys keep working unedited. Nor is it consumer-free: the
branch routes the four direct CLI write verbs through the facade via
`cmd/bd/issueops_adapter.go`. Its proxied-server rewire is the facade's own deferred follow-up
PR, gated by its spec on proxied contract-pin tests being committed and green first — a
precondition slices 1-4 of this plan partly supply.

Two binding decisions are now OWNER-DECIDED (rationale, strongest objection, and merge order in
"Reconciliation with the issueops Lifecycle refactor" near the end of this document):

1. **`issueops.Reader` is the public read ROLE** — the read counterpart of `Lifecycle`, exposed
   via `IssueReader()` accessors following the facade's role/accessor convention, with
   `internal/workapi` as the single shared implementation substrate behind every Reader.
2. **Subsume, not compose** — filter/default construction happens once, inside the Reader
   implementations, behind the accessor; both front doors can only say `rd.List(ctx, req)`.

The claim write still does not wait for the facade: it uses `domain.IssueUseCase.ClaimIssue` via
`workapi.ClaimOnUOW`, on main today and deliberately below `Lifecycle` (see § 2, claim-outcome
typing, post-facade justification). The read-endpoint slices, however, now gate on the facade's
API-shape phase merging to OSS main (the leaf `issueops` package must exist before `Reader` can
be declared) — a schedule coupling flagged in § 5 Risks and open questions rather than hidden.
The composition obligation stands: the shared contract's vocabulary reuses the existing
`storage.Err*` sentinels and CAS-outcome shapes rather than minting new types.
One ordering decision is made now to pre-empt a future fight: the HTTP 409 `code` strings become
frozen wire surface the moment Gas City consumes them, so if the facade lands with a conflicting
claim vocabulary, the facade adapts to the wire — not the reverse. Tracked: a coordination bead
linking the facade branch to the claim-endpoint slice, checked before that slice ships (the
facade owner reviews the claim endpoint's documented semantics against `UpdateRequest.Claim`);
plus the standing note that any future write endpoints (non-goal above) are specified against the
facade's `Lifecycle` vocabulary once it merges, not invented independently.

## 2. The shared library contract

### Scope: what is in the contract, and the explicit UOW-vs-Storage resolution

The HTTP API fronts UOW only (decided), but the direct CLI has no UOW provider (`uowProvider` is
built only in proxied mode, cmd/bd/main.go:1378-1388) and the proxied CLI has no `storage.Storage`,
so an execution-interface contract over either seam would exile one CLI family. The resolution:
**the contract contains no execution layer at all** (binding rule 6). Both seams already implement
every MVP query with one-line call sites — `store.GetReadyWorkWithCounts` returns
`[]*types.IssueWithCounts` (internal/storage/storage.go:168) while
`uw.IssueUseCase().GetReadyWorkWithCounts` returns `domain.SearchCountsPage{Items, HasMore}`
(internal/storage/domain/issue.go:140-143, :261) — and that mismatch never enters the contract
because the contract stops one call before it.

**IN the contract** (`internal/workapi`, new package):

1. Filter/default construction: `BuildReadyFilter` (collapsing the duplicated ~110-line builders at
   cmd/bd/ready.go:107-218 and ready_input.go:28-174) and `BuildListFilter` (the ~200-line
   `buildListFilter` moved from cmd/bd/list_filter.go:119-314), plus the `ConfigSource` seam and
   its two adapters (moved from list_filter.go:48-117).
2. Shared default constants (rule 9): `DefaultReadyLimit = 100`, `DefaultListLimit = 50`, and
   `types.WireSchemaVersion = 1` (new `internal/types/wire.go`, consumed by cmd/bd/output.go:12's
   `JSONSchemaVersion` and by the `/v0/beads/context` handler).
3. Not-found normalization (rule 4): `IsNotFound` / `ResolveIssue`.
4. Detail assembly (rule 13): `DetailSource` + `BuildIssueDetails`, replacing the duplicated blocks
   at cmd/bd/show.go:144-241 and show_proxied_server.go:478-556 (+ helpers :127-168).
5. Claim-outcome typing: `ClaimOutcome`, `ClaimConflictError`, `ClassifyClaimError`, per-attempt
   `ClaimOnUOW` — the retry/commit protocol is explicitly NOT here (rule 5; it is `uow.RunTxResult`,
   internal/storage/uow/tx.go:76-121, already consumed by proxied `bd ready --claim` at
   ready_proxied_server.go:184).

**Deliberately LEFT on the two existing seams / in cmd/bd** (each with the reason):

- **Query execution and page shapes.** `[]*types.IssueWithCounts` vs `domain.SearchCountsPage`
  stays per-seam; an execution Port would exist only to launder that mismatch and would force the
  limit+1 over-fetch onto the direct path (rule 6). Each caller executes its seam's method itself.
- **Overflow/truncation detection.** Direct CLI: `withFetchOneExtra` + `effectiveLimit` trim +
  `PaginationMeta` (cmd/bd/list.go:42-76, :587-605); UOW/HTTP: `SearchCountsPage.HasMore`. Two
  detectors remain per-seam by design; tied together only by the parity oracle (gap G4).
- **cwd-derived directory-label scoping** (ready.go:141-145, ready_input.go:101-105). Callers
  pre-fill `ReadyParams.LabelsAny`: the CLI applies `config.GetDirectoryLabels()`, the server never
  does — a server process's cwd is meaningless. The one caller-side default; the builder cannot
  reach it (import policy below bans that accessor).
- **Fuzzy/substring ID resolution and cross-repo routing** (`resolveAndGetIssueWithRouting`,
  `openRoutedReadStore`): interactive CLI affordances. The contract takes exact IDs only.
- **MaxRows resolution and proxied rejection**: `resolveMaxRows` /
  `rejectMaxRowsUnderProxiedServer` stay cobra-side (cmd/bd/max_rows.go:35-106; list.go:522,
  ready.go:57-70). `ReadyParams` carries the fields; who fills them is seam policy.
- **Output envelopes**: `outputJSONWithPagination`, `BD_JSON_ENVELOPE`, stderr hints, the `bd show`
  array envelope — byte-pinned by the cmd/bd/protocol corpus, CLI-only. HTTP envelopes are the
  httpapi layer's generated types.
- **The claim retry/commit protocol**: `uow.RunTxResult` (rule 5); the contract types the outcome,
  never the transaction lifecycle.
- **The keyset cursor token codec**: `ListParams` accepts the decoded position
  (`AfterCreatedAt`/`AfterID`, 1:1 onto `types.IssueFilter`'s keyset fields, types.go:1618-1633);
  token encode/decode is HTTP-layer (§ 3, "Pagination").

### The Reader role — the public read seam (owner-decided; reconciliation section)

`issueops.Reader` is the guarded issue-query role: the read counterpart of `Lifecycle`. It lives
in the leaf `issueops` package at the repo root (imports `internal/types` + stdlib only —
verified leaf-legal: `types.IssueWithCounts`, `types.IssueDetails`, `types.MolType`,
`types.WispType` all live in `internal/types`). One method per read operation; each takes the
high-level request and performs filter/default construction INTERNALLY (subsume). No
constructor; the accessor is the API.

```go
type Reader interface {
    Ready(ctx context.Context, req ReadyRequest) (IssuePage, error)
    List(ctx context.Context, req ListRequest) (IssuePage, error)
    Get(ctx context.Context, req GetRequest) (*types.IssueDetails, error)
}

type ReadyRequest struct { // field-for-field the vocabulary slice 2 built as workapi.ReadyParams
    IssueType string; Assignee string; Unassigned bool
    Labels, LabelsAny, ExcludeLabels []string   // raw; normalized inside
    LabelPattern, LabelRegex string
    Priority *int                               // nil = unset (replaces Priority+PrioritySet pair)
    ParentID string; MolType *types.MolType
    IncludeDeferred, IncludeEphemeral bool
    ExcludeTypes []string
    MetadataFields map[string]string; HasMetadataKey string
    Sort string                                 // "" = hybrid; validated inside
    Limit *int                                  // nil = default 100; 0 = unlimited (CLI-identical)
    Offset int                                  // honored where the backend supports it
}
// NOTE: MaxRows is NOT on the contract — the ready extraction as built keeps it CLI-side, and
// serve's --max-rows (open question 2) is an httpapi-layer operator clamp, not a query field.

type ListRequest struct {
    // field-for-field workapi.ListParams as built on refactor/bd-fv4-workapi-list-filter
    // (Status..IDFilter strings; label trio + pattern/regex; *Contains fields; the ten
    // *time.Time windows; EmptyDesc/NoAssignee/NoLabels/SkipLabels; priority trio;
    // Pinned/Include* flags; ExcludeTypes; ParentID/NoParent; MolType/WispType;
    // Deferred/Overdue; MetadataFields map[string]string; HasMetadataKey; AllFlag/ReadyFlag;
    // SortBy/Reverse; Offset) with exactly TWO deltas:
    Limit *int                 // nil = default 50; 0 = unlimited — REPLACES ListParams.SQLLimit;
                               // the sort-pushdown/fetch-all-and-trim decision moves INSIDE the impl
    AfterCreatedAt *time.Time  // decoded keyset position; token codec stays HTTP-layer
    AfterID string
}

type GetRequest struct {
    ID string               // exact canonical ID; issue-to-wisp fallback happens inside (ResolveIssue)
    IncludeDependents bool  // bd show --include-dependents; HTTP passes zero-value in v0
    IncludeComments   bool  // bd show --include-comments; HTTP passes zero-value in v0
}

type IssuePage struct {     // one page type: ready and list both carry IssueWithCounts (rule 3)
    Items   []*types.IssueWithCounts
    HasMore bool
}
```

`ReadyRequest` and `ListRequest` derive field-for-field from the workapi params structs the
extraction slices build; the deltas are exactly the three listed above (`Limit *int` replacing
default/SQLLimit mechanics, the keyset pair on `ListRequest`, `Priority *int` replacing the
`Priority`+`PrioritySet` pair). In the Reader slice the workapi builders are re-typed to accept
these leaf request types, so no duplicated params struct exists to drift.

**Operation-to-method map.**

| Operation | Reader method |
|---|---|
| `GET /v0/beads/ready` | `Reader.Ready` |
| `GET /v0/beads/issues` | `Reader.List` |
| `GET /v0/beads/issues/{id}` | `Reader.Get` |
| `GET /healthz` | none — process liveness, no DB touch |
| `GET /v0/beads/context` | none — contextinfo snapshot, not an issue query |
| `POST /v0/beads/issues/{id}:claim` | none — a WRITE; stays on `domain.IssueUseCase.ClaimIssue` via `workapi.ClaimOnUOW` inside caller-owned `uow.RunTxResult`, BELOW the facade (see claim-outcome typing) |

**Accessors — exactly the Lifecycle pattern** (`IssueReader` grep-verified free in both trees;
no collision with the internal `IssueLifecycleStore` lane):

```go
// internal/storage
type Storage interface { /* existing 71 */
    IssueLifecycle() (issueops.Lifecycle, error)
    IssueReader()    (issueops.Reader, error)
}
// internal/storage/dolt — implementation is NOT leaf; imports workapi freely
func (s *DoltStore) IssueReader() (issueops.Reader, error) { return &storeReader{st: s}, nil }
// internal/storage/embeddeddolt (cgo file; !cgo stub mirrors whatever IssueLifecycle does)
func (s *EmbeddedDoltStore) IssueReader() (issueops.Reader, error) { return &storeReader{st: s}, nil }
// internal/storage — reads fire no hooks: passthrough BY RECURSION, accessor exists for seam uniformity
func (h *HookFiringStore) IssueReader() (issueops.Reader, error) { return h.inner.IssueReader() }
// internal/telemetry — adds its layer by recursion (read spans; plain passthrough if none exist today)
func (s *InstrumentedStorage) IssueReader() (issueops.Reader, error) {
    inner, err := s.Unwrap().IssueReader(); if err != nil { return nil, err }
    return s.WrapIssueReader(inner), nil
}
// internal/storage/uow — the provider grows the same accessor; EACH METHOD OPENS EXACTLY ONE UOW,
// runs the whole workapi pipeline inside it, and closes (detached-close protection lives here).
func (p *<providerType>) IssueReader() (issueops.Reader, error) { return &uowReader{p: p}, nil }
```

**Transactions: one UOW per Reader method.** Because the methods are request-granular, the uow
reader is one-UOW-per-call — which IS this proposal's one-UOW-per-request shape (this dissolves
the council's transaction-fragmentation objection, which targeted a fine-grained compose-shaped
Reader). The detached-close protection moves inside the uow reader. Implementation internals:

- `uowReader.List`: `NewUOW` → `workapi.NewUOWConfigSource(uw)` → `LoadListConfig` →
  `BuildListFilter` → `IssueUseCase().SearchIssuesWithCounts` → rewrap `domain.SearchCountsPage`
  as `issueops.IssuePage`.
- `storeReader.List`: `workapi.NewStoreConfigSource(st)` → `LoadListConfig` → `BuildListFilter`
  → limit+1 over-fetch (or fetch-all for DB-inexpressible sorts, the logic today at
  `cmd/bd/list.go:42-76`) → trim + `HasMore`.
- `Ready`: `workapi.BuildReadyFilter` → `GetReadyWorkWithCounts` per seam; the store side
  synthesizes `HasMore` by over-fetch, the uow side has it natively.
- `Get`: `workapi.BuildIssueDetails` over workapi's `NewStoreDetailSource` / `NewUOWDetailSource`
  (unchanged from the detail extraction) inside the one UOW; not-found normalization (rule 4)
  now sits doubly behind the accessor.

`ConfigSource` is supplied by the implementation from what it already holds —
`NewStoreConfigSource(s.st)` in dolt/embedded, `NewUOWConfigSource(uw)` around the per-call UOW
— never by front doors, which is the drift kill: handlers and RunE bodies can only say
`rd.List(ctx, req)`; the skip-the-ritual failure mode is unwritable.

### Package path, file layout, import policy

`internal/workapi` — "the work-surface API both frontends delegate to". Verified free (no
collision; `internal/query` and `internal/storage/issueops` are taken). It stays `internal/`
PERMANENTLY as the shared implementation substrate behind every `Reader` — the room behind every
door. The public promotion is the `issueops.Reader` role (decided, not hypothetical — see "The
Reader role" above), never methods on `Lifecycle`; the builders are re-typed to accept the leaf
request types in the Reader slice so there is no duplicated params struct to drift. Its
vocabulary is facade-consistent (existing `storage.Err*` sentinels re-used, never new ones;
plain param structs; CAS-style outcomes).

```
internal/types/wire.go            WireSchemaVersion = 1 (new; output.go:12 becomes = types.WireSchemaVersion)
internal/workapi/doc.go           package rationale + import policy
internal/workapi/defaults.go      DefaultReadyLimit, DefaultListLimit
internal/workapi/ready.go         ReadyParams, BuildReadyFilter
internal/workapi/list.go          ListParams, ListConfig, ConfigSource, NewStoreConfigSource,
                                  NewUOWConfigSource, LoadListConfig, BuildListFilter (+ unexported
                                  applyStatusFilter, validStatusList, parseMetadataFields)
internal/workapi/notfound.go      IsNotFound, NotFound, IssueSource, ResolveIssue
internal/workapi/detail.go        DetailSource, DetailOptions, BuildIssueDetails
internal/workapi/detail_store.go  NewStoreDetailSource (over storage.DoltStorage)
internal/workapi/detail_uow.go    NewUOWDetailSource (over uow.UnitOfWork)
internal/workapi/claim.go         ClaimOutcome, ClaimErrorKind, ClaimConflictError,
                                  ClassifyClaimError, ClaimOnUOW
internal/workapi/*_test.go        moved list_filter_status_test.go + ready_input_test.go cases,
                                  golden equality tables, new table-driven tests
```

cmd/bd deltas: `list_filter.go` deleted (moved); `ready.go` inline builder :107-218 deleted (the
direct path routes through `gatherReadyInput` as proxied already does at ready_proxied_server.go:18)
and `ready_input.go` thins to flags→`ReadyParams`; the `show.go`/`show_proxied_server.go` assembly
blocks and five `proxied*` detail helpers deleted in favor of `BuildIssueDetails`;
`update_proxied_server.go`'s bespoke backoff loop (:93-125) rewritten onto `uow.RunTxResult`;
`output.go` pins `JSONSchemaVersion` to the shared constant.

Import policy (workapi imports exactly):
`internal/types`, `internal/storage` (sentinels, `ValidateMetadataKey`, `DoltStorage`),
`internal/storage/uow`, `internal/storage/domain` (`DepListFilter`), `internal/utils`,
`internal/config` (ONLY `GetCustomTypesFromYAML`, the workspace-scoped custom-types fallback that
`loadListFilterConfig` already performs at list_filter.go:91-95 — workspace config is meaningful in
a single-workspace server; client-cwd-derived reads like `GetDirectoryLabels` are banned),
`database/sql` (ErrNoRows). No cobra, no net/http, no `os.Getenv`.

Enforcement is mechanical, matching the repo's existing idiom for the charter's storage boundary
(`.golangci.yml` depguard rule `dolt-storage-boundary`), not review-and-doc-comment:

- **`.golangci.yml` depguard rule `workapi-frontend-boundary`**: for
  `**/internal/workapi/**` files, deny `github.com/spf13/cobra` and `net/http` with a desc
  pointing at this policy — the package is the shared substrate under both frontends and must
  not know either exists.
- **`scripts/ci/pr-policy.sh` grep** for the symbol-level half depguard cannot express (workapi
  legitimately imports `internal/config` for `GetCustomTypesFromYAML`): fail the PR if
  `internal/workapi/` matches `config\.GetDirectoryLabels|os\.Getenv` — cwd/env-derived reads
  are meaningless in a server process (rule 15).

`doc.go` still states the rationale, but the boundary itself fails CI, not review attention.

### Shared default constants (rule 9)

```go
package workapi

const (
    DefaultReadyLimit = 100 // was the literal in cmd/bd/ready.go:796
    DefaultListLimit  = 50  // was the literal in cmd/bd/list.go:743
)
```

Cobra flag registration changes to `IntP("limit", "n", workapi.DefaultReadyLimit, ...)` /
`workapi.DefaultListLimit`; the HTTP query decoder maps an absent `limit` to `nil`, and the builders
default `nil` to the same constants. `limit=0` means **unlimited on both surfaces, identically**
(rule 2): the builders pass 0 through untouched, and `WorkFilter.Limit==0` / `IssueFilter.Limit==0`
already mean unlimited at both seams. No clamp, no 400, anywhere in the contract. Alongside these,
`internal/types/wire.go` declares `const WireSchemaVersion = 1` — the one schema_version constant,
consumed by the CLI JSON envelope (output.go's `JSONSchemaVersion`) and `GET /v0/beads/context`.

### Filter and default builders

```go
// ready.go
type ReadyParams struct {
    Assignee               string
    Unassigned             bool
    Type                   string   // raw; Build normalizes via utils.NormalizeIssueType
    ExcludeTypes           []string // raw, comma-splittable; Build splits + normalizes
    Labels                 []string // AND; Build normalizes via utils.NormalizeLabels
    LabelsAny              []string // OR; CLI pre-fills cwd directory labels, server never does
    ExcludeLabels          []string
    LabelPattern, LabelRegex string
    Priority               *int     // nil = unset (preserves P0-vs-unset, ready.go:179-182)
    ParentID               string
    MolType                string   // Build validates via types.MolType.IsValid
    MetadataFields         []string // raw "k=v"; Build parses via parseMetadataFields
    HasMetadataKey         string   // Build validates via storage.ValidateMetadataKey
    Sort                   string   // "" => hybrid; validated via types.SortPolicy.IsValid
    Limit                  *int     // nil => DefaultReadyLimit; 0 => unlimited (CLI-identical)
    Offset                 int      // honored under the UOW seam only (proxied CLI today)
    IncludeDeferred        bool
    IncludeEphemeral       bool
    MaxRows                int      // direct CLI fills (resolveMaxRows); proxied rejects the flag; HTTP leaves 0 in v0
    MaxRowsSource          string
}

// BuildReadyFilter is the ONE place ready defaults exist. It hard-codes
// Status:"open" (today duplicated at ready.go:162 and ready_input.go:118 —
// and the reason the hosted viewer's empty WorkFilter silently meant
// open+in_progress). Also: label normalization, exclude-type splitting,
// metadata parsing/validation, sort validation, limit defaulting.
func BuildReadyFilter(p ReadyParams) (types.WorkFilter, error)
```

```go
// list.go
// ListParams is the field-for-field port of cmd/bd/list_input.go's listInput
// (:18-96) minus presentation-only fields (longFormat, prettyFormat,
// flatFormat, depsMode, watchMode, noPager, formatStr, jsonOutput,
// repoOverride, limitChanged, effectiveLimit), with exactly three changes:
//   Limit *int                 // nil => DefaultListLimit; 0 => unlimited
//   MetadataFields []string    // raw "k=v"; parse moves from gatherListInput into Build
//   AfterCreatedAt *time.Time; AfterID string // decoded keyset position ->
//                              // IssueFilter.AfterCreatedAt/AfterID (types.go:1618-1633)
// All other fields keep listInput's names (exported) and types verbatim:
// Status..IDFilter strings; label trio + pattern/regex; *Contains fields; the
// ten *time.Time windows; EmptyDesc/NoAssignee/NoLabels/SkipLabels;
// Priority/Min/Max (+Set flags); PinnedFlag/NoPinnedFlag/Include{Templates,
// Gates,Infra}; ExcludeTypeStrs; ParentID/NoParent; MolType/WispType;
// DeferredFlag/OverdueFlag; HasMetadataKey; AllFlag/ReadyFlag; Offset; SortBy/Reverse.
type ListParams struct { ... }

type ListConfig struct { /* moved listFilterConfig: customStatuses, customTypes, infraSet + methods */ }

type ConfigSource interface { // moved listFilterConfigSource, list_filter.go:48-52
    GetCustomStatuses(ctx context.Context) ([]types.CustomStatus, error)
    GetCustomTypes(ctx context.Context) ([]string, error)
    GetInfraTypes(ctx context.Context) (map[string]bool, error)
}
func NewStoreConfigSource(st storage.DoltStorage) ConfigSource // moved directConfigSource :54-64
func NewUOWConfigSource(uw uow.UnitOfWork) ConfigSource        // moved proxiedConfigSource :66-76
func LoadListConfig(ctx context.Context, src ConfigSource) (ListConfig, error) // moved :78-106

// BuildListFilter is the verbatim move of buildListFilter (list_filter.go:119-314):
// closed + custom done/frozen exclusion, Pinned=false, IsTemplate=false, gate
// exclusion, infra-type exclusion, SkipWisps, with the same opt-outs
// (AllFlag, IncludeGates, IncludeInfra, IncludeTemplates).
func BuildListFilter(p ListParams, cfg ListConfig) (types.IssueFilter, error)
```

Builder error strings are the canonical copies of today's messages (e.g. `invalid sort policy '%s'.
Valid values: hybrid, priority, oldest`); the CLI wraps them with `HandleErrorRespectJSON("%v", err)`
so corpus error output stays byte-identical; the HTTP layer maps them to 400. `ConfigSource` is one
of only two interfaces in the package; both satisfy the rule-6 test — two implementations exist on
main today (list_filter.go:54-76). Post-reconciliation, HTTP handlers never wire a `ConfigSource`:
the `uowReader` supplies `NewUOWConfigSource(uw)` internally around its per-call UOW (§ 2, "The
Reader role"), and the handler only says `rd.List(ctx, req)`.

### Not-found normalization (rule 4)

The two seams miss differently, verified: the direct seam returns wrapped `storage.ErrNotFound` and
self-routes wisps (`DoltStore.GetIssue` → `storageissueops.GetIssueInTx`,
internal/storage/issueops/get_issue.go:18-35); the UOW seam's
`db.issueSQLRepositoryImpl.Get` returns `(nil, sql.ErrNoRows)`
(internal/storage/domain/db/issue.go:394-410) which the domain layer wraps with `"get %s: %w"`
(domain/issue.go:387-396) — it is NEVER `storage.ErrNotFound`. The CLI's own proxied code already
treats both sentinels as not-found (`uowIssueExists`, cmd/bd/create_proxied_server.go:495-499).
The contract makes that the law:

```go
// notfound.go

// IsNotFound reports the miss condition across both seams:
// wrapped storage.ErrNotFound (direct) or wrapped sql.ErrNoRows (UOW).
func IsNotFound(err error) bool {
    return errors.Is(err, storage.ErrNotFound) || errors.Is(err, sql.ErrNoRows)
}

// NotFound is the canonical normalized miss: errors.Is(NotFound(id), storage.ErrNotFound).
func NotFound(id string) error { return fmt.Errorf("%w: issue %s", storage.ErrNotFound, id) }

// IssueSource is the resolution slice of both seams. DetailSource embeds it.
type IssueSource interface {
    GetIssue(ctx context.Context, id string) (*types.Issue, error)
    GetWisp(ctx context.Context, id string) (*types.Issue, error)
}

// ResolveIssue is THE shared exact-ID issue→wisp resolution (the
// proxiedGetIssueOrWisp protocol, show_proxied_server.go:127-140, promoted):
// GetIssue; on miss — IsNotFound(err) OR (nil issue, nil err) — try GetWisp;
// on double miss return (nil, false, NotFound(id)). A non-miss error passes
// through UNCHANGED and is never normalized to not-found: a dropped
// connection can never read as 404, nor a missing issue as 500.
func ResolveIssue(ctx context.Context, src IssueSource, id string) (issue *types.Issue, isWisp bool, err error)
```

Normalization lives HERE, in the shared function every caller goes through — the HTTP get/claim
handlers, `BuildIssueDetails`, and the migrated proxied show/update paths — not in the HTTP error
mapper alone. The mapper keys 404 exclusively on `errors.Is(err, storage.ErrNotFound)`, the only
way a miss can surface after normalization. `isWisp` is a routing hint for seams with per-table
APIs (UOW); the direct adapter self-routes and ignores it (its `GetIssue` already probes both
tables; its `GetWisp` returns `NotFound(id)` immediately, reporting `isWisp=false`).

### Shared detail assembly (rule 13)

```go
// detail.go
type DetailSource interface {
    IssueSource
    Labels(ctx context.Context, id string, isWisp bool) ([]string, error)
    // Dependencies: outgoing edges, full items with dep metadata.
    Dependencies(ctx context.Context, id string, isWisp bool) ([]*types.IssueWithDependencyMetadata, error)
    // DependentsShallow: incoming edges projected to the 5-field shallow shape
    // (ID, Status, IssueType, Priority, Title) + DependencyType — the projection
    // both existing paths already emit (show.go:168-181, show_proxied_server.go:
    // 500-512); part of the adapter contract so the direct side keeps streaming via Iter.
    DependentsShallow(ctx context.Context, id string, isWisp bool) ([]*types.IssueWithDependencyMetadata, error)
    CountDependents(ctx context.Context, id string, isWisp bool) (int64, error)
    CountDependencies(ctx context.Context, id string, isWisp bool) (int64, error)
    CountComments(ctx context.Context, id string, isWisp bool) (int64, error)
    Comments(ctx context.Context, id string, isWisp bool) ([]*types.Comment, error)
}

func NewStoreDetailSource(st storage.DoltStorage) DetailSource
func NewUOWDetailSource(uw uow.UnitOfWork) DetailSource

type DetailOptions struct {
    IncludeDependents bool // bd show --include-dependents (CLI); HTTP passes zero-value opts in v0
    IncludeComments   bool // bd show --include-comments (CLI); HTTP passes zero-value opts in v0
}
// The HTTP GET /v0/beads/issues/{id} handler passes a zero-value GetRequest to
// Reader.Get in v0 (the uow reader calls BuildIssueDetails with DetailOptions{})
// — mirroring bd show's count-only default; no include_*
// query params ship (§ 3 exposes none; open question 3 covers a comments knob).

// BuildIssueDetails is the single assembly: ResolveIssue → Labels →
// Dependencies → three counts (count-only default, be-ijck6q) → optional
// DependentsShallow + epic progress (EpicTotal/Closed/Closeable from
// parent-child deps) → optional Comments, else CommentsOmitted=true iff
// CommentCount>0 (ga-clgh) → Parent = first DepParentChild dependency.
// A miss returns the normalized NotFound error.
func BuildIssueDetails(ctx context.Context, src DetailSource, id string, opts DetailOptions) (*types.IssueDetails, error)
```

Adapter method mapping — every row grep-verified on main (rule 14), no invented capability:

| DetailSource method | Store adapter (self-routing; ignores isWisp) | UOW adapter (explicit axis) |
|---|---|---|
| GetIssue / GetWisp | `st.GetIssue` (GetIssueInTx probes issues then wisps) / immediate NotFound | `IssueUseCase().GetIssue` / `GetWisp` |
| Labels | `st.GetLabels` (GetLabelsInTx table="" self-routes, internal/storage/issueops/labels.go:14-18) | `LabelUseCase().GetLabels` / `GetWispLabels` |
| Dependencies | `st.GetDependenciesWithMetadata` (isActiveWisp self-route, dolt/dependencies.go:156-159) | `DependencyUseCase().ListWithIssueMetadata` / `ListWispWithIssueMetadata`, `DepListFilter{Direction: Out}` |
| DependentsShallow | `st.IterDependentsWithMetadata` + shallow projection (today's show.go:161-186 moved) | `ListWithIssueMetadata`/wisp variant, `Direction: In`, + projection (today's :498-512 moved) |
| CountDependents / CountDependencies | `st.CountDependents` / `CountDependencies` (aggregate permanent+wisp tables, dolt/counts.go:60-71) | `CountByIssueID` / `CountByWispID` with `Direction: In` / `Out` |
| CountComments | `st.CountIssueComments` | `CommentUseCase().CountCommentsForIssue` / `CountCommentsForWisp` |
| Comments | `st.IterIssueComments` materialized (today's show.go:210-223 moved) | `GetCommentsForIssue` / `GetCommentsForWisp` |

Consumers after migration: direct `bd show` (adapter over its routed `result.Store`), proxied
`bd show` (UOW adapter; `proxiedBuildDetails` and the five helpers deleted), and the HTTP
`GET /v0/beads/issues/{id}` handler — three consumers, one assembly. CLI-only concerns stay
outside: fuzzy resolution feeds the exact ID in, the array-of-details envelope wraps the result,
and `--thread`/`--refs`/`--children`/`--as-of`/watch mode are untouched.

### Claim-outcome typing (and the one retry/commit implementation, rule 5)

The CAS already has one implementation per seam with identical semantics —
`domain.(*issueUseCaseImpl).claim` (issue.go:458-492) and `storageissueops.ClaimIssueInTx`
(`internal/storage/issueops`, the INTERNAL package — distinct from the public leaf `issueops`;
the facade branch's `storageissueops` import-alias convention is adopted in prose throughout) —
both wrapping `storage.ErrAlreadyClaimed`/`ErrNotClaimable` (post-facade, the canonical
declarations live in the leaf `issueops` package via the errors relocation, aliased back from
`internal/storage` with the same pointer — `errors.Is` keys on `storage.Err*` keep working
unedited). The direct seam's
`ClaimIssue(ctx, id, actor) error` (bulk_issues.go:16) yields only sentinels; the domain seam
returns `ClaimResult{AlreadyClaimed, PriorAssignee}` (issue.go:222-225). The contract adds typing
and one per-attempt helper — no transaction lifecycle:

```go
// claim.go
type ClaimOutcome struct {
    Issue        *types.Issue // fresh same-transaction re-read after the CAS landed
    AlreadyYours bool         // domain ClaimResult.AlreadyClaimed: idempotent same-actor re-claim
}

type ClaimErrorKind int
const (
    ClaimErrNone           ClaimErrorKind = iota
    ClaimErrNotFound       // errors.Is storage.ErrNotFound (post-normalization)
    ClaimErrAlreadyClaimed // errors.Is storage.ErrAlreadyClaimed
    ClaimErrNotClaimable   // errors.Is storage.ErrNotClaimable
    ClaimErrWriteConflict  // uow.IsSerializationError (retries exhausted)
)

// ClassifyClaimError is the errors.Is dispatch both surfaces use — the typed
// replacement for downstream substring matching on "already claimed by".
func ClassifyClaimError(err error) ClaimErrorKind

// ClaimConflictError carries structured Holder/Status for a lost CAS, read
// back in the SAME transaction — never parsed from message text. Unwrap()
// returns the sentinel chain, so errors.Is(err, storage.ErrAlreadyClaimed)
// keeps working; CLI output never sees this type and is unaffected.
type ClaimConflictError struct {
    Kind   ClaimErrorKind // ClaimErrAlreadyClaimed | ClaimErrNotClaimable
    Holder string         // conflicting assignee ("" for not_claimable)
    Status string         // issue status at conflict time
    Err    error
}

// ClaimOnUOW is ONE ATTEMPT over one open UOW: ResolveIssue (shared dispatch
// + not-found normalization), then the matching CAS (IssueUseCase().ClaimIssue
// or ClaimWisp — the domain methods that expose AlreadyClaimed, which
// ApplyUpdate discards), then a fresh same-tx re-read for the response body.
// On a lost CAS it re-reads the row and returns *ClaimConflictError.
// NO retry, NO commit: callers run it inside uow.RunTxResult.
func ClaimOnUOW(ctx context.Context, uw uow.UnitOfWork, id, actor string) (ClaimOutcome, error)
```

The retry/commit protocol has exactly one implementation: **`uow.RunTxResult`**
(internal/storage/uow/tx.go:76-121 — serialization-error retry with backoff, permanent-error
short-circuit, nothing-to-commit tolerance, test-covered on main), already how proxied
`bd ready --claim` commits (ready_proxied_server.go:184-199). The HTTP claim handler is
`uow.RunTxResult(ctx, provider, func(ctx, uw) (workapi.ClaimOutcome, string, error) { o, err :=
workapi.ClaimOnUOW(ctx, uw, id, actor); ...; return o, "bd serve: claim <id> by <actor>", nil })`.
The migration slice rewrites `applyUpdateProxiedOne`'s bespoke `cenkalti/backoff` loop
(update_proxied_server.go:93-125) as `uow.RunTxResult` around the existing attempt body — after
which the bespoke loop is deleted and cannot drift because it does not exist. No `ClaimViaUOW`
wrapper is introduced (rule 5). `bd update --claim` keeps `ApplyUpdate(spec{Claim:true})` for
atomic claim+field updates; its per-attempt resolve (the GetIssue→GetWisp fallback at
update_proxied_server.go:146-158) migrates onto `ResolveIssue` so wisp dispatch is shared too.
Honest residue: `ApplyUpdate` internally decides wispness via `isWispID` (domain/issue.go:499) — a
pre-existing second wispness decision inside the domain layer, out of scope here; flagged as gap G5.

**Post-facade justification: why the claim endpoint stays BELOW `issueops.Lifecycle`** (recorded
so the choice is not re-litigated when the facade merges). Verified on the facade branch:
(a) `Lifecycle` verbs run their own `RunTxResult` with a fixed `"update issue"` commit message —
routing claim through `Lifecycle.Update{Claim: true}` would double-wrap the retry and lose the
`"bd serve: claim <id> by <actor>"` commit message; (b) `ApplyUpdate` discards domain
`ClaimResult.AlreadyClaimed`, which the wire's `already_claimed` field maps; (c) on a lost CAS
the facade returns only the sentinel with the transaction already rolled back, making the 409
`assignee`/`issue_status` same-transaction re-read extensions unreachable (message parsing is
forbidden); (d) per-issue commit granularity is moot for a single-issue claim — one commit
either way; (e) the facade's governing rule constrains the PUBLIC capability surface, not
internal seam consumers — `bd serve`'s claim rides the same seam proxied `bd update --claim`
uses today, and the facade itself defers the proxied rewire.

### Result-shaping vocabulary (what the wire carries, verified)

- **Ready items are `[]*types.IssueWithCounts`** (rule 3): direct `bd ready --json` at
  ready.go:266, proxied at ready_proxied_server.go:98. HTTP `/ready` carries the same items or it
  drifts at the field level (dependency_count/dependent_count/comment_count/parent,
  types.go:976-982) on day one.
- **List items are ALSO `IssueWithCounts`**: `bd list --json` calls `SearchIssuesWithCounts` on
  both paths (cmd/bd/list.go:576-578, list_proxied_server.go:79). `/issues` items are
  `IssueWithCounts`; a bare-`Issue` list would fail the full-item-JSON oracle immediately.
- **Detail is `types.IssueDetails`** (types.go:984-1013) from `BuildIssueDetails`, a single object
  over HTTP (the array is a CLI multi-arg envelope).
- **`skip_labels`/lite is not in the contract**: `IssueFilter.SkipLabels` sets `Issue.IsLitePartial`
  which is `json:"-"` — wire-indistinguishable from an empty issue. The CLI keeps
  `newSkipLabelsListJSONResponse` locally; HTTP does not expose it in v0 (consequence for the
  one downstream `--skip-labels` caller, the reconciler full scan: § 3, "what this list
  surface deliberately cannot serve").
- All three item types are marshaled from the canonical Go structs directly — the spec pins them via
  `x-go-type` (rule 1; mechanics in the spec/codegen section). No `convert.go`, no `apigen.Issue`
  mirror, no Go-to-Go mapping anywhere.

### What enforces CLI/HTTP parity — mechanism by mechanism

An unenforced convention is not a guarantee. For each shared behaviour, the concrete artifact:

| # | Shared behaviour | Enforcing artifact (type / function / named test) |
|---|---|---|
| 1 | Ready defaults (Status:"open", hybrid sort, normalization) | `workapi.BuildReadyFilter` — single implementation; both old builders deleted, so a second copy is a compile-time impossibility. Golden test `TestBuildReadyFilterGolden` (tables written FROM the old builders BEFORE deletion) pins old==new across an input matrix. |
| 2 | List exclusion defaults (~200 lines: closed/done/frozen, pinned, templates, gates, infra, wisps) | `workapi.BuildListFilter` — verbatim move, old file deleted; moved suite `list_filter_status_test.go` + golden test `TestBuildListFilterGolden`. |
| 3 | Default page sizes (ready 100, list 50) | `DefaultReadyLimit`/`DefaultListLimit` constants referenced by BOTH the cobra flag registrations and the HTTP param decoder (compile-time), plus `TestLimitDefaults_SingleSource` asserting each cobra `Flag.DefValue == strconv.Itoa(const)` (DefValue is a string snapshot and could otherwise drift). |
| 4 | `limit=0` = unlimited on both surfaces | Builders pass 0 through; seam semantics already unlimited. Enforced by the `limit=0` cases of `TestServerListCursorPagination` and the Tier-C parity oracles `TestProxiedServerServeReadyParity`/`TestProxiedServerServeListParity` (§ 5): HTTP `limit=0` output equals `bd list --limit 0 --json` / `bd ready --limit 0 --json` against seeded Dolt. |
| 5 | Wire item shape (Issue / IssueWithCounts / IssueDetails) | The canonical structs themselves via `x-go-type` (no second wire struct, rule 1) + two-way JSON-tag bijection test `TestWireTagBijection` (spec properties ↔ struct tags, set-equality both directions, explicit in-test allowlist) — a fixture round-trip is NOT relied on. Runs in the required PR-time path. |
| 6 | Not-found semantics (404 iff missing; 500 never masks as 404 and vice versa) | `workapi.ResolveIssue` + `IsNotFound` — normalization inside the shared function every caller uses, so no future caller can reintroduce the bug; HTTP mapper keys only on `errors.Is(err, storage.ErrNotFound)`. Named integration test `TestServerGetUnknownID404` against real Dolt (UOW seam, where the raw miss is wrapped `sql.ErrNoRows`) + unit `TestResolveIssue_MissShapes` covering {wrapped ErrNoRows, wrapped ErrNotFound, nil-issue-nil-error, non-miss error passthrough}. |
| 7 | Detail assembly (labels, deps-with-metadata, counts, comments-omitted, epic progress, parent) | `workapi.BuildIssueDetails` — three consumers, one implementation, old blocks deleted (compile-time). Full-item parity test `TestProxiedServerServeShowParity`: `GET /issues/{id}` JSON equals `bd show --json`[0] modulo documented allowlist; fixture set MUST include a wisp, an epic with children, and an issue with comments (count-only path). |
| 8 | Claim CAS semantics + conflict taxonomy | Single CAS in the domain / `internal/storage/issueops` layer (existing, per-seam); `workapi.ClaimOnUOW` + `ClassifyClaimError` for outcome/conflict typing; retry/commit solely `uow.RunTxResult` (bespoke loop deleted — compile-time). E2E `TestProxiedServerServeClaimCrossCheck`: HTTP claim then `bd update --claim` by another actor yields the same classification both ways; concurrent-claim race asserts exactly one 200; plus the rule-8 item-level oracle — `ClaimResponse.issue` full-item-JSON vs a same-actor `bd update --claim --json`[0] (§ 5, test 18). |
| 9 | schema_version reported by CLI envelope and `/context` | `types.WireSchemaVersion` consumed by both (compile-time); protocol corpus pins the CLI value byte-level. |
| 10 | Full-surface parity oracle (rule 8) | `TestProxiedServerServeReadyParity` and `TestProxiedServerServeListParity`: decoded-map equality of FULL item JSON (not ID sets) between HTTP and CLI for the same filter matrix, modulo an explicit, in-test, commented allowlist. Plus `TestParityMatrix_CoversAllParams`: reflection over `ReadyParams`/`ListParams` fields asserting every field appears in the matrix registry, so adding a param without oracle coverage fails CI. |
| 11 | workapi frontend-boundary import policy (rule 15) | `.golangci.yml` depguard rule `workapi-frontend-boundary` (deny cobra + net/http in `internal/workapi`, same idiom as `dolt-storage-boundary`) + `scripts/ci/pr-policy.sh` grep banning `config.GetDirectoryLabels`/`os.Getenv` in the package (§ 2, import policy). Fails CI, not review. |

**Honest gaps — conventions that remain unenforced (say so, per the mandate):**

- **G1 — query-param ↔ flag-name correspondence.** Nothing machine-checks that HTTP `label_any`
  maps to the same `ReadyParams` field as `--label-any`. The matrix-completeness test (row 10)
  catches a mis-wired mapping by value divergence, but the param SPELLING is convention + review.
- **G2 — cobra help-text numerals** ("default 50" in usage strings) are not tied to the constants.
- **G3 — builder error strings** double as corpus-pinned CLI output and HTTP 400 `detail`. The
  corpus enforces the CLI side only; acceptable because `detail` is prose, `code` is the contract.
- **G4 — overflow detection** stays two implementations by design (direct `withFetchOneExtra` vs
  UOW `HasMore`); only the parity oracle's truncation cases tie them together.
- **G5 — wisp dispatch**: `ResolveIssue` is shared by get/detail/claim, but `ApplyUpdate`'s
  internal `isWispID` remains a second decider in the domain layer (pre-existing; untested-for).
- **G6 — `bd ready --claim` JSON shaping** (`buildReadyIssueOutput` vs
  `buildReadyIssueOutputProxied`) stays duplicated in cmd/bd; claim-ready is outside the MVP
  surface — documented residual drift, not silently absorbed.
- **G7 — list ORDER divergence, and oracle blindness to it.** HTTP `/v0/beads/issues` is
  always `(created_at DESC, id ASC)` (welded to the cursor, § 3); `bd list`'s default is the
  priority ordering (sqlbuild/sort.go:70-71). This is a deliberate, documented divergence in
  ordering only — item set and item JSON stay identical, so the rule-8 parity oracles, which
  sort both sides by id before comparing, are structurally unable to detect ANY ordering
  regression. Ordering is carried solely by `TestServerListCursorPagination`'s first-page
  ordering assertion; it is not on the parity allowlist because the oracles never see it.

### Migration deltas the extraction slices must triage (not silently absorb)

Golden tables (rows 1-2 above) are written from the OLD code before deletion. The two ready
builders differ today in exactly two fields, and the unified `ReadyParams` carries both with the
policy unchanged and stated in the extraction PR: `MaxRows`/`MaxRowsSource` exist only in the
direct inline copy (proxied bulk ready REJECTS an explicit cap, ready.go:57-70, as list does at
list.go:522 — seam policy that stays in cmd/bd); `Offset` exists only in `gatherReadyInput`
(proxied-only, rejected on direct list at list.go:531). Any further delta the golden tables surface
is triaged in review — the extraction slices are otherwise no-op refactors, verified against the
byte-pinned cmd/bd/protocol corpus, landing separately per rule 11.

## 3. The wire: OpenAPI document, schemas, errors, pagination

### The document

The spec lives at `internal/httpapi/spec/openapi.v0.yaml` — OpenAPI 3.0.3, hand-written, the
source of truth per decided parameter 2. `internal/httpapi/spec/embed.go` embeds it
(`//go:embed openapi.v0.yaml`) so the parity and bijection tests parse the same bytes that ship.
Its `info.description` declares it spec-first and vendor-neutral (no product names, no `x-gc-*`
extensions — the marker is `x-bd-source: spec-first`).

Codegen follows the hosted-proven arrangement exactly: `oapi-codegen` v2.6.0, types-only, no
server stubs, no router, no YAML config. `internal/httpapi/apigen/doc.go` carries

```go
//go:generate go tool oapi-codegen -generate types,skip-prune -package apigen -o types.gen.go ../spec/openapi.v0.yaml
```

and `go.mod` (module is on Go 1.26.5, so `tool` directives are supported) gains
`tool github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen` pinned at v2.6.0. Generated
output is committed at `internal/httpapi/apigen/types.gen.go`; the CI drift gate
(regenerate → `git diff --exit-code -- internal/httpapi/apigen/`) runs in the PR-time policy
job, never in the concurrency-cancelled `Main` workflow.

### Schema pinning: no second wire struct (one-way door)

Every schema that already has a canonical Go wire type is pinned to it with `x-go-type` /
`x-go-type-import`, so oapi-codegen emits a type alias (`type Issue = types.Issue`) instead of a
mirror struct:

| Spec schema | Pinned Go type | Verified at |
|---|---|---|
| `Issue` | `types.Issue` | internal/types/types.go:20-171 (JSON tags) |
| `IssueWithCounts` | `types.IssueWithCounts` | types.go:976-982 |
| `IssueDetails` | `types.IssueDetails` | types.go:986-1013 |

Nested wire types (`Dependency`, `Comment`, `IssueWithDependencyMetadata`) ride along inside the
pinned structs and are documented as sub-schemas of the pinned ones; they are never re-declared
as generated Go types. There is **no hand-written `toAPI()` mapping and no generated Go struct
that mirrors `types.Issue` anywhere** — the CLI's `--json`, JSONL interchange, and HTTP all
marshal the one canonical struct. This is what makes a future `types.Issue` field addition
appear on the HTTP wire automatically instead of silently vanishing (the hosted viewer's drift
disease).

Consequence, marked explicitly: **pinning is a one-way door in both directions.** `types.Issue`'s
JSON encoding *is* the HTTP contract from v0 onward; removing or renaming a serialized field on
`types.Issue` becomes an HTTP breaking change, not just a CLI one. That is the intended
trade — one contract, one compatibility domain.

**Evolution policy for the welded domain (while `/v0` is live).** The repo already has a
sanctioned deliberate-change protocol for the CLI wire: `make corpus-regen` plus a
`JSONSchemaVersion` bump (§ 4, CLI migration mechanics). After pinning, that same maneuver would
silently change `/v0` responses for clients pinned to the path, with only a `schema_version`
field in `/context` they have no reason to re-poll. So the policy is stated here, not left to
discovery:

- **Additive-only while `/v0` is the newest path version.** Changes to the `x-go-type`-pinned
  structs (`Issue`, `IssueWithCounts`, `IssueDetails` and their ride-along sub-types) are limited
  to new `omitempty` fields. Each addition must land with its spec `properties` entry in the same
  PR — the bijection test makes forgetting a compile-visible CI failure — and appears on both
  surfaces simultaneously, which is the feature, not a leak.
- **A `WireSchemaVersion` bump is forbidden while `/v0` is the only HTTP path version.** A bump
  is by definition a breaking change to the pinned schemas, and a single-struct domain cannot
  serve two shapes from one path. The sanctioned breaking-change maneuver becomes: cut `/v1`
  (new spec document, new path prefix) in the same change that bumps `WireSchemaVersion` and
  regenerates the corpus.
- **What happens on that day, mechanically:** the bijection test re-binds the canonical structs
  to the *newest* path version's spec (the test's "no second wire struct" rule is scoped to the
  live version). `/v0` either (a) is deprecated and removed on a documented sunset, or (b) is
  frozen by snapshotting the pre-bump shapes as a deliberate, version-suffixed mirror struct
  served only by `/v0` handlers — a frozen mirror cannot drift, so a round-trip fixture (adequate
  for a shape that can no longer gain fields) replaces the bijection test for it. Choosing (a)
  vs (b) is deferred to that day (recorded as open question 8); what is NOT deferred is that the
  corpus-regen + `JSONSchemaVersion`-bump procedure alone is no longer sufficient once `/v0` ships.

Because `x-go-type` means the spec's property list is documentation rather than
compiler-enforced, the guard is a **two-way JSON-tag bijection test**
(`internal/httpapi/wire_bijection_test.go`): reflect over the JSON tags of `types.Issue`,
`types.IssueWithCounts`, and `types.IssueDetails` and assert exact set-equality with the spec's
`Issue`/`IssueWithCounts`/`IssueDetails` `properties`. Fields tagged `json:"-"` (`ContentHash`,
`RowVersion`, `SourceRepo`, `IDPrefix`, `PrefixOverride`, `IsLitePartial` — types.go:22,89,111-113,170)
carry no wire name and are excluded by construction. The deliberate-omission allowlist **starts
empty**; any future entry requires an in-test comment justifying the omission. A round-trip
fixture test is NOT an acceptable substitute: a new `omitempty` field absent from the fixture
round-trips cleanly in both directions and the drift ships silently. The bijection test runs in
the required PR-time path (`go test ./internal/httpapi/...` under `ci-pr-core`), because with
pinning it is the only guard on the Issue schema.

Envelope schemas that have no pre-existing canonical Go type — `ReadyPage`, `IssuesPage`,
`ClaimRequest`, `ClaimResponse`, `ContextResponse`, `Health`, `Problem` — are spec-native and
generated normally. They are new wire surface, not mirrors, so rule "no second wire struct" does
not apply to them; drift is impossible because the generated type is the only implementation.

### The six operations

All success bodies are `application/json; charset=utf-8`; every non-2xx body from any handler is
`application/problem+json` (§ error envelope). Base path has no `{project}` segment
(single-tenant, decided). The claim path is declared in the spec as the literal AIP-136-style
`/v0/beads/issues/{id}:claim`; the route-parity test carries the explicit spec-path ↔ mux-pattern
mapping (router mechanics: § 4, "Server lifecycle and security").

| operationId | Method + path | Request | 200 body | Documented non-2xx |
|---|---|---|---|---|
| `health` | `GET /healthz` | — | `Health` `{"status":"ok"}` (process LIVENESS only, no DB touch; readiness is probed via `/v0/beads/ready?limit=1` — § 4, concurrency limits) | — |
| `getContext` | `GET /v0/beads/context` | — | `ContextResponse` | 500 |
| `listReadyWork` | `GET /v0/beads/ready` | query → `types.WorkFilter` (table below) | `ReadyPage` | 400, 500, 503 |
| `listIssues` | `GET /v0/beads/issues` | query → `types.IssueFilter` (table below) | `IssuesPage` | 400, 500, 503 |
| `getIssue` | `GET /v0/beads/issues/{id}` | path `id`: exact canonical ID, no fuzzy/substring resolution; wisp fallback | `IssueDetails` | 404, 500, 503 |
| `claimIssue` | `POST /v0/beads/issues/{id}:claim` | body `ClaimRequest` `{"actor": "<non-empty>"}` | `ClaimResponse` | 400, 404, 409, 500, 503 |

One code applies to **every** route, `/healthz` included, and is documented once in the spec
rather than repeated per row: the middleware-level 400 `invalid_argument` for a foreign `Host`
header (§ 4, "Browser exposure and DNS rebinding") — it runs before any handler.

Per rule 12, **only** the codes above are documented. Deliberately NOT in the spec: 413 (an
oversized claim body tripping `http.MaxBytesReader` maps to 400 `invalid_argument`), 429/504
(the connection cap and semaphore QUEUE rather than refuse; a semaphore wait that times out
surfaces as the already-documented 503 `busy`, and a tripped per-request deadline as the generic
500 — § 4 — so neither new code is ever emitted), 403 `read_only` (no read-only serve
mode ships in v0), 501 (`*storage.ErrUnsupported` is a startup refusal, not a request-time
response). Router-level 404/405 for unregistered paths/methods are emitted in the problem
envelope for uniformity but are not per-operation spec surface.

**Unknown query parameters are rejected, on every operation.** With oapi-codegen generating
types only — no server stubs, no generated request validation — the query decoders in
`internal/httpapi/params.go` are hand-written over stdlib `url.Values`, and Go's default is to
silently ignore any key the decoder does not read. That default is banned, in the spec and in
the decoders: each decoder iterates the request's raw query keys, and any key outside the
operation's parameter table → 400 `invalid_argument` with extension members
`param: "<the offending key>"` and `reason: "unknown_parameter"` (§ error envelope). Operations
with no documented query parameters (`/healthz`, `/context`, get, claim) reject every query key
the same way. This is not pedantry: a silently ignored FILTER parameter widens the result set.
The concrete skew failure — a client one version ahead sends a newly added filter (the
"additive later" params the tables below name, e.g. `mol_type`); an older server drops it; a
dispatch probe that asked for "unassigned, routed to me" gets back work routed to someone else — is the
hosted viewer's bare-`IssueFilter{}` leak class all over again, triggered by version skew
instead of a bare struct. Strict rejection is also the only reliable per-parameter capability
probe a client has, since `capabilities` is operation-level (§ version skew). Enforced by
Tier-A test 8c.

#### `GET /v0/beads/ready` — query parameters → `types.WorkFilter` (types.go:1812-1872)

| Param | Type/format | WorkFilter field | Notes |
|---|---|---|---|
| `assignee` | string | `Assignee *string` | |
| `unassigned` | bool | `Unassigned` | |
| `type` | string | `Type` | normalized via `utils.NormalizeIssueType` in the shared builder |
| `exclude_type` | repeated, comma-splittable | `ExcludeTypes []IssueType` | ignored when `type` set (WorkFilter contract) |
| `label` | repeated | `Labels` | AND semantics |
| `label_any` | repeated | `LabelsAny` | OR semantics |
| `exclude_label` | repeated | `ExcludeLabels` | |
| `label_pattern` | string | `LabelPattern` | glob |
| `label_regex` | string | `LabelRegex` | |
| `priority` | int | `Priority *int` | |
| `parent` | string | `ParentID *string` | recursive descendants |
| `metadata_field` | repeated `key=value` (split on first `=`) | `MetadataFields map[string]string` | keys validated via `storage.ValidateMetadataKey` (metadata.go:216) |
| `has_metadata_key` | string | `HasMetadataKey` | |
| `include_ephemeral` | bool | `IncludeEphemeral` | Gas City passes `--include-ephemeral` |
| `include_deferred` | bool | `IncludeDeferred` | |
| `sort` | `hybrid`\|`priority`\|`oldest` | `SortPolicy` | validated via `types.SortPolicy.IsValid` (types.go:1746-1758); empty → hybrid |
| `limit` | int ≥ 0 | `Limit` | absent → shared ready-default constant = 100 (cmd/bd/ready.go:796); `0` → unlimited (rule 2) |

Not exposed: `offset` (a proxied-CLI-ism), `max_rows`/`max_rows_source` (operator knob, not wire),
`molecule_id`/`mol_type`/`wisp_type` (no consumer; additive later). `Status`/`Statuses` are not
params: the shared builder hard-codes `Status:"open"` — the empty-WorkFilter
open+in_progress default is unreachable from the wire.

**Response `ReadyPage` = `{items: [IssueWithCounts], has_more: bool}` (rule 3).** `bd ready
--json` emits `[]*types.IssueWithCounts` on both CLI paths (cmd/bd/ready.go:246,299,509;
ready_proxied_server.go:181-210,441-471), so the HTTP items carry `dependency_count`,
`dependent_count`, `comment_count`, `parent` from day one — executed via
`IssueUseCase.GetReadyWorkWithCounts` → `domain.SearchCountsPage` (domain/issue.go:140-142,261).
`has_more` comes from `SearchCountsPage.HasMore`. No cursor on ready: the SortPolicy orders
(hybrid/priority) admit no keyset predicate and the consumer pattern is snapshot-and-requery on a
patrol cadence. Adding a cursor later under a fixed sort is additive — not a one-way door.

#### `GET /v0/beads/issues` — query parameters → `types.IssueFilter` (types.go:1584-1736)

Constructed exclusively through the shared `buildListFilter` extraction, so the default
exclusions are byte-identical to `bd list` (cmd/bd/list_filter.go:119-143,240-261,302-310) and the
hosted viewer's bare-`IssueFilter{}` leak is structurally impossible.

| Param | Type/format | IssueFilter field / builder behavior | Notes |
|---|---|---|---|
| `status` | comma-separated or repeated | `Status` (single) / `Statuses` (multi) via `applyStatusFilter` | custom statuses honored |
| `type` | string | `IssueType *IssueType` | |
| `assignee` | string | `Assignee *string` | |
| `label` / `label_any` / `exclude_label` | repeated | `Labels` / `LabelsAny` / `ExcludeLabels` | AND / OR / NOT |
| `parent` | string | `ParentID *string` | |
| `all` | bool | disables the default `ExcludeStatus` construction (closed + custom done/frozen categories) | default false ⇒ exclusions apply |
| `include_templates` | bool | default false ⇒ `IsTemplate=&false` | |
| `include_gates` | bool | default false ⇒ `"gate"` appended to `ExcludeTypes` | |
| `include_infra` | bool | default false ⇒ configured infra types appended to `ExcludeTypes` | |
| `created_before` / `created_after` | RFC3339 / RFC3339Nano | `CreatedBefore` / `CreatedAfter *time.Time` | Gas City passes RFC3339Nano |
| `metadata_field` | repeated `key=value` | `MetadataFields` | |
| `has_metadata_key` | string | `HasMetadataKey` | |
| `limit` | int ≥ 0 | `Limit` | absent → shared list-default constant = 50 (cmd/bd/list.go:743); `0` → unlimited (rule 2) |
| `cursor` | opaque string | `AfterCreatedAt` / `AfterID` | § pagination; sort forced on EVERY request, cursored or not (below) |

**Order is forced on every request, not only under a cursor.** The handler sets
`SortBy="created"`, `SortDesc=false` unconditionally on every list request. This is load-bearing:
`buildListFilter` passes the caller's sort straight through (list_filter.go:123-124), and an empty
`SortBy` renders `ORDER BY priority <dir>, created DESC, id ASC`
(internal/storage/sqlbuild/sort.go:70-71) — so forcing the order only when a cursor is present
would make page 1 priority-ordered while its `next_cursor` encodes a `(created_at DESC, id ASC)`
position, and page 2 would skip and duplicate rows relative to page 1. Consequence, stated in the
spec's endpoint description as deliberate: **HTTP list order diverges from `bd list`'s default
priority ordering** — every `/v0/beads/issues` page is `(created_at DESC, id ASC)`. This is an
ordering-only divergence (same item set, same item JSON); see gap G7 for why the parity oracles
are structurally blind to it and which test carries it instead.

Default-off wisp merge (`SkipWisps=true`, list_filter.go:310) applies exactly as in the CLI. Not
exposed in v0: `sort`/`reverse` (welded to the cursor contract, § pagination), `offset` (keyset
supersedes it; never load-bearing), `skip_labels` (§ traps), `q`-style text search,
`title_contains` and friends, time windows other than created (no consumer; all additive later).

**What this list surface deliberately cannot serve in v0 — the reconciler full scan.** Gas
City's cache reconciler snapshots the COMPLETE active universe every 30/60/120 s per store as
`{limit 0, SkipLabels: true, TierMode: TierBoth}` (caching_store_reconcile.go:61-63): the
persistent tier via `bd list`, the ephemeral tier via a SECOND subprocess,
`bd query "ephemeral=true AND ..."` — "the installed bd list surface does not expose ephemeral
rows" (bdstore.go:2262-2264). v0's `/issues` hard-wires `SkipWisps=true` and always hydrates
labels, so that snapshot is unreachable over HTTP — by scope, not by accident — and this
document says so in the thesis and slice 11 instead of counting the reconciler among the
deletable forks (migrating it without `skip_labels` would also hydrate labels for the full
active set every cycle, past the reconciler's own scan-warn threshold). The additive path is
real and needs no new storage capability: `IssueFilter.Ephemeral *bool` (types.go:1648) and
`SkipWisps` (types.go:1700) already exist to back an ephemeral/wisp-inclusion parameter, and a
labels-omission knob may ship only together with an explicit omission marker on the NEW page
envelope (e.g. `omitted: ["labels"]`), where the `IsLitePartial` `json:"-"` trap does not apply
(§ traps). Neither knob has a `bd list` counterpart to run the rule-8 parity oracle against —
`bd list` cannot emit ephemeral rows at all — which is the second reason they are out of the
MVP: they would be the only wire surface with no CLI oracle behind it.

**Response `IssuesPage` = `{items: [IssueWithCounts], has_more: bool, next_cursor?: string}`.**
`bd list --json` calls `SearchIssuesWithCounts` on both CLI paths (cmd/bd/list.go:574-580,
list_proxied_server.go:78), so the HTTP items carry the same counts fields or the full-item-JSON
oracle (rule 8) fails immediately — same reasoning as `/ready` (rule 3). `next_cursor` is present
iff `has_more` is true.

#### `GET /v0/beads/issues/{id}`

Response is a **single `IssueDetails` object, never an array** (the array is a CLI
multi-argument-ism; this deletes Gas City's decode-array-index-`[0]` + `ErrIDCollision` guard).
Shape per types.go:986-1013: embedded `Issue` + `labels[]` + `dependencies[]`
(`IssueWithDependencyMetadata`: full issue + `dependency_type`) + `parent` + count-only-mode
cardinality fields `dependency_count`/`dependent_count`/`comment_count` (`*int64`) with
`Dependents`/`Comments` slices nil and `comments_omitted` semantics preserved exactly as `bd
show`'s default — assembled by the shared detail function (rule 13; § 2, "Shared detail
assembly", owns its signature). This is a superset of Gas City's decoded 18-field contract.

#### `POST /v0/beads/issues/{id}:claim`

`ClaimRequest` = `{"actor": string}`, required, non-empty; the server never infers an actor (its
git identity is meaningless for remote callers). The handler validates the actor at the wire
edge, BEFORE the CAS, because the domain layer checks only `actor == ""`
(domain/issue.go:462-463) — a whitespace-only or near-1MB actor would otherwise pass, be
persisted to the assignee column, and be interpolated into the Dolt commit message
(`"bd serve: claim <id> by <actor>"`), corrupting the audit trail the claim model rests on.
Rules: trim, reject empty-after-trim, reject length > 256 bytes, reject control characters
(including newline — a multiline actor would forge extra commit-message lines); all → 400
`invalid_argument`. The spec pins these as `minLength`/`maxLength`/`pattern` on the `actor`
property so clients see the contract, not just the refusal. `ClaimResponse` = `{issue: Issue,
already_claimed: bool}` — `already_claimed` maps `domain.ClaimResult.AlreadyClaimed`
(domain/issue.go:222-225): a same-actor re-claim is idempotent and returns 200 with
`already_claimed: true`, matching CLI semantics. There is no constant `claimed: true` member —
a field that can only ever be one value is dead wire surface. `issue` is re-read inside the same
transaction after the CAS. The write path is `uow.RunTxResult` (rule 5); its retry semantics
surface on the wire only as the 503 `busy` row below.

#### `GET /v0/beads/context`

`ContextResponse` is a spec-native schema (§ schema pinning) SOURCED FROM — not a mirror of —
`domain.ContextInfo` (domain/context.go:36-53), captured via
`contextinfo.NewContextProvider(workDir, version)` (internal/storage/contextinfo/provider.go:12).
Its exact field set, enumerated here because each one is permanent wire surface:

```json
{"api_version": "v0", "bd_version": "...", "schema_version": 1,
 "backend": "dolt", "dolt_mode": "...", "database": "...",
 "beads_dir": "...", "repo_root": "...", "project_id": "...",
 "capabilities": ["ready.list", "issues.list", "issues.get", "issues.claim"]}
```

Seven fields carry `ContextInfo` values (`bd_version`←`BdVersion`, `backend`←`Backend`,
`dolt_mode`←`DoltMode`, `database`←`Database`, `beads_dir`←`BeadsDir`, `repo_root`←`RepoRoot`,
`project_id`←`ProjectID`); three are HTTP-only (`api_version`, `schema_version`,
`capabilities` — `ContextInfo` has none of them). Each kept identifier is justified, not
inherited: `database`/`project_id` are logical names, not host topology; `beads_dir`/`repo_root`
ARE absolute host paths, kept because the default deployment is same-host loopback and they are
the single-workspace server's only workspace-identity handshake (they replace Gas City's direct
`.beads/metadata.json` read) — a network peer learning filesystem layout from them is part of
the disclosure an operator accepts with the `--allow-non-loopback` warning.

The remaining nine `ContextInfo` fields are **deliberately withheld**, and one of them is
**hard-excluded**:

- `SyncRemote` is hard-excluded, not merely unserved-for-now. `GetContextInfo` populates it
  unconditionally from config `sync.remote`, falling back to the deprecated `sync.git-remote`
  (fs/context.go:87-93), and the key is documented as "any Dolt-compatible remote URL"
  (internal/config/yaml_config.go:45). Remote URLs routinely embed credentials
  (`https://x-access-token:TOKEN@github.com/org/repo`), so serving the field verbatim would hand
  a repo credential to every loopback client — and to every network peer under
  `--allow-non-loopback`. It never joins the response, in any future version, without an
  explicit credential-redaction rule designed first. Same rule for any successor sync-URL field.
- `ServerHost`/`ServerPort` name the **unauthenticated Dolt SQL bind endpoint** — advertising it
  invites clients to bypass the API and dial the DB directly; `ProxiedDir`/`DataDir` are
  server-local filesystem topology with no client use; `Role` is sync plumbing with no v0
  consumer.
- `CWDRepoRoot`/`IsRedirected`/`IsWorktree` are cwd-derived, meaningless in a server process
  (same reasoning as rule 15).

Withholding is the safe default: adding any of them later is additive; shipping one now is
permanent. The allowlist is enforced, not aspirational: `TestContextResponseAllowlist` (§ 5,
test 8b) serializes the handler's response for a `ContextInfo` with every field populated —
`SyncRemote` set to a URL embedding a fake credential — and fails on any JSON key outside the
ten above or on the credential substring appearing anywhere in the body, so a future
`ContextInfo` field cannot leak onto the wire by default.

`schema_version` is sourced from a shared `WireSchemaVersion` constant that
`cmd/bd/output.go`'s `JSONSchemaVersion = 1` (output.go:12) also consumes, so the value reported
over HTTP and over stdout cannot diverge. It is diagnostic, never a gate: because the constant
is shared with the CLI stdout envelope, it can move for CLI-only reasons with zero HTTP wire
change, so clients MUST NOT branch HTTP behavior on it (§ version skew). `capabilities` is
derived from the route table's **implemented** handlers (one source; transitional 501 stubs are excluded by construction, so
the endpoint never advertises an operation that would 501 — § 5 slice 6); additions are
additive, never a door. This endpoint replaces Gas City's
three-way context hack (`bd context --json`, `bd version` string surgery, `.beads/metadata.json`
reads). Documented codes: 200, 500, plus the middleware-level 400 for a foreign `Host` header
(§ 4, reachable on every route) — v0 serves a startup snapshot and does not touch the DB
per request; if a later slice makes it a DB-probing readiness endpoint, 503 is added then
(additive).

### Version skew: what a client may gate on (the client contract)

"What breaks when the server is a version ahead of or behind my client" is answered elsewhere
for item fields (additive-only evolution + tolerant decode, § schema pinning) and for cursors
(opaque, versioned, § pagination). The remaining skew surfaces — codes, parameters, and the
gating rule itself — are stated here as contract, in the spec's `ContextResponse` and
`Problem` descriptions, because an unstated gating rule regenerates exactly the pathology this
API retires: version-string surgery and substring matching. Which field gates what:

- **`capabilities` gates operation presence.** It is derived from the implemented route table
  (§ context above); a client that needs `issues.claim` checks the list, never the version.
- **`bd_version` gates feature availability.** It is the release's structured semver and the
  only field a client may compare as a version — for BEHAVIORAL changes tied to a release (a
  fixed bug, a changed default), never for operation or parameter presence.
- **`api_version` gates the path major.** It changes only when `/v1` is cut (§ schema pinning,
  evolution policy).
- **Parameter presence is probed, not versioned, in v0.** `capabilities` is operation-level,
  so the strict unknown-parameter rejection (§ the six operations) is the per-parameter gate:
  a 400 with `reason: "unknown_parameter"` + `param: "<name>"` is a loud, machine-attributable
  "this server does not know this parameter" — never a silently widened result set. If
  probe-by-request proves awkward in practice, the additive fix is an `api_revision` context
  field exposing the spec's `info.version` (bumped on every spec change, enforceable by the
  same pr-policy drift gate) so parameter-level gating never regresses to semver surgery —
  recorded as open question 7 rather than shipped, because it grows the § context allowlist.
- **Clients MUST default-branch on unknown `code` values within a status class.** The code
  vocabulary grows additively (rule 12), and `code` is the only dispatchable member — so a
  client that switches exhaustively on `code` breaks on the first addition, which would make
  "adding codes later is additive" false for precisely the clients this envelope invites.
  Contract: dispatch on the codes you know; treat an unknown code as its status class (unknown
  4xx → client bug, fail loud; unknown 503 → retry per `Retry-After`; other unknown 5xx →
  server fault). Same rule for unknown `reason` values (§ sentinel mapping). The spec states
  this on the `Problem` schema.
- **`schema_version` is NOT an HTTP gate.** It is `types.WireSchemaVersion`, deliberately
  shared with the CLI stdout envelope (§ 2, parity row 9) so the two surfaces report one
  number — which means a CLI-envelope-only corpus change bumps the value a remote client sees
  with ZERO HTTP wire change. A client that gates HTTP behavior on `schema_version` trips
  spuriously on the first CLI-side corpus regen: the shared constant quietly welds the two
  compatibility domains the error-envelope section keeps independent, and the weld is
  tolerable ONLY because nothing on the HTTP side may key on the value. Report it, log it,
  never branch on it. (While `/v0` is the newest path version any bump is forbidden outright —
  § schema pinning — so the field is constant `1` today; if a CLI-envelope-only schema change
  ever collides with that freeze, the unweld — a separate CLI-envelope version constant — is
  additive on the HTTP side.)

### Error envelope: RFC 9457 problem+json, one shape everywhere (one-way door)

Every non-2xx byte the server emits is `application/problem+json`:

```json
{"status": 409, "title": "Conflict", "code": "already_claimed",
 "detail": "issue bd-abc already claimed by alice",
 "assignee": "alice", "issue_status": "in_progress"}
```

Required members: `status`, `title`, `code`. `code` is the stable machine-readable string —
the only member clients may dispatch on; it is what deletes Gas City's ~100 lines of
substring-matching on conflict text (bdstore.go:812-828). `detail` is optional prose, never
load-bearing. `type` is omitted (`about:blank` implied; no URI ceremony in v0). Extension
members are per-code as listed below; adding members or codes later is additive — additive for
the client only under the § version-skew contract (default-branch on unknown `code` values
within a status class; an exhaustive switch on `code` breaks on the first addition) — but
**renaming or removing a documented status+code pair is a wire break — the code vocabulary is
a one-way door**, which is why rule 12 caps it at what the six operations need.

Why this envelope and not the two shapes already in play:

1. **The CLI `{error, schema_version}` envelope** (cmd/bd/output.go:40,71) is a stdout protocol,
   byte-pinned by the `cmd/bd/protocol` corpus. It carries no HTTP status semantics and no
   machine code; extending it would churn a pinned corpus and weld two independent compatibility
   domains (CLI stdout schema, HTTP wire) together. It stays CLI-only.
2. **The hosted `{"error": "..."}` handler shape** is the documented cautionary tale: split
   three ways (problem+json from its identity gate, bare `{"error"}` from handlers, 403 modeled
   both ways, 500 undocumented). Adopting it would import the inconsistency we exist to kill.

RFC 9457 is the registered standard, gives typed conflicts via extension members, and the hosted
identity gate already emits it — so the hosted product converges toward OSS (decided
parameter 1). We fix the hosted spec's omission by documenting 500 (and 503 where reachable) on
every DB-backed operation.

#### Sentinel → status mapping

One table, in one file (`internal/httpapi/problem.go`), matched exclusively with
`errors.Is`/`errors.As` — never `err != nil → status`:

| Condition | Status | `code` | Extensions |
|---|---|---|---|
| request parse/validation failure: unknown query parameter (§ 3, unknown-parameter rejection), bad param value, invalid `sort`/`status`/`type`, bad `metadata_field` key, malformed/oversized claim body, invalid actor (empty after trim, > 256 bytes, or control characters — § 3 claim), `limit=0` under `--allow-non-loopback` (§ 3 limit semantics), foreign `Host` header (§ 4 bind posture) | 400 | `invalid_argument` | `param` — the offending query parameter, body field, or header name; present on every condition in this row except a body that fails to parse at all. `reason` ∈ {`unknown_parameter`, `invalid_value`} — always present |
| undecodable or wrong-version cursor | 400 | `invalid_cursor` | — |
| `errors.Is(err, storage.ErrNotFound)` | 404 | `not_found` | — |
| `errors.Is(err, storage.ErrAlreadyClaimed)` | 409 | `already_claimed` | `assignee`, `issue_status` |
| `errors.Is(err, storage.ErrNotClaimable)` | 409 | `not_claimable` | `issue_status` |
| `uow.RunTxResult` returns with `uow.IsSerializationError(err)` true (retry budget exhausted; uow/tx.go:76, uow/errors.go:16), or the in-flight semaphore wait timed out (§ 4, concurrency limits) | 503 | `busy` | header `Retry-After: 5` on exhausted claim retries (the server just observed ≥ 15 s of write contention; a 1 s comeback invites a convoy of claim requests each holding a semaphore slot up to 15 s, starving reads exactly when dispatch is busiest) / `Retry-After: 1` on a semaphore-wait timeout (slot pressure clears quickly); static `detail` (scrubbing rule below) |
| `NewUOW`/driver/net failure — Dolt server or dbproxy down/idle-stopped | 503 | `db_unavailable` | header `Retry-After: 5`; static `detail` (scrubbing rule below) |
| anything else, including panic recovery | 500 | `internal` | — static `detail`; real error logged server-side only (scrubbing rule below) |

**400s carry a machine-readable pointer, not just prose (decided during client review).** One
umbrella `invalid_argument` code over every validation failure is rule-12-minimal, but it
forces a choice on clients that the envelope's own principle forbids: `detail` is never
load-bearing, yet a bad `sort` value, a bad `metadata_field` key, and an unknown parameter
would be distinguishable only by parsing prose — the substring-matching pathology this API
exists to delete, reintroduced at the 400 boundary. So the distinction is carried by extension
members, not new codes: `param` points at the offending input by name, and `reason` separates
the two client postures — `invalid_value` ("my value is malformed": a client bug, fail loud)
from `unknown_parameter` ("this server does not know this parameter": version skew, degrade or
fall back — § version skew). `invalid_cursor` needs neither: the dedicated code already does
this job for cursors. Extension members are additive vocabulary (§ one-way doors), and the
`reason` set may grow; clients MUST default-branch on unknown `reason` values exactly as they
do on unknown codes.

**5xx detail scrubbing (decided — formerly open question 4, closed as safe-by-default).** For
the `busy`, `db_unavailable`, and `internal` rows, `detail` on the wire is a fixed static
string per code (e.g. "database temporarily unavailable; retry"); the underlying error is
written to the server log only, never to the response body. This is mandatory, not an
operator-verbosity preference: `NewUOW`/driver failures routinely embed the DSN and topology —
go-sql-driver renders connection targets as `user@tcp(127.0.0.1:PORT)/db` and net dial errors
carry `dial tcp 127.0.0.1:PORT` — and query-layer errors can carry SQL fragments. The moment
the same binary is bound with `--allow-non-loopback` (an explicitly supported mode), a verbose
5xx `detail` becomes an information-disclosure channel to network peers. The envelope's own
principle — `code` is the contract, `detail` is prose — is tightened here to **prose is never
sensitive**. The 4xx rows keep their specific, parameter-naming `detail`: they reflect the
client's own input back, not server internals. `TestProblemMapping` (§ 5) asserts the mapped
5xx bodies never contain the input error's message text.

Sentinel declaration sites, post-facade: the canonical declarations of the `Err*` sentinels live
in the leaf `issueops` package (the facade's errors relocation), aliased back from
`internal/storage` with the same pointer — `errors.Is` keys on `storage.Err*` keep working
unedited and remain the mapper's keys; keying on `issueops.Err*` is equivalent and optional.
`ClaimedByFragment`/`NotClaimableStatusFragment` stay in `internal/storage` (not among the 18
relocated symbols).

The 409 extensions are populated from a fresh read in the same transaction, never by parsing the
sentinel message fragments (`storage.ClaimedByFragment`/`NotClaimableStatusFragment`, which
exist for CLI text reconstruction only). `*storageissueops.ErrTooManyRows`
(`internal/storage/issueops`, the internal package) is
deliberately absent: with no operator opt-in, v0 never sets `MaxRows` on a server-built filter
(a cap is an operator knob per rule 2), so the error is unreachable; when the knob ships
(`--max-rows`, recommended for v0 by SRE review, owner sign-off pending — open question 2), a
400 `too_many_rows` row is added in the same change.

**Not-found is normalized in the shared function, not the handler (rule 4).** The dual miss
shapes and the normalization mechanics are specified once in § 2, "Not-found normalization" —
the consequence for this table is that `problem.go` needs only the single
`errors.Is(err, storage.ErrNotFound)` row, and no future caller can reintroduce the bug in
either direction (missing-issue-500, or the hosted viewer's every-error-is-404).
`TestServerGetUnknownID404` asserts 404-on-unknown-id end to end against real Dolt; a simulated
mid-request connection drop must yield 503/500, never 404.

### Pagination

**Keyset cursor over `(created_at DESC, id ASC)`, on `GET /v0/beads/issues` only (one-way
door).** The predicate and total order are already implemented and documented on
`types.IssueFilter.AfterCreatedAt`/`AfterID` (types.go:1617-1633): rows strictly after the
position under `(created_at < ca) OR (created_at = ca AND id > id)`; `id` is the primary key, so
same-second groups larger than a page still page completely with no dropped or duplicated row.
It composes with every other filter, including `created_before`.

- **Encoding:** `base64url(JSON {"v": 1, "ca": "<RFC3339Nano UTC>", "id": "<last-item-id>"})`.
  Opaque by contract — clients MUST NOT construct or parse it; the `v` member is the server's
  private evolution hinge. Undecodable, unpadded-garbage, or unknown-`v` input → 400
  `invalid_cursor`.
- **Server side:** the handler forces `SortBy="created"`, `SortDesc=false` on EVERY list
  request — cursored or not — and a cursor additionally decodes into `AfterCreatedAt`/`AfterID`.
  Per the field contract (types.go:1637-1639) this yields `ORDER BY created_at DESC, id ASC`,
  the order the predicate assumes. Forcing only in the cursor-decode path would be a real bug:
  an un-cursored page 1 would fall through to the default priority ordering
  (sqlbuild/sort.go:70-71 via list_filter.go:123-124) while `next_cursor` encodes a keyset
  position in the created order, so page 2 would skip and duplicate rows. This is why v0's list
  has no `sort` param: sort order is welded to the cursor contract, and that weld is the
  one-way part. New sort orders require new (cursorless or separately-cursored) surface.
  `TestServerListCursorPagination` asserts the first (un-cursored) page is already in
  `(created_at DESC, id ASC)` order, because the id-sorted parity oracles cannot see ordering.
- **Response:** `has_more` from `domain.SearchPage.HasMore`, which the repo computes by limit+1
  over-fetch and trim (domain/db/issue_search.go:81-102,476-481, limitOffsetSQL:511-521).
  `next_cursor` is present iff `has_more`, built from the last item of the page.

**Limit semantics (rule 2, one-way door):**

- `limit` absent → the shared default constant: **50** for list (= `bd list`'s default,
  cmd/bd/list.go:743), **100** for ready (= `bd ready`'s default, cmd/bd/ready.go:796). Both
  surfaces read the same constants (owned by the shared-contract package), so the document that
  promises no divergence cannot itself diverge (rule 9).
- `limit=0` → **unlimited, exactly as in the CLI.** Gas City verifiably runs
  `bd ready --limit 0` and `bd list --limit 0` (its reconciler does an unfiltered active-set
  scan forever); a v0 that 400s or clamps it fails the brief. Verified mechanics: `Limit<=0`
  emits no SQL LIMIT clause and `trimToLimit` returns false (issue_search.go:476-481,511-517),
  so an unlimited page reports `has_more=false` and no `next_cursor` — correct by construction,
  no special-casing needed. On the default loopback bind, a cap — if ever wanted — is an
  operator knob (`MaxRows` vocabulary), never a wire-level refusal; whether that knob is worth
  shipping stays open question 2. **Under `--allow-non-loopback` the calculus inverts (decided
  during security review):** `limit=0` is refused with 400 `invalid_argument` (detail:
  "unlimited reads are loopback-only; pass an explicit limit"). A single
  `GET /v0/beads/issues?limit=0` buffers the entire active set plus its JSON marshal, and 16
  concurrent scans multiply it (§ 5 risks) — that must not be reachable by arbitrary network
  peers via a shipped flag, and the mitigation must not wait on a post-hoc knob nobody enabled.
  In-vocabulary (rule 12: `invalid_argument`, no new code), mode-dependent, documented in the
  spec's `limit` parameter description, and invisible to Gas City's loopback reconciler (rule 2
  carve-out noted at the rule).
- `limit` < 0 → 400 `invalid_argument`.

### Traps and how the wire handles them

**`IsLitePartial` is `json:"-"` (types.go:170).** `IssueFilter.Lite` produces rows whose heavy
fields are zero-valued with only the non-serialized `IsLitePartial` marker set — a lite row is
wire-indistinguishable from an issue with a genuinely empty description. Similarly
`IssueFilter.SkipLabels` (types.go:1694-1697) leaves `Labels` nil, indistinguishable under
`json:"labels,omitempty"` from "no labels". Wire handling: **v0 never sets `Lite` or
`SkipLabels` on any server-built filter and exposes no `skip_labels`/`view=lite` parameter**;
labels are always hydrated. Belt-and-braces: the server's execution seam (UOW → domain/db) does
not implement `Lite` at all and always returns fully-hydrated issues (types.go:1729-1734), so a
lite-partial row is unreachable from `bd serve` by construction. Future exposure is additive
but carries a precondition recorded here: a lite/skip-labels view may only ship together with an
explicit wire marker for the omission (e.g. a response-level `partial` field), because the
canonical structs cannot express it. Gas City's `--skip-labels` subprocess caller is the
reconciler full scan, which is not a "gives up an optimization" case: that loop stays on
subprocess entirely in v0 for the stronger reason that it also needs the ephemeral tier v0's
list does not expose (§ 3, "what this list surface deliberately cannot serve"); nothing about
its CLI usage changes.

**Metadata typing.** The brief's trap ("bd type-infers `--set-metadata k=true` into JSON
bool/number") has shifted on main but the wire consequence stands: since GH#4146,
`MetadataEditValue` always stores `--set-metadata` values as JSON strings
(internal/storage/metadata.go:314-324), but typed values still enter via the explicit
`--metadata`/`--metadata-json` path (`MergeMetadataJSON`, metadata.go:246-249) and persist in
every pre-GH#4146 row. Wire handling: `Issue.metadata` is `json.RawMessage` (types.go:102) and
the spec declares it `type: object, additionalProperties: true` with an explicit description
that **values may be any JSON type and clients MUST NOT strict-decode to `map[string]string`**
(the strict decode of one typed bead once poisoned an entire claim batch). The parity/bijection
fixtures include bool, number, and nested-object metadata values so a strict-decoding
regression can never land. `metadata_field` query values filter by top-level key=value equality
exactly as the CLI flag does — same `map[string]string` vocabulary, same semantics.

**`timeout` is nanoseconds.** `types.Issue.Timeout` is `time.Duration` (types.go:145), which
`encoding/json` marshals as an int64 nanosecond count. Pinning means the spec must document
reality: `timeout: {type: integer, format: int64, description: nanoseconds}`. Pre-existing wire
fact, not a choice.

### One-way doors, summarized

| Decision | One-way? |
|---|---|
| URL shapes `/v0/beads/*`, `/healthz` (Gas City's probe already asserts `$GC_BEADS_API/v0/beads/ready`) | **Yes** |
| `x-go-type` pinning: `types.Issue` JSON encoding = HTTP contract | **Yes** (by design — one compatibility domain) |
| Ready items = `IssueWithCounts` (counts fields permanent) | **Yes** |
| problem+json envelope + each documented status/`code` pair | **Yes** (additions additive; removals/renames breaking) |
| Keyset order `(created_at DESC, id ASC)` welded to `/v0/beads/issues` | **Yes** (new orders need new surface) |
| `limit=0` = unlimited; defaults 50/100 from shared constants | **Yes** (clients will depend on both) |
| Page envelopes as top-level objects `{items, ...}` | Door opened in the extensible position |
| Cursor internal encoding (`v` member) | No — opaque + versioned, server-private |
| No cursor on `/ready`; no `sort` on `/issues`; no `skip_labels` | No — all additive later |
| `capabilities` list contents, problem extension members | No — additive |

## 4. Codegen, CLI migration, server lifecycle and security

### Codegen and the drift gate

The wiring copies the hosted product's proven mechanism exactly (hand-written spec, `oapi-codegen`
types-only, `go:generate` + Go `tool` directive, three-part CI gate), relocated and renamed for OSS.
Verified against the hosted repo: the generate line lives in `cmd/beads-web/apigen/doc.go`
(`//go:generate go tool oapi-codegen -generate types -package apigen -o types.gen.go ...`), the tool
directive at `go.mod:251` (`tool github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen`, pinned
v2.6.0), and the gate is Makefile `spec`/`spec-ci`: regenerate, `git diff --quiet -- $(SPEC_FILES)`
or fail, then `go test ./cmd/beads-web/ -run 'TestSpec' -count=1`.

OSS layout:

```
internal/httpapi/spec/openapi.v0.yaml     # hand-written, source of truth
internal/httpapi/spec/embed.go            # //go:embed openapi.v0.yaml (parity tests read it)
internal/httpapi/apigen/doc.go            # go:generate line below
internal/httpapi/apigen/types.gen.go      # generated, committed
```

`internal/httpapi/apigen/doc.go`:

```go
// Package apigen holds Go types generated from the bd serve OpenAPI spec
// (spec-first). Do NOT hand-edit types.gen.go — change the spec and run
// `make api-gen`.
//
//go:generate go tool oapi-codegen -generate types,skip-prune -package apigen -o types.gen.go ../spec/openapi.v0.yaml
package apigen
```

`go.mod` gains `tool github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen` pinned at v2.6.0 (the
hosted-proven version; beads is on Go 1.26.5, tool directives supported; the repo currently has no
`tool` directives, so this is the first — review the go.sum diff in isolation). Because `Issue`,
`IssueWithCounts`, and `IssueDetails` are pinned to the canonical Go types via
`x-go-type`/`x-go-type-import` (no second wire struct, per the binding corrections), `types.gen.go`
contains only the envelope/request types (`ReadyPage`, `IssuesPage`, `ClaimRequest`,
`ClaimResponse`, `ContextResponse`, `Health`, `Problem`) referencing `internal/types` directly.

Makefile targets (names final):

```make
api-gen:      ## regenerate OpenAPI types from internal/httpapi/spec/openapi.v0.yaml
	go generate ./internal/httpapi/apigen
api-check: api-gen   ## three-part spec drift gate (regen, diff-or-fail, named tests)
	@git diff --quiet -- internal/httpapi/apigen internal/httpapi/spec || \
	  { git --no-pager diff --stat -- internal/httpapi; \
	    echo "openapi drift: run 'make api-gen' and commit"; exit 1; }
	go test ./internal/httpapi/ -run 'TestSpecRouteParity|TestWireTagBijection|TestProblemMapping' -count=1
```

CI wiring: one line appended to `scripts/ci/pr-policy.sh` in the existing `ci_time` idiom —
`ci_time "check openapi spec gate" -- make api-check` — so the gate runs in the required PR-time
`ci-pr-policy` job (`Makefile:186`). It must NOT live only in the `Main` workflow, which is
perpetually concurrency-cancelled and hides breakage. The three named tests (all CGO-free, so they
also run under `ci-pr-core`'s `go test ./...`):

- `TestSpecRouteParity` (`internal/httpapi/spec_parity_test.go`) — parses the embedded YAML and
  asserts exact set-equality between spec `paths` (method+path) and the server's route table; each
  route entry carries an explicit `specPath` so the `{id}:claim` ↔ ServeMux-pattern mapping is
  declared, not inferred. Modeled on the hosted `TestSpecBijectionAndScopeParity`.
- `TestWireTagBijection` (`internal/httpapi/wire_bijection_test.go`) — reflects over the JSON tags
  of `types.Issue`, `types.IssueWithCounts`, and `types.IssueDetails` and asserts TWO-WAY
  set-equality with the spec's schema properties, modulo an explicit in-test allowlist for
  documented omissions. This is the only guard on the x-go-type-pinned schemas (the compiler cannot
  see the spec), so a new `types.Issue` field fails CI until the spec documents it, and vice versa.
  A round-trip fixture test is NOT a substitute (a new `omitempty` field absent from the fixture
  passes both directions); the fixture round-trip (including bool/number `metadata` values from
  `--set-metadata k=true` / `k=42` inference) exists additionally, as a decode-strictness check.
- `TestProblemMapping` (`internal/httpapi/problem_test.go`) — table test driving every sentinel
  (wrapped and bare) through the error mapper and asserting status+code, including the
  nil-issue and wrapped-`sql.ErrNoRows` not-found cases.

### CLI migration mechanics

Per binding rule 11, every extraction lands as its own no-op refactor slice — never the mega-slice.
Each slice is bisectable against the byte-pinned protocol corpus.

**How the corpus protects the migration.** `cmd/bd/protocol` (32 files) is the producer half of a
cross-version contract system: `TestCorpusGolden` runs a deterministic command plan against a live,
source-built, Dolt-backed `bd`, canonicalizes (timestamps → `<TS>`, sorted keys/arrays), and
byte-compares against committed goldens under `cmd/bd/protocol/testdata/corpus/`. A diff is a hard
failure; deliberate wire changes require `make corpus-regen` plus a `JSONSchemaVersion` bump in
`cmd/bd/output.go` — and once `/v0` ships, a bump additionally requires a new HTTP path version
per the welded-domain evolution policy (§ 3, schema pinning). The extraction slices therefore
have an executable no-op oracle: they must land
with ZERO corpus diff and no `JSONSchemaVersion` change. Two additional guards: (1) golden
filter-equality tables are written FROM the old inline builder code BEFORE it is deleted
(old-construction == new-builder output across an input matrix); any delta found is triaged as an
explicit, corpus-verified behavior fix in the PR — never silently absorbed; (2) the known local
trap: `cmd/bd` subprocess tests silently reuse a stale repo-root `bd` — rebuild it before trusting
local corpus runs.

Slice-by-slice (shared package `internal/workapi`; its internal shape is specified in § 2 — this
section fixes only the file mechanics; slices M1–M4 here are slices 1–4 of § 5's delivery plan):

**Slice M1 — `bd list` (no-op).** `cmd/bd/list_filter.go` (343 lines) is DELETED; its contents move
verbatim to `internal/workapi/list.go` with exported names: `buildListFilter` (`list_filter.go:119`)
→ `workapi.BuildListFilter`, `loadListFilterConfig` (`:78`) → `workapi.LoadListConfig`,
`listFilterConfig`/`listFilterConfigSource` → `workapi.ListConfig`/`workapi.ConfigSource`, and the
two adapters `directConfigSource`/`proxiedConfigSource` (`:54`/`:66`) →
`workapi.NewStoreConfigSource(storage.DoltStorage)` / `workapi.NewUOWConfigSource(uow.UnitOfWork)`.
Call sites edited: `cmd/bd/list.go:539` and `cmd/bd/list_proxied_server.go:55`. `list_input.go`
(`gatherListInput`, `:102`) stays in `cmd/bd` — cobra flag parsing is CLI-local — now emitting
`workapi.ListParams`. `list_filter_status_test.go` cases move to `internal/workapi`. The shared
default constant `workapi.DefaultListLimit = 50` is introduced here and `list.go:743`'s flag
registration consumes it (binding rule 9: HTTP later reads the same constant; 0 = unlimited).

**Slice M2 — `bd ready` (no-op).** The inline duplicate filter construction at
`cmd/bd/ready.go:104-215` is DELETED; the direct RunE path calls `gatherReadyInput(cmd)` exactly as
the proxied path already does (`ready_proxied_server.go:18`). `cmd/bd/ready_input.go` shrinks to a
cobra→`workapi.ReadyParams` mapper and calls `workapi.BuildReadyFilter` — the single home of the
ready defaults (`Status:"open"`, sort validation, label/type normalization,
`workapi.DefaultReadyLimit = 100` consumed by `ready.go:796`, 0 = unlimited). The cwd-derived
directory-label substitution (`config.GetDirectoryLabels()`, `ready.go:141-145`) is applied in
`cmd/bd` BEFORE calling the builder and never enters the library — meaningless in a server process.
`resolveMaxRows` also stays cobra-side, feeding `ReadyParams.MaxRows`.

**Slice M3 — `bd show` detail assembly (no-op).** New `internal/workapi/detail.go`:
`BuildIssueDetails(ctx, DetailSource, id, DetailOptions)` plus `NewStoreDetailSource(storage.DoltStorage)` and
`NewUOWDetailSource(uow.UnitOfWork)` (the DetailSource interface carries the explicit `isWisp` axis;
shape in the contract section). DELETED in favor of it: the direct assembly block in
`cmd/bd/show.go:147-235` (labels + `GetDependenciesWithMetadata` +
`CountDependents`/`CountDependencies`/`CountIssueComments`) and the proxied helpers
`proxiedGetIssueOrWisp`/`proxiedListDeps`/`proxiedCountDeps`/`proxiedCountComments`
(`show_proxied_server.go:127-170`) together with the assembly core of `proxiedBuildDetails`
(`:478-556`). Binding rule 4 is implemented HERE, in the shared function: on the UOW seam the miss
shape is a bare/wrapped `sql.ErrNoRows` (`internal/storage/domain/db/issue.go:403-405` returns
`sql.ErrNoRows` unwrapped; the domain layer wraps it) and is NEVER `storage.ErrNotFound`;
`BuildIssueDetails` normalizes {`errors.Is(err, sql.ErrNoRows)`, `errors.Is(err,
storage.ErrNotFound)`, nil-issue-with-nil-error} to the `storage.ErrNotFound` sentinel — the same
dual-sentinel treatment the CLI's own proxied code already applies
(`cmd/bd/create_proxied_server.go:497`) — so no future caller can reintroduce the
missing-issue-500. CLI-local and untouched: fuzzy/substring ID resolution, `--current`, watch mode,
the array-of-one `--json` envelope, and the `--include-dependents`/`--thread`/`--refs`/`--children`
extras, which layer additional queries on top of the shared assembly.

**Slice M4 — claim consolidation (no-op for outcomes, one implementation for retry/commit).**
Binding rule 5: the ONLY retry/commit implementation is `uow.RunTxResult`
(`internal/storage/uow/tx.go:76` — fresh UOW per attempt, 25ms→15s serialization backoff,
nothing-to-commit tolerance, commit-with-message; test-covered on main). The bespoke
`cenkalti/backoff` loop in `applyUpdateProxiedOne` (`cmd/bd/update_proxied_server.go:93-126`) is
DELETED and its per-attempt body (`applyUpdateProxiedAttempt`) becomes the `TxFuncResult` passed to
`RunTxResult`; retry classification is identical (retry only on `uow.IsSerializationError`,
permanent otherwise), so this is a mechanical re-expression locked by the `lease_claim` and
`errors_contract` corpus files and the update-conditional test suites. The `updateIDFailure`
rendering on exhaustion stays in cobra-land. The direct path: post-facade it is NOT untouched —
direct `bd update` (including `--claim`) routes through `issueops.Lifecycle.Update` via
`cmd/bd/issueops_adapter.go` (same CAS, same sentinels underneath, so the classification
contract is unchanged). Slice M4's scope is unaffected either way: it is proxied-only
(`cmd/bd/update_proxied_server.go`, a file the facade branch does not touch).
The HTTP claim handler (server slice) calls the very same `RunTxResult` — zero claim-protocol code
in `internal/httpapi` beyond the sentinel→status mapping.

The other ~46 direct/proxied command pairs are untouched, explicitly out of scope.

### Server lifecycle and security

**Command and flags.** New `cmd/bd/serve.go` (no `serve` command exists today): `bd serve
--addr 127.0.0.1:0 [--allow-non-loopback]`. No other flags in v0 (one pending exception: the
off-by-default `--max-rows` recommended by SRE review, awaiting owner sign-off — open
question 2). The bound address is printed as
the first stdout line (`bd serve: listening on http://127.0.0.1:PORT`); no default port is
blessed, but deployments are steered to an explicit fixed port (rule 7 note below — the
ephemeral default carries no mutual exclusion).

**Mode gate — reconciled honestly (rule 10).** Verified on main: `uowProvider` is constructed only
when `proxiedServerMode` (`cmd/bd/main.go:1378-1388` → `newProxiedServerUOWProvider`,
`cmd/bd/uow_factory.go`, which routes to `uow.NewDoltServerUOWProvider` for the managed child and
`uow.NewExternalDoltServerUOWProvider` when `info.External` is set — i.e. the external-dolt
topology). Plain server / shared-server mode (`usesSQLServer()` true via `serverMode` or
`doltserver.IsSharedServerMode()`, `cmd/bd/store_factory.go:20-32`) builds a `DoltStore` and has NO
UOW provider on main. Therefore:

| Workspace mode | v0 behavior | Delivered by |
|---|---|---|
| proxied (managed Dolt child) | works — serve reuses the `uowProvider` built in `PersistentPreRunE` | serve-skeleton slice |
| external dolt (proxied topology with `info.External`) | works — same `uowProvider` path | serve-skeleton slice |
| dolt server / shared-server mode | typed refusal, then wired | server-mode wiring slice: export `uow.NewSQLServerUOWProvider(ctx, endpoint, database, user, password, tlsName, teamServer)` as a thin wrapper over the existing unexported `openAndInitSchema` (`internal/storage/uow/dolt_sql_provider.go:179`), fed from the live doltserver state (127.0.0.1 + `state.Port`); serve is also excluded from post-run maintenance (`effectiveRootStorePolicy`) in this slice, since the non-proxied `PersistentPostRunE` branch otherwise runs auto-backup/export/push |
| embedded | permanent typed refusal (decided parameter 5) | — |

Refusal messages must not promise what the gate refuses. Embedded:
`&storage.ErrUnsupported{Op: "serve", Backend: "embedded-dolt"}` with detail "bd serve requires a
Dolt SQL server; this workspace uses embedded Dolt" (`embedded-dolt` names the distinct
`EmbeddedDoltStore` backend implementation, in-vocabulary for the field). Server-mode (until its
slice lands): `&storage.ErrUnsupported{Op: "serve", Backend: "dolt"}` with detail "bd serve
supports proxied-server workspaces (managed or external dolt) today; dolt server mode support is
tracked". The mode belongs in the detail TEXT, not the `Backend` field: the type documents
`Backend` as a backend name (`// e.g. "sqlite"`, errors_unsupported.go:10) and is the embryo of
the pluggable-backend seam's error taxonomy, so stuffing a topology string like
`"dolt-server-mode"` into it would hand every downstream `errors.As` dispatch on `Backend` a
mixed backend/mode vocabulary that is wire-adjacent and hard to walk back.
`storage.ErrUnsupported` (`internal/storage/errors_unsupported.go:8`) is reused, not reinvented.

**Bind posture.** `validateServeAddr(addr, allowNonLoopback)` follows
`validateManagedServerConfigPolicy` (`cmd/bd/proxied_server.go:449-498`) exactly: `net.SplitHostPort`;
the host MUST parse as a numeric IP literal (`net.ParseIP != nil`) — hostnames including
`"localhost"` are refused, because a name can silently license other listeners (the proxied-server
validator documents that a "localhost" host also licenses the default unix socket); unix sockets are
not supported at all; `!ip.IsLoopback()` requires `--allow-non-loopback` and prints a loud stderr
warning that v0 has no authentication. Trust model on loopback: identical to the managed Dolt child
(root/empty password) the server fronts — HTTP adds no new exposure on the same boundary. No auth,
no TLS, no CORS in v0; `Cache-Control: no-store` on every response; claim body capped at 1 MB via
`http.MaxBytesReader`. The designated future auth mechanism (only when non-loopback grows real
users) is the existing rotating 0600 shared secret with `crypto/subtle` comparison
(`internal/storage/dbproxy/identity`) surfaced as a bearer token — reused, vendor-neutral.

**Browser exposure and DNS rebinding (defense-in-depth, stated rather than accidental).** An
unauthenticated loopback HTTP service is reachable from any browser running on the host, so the
browser attack surface must be reasoned about explicitly, not left to accident. Two properties
protect it today: the server never emits `Access-Control-Allow-Origin`, so a cross-origin page
cannot READ responses; and the claim handler REQUIRES `Content-Type: application/json` (anything
else → 400, in-vocabulary) — a JSON content type is not CORS-"simple", so a cross-origin claim
always triggers a preflight the server never approves. Making the content-type check explicit
matters: without it, an attacker's `text/plain` POST carrying JSON would skip the preflight.
Neither property stops **DNS rebinding**, where the attacker's page re-resolves its own hostname
to 127.0.0.1 and the browser then issues plain same-origin requests with no CORS in play. The
standard local-dev-server defense is applied: every request's `Host` header must match a small
allowlist — `127.0.0.1`, `[::1]`, or `localhost` (any port), plus the configured bind address
when bound non-loopback — anything else → 400 `invalid_argument` before any handler runs. A
rebound request arrives bearing the attacker's hostname in `Host` (browsers preserve the
original name) and is rejected. `TestProxiedServerServeModeGate` (§ 5) pins the foreign-`Host`
rejection.

**No flock, no pidfile, no serve-info.json (rule 7).** dbproxy needs that machinery because clients
race to auto-spawn it; `bd serve` is operator-invoked and the TCP bind IS the mutual exclusion —
`EADDRINUSE` produces a clean error naming the address. Stated honestly rather than over-claimed:
that exclusion exists ONLY under an explicitly fixed port. The default `--addr 127.0.0.1:0`
allocates an ephemeral port, so N concurrent serves against one workspace run in parallel
silently, each on its own port, with no enumeration mechanism (no pidfile, by this same rule) —
data-safe, but "which instance are clients actually talking to" has no answer beyond each
instance's startup log line. The deployment recommendation, blessed in `bd serve --help` and the
slice-11 runbook, is therefore an explicit fixed-port `--addr`; the ephemeral default is for
ad-hoc and test use, where the printed bound address is consumed immediately. Two concurrent
serves against one workspace
are read-safe and claim-safe (the CAS is arbitrated in the SQL server). A discovery file would be a
brand-new unversioned file-format contract nobody asked for. Likewise no invented workspacegate
lease: verified that nothing on main outside `cmd/bd/doctor/gitignore.go` touches
`internal/workspacegate`, so serve acquires nothing — parity with every other command — and this is
documented rather than papered over.

**One UOW per request, rollback guaranteed.** Every read handler opens one UOW from the provider and
closes it via a detached context:

```go
uw, err := provider.NewUOW(ctx)            // request ctx: honors client deadline for BEGIN
...
defer func() {
    closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
    defer cancel()
    uw.Close(closeCtx)                     // RollbackUnlessCommitted (uow.go)
}()
```

Rationale, verified in the tx layer: `Close` → `RollbackUnlessCommitted` sends `ROLLBACK` on the
pinned `*sql.Conn`; if that send fails the conn is POISONED, not returned (`doltserver_tx.go:88-95`
— go-sql-driver's ResetSession does no COM_RESET_CONNECTION, so a session with an open transaction
must never be reused). Correctness is therefore already protected, but closing with the cancelled
request context would burn one pinned session per client disconnect; the detached context lets the
ROLLBACK complete and the conn return clean. Reads never call `Commit`. The generic
`uow.RunTxRead` closes with the caller's context, so serve uses its own `withUOW` helper with the
detached close instead.

**The claim endpoint** is the only committing path: `uow.RunTxResult(ctx, provider, work)` with
commit message `"bd serve: claim <id> by <actor>"` — the identical implementation the proxied CLI
consumes after Slice M4 (rule 5). `backoff.WithContext` already bounds the 15s retry budget by the
request context. Retry exhaustion maps to 503 `busy` with `Retry-After: 5` (retryable by contract,
the server-side analogue of the CLI's "retries exhausted on write conflicts" failure). Not
`Retry-After: 1`: the server has just spent up to 15 s failing to commit under write contention,
and inviting every 409/503 loser back one second later convoys claim requests that each occupy a
semaphore slot for up to 15 s — degrading read availability exactly when dispatch is busiest. A
5 s comeback keeps slots circulating; the `busy`-vs-`db_unavailable` triage distinction is
unchanged. "Nothing to
commit" is tolerated inside `RunTxResult` (wisp claims write only `dolt_ignored` tables).

**The write path gets the same detached-close protection as reads — fixed in `RunTxResult`, not
forked.** Verified: `RunTxResult`'s per-attempt cleanup is `defer uw.Close(ctx)` with the
CALLER's context (uow/tx.go:90). On a client disconnect or the 60 s deadline expiring
mid-attempt, that `Close` runs with an already-cancelled context, the ROLLBACK `ExecContext`
fails immediately, and `doltserver_tx` poisons the pinned conn — precisely the failure mode the
read path's detached close above exists to prevent. Routing the only committing endpoint through
the one helper that still closes with the live request context would be an internal
contradiction: under flaky clients, or claim contention pushing requests toward the deadline, the
write path would churn one poisoned session per disconnect. The fix lands in `RunTxResult`
itself, not as a serve-side variant (rule 5 — one implementation, so no fork): the per-attempt
close adopts the same `context.WithoutCancel` + 5 s timeout pattern as `withUOW`, and the proxied
CLI inherits the same robustness (today a Ctrl-C mid-claim burns a session the same way).
Delivered with slice 9 (§ 5).

**Concurrency limits.** Facts: `openDB` sets no pool limits (`dolt_sql_provider.go:168-177`,
`database/sql` default = unlimited) and every UOW pins one connection (`BeginTx`,
`dolt_sql_provider.go:50-56`), so an unbounded request burst means unbounded SQL connections.
serve bounds in-flight requests with a BLOCKING semaphore — `const serveMaxInflight = 16`, acquired
with the request context — which queues rather than refuses, so no 429/refusal status enters the
permanent wire surface (rule 12). The queue is bounded in TIME, not only in width: semaphore
acquisition waits at most 10 s (inside the request's 60 s deadline); a timed-out acquisition maps
to the already-documented 503 `busy` with `Retry-After: 1` — in-vocabulary, so rule 12 holds —
instead of parking the request for the full deadline and then answering a non-retryable 500.
16 pinned conns sit under Dolt's default `max_connections` only when serve is the sole tenant —
which is FALSE in external-dolt mode, where other `bd` CLI invocations, reconciler-driven forks,
and any second serve instance share the same server's `max_connections` budget; the slice-11
runbook documents that budget arithmetic rather than assuming it away. The in-flight and wait
constants can become operator flags later without wire impact.

**Pool limits on the serve-owned `*sql.DB` (defense in depth, not semaphore trust).** The HTTP
semaphore bounds handler concurrency, not connections: poisoned-conn replacement after a failed
ROLLBACK, `RunTxResult`'s retry attempts (a fresh UOW — a fresh pinned conn — per attempt), and
any future semaphore-exempt endpoint that touches the DB all escape it. serve therefore sets
explicit limits on the provider's pool at startup (via a small exported knob on the uow provider —
the `*sql.DB` is provider-internal today): `SetMaxOpenConns(serveMaxInflight + 4)` (headroom for
retry/replacement churn), `SetMaxIdleConns(serveMaxInflight)`, `SetConnMaxIdleTime(5m)`,
`SetConnMaxLifetime(1h)`. This is the repo's own idiom applied to the one seam that lacks it:
`embeddeddolt/open.go:51-54` and `dolt/transaction.go:162` set pool limits and
`dolt/connection_pool_test.go` proves the cap bounds driver opens — only uow's `openDB` sets
nothing. One operational-footprint consequence is documented rather than discovered: idle pooled
conns are live TCP connections through dbproxy, whose idle-watcher counts them as active
(`dbproxy/proxy/server.go:297`, `activeConns`), so a running serve keeps the watcher
armed-never-firing and pins the managed Dolt child alive for serve's lifetime;
`SetConnMaxIdleTime` bounds how long that outlasts serve's last actual traffic.

The semaphore bounds the DB tier only; two front-tier gaps are closed explicitly rather than
left to it:

- **Accepted connections are capped.** Go's `http.Server` spawns a goroutine per connection, and
  a handler parked on a full semaphore holds its goroutine, fd, and buffers — so a flood of
  valid requests (or a buggy client in a retry loop) accumulates unbounded blocked goroutines
  behind 16 slots. The listener is wrapped in `netutil.LimitListener(l, 64)`
  (golang.org/x/net/netutil — already in the module graph); excess connections wait in the
  kernel accept backlog, not in Go memory. Like the semaphore, a cap that queues is not wire
  surface (rule 12); the constant can become an operator flag later.
- **`/healthz` and `GET /v0/beads/context` bypass the DB semaphore** — both serve
  startup-snapshot data and touch no DB (§ 3), so liveness and identity stay observable while
  all 16 DB slots are held by long scans. They still count against the connection cap. This
  makes `/healthz` LIVENESS-only by definition: it answers 200 while Dolt is down, wedged, or an
  hour dead — a documented property, not a gap. The READINESS story is explicit rather than
  absent: v0 adds no DB-probing endpoint (rule 12 minimality), so the documented operator
  readiness probe — in `bd serve --help` and the slice-11 runbook, suitable for systemd/k8s
  probes — is `GET /v0/beads/ready?limit=1`: a real query through the full semaphore + UOW path,
  where 200 means ready and 503 means live-but-not-ready. A dedicated readiness endpoint later
  is additive (§ 3's context note already reserves that door).

**Timeouts at every layer.** `http.Server{ReadHeaderTimeout: 10s, ReadTimeout: 30s, IdleTimeout:
120s, WriteTimeout: 0, MaxHeaderBytes: 64<<10}`. `WriteTimeout` is deliberately 0: `limit=0` means
unlimited exactly as in the CLI (binding rule 2 — Gas City's reconciler does full active-set scans),
and a large response must not be truncated mid-body; slowloris exposure is covered by the header and
idle timeouts on a loopback socket. DB dial/ping honors context (`openDB` uses `PingContext`).

`WriteTimeout: 0` removes the server's only whole-request backstop, so the deadline moves into
the handlers: EVERY handler — reads included, not just claim — wraps semaphore acquisition + UOW
+ query in a 60s `context.WithTimeout` (for claim that is ≥ the 15s retry budget plus commit;
generous by design, and like the semaphore and connection-cap constants it can become an
operator flag later without wire impact). A
request parked behind sixteen `limit=0` scans therefore times out and releases its slot instead
of pinning a goroutine indefinitely — without this, ReadTimeout covers only the header/body read
and nothing bounds the post-read lifetime of a blocked request. The deadline deliberately
excludes the response write: a large `limit=0` body must not be truncated mid-write, and a
slow-reading client holds one of the 64 `LimitListener` slots (bounded), not unbounded memory. A
server-side deadline surfaces as the generic 500 with the real error logged — with one carved-out
case already in vocabulary: a semaphore-acquisition wait that times out surfaces as the retryable
503 `busy` (above) — v0 documents only
the status codes its six operations need (rule 12), so there is no 504/413/429 vocabulary.

**The wedge scenario, closed end to end.** The failure mode the two bounds above exist for: Dolt
stops answering but keeps connections alive (`dolt gc`, lock contention). Without them, the 16
slots fill with un-cancellable queries; every subsequent request — 13 dispatch probes per 30 s
patrol plus reconciler full scans per store — queues on the blocking semaphore with no wait
bound; goroutines accumulate behind the 64-connection cap; `/healthz` stays green
(liveness-only, above); and when Dolt unwedges, a thundering herd of stale queued requests
executes. With them: the 60 s handler deadline cancels in-flight queries
(`QueryContext`/`ExecContext` propagate cancellation through go-sql-driver) and their conns are
returned or poisoned-and-replaced under the pool cap; the 10 s semaphore-wait bound sheds the
queue as retryable 503 `busy` instead of accumulating it; and the semaphore-saturation log event
(observability floor, below) is the 3 a.m. signal that distinguishes "wedged" from "no traffic".
The server self-recovers instead of converting a hung Dolt into a silently growing black hole,
and no new status vocabulary is introduced at any point.

**Graceful shutdown.** serve blocks on `rootCtx.Done()` — already signal-aware via
`setupGracefulShutdown` (`cmd/bd/main.go:1719-1726`, SIGINT/SIGTERM/SIGHUP) — then
`srv.Shutdown(ctx, 20s)` (listener closes immediately; in-flight requests, including a claim
mid-commit, drain), `srv.Close()` on overrun, exit 0. The drain budget is 20 s, not 10, and the
number is reconciled with the claim path rather than picked: `srv.Shutdown` does NOT cancel
in-flight handler contexts, so a claim inside its 15 s serialization-retry budget would outlive
a 10 s drain and have its TCP connection killed by `srv.Close()` — possibly AFTER the Dolt
commit landed, leaving the client unable to know whether the claim succeeded. 20 s covers the
retry budget plus commit; for the residual window (a claim near its 60 s deadline at shutdown),
the client-retry contract closes the ambiguity: the claim is idempotent per actor, so
re-claiming after a killed connection returns 200 with `already_claimed: true` if the commit had
landed — the spec description and runbook state this recovery explicitly. One signal consequence
is documented rather than discovered: `setupGracefulShutdown` registers SIGHUP, so closing the
terminal of a foreground `bd serve` triggers a graceful shutdown — run it under a supervisor
(systemd, tmux, nohup) for anything longer than an experiment. The provider is closed where it is closed for
every proxied command today: `PersistentPostRunE` (`main.go:1509-1517`, which in proxied mode does
nothing but close `uowProvider` — no auto-commit/export/push machinery can fire).

**Observability floor (the operator's 3 a.m. contract).** "No metrics endpoint" is a defensible
non-goal; no log is not — without a request log the operator of a failing serve cannot
distinguish no-traffic from all-traffic-hanging from all-traffic-503ing, and the endpoints alone
cannot tell them (`/healthz` is liveness-only, `/context` is a startup snapshot). serve ships a
minimum floor in the skeleton slice as an acceptance criterion (§ 5, slice 6), not a nicety:

- **One structured line per request, to stderr** (stdout carries only the bound-address line):
  method, path, status, problem `code` when non-2xx, total duration, semaphore-wait duration,
  and UOW-acquire duration. The three timings are what separate "Dolt is slow" from "serve is
  saturated" from "handler bug".
- **Startup lines**: bound address, workspace root, resolved mode (proxied / external / server),
  and the effective limits (semaphore, connection cap, pool caps, deadline).
- **Shutdown lines**: signal received, drain start, drain complete — or forced close, with the
  count of connections killed.
- **A saturation event** when the in-flight semaphore blocks a request for more than 1 s, and
  one when a semaphore wait times out — the wedge-detection signal named above.
- **A `request_id` extension member** on every problem response (additive, allowed by the
  envelope's rules; documented once on the Problem schema like the middleware 400), echoed in
  the request log line, so a client-reported failure correlates to exactly one log line.

Precedent, not invention: dbproxy already writes structured trace lines through its `tracef`
logger (`dbproxy/proxy/server.go:105`); serve must not ship with less visibility than the proxy
it fronts. Plain structured key=value text on stderr; log rotation, levels, and a metrics
endpoint remain non-goals.

**When the SQL server dies mid-request.** In-flight statements fail; the mapper classifies
driver/net/`ErrBadConn`-class failures from `NewUOW` or query execution to a retryable 503 (never
404 — the hosted viewer's every-error-is-404 bug is structurally excluded because 404 is reachable
only via the normalized `ErrNotFound` sentinel from the shared function, rule 4). The provider is
retained across failures: the next request's `NewUOW` re-pings and, on the managed path, the
dbproxy respawn machinery re-establishes the child. Required integration test: kill the Dolt server
under a live serve → request returns 503 → restart Dolt → next request returns 200 with NO serve
restart. If self-healing proves incomplete at implementation time, the fix is a provider-recycle on
`driver.ErrBadConn` inside serve — a small, contained addition; flagged as the one open risk here.

**Auto-commit — decided.** serve never touches the CLI's per-command protocol (`doltAutoCommit`,
`commandDidWrite`, `PersistentPostRunE` maintenance): those are non-atomic package globals sized for
one-shot commands. Durability is per-request: the claim's `uw.Commit` inside `RunTxResult` creates
exactly one Dolt commit per successful claim — identical cost and semantics to proxied
`bd update --claim` today. This is also the concurrency-correct choice by the codebase's own
testimony: `PersistentPreRunE` defaults auto-commit OFF in server mode because "firing DOLT_COMMIT
after every write under concurrent load causes 'database is read only' errors" (`cmd/bd/main.go`,
verified comment). Reads never commit; version-control cadence beyond per-claim commits stays the
operator's existing workflow.

**Hooks — decided.** serve fires NO hooks. Facts: the UOW seam itself fires none, but the proxied
CLI update path DOES fire `on_update` synchronously in cobra-land (`fireProxiedUpdateHooks`,
`cmd/bd/update_proxied_server.go:230-252`, runner resolved from the process cwd,
`proxiedHookRunner` `:254-268`). So this is an honest, documented front-end divergence, not silent
drift: an HTTP claim will not run `on_update`, a CLI claim will. Justification: a hook is a
user-controlled subprocess per mutation — in a concurrent server that is an unbounded-latency
multiplier, an orphaned-child hazard at shutdown, and its cwd-derived hook-dir resolution is
meaningless in a server process; hooks are workspace-owner policy, part of the front end, not of the
shared contract (CAS semantics and result shaping remain identical). Recorded in the spec
description and `bd serve --help`; revisit behind an explicit `--fire-hooks` opt-in only on demand.

**Who is the actor on an HTTP claim, and is the CAS fence real?** The actor is the required
`{"actor": "..."}` body field, validated at the wire edge (§ 3 claim: trimmed, non-empty,
≤ 256 bytes, no control characters — the domain layer alone checks only `actor == ""`). The server NEVER infers it: the CLI's derivation chain
(`getActorWithGit`, `cmd/bd/main.go:612-641` — `--actor` flag > `BEADS_ACTOR` > `BD_ACTOR` > git
`user.name` > `$USER` > "unknown") describes the server operator's identity, not the caller's, and
is meaningless for a remote client. Can a client spoof it? Yes: v0 has no authentication, so any
loopback client can present any actor — which is EXACTLY the CLI's trust model today, where any
local process can run `bd update --claim --actor alice`. Actor has always been caller-asserted
provenance for the audit trail, not authenticated identity. What this means for the fence: the CAS
is real as a mutual-exclusion guarantee and advisory as an identity guarantee. The claim is a
single-row compare-and-set inside the request transaction (`domain.issueUseCaseImpl.claim`,
`internal/storage/domain/issue.go:458-491`: `Updated` wins; held-by-another →
`storage.ErrAlreadyClaimed`; non-open → `storage.ErrNotClaimable`; same-actor re-claim → idempotent
success with `AlreadyClaimed: true`), so under any mix of honest and spoofed actors, N concurrent
claimers on one issue yield exactly one 200 and N−1 typed 409s, and no update is ever lost — the
property Gas City's dispatch actually depends on. A spoofer cannot STEAL a live claim through the
claim endpoint: the CAS refuses non-open rows, and eviction (`bd update --force`) is not exposed
over HTTP. What spoofing buys is misattribution: claiming an OPEN issue under someone else's name,
or a state-neutral idempotent "success" by presenting the current holder's name. Within the
loopback boundary that is the pre-existing local-user trust model; under `--allow-non-loopback` it
extends to every network peer, which is precisely why that flag prints the no-auth warning. The v1
path, if remote exposure becomes real, is the dbproxy identity secret authenticating the client
channel, with actor then bound server-side to the authenticated principal.

## 5. Test strategy, slice plan, risks, and open questions

Package names below follow § 2: `internal/workapi` is the shared
filter/shaping contract, `internal/httpapi` is the HTTP layer. Every mechanism referenced here was
verified against the tree at `/data/projects/beads/.claude/worktrees/sd-server`.

### Test strategy

The repo has four test tiers, and this design uses all four. Verified mechanics:

- **Pure unit tests** (no build tag) run in the required PR job via `make ci-pr-core` →
  `scripts/ci/pr-core.sh` → `go test -race -short -skip '^TestEmbedded' ./...`. Anything
  load-bearing that can run without Dolt MUST live here, because the cgo tiers are conditional.
- **`*_embedded_test.go`**: `//go:build cgo`, functions named `TestEmbedded*` (e.g.
  `TestEmbeddedReady`, cmd/bd/ready_embedded_test.go:19). Excluded from ci-pr-core by the
  `-skip '^TestEmbedded'` pattern; run in dedicated shards.
- **`*_proxied_integration_test.go`**: `//go:build cgo`, functions named `TestProxiedServer*`,
  runtime-gated by `requireProxiedServerEnv` (`BEADS_TEST_PROXIED_SERVER=1` +
  `testutil.RequireDoltBinary`, cmd/bd/proxied_integration_helpers_test.go:28-34). These drive the
  freshly built `bd` binary as a subprocess with `BEADS_DOLT_PROXIED_SERVER=1` env (`bdProxiedRun`).
  They run in 15-way shards in `.github/workflows/pr-risk.yml` (PR-time, only when
  `ci-embedded-tier.sh` flips `full_embedded=true`) and in `main.yml` (push-to-main, which is
  perpetually concurrency-cancelled — never rely on it as the only gate). New `TestProxiedServer*`
  tests hash-distribute across shards until pinned in
  `.github/scripts/proxied-cmd-test-shards.txt`; pin the serve tests there when they land.
- **Protocol corpus** (`cmd/bd/protocol`): `TestCorpusGolden` runs a deterministic command plan
  against a from-source Dolt-backed `bd`, canonicalizes, and byte-compares against
  `testdata/corpus/`. It is the primary no-op-refactor guard for slices 1-4. Any deliberate wire
  change requires `make corpus-regen` plus a `JSONSchemaVersion` bump — the extraction slices must
  require neither.
- Subprocess-test gotcha (known, recurring): rebuild the binary under test; a stale repo-root `bd`
  silently passes old behavior. All new subprocess tests take the built-binary path from the
  existing helpers, never `./bd`.

#### Named tests and what each one proves

**Tier A — pure, in ci-pr-core (these are the drift gates; they must never move to a cgo shard):**

1. `TestWireTagBijection` (`internal/httpapi/wire_bijection_test.go`) — **rule 1**. Reflects over
   the JSON tags of `types.Issue`, `types.IssueWithCounts` (which embeds `*Issue` and adds
   `dependency_count`/`dependent_count`/`comment_count`/`parent`, internal/types/types.go:976-982),
   and `types.IssueDetails`, walking embedded structs, and asserts **two-way set equality** with the
   property names of the spec's `Issue`/`IssueWithCounts`/`IssueDetails` schemas (parsed from the
   `//go:embed`-ed YAML). Divergence is allowed only through an explicit in-test allowlist where
   every entry carries a comment justifying it. Proves: a field added to `types.Issue` without a
   spec edit fails CI, and a spec property with no Go field fails CI. A round-trip fixture cannot
   prove this (a new `omitempty` field absent from the fixture passes both directions), which is
   why this test is reflection-based, not fixture-based.
2. `TestSpecRouteParity` (`internal/httpapi/spec_parity_test.go`) — the analog of the hosted
   `TestSpecBijectionAndScopeParity`: exact set equality between the route table's
   (method, specPath) pairs and the spec's `paths`. The `{id}:claim` custom-method route carries an
   explicit `specPath` in the route table (ServeMux registers `POST /v0/beads/issues/{idop}`), so
   the mapping is declared, never inferred.
3. `TestSpecDefaultsMatchSharedConstants` (`internal/httpapi/spec_parity_test.go`) — **rule 9**.
   Parses the spec's `limit` parameter `default:` values and asserts equality with
   `workapi.DefaultListLimit` (= 50, the value at cmd/bd/list.go:743) and
   `workapi.DefaultReadyLimit` (= 100, cmd/bd/ready.go:796) — the same constants the cobra flag
   registrations consume after the extraction slices. Also asserts the spec documents `limit=0` as
   unlimited on both list and ready (**rule 2**), so the document cannot drift from the behavior.
4. `TestSpecStatusCodesMatchHandlerTable` — **rule 12**. Asserts the set of documented status codes
   per operation equals exactly the reachable set in `problem.go`'s mapping table. No 413/429/504
   can be documented unless a mechanism ships that can emit it, and no handler can emit an
   undocumented status.
5. `TestProblemMapping` (`internal/httpapi/problem_test.go`) — table over every sentinel:
   `storage.ErrNotFound` and wrapped `sql.ErrNoRows` → 404 `not_found`;
   `storage.ErrAlreadyClaimed`/`ErrNotClaimable` → 409 with extensions; serialization-exhausted →
   the retryable code; arbitrary error → 500, **never** 404. Also pins the § 3 scrubbing rule:
   for the 500/503 rows, an input error whose message embeds a fake DSN
   (`user@tcp(127.0.0.1:3306)/db`) or dial target yields a problem body containing only the
   static per-code `detail` — none of the input message may appear on the wire.
6. `TestBuildListFilterGolden` / `TestBuildReadyFilterGolden` (`internal/workapi`) — golden
   equality tables **generated from the old inline builders before deletion** (the table literals
   are produced in the extraction PR from the pre-move code): for a matrix of inputs, old
   construction == `workapi.Build*Filter` output. The ready table additionally pins
   `BuildReadyFilter(zero)` → `Status:"open"`, `Limit:100`. Any delta found while writing the
   tables (e.g. the proxied path's dropped `MaxRows`) is triaged in the PR as an explicit behavior
   fix, never silently absorbed.
7. `TestBuildIssueDetailsNotFoundNormalization` (`internal/workapi/detail_test.go`) — **rule 4,
   unit half**. Fake `DetailSource` returning (a) an error wrapping `sql.ErrNoRows` — the real UOW
   seam shape, `db.issueSQLRepositoryImpl.Get` returns bare `sql.ErrNoRows`
   (internal/storage/domain/db/issue.go:394-410) — and (b) `(nil, nil)`. Both must normalize to
   `errors.Is(err, storage.ErrNotFound)` **inside the shared function**, matching the dual-sentinel
   treatment the proxied CLI already applies (cmd/bd/create_proxied_server.go:497). No caller-side
   mapping is trusted.
8. `TestCursorRoundTrip` / `TestCursorRejects` (`internal/httpapi/cursor_test.go`) — codec
   round-trip; tampered/undecodable cursor → typed error → 400 `invalid_cursor`.

8b. `TestContextResponseAllowlist` (`internal/httpapi/context_test.go`) — **the § 3 context
   field allowlist, enforced**. Builds the context handler over a `domain.ContextInfo` with
   EVERY field populated with a distinct sentinel — `SyncRemote` set to a URL embedding a fake
   credential — decodes the response into `map[string]any`, and asserts (a) the key set is
   exactly the ten allowlisted properties and (b) the credential substring appears nowhere in
   the raw body. Proves a future `ContextInfo` field (or a careless "serialize the struct"
   refactor) cannot leak a sync-remote credential or host topology onto the wire by default.

8c. `TestParamsRejectUnknown` (`internal/httpapi/params_test.go`) — **the § 3
   unknown-parameter rule, enforced at the decoder**. Table over every operation: a request
   carrying an unrecognized query key (including a near-miss typo like `labels` and a
   plausible next-version param like `mol_type`) → 400 `invalid_argument` with `param` naming
   the offending key and `reason: "unknown_parameter"`; the parameterless operations
   (`/healthz`, `/context`, get, claim) reject any query key at all; a known key with a bad
   value yields `reason: "invalid_value"` with the same `param` member. Pure — drives the
   hand-written decoders directly with `url.Values`, no server, no DB. Proves the decoders can
   never regress to Go's silent-ignore default (which would silently WIDEN filtered result
   sets under version skew) and pins the § 3 rule that strict rejection is the client's
   per-parameter capability probe.

**Tier B — cgo handler tests** (`internal/httpapi`, real UOW provider via
`testutil.EnsureDoltContainerForTestMain`, internal/testutil/testdoltserver.go:228):

9. `TestServerListCursorPagination` — same-second `created_at` groups page without loss or
   duplication across the keyset boundary; `limit=0` returns the full set with `has_more=false`
   (**rule 2**; if `SearchPage.HasMore` turns out to be limit-probe-based, the handler forces
   `has_more=false` at limit 0 and this test pins it). Also asserts the FIRST, un-cursored page
   arrives in `(created_at DESC, id ASC)` order over seed data whose priority order differs from
   its created order — pinning that the handler forces the sort on every request, not only in
   the cursor-decode path (gap G7: the id-sorted parity oracles cannot catch this).
10. `TestServerReadyReturnsCounts` — **rule 3**. Response items strict-decode into
    `types.IssueWithCounts` and carry correct nonzero `dependency_count`/`dependent_count`/
    `comment_count`/`parent` for seeded data — the field-level guard that the ready endpoint
    matches `bd ready --json`'s actual element type.
11. `TestServerGetUnknownID404` — **rule 4, integration half, against real Dolt**: unknown id
    through the real UOW provider → 404 `not_found` problem+json; a seeded id → 200; an
    induced non-not-found failure (closed provider) → 500, proving 404 is unreachable from
    arbitrary errors.
12. `TestServerClaimMatrix` — fresh claim 200; same-actor idempotent re-claim; foreign holder →
    409 `already_claimed` with `assignee` extension; closed issue → 409 `not_claimable`; unknown
    id → 404; whitespace-only, > 256-byte, and newline-bearing actors → 400 `invalid_argument`
    carrying `param: "actor"`, `reason: "invalid_value"`, with no row written (§ 3 wire-edge
    actor rules); N concurrent claimers → exactly one success
    (exercises `uow.RunTxResult`'s serialization retry, internal/storage/uow/tx.go:76 — the
    single implementation, **rule 5**).
13. `TestServerDoltRestartRecovers` — kill the Dolt server mid-session → mapped retryable 503;
    restart → next request 200 with the same provider (per-request self-heal requirement).
14. `TestDetailSourceConformance` (`internal/workapi`, cgo) — **rule 13**: the store-backed and
    UOW-backed `DetailSource` adapters, run over the same seeded data (issue with labels, deps,
    comments, and a wisp), produce byte-identical `BuildIssueDetails` JSON. Two implementations,
    one assembly, machine-checked.

**Tier C — end-to-end CLI/HTTP parity** (`cmd/bd/serve_proxied_integration_test.go`, cgo +
`BEADS_TEST_PROXIED_SERVER=1`, subprocess `bd serve --addr 127.0.0.1:0` with the printed bound
address parsed). These are the anti-drift oracles; per **rule 8** every one compares **full item
JSON** (decode both sides to `[]map[string]any`, sort by id, `go-cmp` after deleting only
allowlisted field paths, each with a written justification), never ID sets:

15. `TestProxiedServerServeReadyParity` — `GET /v0/beads/ready` items vs `bd ready --json` for the
    same filters (including `--limit 0`), over seeded data with deps/comments so the counts fields
    are load-bearing. An ID-set oracle would have passed while the endpoint shipped bare `Issue`;
    this one cannot.
16. `TestProxiedServerServeListParity` — `GET /v0/beads/issues` under defaults vs `bd list --json`,
    seeded with a gate, a template, a closed issue, an infra-type issue, and a wisp — proving the
    ~200 lines of exclusions apply identically (the hosted viewer's bare-`IssueFilter{}` bug class).
    Also asserts `limit=0` HTTP output equals `bd list --limit 0 --json`.
17. `TestProxiedServerServeShowParity` — `GET /v0/beads/issues/{id}` vs `bd show --json <id>`
    element [0], field-for-field; allowlist contains exactly one entry (the CLI's array-of-one
    envelope vs the HTTP bare object).
18. `TestProxiedServerServeClaimCrossCheck` — claim over HTTP, then `bd update --claim` by a second
    actor: the CLI sees the same conflict, and the HTTP 409's `assignee` extension matches; then
    `bd show --json` confirms the HTTP-claimed state. Plus the item-level parity case claim was
    missing (rule 8 — every other MVP operation has a full-item oracle, and "parity holds by
    construction" is exactly the reasoning rule 8 forbids): a same-actor CLI re-claim
    (`bd update <id> --claim --json`, idempotent under CLI semantics) of the HTTP-claimed issue
    full-item-JSON-compares its element `[0]` (the proxied path emits `[]*types.Issue`,
    update_proxied_server.go:48) against `ClaimResponse.issue`, under the same allowlist
    discipline as tests 15-17 — so a later CLI-side enrichment of update output (e.g. label
    hydration) fails the oracle instead of drifting silently against the HTTP body.
19. `TestProxiedServerServeModeGate` — embedded workspace → typed `storage.ErrUnsupported` message,
    exit 1; dolt-server-mode workspace before the server-mode slice lands → the staged refusal text
    (asserting it does NOT claim server mode is supported, **rule 10**); non-loopback `--addr`
    without opt-in → refusal; with `--allow-non-loopback` set, `?limit=0` → 400
    `invalid_argument` (the § 3 unlimited-read refusal is keyed on the flag, so this is testable
    on a loopback bind); a request with a foreign `Host` header (e.g. `Host: evil.example`) →
    400 (the DNS-rebinding allowlist); SIGTERM → graceful drain, exit 0.

**Corpus:** slices 1-4 are accepted only with `TestCorpusGolden` unchanged (no regen, no
`JSONSchemaVersion` bump) — `ready_front`, `json_contract`, `lease_claim`, and `errors_contract`
blobs are the byte-level proof the extractions were no-ops.

**CI placement (explicit, because the CI map hides breakage):** tests 1-8c run in `ci-pr-core`
(pure, required). The regen-and-diff drift gate (`go generate ./internal/httpapi/...` +
`git diff --exit-code`) is added to `scripts/ci/pr-policy.sh` (invoked by the required
`make ci-pr-policy`). Tiers B and C run in the pr-risk cgo shards; the serve tests get pinned
entries in `proxied-cmd-test-shards.txt`. Nothing load-bearing lives only in `main.yml`.

### Slice plan

Ordered; each slice is independently shippable and leaves main green. Per **rule 11**, slices 1-4
are pure no-op refactors, one extraction each, individually bisectable against the corpus. Per
**rule 10**: slice 6 delivers `bd serve` for **proxied** mode (where `uowProvider` already exists,
cmd/bd/main.go:1378-1388); slice 10 delivers **server / external-server / shared-server** modes by
constructing the existing exported providers at serve startup; between slices 6 and 10 those modes
get a typed refusal whose text promises nothing ("not yet supported by bd serve; supported today
under the proxied server").

1. **Extract list-filter construction** (no-op refactor).
   Outcome: `buildListFilter` + `listFilterConfig` + the config-source seam move verbatim to
   `internal/workapi/list.go` as `BuildListFilter`/`ListConfig`/`ConfigSource`, exported, with
   `DefaultListLimit = 50`; both `bd list` variants delegate; `cmd/bd/list_filter.go` deleted.
   Files: new `internal/workapi/list.go` + tests (incl. moved `list_filter_status_test.go` cases and
   `TestBuildListFilterGolden`); delete `cmd/bd/list_filter.go`; edit `cmd/bd/list.go`,
   `cmd/bd/list_input.go`, `cmd/bd/list_proxied_server.go`; `.golangci.yml` (depguard rule
   `workapi-frontend-boundary`) and `scripts/ci/pr-policy.sh` (banned-accessor grep) per § 2's
   import-policy enforcement — the boundary is machine-enforced from the package's first commit.
   Accept: corpus unchanged; `list_embedded_test.go` + `list_proxied_integration_test.go` green;
   golden table old==new; `grep -rn buildListFilter cmd/bd` returns nothing; lint fails on a
   deliberate cobra import in `internal/workapi` (verified once, then reverted).
2. **Collapse ready defaults** (no-op refactor).
   Outcome: one `BuildReadyFilter` (`Status:"open"`, `DefaultReadyLimit = 100`, normalization) in
   `internal/workapi/ready.go`; the inline duplicate at `cmd/bd/ready.go:104-215` deleted; direct
   path consumes `gatherReadyInput` like the proxied path; the cwd directory-label default stays in
   `cmd/bd` (caller pre-fills the request).
   Files: new `internal/workapi/ready.go` + tests; edit `cmd/bd/ready.go`, `cmd/bd/ready_input.go`
   (+ moved `ready_input_test.go` cases).
   Accept: corpus `ready_front` unchanged; `ready_test.go`, `ready_embedded_test.go`,
   `ready_proxied_integration_test.go`, `metadata_ready_test.go`, `ready_max_rows_test.go` green;
   golden table old==new; the proxied MaxRows delta (if confirmed) called out and decided in the PR.
3. **Shared detail assembly + not-found normalization** (no-op refactor).
   Outcome: `BuildIssueDetails` + `DetailSource` (with the isWisp axis) + store/UOW adapters in
   `internal/workapi/detail.go`; `bd show`'s direct assembly block (`show.go:147-235`) and the
   proxied helpers (`proxiedGetIssueOrWisp` etc.) deleted; the shared function normalizes
   {wrapped `sql.ErrNoRows`, nil-issue-with-nil-error} → `storage.ErrNotFound` (**rules 4, 13**).
   Fuzzy resolution, `--current`, watch, and the array-of-one envelope stay CLI-local.
   Files: new `internal/workapi/detail.go` + `detail_test.go` + `TestDetailSourceConformance`; edit
   `cmd/bd/show.go`, `cmd/bd/show_proxied_server.go`.
   Accept: corpus `show` blob unchanged; `show_test.go`, `show_embedded_test.go`,
   `show_proxied_integration_test.go`, `cli_coverage_show_test.go` green; normalization unit test
   green.
4. **One claim retry implementation** (no-op refactor, **rule 5**).
   Outcome: `applyUpdateProxiedOne`'s bespoke `cenkalti/backoff` loop
   (cmd/bd/update_proxied_server.go:~94-125) reimplemented as a `uow.RunTxResult` consumer
   (T = the attempt's {issue, fail} pair); the whole-attempt-redo semantics, nothing-to-commit
   tolerance, and the retries-exhausted stderr/exit contract are preserved by RunTxResult's
   existing tested behavior.
   Files: edit `cmd/bd/update_proxied_server.go` only.
   Accept: corpus `lease_claim` + `errors_contract` unchanged; `update_proxied_integration_test.go`,
   `update_conditional_proxied_test.go`, `update_proxied_server_test.go` green; no `backoff.Retry`
   call remains in `update_proxied_server.go`.
   Note: the facade's own deferred proxied-rewire PR is a downstream rebase consumer of this
   RunTxResult consolidation (it requires proxied contract-pin tests committed and green first;
   this slice contributes to that precondition).
5. **OpenAPI spec, codegen, drift gates** (inert — no runtime change).
   Outcome: hand-written `internal/httpapi/spec/openapi.v0.yaml` (all six operations; `Issue`/
   `IssueWithCounts`/`IssueDetails` pinned to the canonical types via `x-go-type`, **rule 1**; only
   the status codes the six operations need, **rule 12**; `limit` defaults 50/100 and `0=unlimited`
   documented, with the `--allow-non-loopback` `limit=0` refusal in the `limit` description (§ 3);
   `ClaimRequest.actor` constraints as `minLength`/`maxLength`/`pattern` (§ 3); the
   unknown-query-parameter rejection rule and the 400 `param`/`reason` extension members
   documented on every operation and on the `Problem` schema (§ 3); the 5xx static-
   `detail` scrubbing rule in the problem writer + `TestProblemMapping` (§ 3);
   the `/issues` description documents the always-forced `(created_at DESC, id ASC)`
   order and its deliberate divergence from `bd list`'s default priority ordering, § 3 / gap G7);
   `apigen/types.gen.go` via oapi-codegen types-only (go.mod `tool` directive);
   Makefile `api-gen`/`api-check` (names per § 4); regen-diff check in `scripts/ci/pr-policy.sh`.
   Files: `internal/httpapi/spec/{openapi.v0.yaml,embed.go}`, `internal/httpapi/apigen/{doc.go,
   types.gen.go}`, `internal/httpapi/{wire_bijection_test.go,spec_parity_test.go(partial),
   problem.go,problem_test.go}`, `Makefile`, `scripts/ci/pr-policy.sh`, `go.mod`.
   Accept: tests 1, 3, 4, 5 green in ci-pr-core; `make api-check` clean, and fails on a spec
   edit without regen; go.mod diff contains only the tool directive and its closure.
6. **`bd serve` skeleton: lifecycle, mode gate, /healthz, /v0/beads/context** (proxied mode live).
   Outcome: `cmd/bd/serve.go` (`--addr` default `127.0.0.1:0`, `--allow-non-loopback`,
   numeric-loopback validation per the `proxied_server.go:446-476` policy, printed bound address,
   graceful shutdown on the existing signal context); `internal/httpapi` Server/route table/problem
   writer; typed refusals: embedded → permanent `storage.ErrUnsupported`; server/external-server →
   staged refusal (**rule 10** honest text); no flock/pidfile/serve-info.json (**rule 7** — the TCP
   bind is the mutual exclusion); one-UOW-per-request with rollback via a detached close context;
   `/v0/beads/context` from the `contextinfo.NewContextProvider` snapshot, serializing only the
   § 3 field allowlist (SyncRemote hard-excluded). The § 4 front-tier hardening lands with the
   skeleton, not later: `Host`-header allowlist middleware, `netutil.LimitListener` connection
   cap, the DB semaphore (bounded 10 s acquisition wait → 503 `busy`) with `/healthz` +
   `/context` bypass, the per-request 60s deadline, and explicit pool limits on the provider's
   `*sql.DB` (§ 4, concurrency limits). The § 4 observability floor lands here too: the
   per-request structured stderr line (method, path, status, problem `code`, duration,
   semaphore-wait, UOW-acquire), startup and shutdown lines, the semaphore-saturation event, and
   the `request_id` problem member.
   Files: `cmd/bd/serve.go`; `internal/httpapi/{server.go,routes.go,handlers.go(healthz,context)}`;
   `cmd/bd/serve_proxied_integration_test.go` (mode-gate + lifecycle cases).
   Accept: test 2 green (route table = spec paths; unimplemented ops registered as transitional
   501 stubs so route/spec parity holds all-at-once). The stubs are an explicit, temporary
   rule-12 exemption, not a silent one: 501 is never documented spec surface, so
   `TestSpecStatusCodesMatchHandlerTable` (test 4) carries a commented in-test exemption listing
   exactly the stubbed operations, deleted together with the last stub in slice 9 — after which
   the exemption list being non-empty fails the test. `/v0/beads/context` `capabilities` is
   derived from implemented handlers only and never lists a stubbed op (§ 3), so a release cut
   between slices 6 and 9 advertises only what works. Tests 8b and 19 green. The observability
   floor is an ACCEPTANCE CRITERION of this slice, not a convention: the lifecycle integration
   cases assert the startup lines, one request log line per request (with the § 4 fields), the
   shutdown drain lines, and the saturation event under a synthetic semaphore-full condition.

6b. **Reader role** (owner-decided; inserted by the issueops reconciliation — GATED on the
   `feat/public-issueops-facade` merge: the leaf `issueops` package must exist on OSS main
   before `Reader` can be declared).
   Outcome: the leaf `issueops.Reader` contract (Reader interface,
   `ReadyRequest`/`ListRequest`/`GetRequest`/`IssuePage` — § 2, "The Reader role");
   `IssueReader()` accessors on `Storage`, `DoltStore`, `EmbeddedDoltStore` (cgo + !cgo stub),
   `HookFiringStore` (passthrough by recursion), `InstrumentedStorage` (wrap by recursion), the
   UOW provider (+ the `configStore` test fake); the `storeReader` and `uowReader`
   implementations over `workapi` (builders re-typed to the leaf request types; one UOW per
   Reader method with detached-close inside the uow reader).
   Files: new `issueops/reader.go` (leaf) + per-backend `*_reader.go` in
   `internal/storage/{dolt,embeddeddolt,uow}` + accessor additions + tests.
   Accept: cross-backend conformance tests green (store vs uow Reader agree on the parity
   oracle corpus shapes); `go list -deps` verification that the leaf package imports only
   `internal/types` + stdlib.
7. **Read endpoints: `/v0/beads/ready` + `/v0/beads/issues`** (gated on 6b).
   Outcome: param decoding → `provider.IssueReader().Ready` / `.List` — handlers NEVER call
   `workapi` builders or wire `ConfigSource`; the Reader subsumes filter/default construction
   and one-UOW-per-call execution; ready returns
   `IssueWithCounts` items (**rule 3**); list gets the opaque keyset cursor (token codec stays
   HTTP-layer, decoded position passed as `ListRequest.AfterCreatedAt`/`AfterID`); `limit=0`
   unlimited on
   both (**rule 2**; refused with 400 under `--allow-non-loopback`, § 3); sentinel mapping live.
   Files: `internal/httpapi/{params.go,cursor.go,handlers.go}` + tests.
   Accept: tests 8, 8c, 9, 10 green (8c includes the strict unknown-parameter rejection on
   every operation, § 3); parity oracles 15, 16 green.
8. **Detail endpoint: `GET /v0/beads/issues/{id}`** (gated on 6b).
   Outcome: HTTP handler consumes `provider.IssueReader().Get` — which runs
   `workapi.BuildIssueDetails` through the UOW detail source inside the reader (no new
   assembly copy, **rule 13**); exact-ID + wisp fallback; 404 only via the normalized sentinel.
   Files: `internal/httpapi/handlers.go`.
   Accept: tests 11, 17 green.
9. **Claim endpoint: `POST /v0/beads/issues/{id}:claim`**.
   Outcome: handler consumes `uow.RunTxResult` (the same single implementation the proxied CLI now
   uses after slice 4, **rule 5**), commit message `"bd serve: claim <id> by <actor>"`; wire-edge
   actor validation before the CAS (trim / length / control characters, § 3); typed 409s
   with `assignee`/`issue_status` extensions; 501 stub replaced; hooks do not fire (seam-consistent
   with every proxied write today); `uow.RunTxResult`'s per-attempt close switched to the
   detached-context pattern (§ 4, "the write path gets the same detached-close protection" —
   fixed in the one implementation per rule 5, so the proxied CLI inherits it).
   Files: `internal/httpapi/handlers.go` (+ the `{idop}` suffix split);
   `internal/storage/uow/tx.go` (detached per-attempt close); spec already covers the op.
   Accept: tests 12, 18 green; concurrent-claim race: exactly one 200; a client disconnect
   mid-claim leaves the provider serving subsequent requests without a burned session (the
   detached-close verification).
   The § 1 coordination-bead check happens before this slice ships, in `Lifecycle` vocabulary:
   the facade owner reviews the endpoint's documented semantics against
   `Lifecycle.Update`/`UpdateRequest.Claim` together with the recorded reasons the claim stays
   BELOW `Lifecycle` (§ 2, post-facade justification: fixed commit message, discarded
   `AlreadyClaimed`, unreachable same-tx 409 re-reads, moot granularity, internal-seam scope).
10. **Server / external-server / shared-server mode support** (**rule 10** delivery).
    Outcome: `bd serve` startup constructs a UOW provider for non-proxied SQL-server workspaces by
    calling the existing exported `uow.NewDoltServerUOWProvider` /
    `uow.NewExternalDoltServerUOWProvider` (internal/storage/uow/doltserver_provider.go:16,
    external_doltserver_provider.go:18 — already tested, already consumed by cmd/bd/uow_factory.go)
    through the same config-resolution path `newProxiedServerUOWProvider` uses; serve is excluded
    from post-run auto-commit maintenance in these modes; staged refusal deleted.
    Files: `cmd/bd/serve.go`; small extraction in `cmd/bd/uow_factory.go`; possibly
    `cmd/bd/main.go` post-run policy.
    Accept: integration test against an external dolt sql-server workspace answers all six
    operations; embedded still refused; test 19's staged-refusal assertion updated.

10b. **CLI re-point** (no-op refactor; inserted by the issueops reconciliation; follows 6b).
    Outcome: direct + proxied `bd ready` / `bd list` / `bd show` route through the
    `IssueReader()` accessors instead of calling the `workapi` builders at the command layer;
    the fetch-one-extra / `SQLLimit` / client-side-sort trim logic (today at
    `cmd/bd/list.go:42-76`) relocates INTO the store reader. CLI-local concerns stay in
    `cmd/bd`: fuzzy/substring resolution, `--current`, watch mode, the array-of-one show
    envelope, the cwd directory-label default (CLI pre-fills `ReadyRequest.LabelsAny`), MaxRows
    resolution and proxied rejection, output envelopes. Commands outside the four-command scope
    (`count.go`, `search.go`, `graph_apply.go`) keep calling `BuildListFilter` directly — they
    are not part of the six-operation anti-drift surface, and the facade's governing rule
    constrains the public capability surface, not internal helper use.
    Files: `cmd/bd/{ready,list,show}*.go`; `internal/storage/dolt` store reader.
    Accept: behavior-sensitive and corpus-refereed exactly like slices 1-4 — protocol corpus
    unchanged, goldens old==new, bisectable to one move.
11. **Docs + downstream handoff**.
    Outcome: engdocs page (surface, error codes, cursor contract, loopback posture, no-hooks and
    no-auto-commit decisions, claim-throughput note) plus the operator runbook material this
    proposal commits to: the fixed-port deployment recommendation and the ephemeral-default
    honesty note (§ 4, rule 7), the `GET /v0/beads/ready?limit=1` readiness-probe recipe with
    `/healthz` documented as liveness-only (§ 4, concurrency limits), the SIGHUP/supervisor
    note and the ambiguous-shutdown re-claim recovery (§ 4, graceful shutdown), the
    connection-budget arithmetic for shared external Dolt servers, and the request-log line
    format (§ 4, observability floor) — all written **vendor-neutrally** per binding
    constraint 1: it addresses generic "automation clients / orchestrators", names no downstream
    consumer, product, or fork, and motivates each decision from the API contract itself (e.g.
    "clients that classified claim conflicts by substring-matching error text should switch to
    the typed 409 `code`"), not from any one consumer's internals. CHANGELOG likewise. The
    consumer-specific adoption checklist (delete substring claim classification, the show
    `[0]`/`ErrIDCollision` guard, the version-string surgery) is a **downstream deliverable**:
    it is handed off to and lives in the consuming fork's own tree, and is never committed to
    OSS beads. That checklist must also state the negative honestly: the reconciler's list path
    (the `TierBoth` full scan) is NOT on the v0 migration list — it stays on its `bd list` +
    `bd query "ephemeral=true..."` subprocess pair (§ 3, "what this list surface deliberately
    cannot serve"), so none of that loop's runner/parse/retry machinery is deletable until an
    ephemeral/wisp-inclusion parameter (and a labels-omission marker) ships in a later version.
    Files: `engdocs/`, `CHANGELOG.md`. Accept: docs-drift scripts pass; no code; a
    vendor-neutrality grep of the new docs (no hosted-product or consumer names) is part of PR
    review for this slice.
    Disposition of THIS document: it is a working design artifact, not a slice deliverable. It
    does not land in the OSS tree as-is — it cites downstream-consumer internals throughout,
    which binding constraint 1 bars from committed OSS docs. If the owner wants a committed
    design record, the engdocs page above is that record; landing this proposal verbatim would
    require the same genericization scrub.

### Risks

- **The bijection test is the only guard on the wire schema.** With `x-go-type` pinning there is no
  compiler check that spec and struct agree; if test 1 ever moves out of `ci-pr-core` (or is
  skipped under `-short`), the drift gate is gone. Mitigation: it is pure and fast by construction;
  CI placement is an acceptance criterion of slice 5, not a convention.
- **Extraction deltas.** Slices 1-4 move corpus-pinned behavior; the golden-table method surfaces
  deltas (the proxied ready `MaxRows` drop is already suspected) but each surfaced delta forces a
  product decision mid-slice. Policy: triage in the PR, never silently absorb; a delta that changes
  wire bytes moves to its own follow-up slice with a corpus regen.
- **`SearchPage.HasMore` semantics under `limit=0` and under keyset filters are unverified.** If
  HasMore is limit-probe-based, `limit=0` must force `has_more=false` explicitly; test 9 pins
  whichever way it lands, but the implementation must check before wiring cursors.
- **`limit=0` responses are fully buffered — and the OOM blast radius is NOT "same as the CLI".**
  Each unlimited response is double-buffered per request (query-layer rows plus the full JSON
  marshal). Today an OOM kills one forked `bd list --limit 0` process and nothing else; under
  serve, the same scans run inside ONE long-lived shared process — 16 concurrent
  reconciler-style full active-set scans can OOM the process that every client, including the
  dispatch loop, depends on. The fork model's isolation is silently lost, and this document says
  so rather than calling the exposure equivalent. It also changes the answer to open question 2:
  this proposal's recommendation (SRE review) is to ship the off-by-default `--max-rows`
  operator flag in v0, not later. The non-loopback half of the hazard stays CLOSED —
  `--allow-non-loopback` refuses `limit=0` with 400 (§ 3, limit semantics), so the unbounded
  buffer is never network-reachable.
- **RunTxResult adoption (slice 4) is behavior-sensitive at the failure edges**: the
  retries-exhausted path currently prints a specific stderr line and returns a per-issue failure
  rather than an error. If RunTxResult's error surface cannot express that without contortion, the
  slice must adapt the call site, not fork the retry loop — `errors_contract` corpus is the
  referee.
- **Provider self-heal after Dolt/dbproxy idle-stop is assumed, not yet proven.** Test 13 is the
  proof obligation; if `NewUOW` does not re-establish, serve needs a provider-recycle on bad-conn,
  which is a small but real scope addition to slice 6.
- **Server-mode staging gap (slices 6→10).** Decided parameter 5 names three modes; between the
  slices only proxied works. The refusal text is written to promise nothing, but docs and release
  notes must not advertise `bd serve` for server-mode workspaces until slice 10 ships.
- **Claim throughput is bounded by Dolt's per-write commit** (~0.4 s measured in prior perf work):
  roughly 2 claims/sec sustained. Fine for a 30 s dispatch patrol; documented so nobody builds a
  hot claim loop against v0.
- **cgo tier conditionality.** Tiers B/C run on PRs only when the risk-tier script flips
  `full_embedded=true`; a doc-only PR touching the spec YAML might skip them. The pure gates
  (tests 1-8c) are deliberately sufficient to catch schema/route/default drift on their own.
- **go.mod churn from the oapi-codegen tool directive.** A prior PR was reverted for unrelated
  go.mod noise; slice 5's acceptance pins the diff to the directive plus its closure.
- **Read-endpoint slices are gated on the facade merge.** Slices 6b/7/8 require the facade's
  API-shape phase to merge to OSS main first — the leaf `issueops` package must exist before
  `Reader` can be declared. The schedule exposure is an owner call (open question 9's
  contingency), managed visibly rather than silently re-deciding the architecture.
- **The CLI re-point slice (10b) moves over-fetch/trim behavior into the store reader.** The
  fetch-one-extra/`SQLLimit`/client-side-sort trim relocation is behavior-sensitive and must be
  corpus-bisectable exactly like slices 1-4; any surfaced delta is triaged in the PR, never
  silently absorbed.

### Contradictions resolved during assembly

The five sections were drafted in parallel; where they disagreed, one position was applied and
the tradeoff is recorded here rather than silently dropped.

- **Ready/list response envelope**: one draft floated a bare JSON array for `/ready` (byte-parity
  with `bd ready --json`); § 3's decided `{items, has_more}` object envelope wins — extensible and
  consistent with the paginated list endpoint — at the cost of one deliberate CLI/HTTP shape
  difference the parity oracles allowlist (wrapper only; item JSON stays field-identical).
- **`/issues` item type**: § 3 drafted `items: [Issue]`; § 2's `IssueWithCounts` wins — verified
  `bd list --json` calls `SearchIssuesWithCounts` on both CLI paths (cmd/bd/list.go:574-580,
  list_proxied_server.go:78) — at the cost of three counts columns on every list row (the price
  of rule 8 field-level parity).
- **Detail `include_*` HTTP knobs**: § 2's `DetailOptions` comments implied HTTP
  `include_dependents`/`include_comments` params; § 3's no-params surface wins for v0 (rule 12
  minimality) — the library keeps `DetailOptions` for the CLI, HTTP passes the zero value, and
  the comments knob remains open question 3 below.
- **Makefile target names**: `api-gen`/`api-check` (§ 4, "names final") over the plan draft's
  `generate-api`/`check-api-drift`.
- **Test names**: normalized to § 5's inventory (`TestWireTagBijection`, `TestProxiedServerServe*`
  — the prefix the cgo shard mechanics require — `TestBuild*FilterGolden`,
  `TestServerGetUnknownID404`); § 5's route test renamed `TestSpecRouteParity` to match § 4's
  `make api-check` gate.
- **Rule numbering**: § 1's design-rules list renumbered 1-15 so the "rule N" citations used by
  every other section resolve against it.

### Open questions for the product owner

1. **v0 release boundary**: tag/announce v0 after slice 9 (proxied-only, staged refusal for
   server modes) — or hold the announcement until slice 10 so decided parameter 5 is fully true on
   day one?
2. **Unlimited-read cap (narrowed by security review; recommendation added by SRE review)**: the
   security half is decided — `--allow-non-loopback` refuses `limit=0` with 400 (§ 3), so no
   unbounded buffer is ever network-reachable. What remains is NOT mere loopback ergonomics:
   moving full active-set scans from per-fork CLI processes into one shared process changes the
   OOM blast radius from "one command dies" to "the API dies for every client, dispatch
   included" (§ 5, Risks). The proposal's recommendation is therefore to ship the off-by-default
   `--max-rows` operator flag in v0 (wire semantics unchanged when unset — binding rule 2
   intact; when an operator opts in, refusals surface as a typed 400 `too_many_rows`, a
   permanent code-vocabulary addition, which is why this still needs owner sign-off). Owner
   either/or: accept the v0 flag as recommended — or keep v0 capless on loopback (exact CLI
   parity) and accept the documented shared-process OOM risk? (A streaming JSON encoder was
   considered and not chosen: it removes only the marshal copy — the query layer still buffers
   every row — so it halves the footprint without bounding it.) Related either/or the security
   review surfaced: keep the non-loopback `limit=0` refusal as shipped, or instead make
   `--max-rows` mandatory at startup when `--allow-non-loopback` is set (a cap instead of a
   refusal)?
3. **Comments in the detail endpoint**: mirror `bd show`'s default exactly (count-only,
   `comments_omitted` semantics, no way to fetch bodies over HTTP in v0) — or add
   `?include_comments=true` in v0 (one more spec knob, but remote clients have no other way to read
   comments)?
4. **500 detail verbosity** — **closed during security review, no longer open**: 500 `internal`
   and 503 `db_unavailable`/`busy` carry a generic static `detail`; the real error (which can
   embed the DSN or dial target) goes to the server log only. See § 3, "5xx detail scrubbing".
   Numbering kept so cross-references to questions 5-6 stay stable.
5. **HTTP contract corpus**: extend the byte-pinned corpus mechanism with a vendorable HTTP-response
   corpus in v0 (same producer/consumer contract Gas City already replays for CLI JSON) — or defer
   to v0.1 and let the spec + bijection tests carry the contract alone until the surface settles?
6. **Committed design record**: rely on slice 11's vendor-neutral engdocs page as the sole
   in-tree design record (this proposal stays out of tree; see slice 11 "Disposition") — or fund
   a genericization scrub of this proposal so a scrubbed copy can land in `engdocs/` alongside it?
7. **`api_revision` context field (client version-skew review)**: v0's parameter-level
   capability gate is strict unknown-parameter rejection — a client probes by sending and gets
   a machine-attributable 400 (§ 3, version skew). Ship an `api_revision` field on `/context`
   exposing the spec's `info.version` (bumped on every spec change, enforceable by extending
   the pr-policy drift gate: a spec diff without an `info.version` change fails) so clients can
   gate on parameters without probing — or keep the context allowlist at its ten fields and let
   probe-by-400 carry parameter gating until a real consumer reports the probe as insufficient?
8. **`/v0` disposition on the day a `WireSchemaVersion` bump forces `/v1`** (maintainer review;
   documented as deferred-to-that-day in § 3's evolution policy — no decision is needed until a
   breaking wire change is actually proposed): when that day comes, sunset-and-remove `/v0` on a
   documented deprecation window — or freeze `/v0` by snapshotting the pre-bump shapes as a
   deliberate version-suffixed mirror struct served only by `/v0` handlers (a round-trip fixture
   replacing the bijection test for the frozen shape)?
9. **Facade-slip contingency (issueops reconciliation)**: the read-endpoint slices (6b/7/8) gate
   on the facade's API-shape phase merging to OSS main. If the facade slips, does the owner
   accept the delay — or authorize declaring the Reader contract temporarily in an internal
   package (relocated to the leaf `issueops` package when the facade lands), trading a later
   mechanical move for schedule decoupling?
10. **`IssueReader` accessor naming**: `IssueReader()` sits beside the existing internal
    `IssueLifecycleStore` lane — the name is grep-verified free in both trees today, but confirm
    the naming convention (`IssueReader` vs e.g. `IssueQueries`) before it becomes public API.
11. **`Offset` on the public contract**: `ReadyRequest`/`ListRequest` carry `Offset` "honored
    where the backend supports it" — keep a best-effort field on the public leaf contract, or
    drop it and leave offset pagination CLI-internal?
12. **`*int` field conventions**: `Limit *int` (nil = default, 0 = unlimited) and `Priority *int`
    (nil = unset) replace the CLI's paired value+`Set` flags on the public request structs —
    confirm the pointer-field convention for the leaf contract before it freezes.
13. **v0 tag vs the CLI re-point slice**: does the v0 tag/announcement wait for slice 10b (so the
    CLI and HTTP surfaces both sit on the accessors from day one) — or ship after slice 9/10
    with 10b as a fast-follow no-op refactor?

---

## Reconciliation with the issueops Lifecycle refactor

This section records the owner's reconciliation of this proposal with effort B, the
`feat/public-issueops-facade` branch. It is the authority for the amendments marked
"issueops reconciliation" throughout the document (non-goals bullet, binding rule 6, § 2's
Reader role, slices 6b/10b, risks, open questions 9-13).

### What effort B changes

The facade branch (35 commits ahead of origin/main at base `8bb0d36be`) establishes the public
`issueops.Lifecycle` role — {Create, Update, Close, Reopen}, the rename from `Operations`
binding per its final design spec, not yet in code at branch HEAD — exposed via an
`IssueLifecycle()` accessor on `Storage`, with the leaf `issueops` package at the repo root
importing only `internal/types` + stdlib. It relocates the existing 18 storage/domain error
symbols into that leaf package (aliased back from `internal/storage` with the same pointer; no
new sentinels), routes the four direct CLI write verbs through the facade via
`cmd/bd/issueops_adapter.go`, and defers the proxied-server rewire to its own follow-up PR gated
on proxied contract-pin tests.

### The decision (owner-decided, binding)

**BOTH, layered.** `issueops.Reader` is the public read ROLE (binding decision 1), and
`internal/workapi` is the single shared IMPLEMENTATION substrate behind every Reader
implementation. Neither replaces the other: the leaf package declares the contract; workapi
keeps the ~310 lines of construction/shaping logic; the per-backend readers glue them under the
accessor. This is **subsume** (binding decision 2): construction happens once, inside the
implementations, and both front doors can only say `rd.List(ctx, req)`. The full contract, the
accessor set, and the implementation internals are specified in § 2, "The Reader role".

`internal/workapi` stays internal PERMANENTLY — the room behind every door. Reader
implementations call: `BuildListFilter` + `LoadListConfig` + the `ConfigSource` adapters (List),
`BuildReadyFilter` (Ready), `BuildIssueDetails` + `DetailSource` adapters +
`ResolveIssue`/`IsNotFound`/`NotFound` (Get), and `Default{Ready,List}Limit` (nil-Limit
defaulting). `ConfigSource` is supplied by the implementation from what it already holds — never
by front doors, which is the drift kill. Still called directly from `cmd/bd`, honestly:
(a) commands outside the four-command scope (`count.go`, `search.go`, `graph_apply.go`) keep
calling `BuildListFilter` — the governing rule constrains the public capability surface, not
internal helper use; (b) the claim path (proxied `bd update --claim` and the HTTP claim handler)
uses `ClaimOnUOW`/`ClassifyClaimError` directly; (c) CLI-local stays outside the contract
entirely: fuzzy/substring id resolution, `--current`, watch mode, the array-of-one show
envelope, the cwd directory-label default, MaxRows resolution and proxied rejection, output
envelopes, and the cursor token codec (HTTP layer).

### Rationale (the load-bearing discoveries)

1. **The leaf constraint binds only the CONTRACT package.** Verified on the facade branch:
   `internal/storage/uow/issue_operations.go` and `dolt/issue_operations.go` implement the
   public contract while importing `internal/storage` freely — so Reader implementations sit
   beside `NewIssueOperations` in dolt/embeddeddolt/uow and import `workapi` and
   `internal/config` without restriction. The "subsume is structurally impossible" claim
   conflated these layers and is refuted.
2. **Subsume, not compose, neutralizes the council's two sharpest objections.** Request-granular
   methods mean the uow reader is one-UOW-per-call (no transaction fragmentation — the objection
   targeted a fine-grained compose-shaped Reader), and construction disappearing behind the
   accessor means the two front doors cannot re-become two builder callers.
3. **The already-built work is the substrate, not a casualty.** Slice 1's `BuildListFilter`
   (456 lines + 2890-line golden + depguard boundary), slice 2's `BuildReadyFilter`, and
   slice 3's `BuildIssueDetails`/`DetailSource` are exactly what the Reader implementations
   call — the list-filter and claim-retry branches merge now as-is; the ready/detail
   extractions finish as specced; one later re-point slice (10b) moves the four commands' call
   sites onto accessors and relocates the fetch-one-extra/SQLLimit trim into the store reader
   (behavior-sensitive, corpus-refereed) — a follow-up, not rework.
4. **The claim endpoint changes nothing.** Verified on the facade branch: `Lifecycle.Update`
   runs its own `RunTxResult` with a fixed "update issue" commit message (double-wrapped retry,
   lost `"bd serve: claim <id> by <actor>"` message), `ApplyUpdate` discards domain
   `ClaimResult.AlreadyClaimed` (the wire's `already_claimed` field needs it), and on a lost CAS
   the facade returns only the sentinel with the transaction gone — making the 409
   `assignee`/`issue_status` same-tx re-read extensions unreachable; the per-issue-granularity
   argument is moot for a single-issue claim. B's governing rule governs the public capability
   surface and does not conscript internal seam consumers — `bd serve`'s claim rides the same
   seam proxied `bd update --claim` uses today, and B itself defers the proxied rewire.
5. **Sequencing is nearly free.** fv4-vs-facade overlap is zero files, m2u-vs-facade is one
   additive test file, and the facade is by its own spec one API-shape phase from PR-ready, so
   it merges last and resolves the single union conflict. The one real cost, surfaced honestly:
   declaring `Reader` requires the leaf `issueops` package to exist on OSS main, so the
   read-endpoint slices now gate on effort B's merge — flagged to the owner (risk + open
   question 9) rather than hidden.

### The strongest objection (recorded, not hidden)

The best honest case against subsume-with-a-Reader-role is the council's own finding, sharpened:
a Reader role today is the execution port rule 6 rejected, wearing effort B's naming. It
converts reads that are currently zero-cost passthroughs (both decorators embed and override
only writes; both seams answer every needed query in one line) into ~5 accessor implementations
plus leaf page types; the store implementation must adopt exactly the limit+1 over-fetch the
council refused to force onto the direct CLI path; and it couples this epic's read endpoints to
an unmerged branch whose binding API shape (Lifecycle rename, accessor, leaf-ification) is not
yet in code — speculative coupling of the kind rule 6 exists to prevent. Meanwhile, the argument
runs, the anti-drift goal was already achieved by deletion: after slices 1-3 the old builders no
longer exist, so a second construction copy is a compile error no matter who calls
`BuildListFilter` — compose gets the drift protection without the adapter weight.

Why it does not win: deletion secures construction UNIQUENESS but not call-site symmetry. Under
compose, each front door still performs the four-step ritual — build a `ConfigSource`, load
config, call the builder, execute on its seam — and a caller that skips or half-performs the
ritual compiles fine; that is precisely the topology that shipped the hosted viewer's bare
`IssueFilter{}` bug this epic exists to kill. Under subsume the ritual is not callable from a
front door at all: handlers and RunE bodies can only say `rd.List(ctx, req)`, so the failure
mode is unwritable — a strictly stronger guarantee than "the shared function exists". The
transaction-fragmentation half of the objection dissolves under subsume specifically:
request-granular methods mean the uow reader opens exactly one UOW per call, the proposal's own
one-UOW-per-request shape; that objection was valid only against a fine-grained compose-shaped
Reader. The over-fetch and adapter costs are real but bounded, paid once inside two
implementations rather than by every caller, and were explicitly accepted by the owner with the
council finding recorded as the counter-argument. The schedule coupling is the one genuinely
live cost, and the answer is to manage it visibly (the facade-merge gate on slice 6b, plus open
question 9's contingency) rather than to let it silently re-decide the architecture.

### Merge order

Order: (1) `refactor/bd-fv4-workapi-list-filter`, (2) `bd-m2u-collapse-claim-retry-loop`,
(3) `feat/public-issueops-facade`. All three share base `8bb0d36be` (current-main lineage).

1. **bd-fv4 first**: reviewed, protocol corpus verified unchanged, zero file overlap with BOTH
   other branches (verified: its 17 files touch `cmd/bd/list*/count*/search*/graph_apply.go` +
   `internal/workapi/*` + `.golangci.yml` + `scripts/ci/pr-policy.sh`; the facade's 147 files
   and bd-m2u's 4 files touch none of them). It also creates the workapi package + depguard
   boundary that the ready/detail extractions stack on (their branches literally contain its
   commits `af56541bb`/`e2d82a54d`).
2. **bd-m2u second**: disjoint from fv4 (`update_proxied_server*`, `uow/tx.go`,
   `uow/tx_test.go`), merges clean. Landing fv4+m2u (and then the ready/detail extractions,
   which slot in anywhere before the facade — the facade touches none of their extraction
   targets) first also supplies part of the "proxied contract-pin tests committed and green
   FIRST" precondition B's own spec sets for its deferred proxied-rewire PR.
3. **feat/public-issueops-facade last** — the only branch not PR-ready (at HEAD the Lifecycle
   rename, `IssueLifecycle()` accessor, `beads_issueops.go` deletion, and 18-symbol relocation
   are all still pending; the YAGNI cuts and relocation are partly uncommitted working-tree
   state). After its API-shape phase is committed, it rebases onto main (now containing
   fv4+m2u).

**The single conflict — `internal/storage/uow/tx_test.go` — resolves as the UNION of both sides**
(the hunks are disjoint regions): keep the facade's widened `mockUnitOfWork` (five use-case
fields + accessor methods returning them), its `mockUnitOfWorkProvider.newUOWCalls` counter, its
`sqlStateError` type and `TestRunTx_RetriesOnPostgresSerializationStates`; AND keep bd-m2u's
added `time` import plus `TestRunTxResultWithin_ExhaustedBudgetReturnsSerializationError`
verbatim. The union is semantically coherent, verified: bd-m2u's test uses a zero-value
`mockUnitOfWorkProvider{}` and `newMySQLError(1213)`, both untouched by the facade's edits; the
facade's `issue_operations.go` calls `RunTx`/`RunTxResult` whose signatures bd-m2u preserved (it
only added `RunTxResultWithin` and exported `DefaultTxRetryMaxElapsed`, and the facade does NOT
touch `tx.go`; the facade's `errors.go` SQLSTATE widening composes cleanly since bd-m2u does not
touch `errors.go`). Post-merge check: re-run the facade's five counted baselines — the uow
package count becomes 128 (127 + bd-m2u's added test), all other counts (72/55/802/40)
unchanged.

### Residual risks

- The read-endpoint gate: slices 6b/7/8 wait on the facade's API-shape phase reaching OSS main
  (leaf `issueops` must exist before `Reader` can be declared); contingency is open question 9.
- The CLI re-point (10b) relocates over-fetch/trim behavior into the store reader —
  behavior-sensitive, corpus-bisectable like slices 1-4.
- The facade's binding shape (Lifecycle rename, accessor, leaf-ification) is spec-committed but
  not yet code at branch HEAD; if the shape shifts during its API-shape phase, the Reader
  contract inherits the shift before slice 6b, not after.

---

## Council review — dispositions

Four reviewer passes were applied to this document in sequence: `council:maintainer`,
`council:security`, `council:sre`, `council:consumer`. Every reviewer verdict was **revised**.
This appendix is the ledger: what each pass changed (one line), every item each pass deliberately
did NOT apply with its reason, and the result of the post-review consistency check across the
values multiple reviewers touched. Owner-facing either/ors raised by the reviews are consolidated
in § 5, "Open questions" (questions 2, 6, 7, 8) — each tension appears there exactly once.

### council:maintainer — revised

**Applied (one line):** forced `(created_at DESC, id ASC)` list order on every request with the
sqlbuild skip-and-duplicate bug mechanics, honest gap G7, and the test-9 first-page ordering
assertion; the welded-domain evolution policy (additive-only `/v0`, `WireSchemaVersion` bump ⇒
cut `/v1`); depguard `workapi-frontend-boundary` rule + `pr-policy.sh` grep replacing
review-enforced import policy (parity row 11, slice-1 accept); slice-11 vendor-neutral
genericization + the "Disposition of THIS document" paragraph + open question 6; context
`capabilities` derived from implemented routes with the transitional 501-stub test exemption;
the exact 10-field context wire surface with the 9 withheld `ContextInfo` fields; mode-gate
refusals keyed on `Backend:"dolt"` vocabulary.

**Deferred:**

- Genericizing the ~23 Gas City / downstream-consumer references scattered through the proposal
  body itself — the reviewer's requirement was to resolve the disposition, not rewrite the
  document: the applied slice-11 "Disposition" paragraph states the proposal does not land in
  the OSS tree as-is and that landing it would require the same scrub, and open question 6 puts
  the scrub-vs-engdocs-only choice to the owner. A full-body scrub would be a whole-document
  rewrite, against the targeted-edit mandate and colliding with three parallel reviewer passes.
- Choosing between `/v0` sunset-and-remove vs frozen version-suffixed mirror structs on the day
  a `WireSchemaVersion` bump forces `/v1` — the new evolution policy deliberately defers the
  (a)/(b) choice to that day while making non-deferrable the rule that corpus-regen +
  `JSONSchemaVersion` bump alone is insufficient once `/v0` ships; picking now would be
  speculative design for an event with no date, and option (b) touches the "no second wire
  struct" rule that the praise list protects for the live version. (Recorded as open
  question 8.)

### council:security — revised

**Applied (one line):** hardened the context 10-field allowlist with `SyncRemote` hard-excluded
for credential-leak mechanics and enforcement test 8b; 5xx detail scrubbing decided (static
per-code `detail`, real error server-log only — closed former open question 4, pinned by
test 5); `netutil.LimitListener(l, 64)` accepted-connection cap and the `/healthz` + `/context`
DB-semaphore bypass; the 60 s per-request deadline; `limit=0` → 400 under
`--allow-non-loopback` with binding rule 2 scoped to loopback (open question 2 narrowed);
wire-edge actor validation on claim (test 12); the Host-allowlist / DNS-rebinding middleware 400
with the mandatory content-type check (test 19); all landed as additive slice-5/6/7/9
deliverables, no new slices.

**Deferred:**

- Refusing `limit=0` whenever the resolved bind is non-loopback (address-keyed) rather than
  whenever `--allow-non-loopback` is set (flag-keyed) — flag-keyed was chosen deliberately: it
  is a stated posture ("asking for network exposure turns off unlimited reads"), strictly safer
  (never weaker than address-keyed), and testable in CI on a loopback bind without binding
  0.0.0.0; noted in test 19's description.
- A dedicated load/exhaustion test for the LimitListener cap and per-request deadline —
  resource-exhaustion tests of this kind are flaky in shared CI and the reviewer did not
  require one; the mechanisms are specified as slice-6 deliverables and the deadline's
  user-visible mapping (generic 500) is already covered by the status-code parity machinery.

### council:sre — revised

**Applied (one line):** the observability floor (structured stderr request/startup/shutdown
lines, saturation event, `request_id` extension) as a slice-6 acceptance criterion; the 10 s
bounded semaphore wait → 503 `busy` and the wedge scenario closed end to end; pool limits on
the serve-owned `*sql.DB` with the sole-tenant honesty on the "16 conns fit" claim; the
detached-close fix landing in `uow.RunTxResult` itself (rule 5); the limit=0 risk rewritten as
a shared-process OOM blast radius with the ship-`--max-rows`-in-v0 recommendation (open
question 2 sharpened); drain raised 10 s → 20 s with the idempotent same-actor re-claim
recovery and SIGHUP/supervisor note; the singleton claim made honest (default `127.0.0.1:0`
allows N parallel serves; fixed-port `--addr` blessed); claim retry-exhaustion `Retry-After`
1 → 5 with the convoy rationale; slice-11 runbook deliverables enumerated.

**Deferred:**

- An actual v0 operator flag for the read-handler deadline (the "flag-tunable" half of the
  wedge-protection item) — the document's decided command surface is deliberately minimal ("no
  other flags in v0", § 4); the deadline is specified as a generous 60 s constant explicitly
  documented as promotable to an operator flag later without wire impact — the same posture the
  doc already takes for the semaphore and connection-cap constants, satisfying the tunability
  intent without growing the v0 flag surface.
- Streaming JSON encoder for list/ready responses — considered and rejected in favor of the
  `--max-rows` recommendation, with the reason recorded in open question 2: streaming removes
  only the marshal copy — the query layer still buffers every row — so it halves the footprint
  without bounding it.
- Lowering the HTTP-side claim retry budget (the alternative branch of the Retry-After minor) —
  binding rule 5 pins the single `uow.RunTxResult` implementation with its tested 15 s budget
  shared with the proxied CLI; the `Retry-After: 5` branch of the reviewer's either/or was
  applied instead, which addresses the convoy without forking or parameterizing the one retry
  implementation.

### council:consumer — revised

**Applied (one line):** strict unknown-parameter rejection on every operation with the
`param`/`reason` 400 extension members and test 8c; the reconciler's TierBoth+SkipLabels full
active-universe scan truthfully scoped out of v0 (thesis exception, the "what this list surface
deliberately cannot serve" paragraph with the verified additive path, the skip_labels trap
correction, slice-11 negative statement); the version-skew client-contract section
(`capabilities`/`bd_version`/`api_version` gates, probe-by-400 parameter gating,
default-branch-on-unknown-codes, `schema_version` not-an-HTTP-gate weld) plus open question 7;
test 18 extended with the item-level claim parity oracle.

**Deferred:**

- Option (a) of the reconciler change: actually expose an ephemeral/wisp-inclusion parameter
  and a labels-omission knob on `/v0/beads/issues` in v0 — took the reviewer's sanctioned
  option (b) — truthful scoping — instead: the knobs would be the only MVP wire surface with no
  CLI parity oracle behind it (`bd list` cannot emit ephemeral rows, so rule-8 full-item
  oracles cannot run), and the labels-omission marker is a new page-envelope design decision;
  the additive path is documented concretely in § 3 with the verified field citations so a
  later slice can ship it without re-derivation.
- Ship `api_revision` (spec `info.version`) as a `ContextResponse` field now — the reviewer
  asked only to "consider" it; shipping it would grow the security reviewer's enforced
  ten-field context allowlist (test 8b asserts exactly ten keys) and permanent wire surface, so
  it is recorded as concrete either/or open question 7 with the enforcement mechanism
  (pr-policy: spec diff without `info.version` change fails) spelled out, while v0's
  parameter-level gate is the strict unknown-parameter probe.
- Unweld `WireSchemaVersion` from `JSONSchemaVersion` (separate CLI-envelope version constant) —
  the reviewer asked only that `schema_version` be documented as not-an-HTTP-gate and the weld
  named; actually splitting the constant would revert § 2's shared-constant design (parity
  row 9) that earlier passes preserved — the skew section instead states the weld, forbids
  HTTP-side keying on the value, and notes the unweld is additive on the HTTP side if a
  CLI-only schema change ever collides with the `/v0` bump freeze.

### Post-review consistency check (values touched by more than one pass)

Spot-checked after all four passes; each value below holds exactly one consistent position
across the document. No contradictory pairs required fixing.

- **List limit defaults**: 50 (list) / 100 (ready), `0` = unlimited — identical in § 1 rule 9,
  both parameter tables, § 3 pagination, the one-way-doors table, and test 4's assertion.
- **In-flight bound**: `serveMaxInflight = 16` (DB semaphore) inside `LimitListener(l, 64)`
  (connection cap), pool `SetMaxOpenConns(16 + 4)` — § 4 concurrency, pool-limits, and slice 6
  agree; the "16 conns fit under max_connections" claim carries the SRE sole-tenant caveat in
  its single location.
- **Timeouts**: 60 s per-request deadline (security-introduced, SRE-annotated, one value),
  10 s semaphore wait, 20 s shutdown drain (§ 4 explicitly reconciles "20 s, not 10" against
  the 15 s claim retry budget), `Retry-After: 5` on retry exhaustion / `db_unavailable` vs
  `Retry-After: 1` on semaphore-wait timeout — the § 3 busy row and § 4 narrative state the
  same split.
- **Status-code table**: the § 3 sentinel table's rows (400 with `param`/`reason` extensions,
  400 `invalid_cursor`, 404, 409 ×2, 503 `busy`/`db_unavailable`, 500) match the
  six-operations table's per-route documented codes plus the once-documented middleware 400;
  the deliberate non-codes (413/429/504/403/501) are consistent with the queue-not-refuse
  semaphore and the deadline→500 mapping after the SRE carve-out.
- **Context field list**: exactly ten fields (7 from `ContextInfo` + `api_version`,
  `schema_version`, `capabilities`) in the § 3 enumeration, the version-skew section, test 8b
  ("any JSON key outside the ten"), and open question 7's "ten fields".

**Consolidation performed in this pass**: the maintainer's deferred-to-that-day `/v0`
sunset-vs-freeze either/or was present in § 3's evolution policy but absent from § 5's owner
list — added as open question 8 with a cross-reference from the policy text. The security and
SRE cap questions were already merged into the single open question 2 (loopback `--max-rows`
either/or plus the non-loopback refusal-vs-mandatory-cap variant); no duplicates remained. The
table of contents gained the appendix entry.
