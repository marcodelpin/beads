# Adding an issueops role

The governing rule is short: **a new capability gets a new role interface and a
new accessor. Never append a method to an existing role.** That rule is why
`storage.Storage` reached 139 methods once and will not again.

What the rule does not say is what a new role COSTS. This page is the measured
answer, derived from `issueops.Counter` — the first role added under the rule,
and the one every later role commit follows. Nothing here is aspirational: each
item names a file that exists in that commit.

## Is it a role at all?

Two questions, both of which must be yes.

- **Is it a different QUESTION?** Not a different filter, a different shape of
  answer. `Counter` answers with a number; `Reader` answers with pages of
  issues. A count has no order, no page and no cursor, so folding it into
  `Reader` would have meant a request carrying paging fields a count must
  ignore — the shape that makes a caller believe `--limit 10` bounded the
  answer.
- **Is it reached THROUGH a substrate?** A role is handed back by an accessor
  on a store or a unit-of-work provider. Anything that CREATES a substrate —
  `bd init`'s filesystem and git provisioning — is constructor territory and
  cannot be a role.

A role may be born with more than one method when they are two shapes of ONE
question (`Count` and `CountByGroup` share a predicate and differ only in
whether the answer is a number or a number per bucket). The rule forbids
APPENDING later, not being born whole.

**And one command may be born with TWO roles.** `bd init` is the case:
`Bootstrapper` writes the workspace identity and `InitVerifier` reads it, and
they are separate even though `VersionReconciler` — whose read and write are
also two shapes of one question — is one. The test that separates them is not
"is it the same question" but **can one caller be entitled to the read and not
the write**. bd reads a workspace identity on paths where it is forbidden to
write one: a bts-provisioned team database whose identity the provisioning tool
owns, an authenticating gateway whose credential may be read-only. Handing those
callers a surface with the write on it is the shape that produces writes the
front door then has to suppress flag by flag — `shouldWriteProjectIDLocally`,
`shouldWriteInitStateToDB`, `shouldInitSharedGlobalDB` are three of those. A
capability a caller must not have is a capability it should not be able to
reach.

**Ask what the role's fields' LIFETIMES are before folding them in.** The same
slice teaches this: `bd init` seeds five values, and only two of them —
the prefix and the project id — are written once and adopted forever. The
repository and clone fingerprints, the synced-at marker and the recorded binary
version are refreshed on EVERY init, adopt or not, because a fresh clone of an
already-identified database needs its own. Putting those on a one-time,
refusable write means either a refusal that skips them or a write that is no
longer one-time. They stayed off the role.

## The checklist

Thirteen steps. Steps 1-9 are the role; 10-12 are what makes it the ONLY way
in; 13 is the HTTP surface, which lands with the command rather than after it.

1. **The leaf contract.** `issueops/<role>.go`. Request and result types plus
   the interface, with the doc comment written as a SPECIFICATION — every
   promise a conformance case will cite by line. The package imports
   `internal/types` and stdlib only, and exports no constructors.
   Template: `issueops/readyclaimer.go`, `issueops/commenter.go`.

2. **The shared request→filter builder**, if the role takes a filter-shaped
   request. Beside its siblings in `internal/workapi` (`count.go`:
   `BuildCountFilter`, `ValidateCountGroup`). Every implementation builds
   through it, so it is the single definition of what the command means, and it
   is unit-testable with no database (`internal/workapi/count_test.go`).

3. **The store-backed body**, shared by dolt and embeddeddolt:
   `internal/workapi/store<role>/`. Its own package, not a file in
   `internal/workapi` — 22 `cmd/bd` files already import workapi, so a
   constructor there is one line away from any front door, and a front door
   that constructed the role directly would get one stripped of its decorators.

   **When the body needs a TRANSACTION, step 3 moves down instead of out.**
   `storage.DoltStorage` publishes methods, not transactions, so a role whose
   work is several reads that must see one snapshot cannot be written against
   it. `issueops.CycleDetector` is the case: it builds a graph and then hydrates
   the nodes it found. Its shared body is a tx-level function beside its
   siblings in `internal/storage/issueops`
   (`cycle_report.go`: `DetectCycleReportInTx`), and each store package holds a
   five-line accessor around its own `withReadTx`/`withConn`
   (`dolt/cycle_detector.go`, `embeddeddolt/cycle_detector.go`). The unit-of-work
   body reaches the same function through a new method on the domain repository
   and use case. Two wrappers over one function is still ONE vote, exactly as a
   shared `store<role>` package would be, and there is no importable constructor
   for step 12's depguard entry to deny — the body types are unexported and the
   accessor is the only door.

   Put the parts that decide what the answer MEANS in PURE functions either way
   (`CanonicalCyclePaths`, `BuildCycles`), so they are pinned in milliseconds
   without a database and the conformance contract is left to assert what only a
   real backend can show.
   **Unless the role needs ONE TRANSACTION.** A body that must read twice and
   have both reads see one snapshot — an existence probe plus the read it
   qualifies, which is what `Relations` and `EdgeReader` both do — cannot live
   above `storage.DoltStorage`, because that interface publishes no way to scope
   two calls to a transaction. Those bodies are `Validate…`/`Execute…InTx`
   functions in `internal/storage/issueops` (`relations.go`, `edges.go`) and
   each store's accessor wraps them in its own `withReadTx`/`withConn`. dolt and
   embeddeddolt still share ONE body, which is what step 3 is for; only its
   address differs. There is no step-12 depguard entry to add for one of these
   — the shared function takes a transaction, so no front door can call it at
   all.

4. **The unit-of-work body and its source interface.**
   `internal/storage/uow/<role>.go`, declaring `type <Role>Source interface {
   <Role>() (publicops.<Role>, error) }` and implementing the accessor on
   `*doltSQLProvider`. This is the one genuinely independent implementation —
   dolt and embeddeddolt share step 3, so "both stores agree" is ONE vote.

5. **The accessor on the interface.** One method plus its doc on
   `storage.Storage` in `internal/storage/storage.go`. **This is the file every
   role commit touches**, one line each: cheap merges, but not free ones.

6. **The hook wrapper.** `internal/storage/hook_<role>.go`. A WRITE role wraps
   the inner surface and fires its completion hooks
   (`hook_commenter.go`); a READ role recurses and returns the inner surface
   unwrapped, because there is no completion to report (`hook_issue_reader.go`,
   `hook_counter.go`). Either way the accessor must EXIST on the decorator: it
   is declared, never inherited, and there is a reflection test that says so.

   **The read/write split is about the HOOK VOCABULARY, not about whether the
   role writes.** `internal/hooks` publishes on_create, on_update and on_close
   and nothing else, so a write the vocabulary cannot name recurses like a
   read: `issueops.Sweeper` deletes rows and `hook_sweeper.go` is three lines,
   because there is no on_delete to fire and the rows a hook script would be
   handed are gone. Say which it is and why in the file, and assert it in
   `role_accessor_decorator_test.go`'s wrapped/unwrapped table — the whole
   point of that table is that "recurses" reads identically whether it was a
   decision or an omission.

7. **The telemetry wrapper.** `internal/telemetry/<role>.go`. No read/write
   distinction here — telemetry spans reads too, so every method gets
   `storage.op` / `storage.done`.

8. **The two store accessors.** `internal/storage/dolt/<role>.go` and
   `internal/storage/embeddeddolt/<role>.go`, both delegating to step 3. The
   embedded one carries `//go:build cgo`. A nil receiver returns
   `*storage.ErrUnsupported` naming the op.

9. **Every other implementer of `storage.Storage`.** The compiler finds them;
   today that is the `configStore` stub in `internal/jira/tracker_test.go`.

10. **The decorator enumerations.** Both `role_accessor_decorator_test.go`
    files (`internal/storage`, `internal/telemetry`) list the roles by name in
    `roleAccessorNames`, declare them on a fake store, implement them on a
    shared sentinel, and drive them in two tables each. All five places, in
    both files. Add the layering pin in `issue_roles_external_test.go` too:
    a write role expects the hook wrapper outermost, a read role expects the
    telemetry wrapper.

11. **The conformance contract and its three wirings.**
    `backend/conformance/<role>_contract.go` holds the cases and the
    `<Role>Fixture`; the wirings are
    `internal/storage/{dolt,embeddeddolt,uow}/<role>_contract_test.go`. Each
    wiring is a `roleFixtureKit` plus the accessor plus a prefix, with no
    adapter in between — the kit is FROZEN and a role slice does not edit it.
    Every case asserts what the leaf doc PROMISES, cited by line. A backend
    that genuinely disagrees is parked at its WIRING site with
    `skipKnownDivergence`, never by weakening the case.

    A case may need to establish a state the ROLE CANNOT PRODUCE — a recorded
    version below its own high-water mark is one, and reconciling is the one
    thing that can never create it. The kit is frozen and reaches the issues
    and config planes only, so that hook is a short closure built at each
    wiring site over a seam the backend already publishes, the way
    `CycleDetectorFixture.Exec` is. It belongs on the fixture, documented as
    out-of-band, not in the kit.

    **SAY HOW MANY VOTES THE THREE LEGS ACTUALLY ARE, at the top of the
    contract file.** dolt and embeddeddolt share step 3, so most roles are two
    votes; `issueops.TreeWalker` is the first where ALL THREE share one body —
    the unit of work reaches the same `…InTx` function through the domain
    repository — so its three-leg run is ONE reading plus an engine check. That
    is still worth running, because every measured drift in the graph family has
    lived in a WRAPPER, but the cases have to be written for it: assert
    SENTINELS rather than message text and assert a typed error's FIELDS, since
    a wrapper losing a transaction, dropping a request field or breaking
    `errors.Is` is what a per-leg failure would actually be. And pull as much of
    the answer's MEANING as will go into pure functions beside the body, so the
    parts one vote cannot cover are pinned without a database.

    Do not add role cases to `backend/conformance`'s older `RunAll` suite: its
    `Factory` hands back a bare `storage.DoltStorage`, which a unit-of-work
    provider can never be, so a case placed there silently never runs on the
    backend most likely to diverge.

12. **Both front doors, and the lint that keeps them there.** The CLI handler
    and any HTTP handler call the role and nothing else — no filter, no config
    load, no unit of work opened by hand. Since the owner's 2026-08-05 scope
    decision the HTTP half lands WITH the command: the operation is written into
    `internal/httpapi/spec/openapi.v0.yaml` FIRST and the types generated from
    it (`make api-gen`), never the other way round. That costs four more edits
    the spec tests will name if you miss one — a row in `routeTable`, an
    `Op<Name>` constant and an `operationCodes` row in `problem.go`, and the
    capability token in `ContextResponse.capabilities`'s prose. A role served
    from the store-shaped `Config` source also becomes a required field there,
    because `checkDatabaseSource` refuses a half-set source rather than letting
    a handler dereference nil on a live server.
    Then close the holes behind them in
    `.golangci.yml`: add the step-3 package to the `cmd-bd-role-constructors`
    depguard deny list, and if the command no longer names `types.IssueFilter`,
    REMOVE it from the forbidigo exception list (and decrement the count in the
    comment above it, and in `issueops/reader.go`'s claim, which both state the
    number). Removing an entry there is how a role commit proves its front door
    actually reached the role.

13. **The HTTP half, when the role has one.** The spec is written FIRST —
    `internal/httpapi/spec/openapi.v0.yaml`, then `make api-gen` — and the
    handler is written against the generated types, never the other way round.
    An operation costs four more edits beside the handler: the `Op*` id and the
    `operationCodes` row in `problem.go`, the `routeTable` row and its
    `capability` token in `routes.go`, and that token in the document's
    `capabilities` vocabulary. `make api-check` is the gate.

    **A role may honestly have none, and `issueops.VersionReconciler` is the
    one that does.** It answers a startup hook rather than a command: its two
    markers are clone-local and dolt-ignored, so they describe the machine
    holding the database and mean nothing to a remote caller, and the one
    version fact a client can act on is already `ContextResponse.bd_version`.
    Writing that decision down — in the leaf doc, beside the promises, and in
    AMBIGUITIES.md for the owner — is the work. Adding an operation because the
    checklist has a row for one is not.

    `issueops.Bootstrapper` and `issueops.InitVerifier` are the second and third,
    and their reason is different enough to be worth its own sentence: a server
    can only serve a database it is already bound to, and binding it required
    the identity the bootstrapper writes, so the only caller that could reach
    such an endpoint would be asking a running server to identify a workspace
    that is by construction already identified. The read half has no endpoint
    because it already has one — `ContextResponse.project_id` is its answer, on
    the wire, today.

    **The ROLES database source is the part that surprises.** `httpapi.Config`
    carries either a unit-of-work provider or the roles, and the roles are
    required TOGETHER: a new operation served from a new role adds a required
    field, so `checkDatabaseSource` grows a term and every caller and test that
    built a Config grows a line. That is deliberate — a Config missing the new
    role would otherwise bind and nil-dereference on the first request that
    reached the new handler — but budget it, because it is the one edit in this
    list that touches code the role commit did not write.

## What the whole thing costs

`bd count` behind `issueops.Counter`, end to end, in one commit:

| | files |
|---|---|
| new production files | 6 (leaf, builder, store body, uow body, hook wrapper, telemetry wrapper) + 2 store accessors |
| edited production files | 3 (`storage.go`, `cmd/bd/count.go`, `cmd/bd/count_proxied_server.go`) |
| new test files | 4 (contract + three wirings) + 1 builder unit test |
| edited test files | 5 (two decorator enumerations, the root layering pins, the command's own, the `internal/jira` stub from step 9) |
| config | 1 (`.golangci.yml`: one deny entry added, one exception entry removed) |

Counter's commit also touched three files the next role will not: renaming the
depguard rule from `cmd-bd-reader-constructor` to `cmd-bd-role-constructors`
moved a word in `internal/workapi/storereader/reader.go`,
`cmd/bd/show_proxied_server.go` and `issueops/reader.go`. That was the one-off
generalization that made step 12 a one-line edit from here on.

Nine of those are mechanical once the leaf contract is written. The two that
are not, and where the time actually goes, are the leaf doc comment and the
conformance cases — because those are the parts that decide what the role
MEANS, and the parts every later reader trusts.

## Two traps

- **Reach a role through its ACCESSOR, never its constructor.** The accessor is
  where each decorator adds its layer, so a caller that constructed the body
  directly gets it unspanned and unhooked, and the code looks perfectly
  ordinary. This is what the depguard rule in step 12 is for, and it is why the
  uow conformance wirings assert through `provider.(<Role>Source)` rather than
  calling `New<Role>`.
- **Three wirings are not three votes.** dolt and embeddeddolt share step 3.
  Say "two independent bodies plus an engine check" in the commit message, not
  "three backends agree".
