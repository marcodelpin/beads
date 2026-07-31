package workapi

import (
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/utils"
)

// DefaultReadyLimit is the default number of rows a ready-work query returns
// when the caller does not ask for a specific limit. Every frontend registers
// its limit knob from this constant so the surfaces cannot drift apart; 0
// still means unlimited.
const DefaultReadyLimit = 100

// ReadyParams is the frontend-independent input to BuildReadyFilter: the set
// of ready-work knobs that shape the query. Presentation choices (pretty vs
// plain, JSON) and the mode switches that pick a different query entirely
// (--claim, --mol, --gated, --explain) are deliberately absent - they belong
// to the caller.
//
// Labels, LabelsAny and ExcludeLabels are normalized here, so a frontend can
// pass raw user input. A frontend that has to decide something from the label
// sets - the CLI's directory-label default is the one such case - reads them
// back off the returned filter, where they are already normalized, rather than
// normalizing its own copy: a value it then puts on the filter itself is its
// own to shape, and running it through here would change it.
type ReadyParams struct {
	IssueType  string
	Assignee   string
	Unassigned bool

	Labels        []string
	LabelsAny     []string
	ExcludeLabels []string
	LabelPattern  string
	LabelRegex    string

	Priority    int
	PrioritySet bool

	ParentID string
	MolType  *types.MolType

	IncludeDeferred  bool
	IncludeEphemeral bool
	ExcludeTypeStrs  []string

	MetadataFields map[string]string
	HasMetadataKey string

	SortPolicy string

	Limit  int
	Offset int
}

// BuildReadyFilter turns ready parameters into the storage-level work filter.
// It is the single definition of what `bd ready` means: open issues only (an
// empty WorkFilter would default to open plus in_progress), issue-type alias
// expansion, label and exclude-type normalization, and the assignee rule that
// lets --unassigned win over a stale --assignee.
func BuildReadyFilter(in ReadyParams) (types.WorkFilter, error) {
	filter := types.WorkFilter{
		// Open only, not in_progress - the same set `bd list --ready` shows.
		Status:           types.StatusOpen,
		Type:             utils.NormalizeIssueType(in.IssueType),
		Limit:            in.Limit,
		Offset:           in.Offset,
		Unassigned:       in.Unassigned,
		SortPolicy:       types.SortPolicy(in.SortPolicy),
		Labels:           utils.NormalizeLabels(in.Labels),
		LabelsAny:        utils.NormalizeLabels(in.LabelsAny),
		ExcludeLabels:    utils.NormalizeLabels(in.ExcludeLabels),
		LabelPattern:     in.LabelPattern,
		LabelRegex:       in.LabelRegex,
		IncludeDeferred:  in.IncludeDeferred,
		IncludeEphemeral: in.IncludeEphemeral,
		ExcludeTypes:     normalizeExcludeTypes(in.ExcludeTypeStrs),
		HasMetadataKey:   in.HasMetadataKey,
	}

	if in.PrioritySet {
		p := in.Priority
		filter.Priority = &p
	}
	if in.Assignee != "" && !in.Unassigned {
		a := in.Assignee
		filter.Assignee = &a
	}
	if in.ParentID != "" {
		pid := in.ParentID
		filter.ParentID = &pid
	}
	if in.MolType != nil {
		filter.MolType = in.MolType
	}
	if len(in.MetadataFields) > 0 {
		filter.MetadataFields = in.MetadataFields
	}

	if !filter.SortPolicy.IsValid() {
		return filter, fmt.Errorf("invalid sort policy '%s'. Valid values: hybrid, priority, oldest", in.SortPolicy)
	}
	return filter, nil
}

// normalizeExcludeTypes splits comma-separated exclusions and expands type
// aliases, so --exclude-type=mr,epic and --exclude-type mr --exclude-type epic
// mean the same thing.
func normalizeExcludeTypes(raw []string) []types.IssueType {
	var out []types.IssueType
	for _, group := range raw {
		for _, t := range strings.Split(group, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				out = append(out, types.IssueType(utils.NormalizeIssueType(t)))
			}
		}
	}
	return out
}
