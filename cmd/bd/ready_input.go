package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/utils"
	"github.com/steveyegge/beads/internal/workapi"
)

// readyInput is everything `bd ready` parsed off the command line: the
// frontend-independent query knobs (workapi.ReadyParams, which the filter is
// built from), the filter itself, and the mode and presentation choices that
// never leave the CLI.
type readyInput struct {
	workapi.ReadyParams

	filter types.WorkFilter

	claim        bool
	gated        bool
	molID        string
	explain      bool
	prettyFormat bool
	plainFormat  bool
	jsonOut      bool
}

// gatherReadyInput parses `bd ready`'s flags and builds the work filter. Both
// routes call it - the direct one in ready.go and the proxied one in
// ready_proxied_server.go - so there is one definition of what the command
// accepts. Usage errors are reported through HandleErrorRespectJSON, which is
// what a --json caller has always gotten from the direct route.
//
// The only knob it does not own is --max-rows: the cap is meaningful only on
// the direct route, which resolves and applies it itself.
func gatherReadyInput(cmd *cobra.Command) (readyInput, error) {
	in := readyInput{}

	in.claim, _ = cmd.Flags().GetBool("claim")
	in.gated, _ = cmd.Flags().GetBool("gated")
	in.molID, _ = cmd.Flags().GetString("mol")
	in.explain, _ = cmd.Flags().GetBool("explain")
	in.prettyFormat, _ = cmd.Flags().GetBool("pretty")
	in.plainFormat, _ = cmd.Flags().GetBool("plain")
	in.jsonOut = jsonOutput

	in.Limit, _ = cmd.Flags().GetInt("limit")
	if cmd.Flags().Changed("offset") {
		offset, _ := cmd.Flags().GetInt("offset")
		if offset < 0 {
			return in, HandleErrorRespectJSON("--offset must be >= 0")
		}
		in.Offset = offset
	}
	in.Assignee, _ = cmd.Flags().GetString("assignee")
	in.Unassigned, _ = cmd.Flags().GetBool("unassigned")
	in.SortPolicy, _ = cmd.Flags().GetString("sort")
	in.Labels, _ = cmd.Flags().GetStringSlice("label")
	in.LabelsAny, _ = cmd.Flags().GetStringSlice("label-any")
	in.ExcludeLabels, _ = cmd.Flags().GetStringSlice("exclude-label")
	in.LabelPattern, _ = cmd.Flags().GetString("label-pattern")
	in.LabelRegex, _ = cmd.Flags().GetString("label-regex")
	in.IssueType, _ = cmd.Flags().GetString("type")
	in.ParentID, _ = cmd.Flags().GetString("parent")
	in.IncludeDeferred, _ = cmd.Flags().GetBool("include-deferred")
	in.IncludeEphemeral, _ = cmd.Flags().GetBool("include-ephemeral")
	in.ExcludeTypeStrs, _ = cmd.Flags().GetStringSlice("exclude-type")

	if molTypeStr, _ := cmd.Flags().GetString("mol-type"); molTypeStr != "" {
		mt := types.MolType(molTypeStr)
		if !mt.IsValid() {
			return in, HandleErrorRespectJSON("invalid mol-type %q (must be %s)", molTypeStr, types.ValidMolTypeNames())
		}
		in.MolType = &mt
	}

	if in.claim && in.Assignee != "" {
		return in, HandleErrorRespectJSON("--claim cannot be combined with --assignee")
	}
	if in.claim && in.gated {
		return in, HandleErrorRespectJSON("--claim cannot be combined with --gated")
	}
	if in.claim && in.molID != "" {
		return in, HandleErrorRespectJSON("--claim cannot be combined with --mol")
	}
	if in.claim && in.explain {
		return in, HandleErrorRespectJSON("--claim cannot be combined with --explain")
	}
	if in.Offset > 0 && in.claim {
		return in, HandleErrorRespectJSON("--offset cannot be combined with --claim")
	}
	if in.Offset > 0 && in.gated {
		return in, HandleErrorRespectJSON("--offset cannot be combined with --gated")
	}
	if in.Offset > 0 && in.molID != "" {
		return in, HandleErrorRespectJSON("--offset cannot be combined with --mol")
	}
	if in.Offset > 0 && in.explain {
		return in, HandleErrorRespectJSON("--offset cannot be combined with --explain")
	}

	// The label sets are normalized here as well as in BuildReadyFilter
	// (NormalizeLabels is idempotent) because the directory-aware default
	// below has to be decided against normalized sets — and that default is
	// derived from the client's cwd, so it cannot live in workapi. The CLI
	// resolves it and hands the result over as an ordinary parameter.
	in.Labels = utils.NormalizeLabels(in.Labels)
	in.LabelsAny = utils.NormalizeLabels(in.LabelsAny)
	in.ExcludeLabels = utils.NormalizeLabels(in.ExcludeLabels)
	if len(in.Labels) == 0 && len(in.LabelsAny) == 0 {
		if dirLabels := config.GetDirectoryLabels(); len(dirLabels) > 0 {
			in.LabelsAny = dirLabels // Directory-aware label scoping (GH#541)
		}
	}

	// Use Changed() to properly handle P0 (priority=0)
	if cmd.Flags().Changed("priority") {
		in.Priority, _ = cmd.Flags().GetInt("priority")
		in.PrioritySet = true
	}

	// Metadata filters (GH#1406)
	metadataFieldFlags, _ := cmd.Flags().GetStringArray("metadata-field")
	if len(metadataFieldFlags) > 0 {
		in.MetadataFields = make(map[string]string, len(metadataFieldFlags))
		for _, mf := range metadataFieldFlags {
			k, v, ok := strings.Cut(mf, "=")
			if !ok || k == "" {
				return in, HandleErrorRespectJSON("invalid --metadata-field: expected key=value, got %q", mf)
			}
			if err := storage.ValidateMetadataKey(k); err != nil {
				return in, HandleErrorRespectJSON("invalid --metadata-field key: %v", err)
			}
			in.MetadataFields[k] = v
		}
	}
	if k, _ := cmd.Flags().GetString("has-metadata-key"); k != "" {
		if err := storage.ValidateMetadataKey(k); err != nil {
			return in, HandleErrorRespectJSON("invalid --has-metadata-key: %v", err)
		}
		in.HasMetadataKey = k
	}

	filter, err := workapi.BuildReadyFilter(in.ReadyParams)
	if err != nil {
		return in, HandleErrorRespectJSON("%v", err)
	}
	in.filter = filter

	return in, nil
}
