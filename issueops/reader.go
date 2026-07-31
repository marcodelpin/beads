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
	// Offset skips the first N matching rows, where the backend supports it.
	Offset int
}

// ListRequest describes one issue-list query.
//
// Like ReadyRequest it is high-level: the default status/template/gate/infra
// exclusions, type validation, status parsing and limit defaulting are all
// applied inside.
type ListRequest struct {
	// Status selects statuses; one name, or a comma-separated OR set. Setting
	// it REPLACES the default exclusions rather than fighting them.
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

	// AllFlag drops the default status exclusions; ReadyFlag switches the
	// query to blocker-aware ready work under the list vocabulary.
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
	// Offset skips the first N matching rows, where the backend supports it.
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
//   - The HTTP surface is on the role for all three operations, and two lint
//     rules make reaching past it a lint failure rather than a review comment:
//     httpapi-transport-boundary (depguard) denies the builders, and a
//     forbidigo rule denies naming types.IssueFilter or types.WorkFilter in
//     that package, which is the half that covers hand-rolling one.
//   - `bd show --json` is on the role.
//   - `bd ready` and `bd list` are NOT, on either route. They consume the
//     FILTER itself for things this role does not express — the --max-rows
//     cap, --claim, --gated, --explain, --mol, --watch, the hierarchical
//     --parent tree, and the text renderings that want []*types.Issue rather
//     than a counted page — so they still call the workapi builders directly.
//     Their protection against drift is one level down: both routes and both
//     Reader implementations build from these same request types through the
//     same builders, which the builders' golden files pin. Routing only their
//     JSON paths through the role would fork each command in two, which is
//     more drift, not less.
//
// Closing that gap needs more roles (a claim role, an explain role), not more
// methods here. Until then the acceptance criterion is "no HTTP handler
// constructs a filter", and it should be stated that way wherever it is
// claimed.
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
