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

## The checklist

Twelve steps. Steps 1-9 are the role; 10-12 are what makes it the ONLY way in.

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

    Do not add role cases to `backend/conformance`'s older `RunAll` suite: its
    `Factory` hands back a bare `storage.DoltStorage`, which a unit-of-work
    provider can never be, so a case placed there silently never runs on the
    backend most likely to diverge.

12. **Both front doors, and the lint that keeps them there.** The CLI handler
    and any HTTP handler call the role and nothing else — no filter, no config
    load, no unit of work opened by hand. Then close the holes behind them in
    `.golangci.yml`: add the step-3 package to the `cmd-bd-role-constructors`
    depguard deny list, and if the command no longer names `types.IssueFilter`,
    REMOVE it from the forbidigo exception list (and decrement the count in the
    comment above it, and in `issueops/reader.go`'s claim, which both state the
    number). Removing an entry there is how a role commit proves its front door
    actually reached the role.

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
