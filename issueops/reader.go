package issueops

import (
	"context"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// IssueWithCounts is one row of a work page: the issue plus its relationship
// cardinalities.
type IssueWithCounts = types.IssueWithCounts

// IssueDetails is one issue with its labels, edges and cardinalities.
type IssueDetails = types.IssueDetails

// MolType classifies a molecule.
type MolType = types.MolType

// WispType classifies an ephemeral record.
type WispType = types.WispType

// ReadyRequest describes one ready-work query.
//
// It is a HIGH-LEVEL request, not a filter: normalization, alias expansion,
// validation and defaulting all happen inside the implementation. A caller
// that wants ready work says what it wants, never how the query is shaped —
// which is the whole reason two front doors cannot answer this question
// differently.
type ReadyRequest struct {
	// IssueType restricts the type. Only shorthand alias expansion is applied
	// (mr, feat, mol, enhancement, dec, adr); an unrecognized type matches
	// nothing rather than failing. Setting it drops the default type
	// exclusions, ExcludeTypes included.
	IssueType string
	// Assignee restricts to one actor. Unassigned wins over a stale Assignee.
	Assignee string
	// Unassigned restricts to rows with no assignee.
	Unassigned bool

	// Labels must ALL be present; LabelsAny requires at least one;
	// ExcludeLabels must be absent. All three are raw: normalization happens
	// inside.
	Labels        []string
	LabelsAny     []string
	ExcludeLabels []string
	// LabelPattern is a glob and LabelRegex a regular expression, both matched
	// against labels.
	LabelPattern string
	LabelRegex   string

	// Priority is an exact priority. It is a pointer because 0 is a real
	// priority: a value-plus-flag pair would let one half be filled in without
	// the other, and P0 has already been lost that way once.
	Priority *int

	// ParentID restricts to recursive descendants of one issue.
	ParentID string
	// MolType restricts to one molecule type.
	MolType *MolType

	// IncludeDeferred admits rows whose defer_until is still in the future;
	// IncludeEphemeral admits wisp-plane rows.
	IncludeDeferred  bool
	IncludeEphemeral bool

	// ExcludeTypes names types to exclude. Entries may be comma-separated;
	// splitting and alias expansion happen inside. Ignored when IssueType is
	// set.
	ExcludeTypes []string

	// MetadataFields is a top-level metadata equality filter and
	// HasMetadataKey a top-level key-presence filter. Keys are validated
	// inside.
	MetadataFields map[string]string
	HasMetadataKey string

	// Sort is the ready ordering: hybrid, priority or oldest. Empty resolves to
	// hybrid at the storage layer, which no front door should rely on — both
	// surfaces send a concrete policy, because hybrid demotes older
	// high-priority work and therefore changes the item SET once Limit
	// truncates.
	Sort string

	// Limit bounds the page. Nil means the shared ready default; 0 means
	// unlimited. It is a pointer so that "unset" and "explicitly unlimited"
	// stay distinguishable, which is what lets one constant serve both
	// surfaces.
	Limit *int
	// Offset skips the first N matching rows. It is honored by the
	// unit-of-work implementation, which renders OFFSET; the store-backed one
	// cannot, and REFUSES a non-zero Offset with a typed *ErrUnsupported
	// naming the operation and the backend. What it never does is silently
	// return an unpaged answer, so a caller that sets it either gets the page
	// it asked for or an error it can classify with errors.As.
	//
	// A ready request carries no keyset position, so there is no portable way
	// to page ready work. A caller that must page across backends pages a
	// ListRequest instead — see ListRequest.Offset.
	Offset int
}

// ListRequest describes one issue-list query.
//
// Like ReadyRequest it is high-level: the default
// status/pinned/template/gate/infra exclusions, type validation, status
// parsing and limit defaulting are all applied inside.
//
// PINNED is TWO exclusions rather than one, because a row can be pinned two
// unrelated ways and each is hidden by its own predicate. The `pinned` STATUS
// is one of the statuses the default status exclusion covers, alongside
// `closed`; the separate Pinned FLAG — a marker on a row of any status — is
// hidden by a predicate of its own. A row carrying either is absent from a
// default listing, and the knob that takes each back off is a different one:
// see Status for the first and PinnedFlag for the second.
type ListRequest struct {
	// Status selects statuses; one name, or a comma-separated OR set. Setting
	// it REPLACES the default exclusions rather than fighting them.
	//
	// It replaces the STATUS ones only. The pinned-flag predicate survives it,
	// so `pinned` and `hooked` are the two spellings that also drop that
	// predicate — every other status narrows to unflagged rows.
	Status string
	// IssueType is validated against the workspace vocabulary — unlike
	// ReadyRequest.IssueType, which matches nothing rather than failing.
	IssueType   string
	Assignee    string
	TitleSearch string
	SpecPrefix  string
	// IDFilter is a comma-separated id set.
	IDFilter string

	Labels        []string
	LabelsAny     []string
	ExcludeLabels []string
	LabelPattern  string
	LabelRegex    string

	TitleContains    string
	DescContains     string
	NotesContains    string
	ExternalContains string
	ExternalRef      string

	CreatedBefore *time.Time
	CreatedAfter  *time.Time
	UpdatedAfter  *time.Time
	UpdatedBefore *time.Time
	ClosedAfter   *time.Time
	ClosedBefore  *time.Time
	DeferAfter    *time.Time
	DeferBefore   *time.Time
	DueAfter      *time.Time
	DueBefore     *time.Time

	EmptyDesc  bool
	NoAssignee bool
	NoLabels   bool
	SkipLabels bool

	// Priority is exact; PriorityMin and PriorityMax bound a range. All three
	// are pointers for the same reason ReadyRequest.Priority is.
	Priority    *int
	PriorityMin *int
	PriorityMax *int

	// PinnedFlag and NoPinnedFlag select on the Pinned FLAG, which is not the
	// `pinned` status — see this type's own doc for why there are two of them.
	//
	// PinnedFlag answers with the FLAGGED rows and only them, at any status: it
	// drops the default status exclusions on the way, so a closed pinned row —
	// which a default listing hides twice over — is in that answer.
	//
	// NoPinnedFlag asks for the UNFLAGGED rows, which a default listing already
	// answers with. Its work is holding that predicate in place where AllFlag
	// or a pinned/hooked Status would otherwise have dropped it, so on a
	// default listing it changes nothing and on those it narrows.
	//
	// On the ReadyFlag arm neither applies: that set decides pinned for itself,
	// which is why PinnedFlag is refused there and NoPinnedFlag merely accepted
	// (see AllFlag).
	PinnedFlag       bool
	NoPinnedFlag     bool
	IncludeTemplates bool
	IncludeGates     bool
	IncludeInfra     bool
	// ExcludeTypes entries may be comma-separated; splitting happens inside.
	ExcludeTypes []string

	ParentID string
	NoParent bool
	MolType  *MolType
	WispType *WispType

	DeferredFlag bool
	OverdueFlag  bool

	// MetadataFields is a top-level metadata equality filter and
	// HasMetadataKey a top-level key-presence filter. Keys are validated
	// inside, as they are for ReadyRequest.
	MetadataFields map[string]string
	HasMetadataKey string

	// AllFlag drops the default status exclusions.
	//
	// ReadyFlag switches the query to the blocker-aware ready set. It does NOT
	// simply add a blocker predicate to this listing: that query reads a
	// narrower filter vocabulary than this request can describe, and only part
	// of the request reaches it.
	//
	// WHAT IT CARRIES: IssueType, all five label forms, Assignee, NoAssignee,
	// the exact Priority, ParentID, MolType, WispType, MetadataFields,
	// HasMetadataKey, the type exclusions (ExcludeTypes, and with them
	// IncludeGates and IncludeInfra), Limit and Offset. SortBy and Reverse
	// still apply, because the display order is applied to the page after the
	// query rather than inside it. Status and AllFlag are resolved to "open"
	// and have no further effect: ready work is open work.
	//
	// WHAT IT REFUSES: every other filter here is one the ready query cannot
	// carry, so combining it with ReadyFlag returns ErrValidation naming the
	// fields rather than answering a wider question than was asked. That is
	// IDFilter, TitleSearch, SpecPrefix, the four *Contains fields,
	// ExternalRef, every Created/Updated/Closed/Defer/Due bound, DeferredFlag,
	// OverdueFlag, EmptyDesc, NoLabels, NoParent, PinnedFlag, PriorityMin,
	// PriorityMax, and the keyset position. There is no fallback: no
	// combination of the two silently widens the answer.
	//
	// TWO THINGS THE READY SET DECIDES FOR ITSELF, which no field here
	// overrides. It never returns pinned issues, which is why NoPinnedFlag is
	// accepted and PinnedFlag refused. And it applies no template predicate at
	// all, so this request's default template exclusion does not reach it and
	// IncludeTemplates changes nothing here: a template is left out of a
	// ReadyFlag listing only when its issue type is one the ready query
	// already excludes. SkipLabels is likewise not carried — labels are
	// hydrated either way, which costs time and not correctness.
	AllFlag   bool
	ReadyFlag bool

	// SortBy names the display order and Reverse inverts it. A sort the
	// database cannot express (natural-numeric id order) is resolved inside by
	// fetching the full result set and trimming, so no caller has to know
	// which sorts those are.
	SortBy  string
	Reverse bool

	// Limit bounds the page the caller RECEIVES. Nil means the shared list
	// default; 0 means unlimited. The row limit actually pushed into the query
	// is derived from it inside the implementation, together with the
	// over-fetch that detects truncation.
	Limit *int
	// Offset skips the first N matching rows. It is honored by the
	// unit-of-work implementation, which renders OFFSET; the store-backed one
	// cannot, and REFUSES a non-zero Offset with a typed *ErrUnsupported
	// naming the operation and the backend. What it never does is silently
	// return an unpaged answer, so a caller that sets it either gets the page
	// it asked for or an error it can classify with errors.As.
	//
	// THE PORTABLE WAY TO PAGE IS THE KEYSET POSITION below,
	// AfterCreatedAt/AfterID: every implementation honors it, and it does not
	// skip or repeat rows when the result set changes underneath a walk.
	// Offset is here for the surfaces that already published it.
	Offset int

	// AfterCreatedAt and AfterID carry a decoded keyset position in the
	// (created_at DESC, id ASC) order. The opaque token that encodes them is a
	// transport concern and never reaches this contract.
	AfterCreatedAt *time.Time
	AfterID        string
}

// GetRequest describes one issue-detail lookup.
type GetRequest struct {
	// ID is the exact canonical id. There is no fuzzy, prefix or substring
	// resolution here: an interactive affordance that can resolve to a
	// different issue than the caller named has no place on a contract two
	// front doors share. The issue-to-wisp fallback DOES happen inside.
	//
	// THAT FALLBACK NEEDS NO PLANE ORDER, and this contract deliberately
	// states none: no LOCAL WRITE PATH can make an id resident in both planes,
	// so the state an order would arbitrate is not reachable by writing
	// through this library. Every local write path closes it. A guarded create
	// probes BOTH tables before inserting
	// and answers ErrAlreadyExists for an id either one already holds. Every
	// batch create — import, a graph apply, a cooked formula, a markdown
	// import — refuses on top of that an id already resident in the SIBLING
	// table, skipping the row rather than hard-failing only on the
	// auto-import recovery path, which leaves the resident row alone.
	// Promotion and demotion insert into the target plane and delete from the
	// source inside ONE transaction, so their dual-presence window never
	// commits. This is why the implementations are free to probe the planes in
	// either order, and why they in fact differ — a documented non-issue
	// rather than a latent divergence (bd-yby99.22).
	//
	// TWO CAVEATS ARE WORTH CARRYING, and the second is the load-bearing one.
	//
	// First: the graph-apply path served by a unit-of-work provider gets its
	// refusal from a preflight ABOVE storage rather than from the create
	// guard, so a future caller that reached the graph-apply use case directly
	// would be the thing that made this reachable.
	//
	// Second, and NOT closed by anything here: REPLICATION. The durable plane
	// merges from a remote on pull, the ephemeral tables are dolt-ignored and
	// local-only, and no post-merge check reconciles the two — the collision
	// guard is reachable only from the create path. So a clone holding a wisp
	// whose id another clone promoted to durable receives that id into the
	// issues plane while its own wisp row survives. That needs a deterministic
	// id to collide at all, which explicit-id creates and the identically
	// minted infra wisps supply. It is a REAL residual path, it is outside
	// this library's reach, and the resulting state is worse than an
	// unspecified plane order: the merge-based lookups behind ready and search
	// hard-error for the whole store. A plane order here would not rescue it.
	//
	// So the honest statement is the scoped one — no local write path reaches
	// dual residency, replication can, and a caller that hits it has a
	// corrupt store rather than an ordering question (bd-yby99.22).
	ID string
	// IncludeDependents and IncludeComments populate the two expensive row
	// lists. Both default off: the detail view carries counts, and a caller
	// that wants the rows asks for them.
	IncludeDependents bool
	IncludeComments   bool
}

// IssuePage is one page of work. Ready and List share it deliberately: both
// surfaces of both operations emit IssueWithCounts today, and a leaner page
// type for ready would drift at the field level the moment anything compared
// the two.
type IssuePage struct {
	// Items is the page, in the operation's order. Never nil for a successful
	// call.
	Items []*IssueWithCounts
	// HasMore reports that the limit truncated the result.
	HasMore bool
}

// Reader describes guarded issue queries: the read counterpart of Lifecycle,
// and — like Lifecycle — a role with its own accessor. A new capability gets a
// new role interface and its own accessor; never append a method here.
//
// Each method takes the whole request and performs filter and default
// construction INTERNALLY. A caller of this interface can only say
// rd.List(ctx, req): the four-step ritual of building a config source, loading
// config, building a filter and executing it is not reachable through it.
// Implementations never mutate caller-owned request values.
//
// WHERE THAT IS ENFORCED, precisely, because the difference matters:
//
//   - The HTTP surface is on the role for all three of its issue reads, and
//     two lint rules make writing a filter there a lint failure rather than a
//     review comment. THE CLAIM at the bottom of this comment names them, and
//     names what they cover and what they do not.
//
//   - `bd show --json`'s DETAIL VIEW is on the role on BOTH routes: the direct
//     one reaches it through store.IssueReader(), the proxied one through the
//     unit-of-work provider's accessor. Its ALTERNATE views are not, and are
//     not the same question: --refs, --children, --thread and --as-of each
//     answer with their own shape, which this contract does not describe and
//     no HTTP operation serves. --short is not either; it prints one line from
//     the issue, not a detail view.
//
//     The shared implementation behind the store accessor
//     lives in internal/workapi/storereader, which the
//     cmd-bd-reader-constructor depguard rule keeps out of cmd/bd: the
//     accessor is where each storage decorator adds its layer, so a command
//     that constructed a reader directly would get an unspanned one. `bd show`
//     keeps its own id RESOLUTION — fuzzy ids, cross-repo routing, --current —
//     and hands this contract the canonical id it resolved, because an
//     affordance that can answer with a different issue than the caller named
//     has no place on a contract an unattended HTTP client also calls.
//
//   - `bd ready` and `bd list` are NOT, on either route, and they will not be
//     until there are more roles to route them through. They consume the
//     FILTER itself for things this role does not express — the --max-rows
//     cap, --claim, --gated, --explain, --mol, --watch, the hierarchical
//     --parent tree, and the text renderings that want []*types.Issue rather
//     than a counted page. Routing only their JSON paths through the role
//     would fork each command in two, which is more drift, not less.
//
// WHAT THOSE TWO DO SHARE, stated exactly, because "not on the role" is not
// the same as "unprotected":
//
//   - CONSTRUCTION. Every route of both commands, and both implementations of
//     this interface, build from these same request types through the same
//     two builders in internal/workapi, which the builders' golden files pin.
//   - EXECUTION, for `bd list` on both routes and in every mode but one: the
//     sort, the trim to the page limit and the has-more verdict are
//     workapi.FinishPage, the one function both implementations of List below
//     also call. The exception is the hierarchical --parent tree under pretty
//     output, which renders the recursive walk's own result and reaches no
//     epilogue on either route. Where the epilogue does run, what is left
//     differing between a CLI listing and an HTTP one is presentation and the
//     --max-rows cap.
//   - EXECUTION, for `bd ready`, on the PROXIED route only. The direct route
//     keeps an epilogue of its own and cannot give it up: it answers the
//     strictly larger question "how many rows did the limit hide" with a
//     second counting query and publishes the total in its pagination meta,
//     where this role answers only "were any hidden". Collapsing them would
//     change one surface's published output.
//
// THE CLAIM, stated once and in full so it can be checked sentence by
// sentence. SHARED: all three issue reads on the HTTP surface go through this
// role, and so does `bd show --json`'s detail view on both its routes; `bd
// list` and `bd ready` are not on it and share instead the request types
// above, the two builders in internal/workapi that their golden files pin, and
// workapi.FinishPage — `bd list` on both routes in every mode but the
// hierarchical --parent tree, `bd ready` on its proxied route only. ENFORCED,
// and by what: depguard (httpapi-transport-boundary) denies internal/workapi
// from every non-test file of internal/httpapi, so no builder is callable
// there, and a forbidigo rule denies naming types.IssueFilter or
// types.WorkFilter there at all, so no filter is writable there either — both
// are directory-scoped with no per-file exception, so a file added to that
// package tomorrow is covered the moment it exists. That same forbidigo rule
// covers cmd/bd deny-by-default with 64 named exceptions, so the files
// implementing `bd list` and `bd show` cannot write a filter, and neither can
// a file they are split or renamed into unless the new name lands on that
// list. NOT ENFORCED: the rule forbids NAMING those types, not holding a
// value, so the property is "no filter is written there", not "every filter
// there came from a builder"; test files are exempt from both rules, because
// the oracles hold filters in order to inspect them; `bd ready`'s files are
// among the 64, since its listing and --claim are handed the filter itself and
// the blocked-issue views in those files name one directly, so it is guarded by
// the builder and the golden files and not by the linter;
// GET /healthz and GET /v0/beads/context are not issue queries and are on no
// role; `bd ready`'s direct route and `bd list`'s hierarchical tree run
// epilogues of their own; and none of this is a merge gate — the rules run in
// `make ci-pr-lint` on every pull request and aggregate into the ci-gate job,
// but main carries no branch protection beyond deletion and non-fast-forward,
// so no check is GitHub-required and a red gate binds by convention.
//
// Closing the rest needs more roles (a claim role, an explain role), not more
// methods here.
type Reader interface {
	// Ready returns unblocked open work in the requested policy's order.
	Ready(ctx context.Context, req ReadyRequest) (IssuePage, error)
	// List returns issues under the request's filters, in the requested
	// display order.
	List(ctx context.Context, req ListRequest) (IssuePage, error)
	// Get returns one issue's detail view. A miss — for both the issue and the
	// wisp table — is ErrNotFound; a backend failure passes through unchanged
	// and never decays into not-found.
	Get(ctx context.Context, req GetRequest) (*IssueDetails, error)
}
