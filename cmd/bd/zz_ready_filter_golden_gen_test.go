package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/utils"
	"github.com/steveyegge/beads/internal/workapi"
)

// TestGenerateReadyFilterGoldenFromOldBuilders records what BOTH pre-collapse
// ready-filter builders produced for a table of command lines, so the
// extracted workapi.BuildReadyFilter can be proven byte-identical against them
// and so the two builders' disagreements are on the record instead of being
// absorbed by the refactor.
//
// This generator is throwaway scaffolding for the bd-ehi collapse: it is run
// once, against the OLD builders, and deleted in the commit that removes the
// inline copy from ready.go. Its args -> ReadyParams mapping cannot launder a
// mistake into a false pass — the golden records the ReadyParams, so a field
// that is set in params but not on the command line (or vice versa) makes the
// replay test fail, not succeed.
//
//	BD_WRITE_READY_FILTER_GOLDEN=1 go test ./cmd/bd -run TestGenerateReadyFilterGoldenFromOldBuilders
func TestGenerateReadyFilterGoldenFromOldBuilders(t *testing.T) {
	if os.Getenv("BD_WRITE_READY_FILTER_GOLDEN") != "1" {
		t.Skip("set BD_WRITE_READY_FILTER_GOLDEN=1 to regenerate the ready-filter golden")
	}

	type builderResult struct {
		Filter json.RawMessage `json:"filter,omitempty"`
		Error  string          `json:"error,omitempty"`
		Stderr string          `json:"stderr,omitempty"`
	}
	type goldenCase struct {
		Name   string          `json:"name"`
		Args   []string        `json:"args"`
		Params json.RawMessage `json:"params"`
		Direct builderResult   `json:"direct"`
		Gather builderResult   `json:"gather"`
	}

	record := func(t *testing.T, name string, filter types.WorkFilter, err error, stderr string) builderResult {
		t.Helper()
		res := builderResult{Stderr: stderr}
		if err != nil {
			res.Error = err.Error()
			return res
		}
		blob, mErr := json.Marshal(filter)
		if mErr != nil {
			t.Fatalf("marshal %s filter for %q: %v", name, t.Name(), mErr)
		}
		res.Filter = blob
		return res
	}

	out := make([]goldenCase, 0, len(readyFilterGoldenCases))
	for _, c := range readyFilterGoldenCases {
		gc := goldenCase{Name: c.name, Args: c.args}

		directCmd := newReadyFlagsCommand(t)
		if err := directCmd.ParseFlags(c.args); err != nil {
			t.Fatalf("parse flags for %q: %v", c.name, err)
		}
		directFilter, directErr, directStderr := captureReadyBuilderStderr(t, func() (types.WorkFilter, error) {
			return legacyDirectReadyFilter(directCmd)
		})
		gc.Direct = record(t, "direct", directFilter, directErr, directStderr)

		gatherCmd := newReadyFlagsCommand(t)
		if err := gatherCmd.ParseFlags(c.args); err != nil {
			t.Fatalf("parse flags for %q: %v", c.name, err)
		}
		gatherFilter, gatherErr, gatherStderr := captureReadyBuilderStderr(t, func() (types.WorkFilter, error) {
			in, err := gatherReadyInput(gatherCmd)
			return in.filter, err
		})
		gc.Gather = record(t, "gather", gatherFilter, gatherErr, gatherStderr)

		// Params are inputs: pruning their zero-valued fields keeps the golden
		// readable and decodes back to the same struct.
		blob, err := marshalWithoutZeroReadyFields(c.params)
		if err != nil {
			t.Fatalf("marshal params for %q: %v", c.name, err)
		}
		gc.Params = blob

		out = append(out, gc)
	}

	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	blob = append(blob, '\n')

	path := filepath.Join("..", "..", "internal", "workapi", "testdata", "ready_filter_golden.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
	t.Logf("wrote %d cases to %s", len(out), path)
}

// captureReadyBuilderStderr runs fn with os.Stderr pointed at a temp file and returns what
// it printed. Both old builders report usage errors through HandleError, whose
// exitError carries no message, so the stderr text is the only record of what
// the user actually sees.
func captureReadyBuilderStderr(t *testing.T, fn func() (types.WorkFilter, error)) (types.WorkFilter, error, string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("create stderr capture: %v", err)
	}
	defer f.Close()

	orig := os.Stderr
	os.Stderr = f
	filter, fnErr := fn()
	os.Stderr = orig

	blob, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read stderr capture: %v", err)
	}
	return filter, fnErr, string(blob)
}

// newReadyFlagsCommand clones readyCmd's flag definitions onto a fresh command
// so each case parses from pristine defaults. Cloning beats re-declaring the
// flags here: a hand-copied default would be a second place for `bd ready` to
// drift, which is the very thing this extraction exists to stop.
func newReadyFlagsCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "ready"}
	readyCmd.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Value.Type() {
		case "bool":
			cmd.Flags().Bool(f.Name, f.DefValue == "true", f.Usage)
		case "int":
			n, err := strconv.Atoi(f.DefValue)
			if err != nil {
				t.Fatalf("--%s has non-integer default %q: %v", f.Name, f.DefValue, err)
			}
			cmd.Flags().Int(f.Name, n, f.Usage)
		case "string":
			cmd.Flags().String(f.Name, f.DefValue, f.Usage)
		case "stringSlice":
			if f.DefValue != "[]" {
				t.Fatalf("--%s has a non-empty slice default %q, which this clone does not reproduce", f.Name, f.DefValue)
			}
			cmd.Flags().StringSlice(f.Name, nil, f.Usage)
		case "stringArray":
			if f.DefValue != "[]" {
				t.Fatalf("--%s has a non-empty array default %q, which this clone does not reproduce", f.Name, f.DefValue)
			}
			cmd.Flags().StringArray(f.Name, nil, f.Usage)
		default:
			t.Fatalf("--%s has unhandled flag type %q", f.Name, f.Value.Type())
		}
	})
	return cmd
}

// marshalWithoutZeroReadyFields marshals v and drops every top-level key whose
// value is the zero value for its type. Decoding the result back into the
// original struct reproduces it exactly.
func marshalWithoutZeroReadyFields(v any) ([]byte, error) {
	blob, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(blob, &fields); err != nil {
		return nil, err
	}
	for k, val := range fields {
		switch t := val.(type) {
		case nil:
			delete(fields, k)
		case bool:
			if !t {
				delete(fields, k)
			}
		case string:
			if t == "" {
				delete(fields, k)
			}
		case float64:
			if t == 0 {
				delete(fields, k)
			}
		case []any:
			if len(t) == 0 {
				delete(fields, k)
			}
		case map[string]any:
			if len(t) == 0 {
				delete(fields, k)
			}
		}
	}
	return json.Marshal(fields)
}

// legacyDirectReadyFilter is cmd/bd/ready.go's inline filter construction
// (the direct, non-proxied path) lifted verbatim into a callable function so
// the golden can be recorded from it before it is deleted. The only edits are
// the ones the lift requires: the returns carry a WorkFilter, and the two
// presentation flags (--pretty, --plain) the surrounding RunE reads are left
// out because they never touch the filter.
func legacyDirectReadyFilter(cmd *cobra.Command) (types.WorkFilter, error) {
	claimReady, _ := cmd.Flags().GetBool("claim")
	labelPattern, _ := cmd.Flags().GetString("label-pattern")
	labelRegex, _ := cmd.Flags().GetString("label-regex")

	limit, _ := cmd.Flags().GetInt("limit")
	assignee, _ := cmd.Flags().GetString("assignee")
	unassigned, _ := cmd.Flags().GetBool("unassigned")
	sortPolicy, _ := cmd.Flags().GetString("sort")
	labels, _ := cmd.Flags().GetStringSlice("label")
	labelsAny, _ := cmd.Flags().GetStringSlice("label-any")
	excludeLabels, _ := cmd.Flags().GetStringSlice("exclude-label")
	issueType, _ := cmd.Flags().GetString("type")
	issueType = utils.NormalizeIssueType(issueType) // Expand aliases (mr→merge-request, etc.)
	parentID, _ := cmd.Flags().GetString("parent")
	molTypeStr, _ := cmd.Flags().GetString("mol-type")
	includeDeferred, _ := cmd.Flags().GetBool("include-deferred")
	includeEphemeral, _ := cmd.Flags().GetBool("include-ephemeral")
	excludeTypeStrs, _ := cmd.Flags().GetStringSlice("exclude-type")
	var molType *types.MolType
	if molTypeStr != "" {
		mt := types.MolType(molTypeStr)
		if !mt.IsValid() {
			return types.WorkFilter{}, HandleErrorRespectJSON("invalid mol-type %q (must be %s)", molTypeStr, types.ValidMolTypeNames())
		}
		molType = &mt
	}
	if claimReady && assignee != "" {
		return types.WorkFilter{}, HandleErrorRespectJSON("--claim cannot be combined with --assignee")
	}

	// Normalize labels: trim, dedupe, remove empty
	labels = utils.NormalizeLabels(labels)
	labelsAny = utils.NormalizeLabels(labelsAny)
	excludeLabels = utils.NormalizeLabels(excludeLabels)

	// Apply directory-aware label scoping if no labels explicitly provided (GH#541)
	if len(labels) == 0 && len(labelsAny) == 0 {
		if dirLabels := config.GetDirectoryLabels(); len(dirLabels) > 0 {
			labelsAny = dirLabels
		}
	}

	// Normalize --exclude-type values.
	var excludeTypes []types.IssueType
	for _, raw := range excludeTypeStrs {
		for _, t := range strings.Split(raw, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				excludeTypes = append(excludeTypes, types.IssueType(utils.NormalizeIssueType(t)))
			}
		}
	}
	maxRows, maxRowsSource, err := resolveMaxRows(cmd)
	if err != nil {
		return types.WorkFilter{}, err
	}
	filter := types.WorkFilter{
		Status:           "open", // Only show open issues, not in_progress (matches bd list --ready)
		Type:             issueType,
		Limit:            limit,
		Unassigned:       unassigned,
		SortPolicy:       types.SortPolicy(sortPolicy),
		Labels:           labels,
		LabelsAny:        labelsAny,
		ExcludeLabels:    excludeLabels,
		LabelPattern:     labelPattern,
		LabelRegex:       labelRegex,
		IncludeDeferred:  includeDeferred,  // GH#820: respect --include-deferred flag
		IncludeEphemeral: includeEphemeral, // bd-i5k5x: allow ephemeral issues (e.g., merge-requests)
		ExcludeTypes:     excludeTypes,
		MaxRows:          maxRows,
		MaxRowsSource:    maxRowsSource,
	}
	// Use Changed() to properly handle P0 (priority=0)
	if cmd.Flags().Changed("priority") {
		priority, _ := cmd.Flags().GetInt("priority")
		filter.Priority = &priority
	}
	if assignee != "" && !unassigned {
		filter.Assignee = &assignee
	}
	if parentID != "" {
		filter.ParentID = &parentID
	}
	if molType != nil {
		filter.MolType = molType
	}

	// Metadata filters (GH#1406)
	metadataFieldFlags, _ := cmd.Flags().GetStringArray("metadata-field")
	if len(metadataFieldFlags) > 0 {
		filter.MetadataFields = make(map[string]string, len(metadataFieldFlags))
		for _, mf := range metadataFieldFlags {
			k, v, ok := strings.Cut(mf, "=")
			if !ok || k == "" {
				return types.WorkFilter{}, HandleErrorRespectJSON("invalid --metadata-field: expected key=value, got %q", mf)
			}
			if err := storage.ValidateMetadataKey(k); err != nil {
				return types.WorkFilter{}, HandleErrorRespectJSON("invalid --metadata-field key: %v", err)
			}
			filter.MetadataFields[k] = v
		}
	}
	hasMetadataKey, _ := cmd.Flags().GetString("has-metadata-key")
	if hasMetadataKey != "" {
		if err := storage.ValidateMetadataKey(hasMetadataKey); err != nil {
			return types.WorkFilter{}, HandleErrorRespectJSON("invalid --has-metadata-key: %v", err)
		}
		filter.HasMetadataKey = hasMetadataKey
	}

	if !filter.SortPolicy.IsValid() {
		return types.WorkFilter{}, HandleErrorRespectJSON("invalid sort policy '%s'. Valid values: hybrid, priority, oldest", sortPolicy)
	}
	return filter, nil
}

// readyFilterGoldenCases pairs a `bd ready` command line with the ReadyParams
// a frontend is expected to hand workapi.BuildReadyFilter for it. Both old
// builders are run against the args; the replay test runs the new builder
// against the params.
var readyFilterGoldenCases = []struct {
	name   string
	args   []string
	params workapi.ReadyParams
}{
	{
		name:   "defaults",
		args:   nil,
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "type_plain",
		args:   []string{"--type", "bug"},
		params: workapi.ReadyParams{IssueType: "bug", Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "type_alias_mr",
		args:   []string{"--type", "mr"},
		params: workapi.ReadyParams{IssueType: "mr", Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "type_alias_uppercase",
		args:   []string{"--type", "FEAT"},
		params: workapi.ReadyParams{IssueType: "FEAT", Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "labels_all",
		args:   []string{"--label", "alpha", "--label", "beta"},
		params: workapi.ReadyParams{Labels: []string{"alpha", "beta"}, Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "labels_any",
		args:   []string{"--label-any", "alpha,beta"},
		params: workapi.ReadyParams{LabelsAny: []string{"alpha", "beta"}, Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "labels_need_normalizing",
		args:   []string{"--label", "  alpha  ", "--label", "alpha", "--label", "  "},
		params: workapi.ReadyParams{Labels: []string{"  alpha  ", "alpha", "  "}, Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "exclude_labels",
		args:   []string{"--exclude-label", "wip", "--exclude-label", "wip"},
		params: workapi.ReadyParams{ExcludeLabels: []string{"wip", "wip"}, Limit: 100, SortPolicy: "priority"},
	},
	{
		name: "label_pattern_and_regex",
		args: []string{"--label-pattern", "tech-*", "--label-regex", "^tech-(debt|legacy)$"},
		params: workapi.ReadyParams{
			LabelPattern: "tech-*", LabelRegex: "^tech-(debt|legacy)$",
			Limit: 100, SortPolicy: "priority",
		},
	},
	{
		name:   "limit_zero_unlimited",
		args:   []string{"--limit", "0"},
		params: workapi.ReadyParams{SortPolicy: "priority"},
	},
	{
		name:   "limit_custom",
		args:   []string{"--limit", "7"},
		params: workapi.ReadyParams{Limit: 7, SortPolicy: "priority"},
	},
	{
		name:   "offset_positive",
		args:   []string{"--offset", "3"},
		params: workapi.ReadyParams{Limit: 100, Offset: 3, SortPolicy: "priority"},
	},
	{
		name:   "offset_zero_explicit",
		args:   []string{"--offset", "0"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "offset_negative",
		args:   []string{"--offset", "-1"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "assignee",
		args:   []string{"--assignee", "alice"},
		params: workapi.ReadyParams{Assignee: "alice", Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "unassigned",
		args:   []string{"--unassigned"},
		params: workapi.ReadyParams{Unassigned: true, Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "assignee_loses_to_unassigned",
		args:   []string{"--assignee", "alice", "--unassigned"},
		params: workapi.ReadyParams{Assignee: "alice", Unassigned: true, Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "priority_zero_is_set",
		args:   []string{"--priority", "0"},
		params: workapi.ReadyParams{Priority: 0, PrioritySet: true, Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "priority_two",
		args:   []string{"--priority", "2"},
		params: workapi.ReadyParams{Priority: 2, PrioritySet: true, Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "sort_hybrid",
		args:   []string{"--sort", "hybrid"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "hybrid"},
	},
	{
		name:   "sort_oldest",
		args:   []string{"--sort", "oldest"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "oldest"},
	},
	{
		name:   "sort_invalid",
		args:   []string{"--sort", "bogus"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "bogus"},
	},
	{
		name:   "parent",
		args:   []string{"--parent", "bd-epic"},
		params: workapi.ReadyParams{ParentID: "bd-epic", Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "mol_type_swarm",
		args:   []string{"--mol-type", "swarm"},
		params: workapi.ReadyParams{MolType: molTypePtr(types.MolTypeSwarm), Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "mol_type_invalid",
		args:   []string{"--mol-type", "bogus"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "include_deferred",
		args:   []string{"--include-deferred"},
		params: workapi.ReadyParams{IncludeDeferred: true, Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "include_ephemeral",
		args:   []string{"--include-ephemeral"},
		params: workapi.ReadyParams{IncludeEphemeral: true, Limit: 100, SortPolicy: "priority"},
	},
	{
		// pflag splits a StringSlice on commas before the builder ever sees
		// it, so the params carry the already-split (and still untrimmed)
		// segments. The builder's own comma split matters for a frontend that
		// does not pre-split; see TestBuildReadyFilterNormalizes.
		name:   "exclude_type_csv_and_repeat",
		args:   []string{"--exclude-type", "convoy, epic", "--exclude-type", "mr"},
		params: workapi.ReadyParams{ExcludeTypeStrs: []string{"convoy", " epic", "mr"}, Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "exclude_type_empty_segments",
		args:   []string{"--exclude-type", "epic,,  ,task"},
		params: workapi.ReadyParams{ExcludeTypeStrs: []string{"epic", "", "  ", "task"}, Limit: 100, SortPolicy: "priority"},
	},
	{
		name: "metadata_fields",
		args: []string{"--metadata-field", "team=platform", "--metadata-field", "env=prod"},
		params: workapi.ReadyParams{
			MetadataFields: map[string]string{"team": "platform", "env": "prod"},
			Limit:          100, SortPolicy: "priority",
		},
	},
	{
		name:   "metadata_field_missing_equals",
		args:   []string{"--metadata-field", "team"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "metadata_field_invalid_key",
		args:   []string{"--metadata-field", "bad$key=x"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "has_metadata_key",
		args:   []string{"--has-metadata-key", "team"},
		params: workapi.ReadyParams{HasMetadataKey: "team", Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "has_metadata_key_invalid",
		args:   []string{"--has-metadata-key", "bad$key"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "max_rows_flag",
		args:   []string{"--max-rows", "5"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "max_rows_negative",
		args:   []string{"--max-rows", "-1"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "claim_with_assignee",
		args:   []string{"--claim", "--assignee", "alice"},
		params: workapi.ReadyParams{Assignee: "alice", Limit: 100, SortPolicy: "priority"},
	},
	{
		name:   "claim_with_gated",
		args:   []string{"--claim", "--gated"},
		params: workapi.ReadyParams{Limit: 100, SortPolicy: "priority"},
	},
	{
		name: "everything_together",
		args: []string{
			"--type", "mr", "--limit", "5", "--offset", "2", "--priority", "1",
			"--assignee", "alice", "--sort", "oldest",
			"--label", "a", "--label-any", "b", "--exclude-label", "c",
			"--label-pattern", "p-*", "--label-regex", "^p-.*$",
			"--parent", "bd-root", "--mol-type", "work",
			"--include-deferred", "--include-ephemeral",
			"--exclude-type", "epic,convoy",
			"--metadata-field", "team=platform", "--has-metadata-key", "env",
		},
		params: workapi.ReadyParams{
			IssueType: "mr", Limit: 5, Offset: 2,
			Priority: 1, PrioritySet: true,
			Assignee: "alice", SortPolicy: "oldest",
			Labels: []string{"a"}, LabelsAny: []string{"b"}, ExcludeLabels: []string{"c"},
			LabelPattern: "p-*", LabelRegex: "^p-.*$",
			ParentID: "bd-root", MolType: molTypePtr(types.MolTypeWork),
			IncludeDeferred: true, IncludeEphemeral: true,
			ExcludeTypeStrs: []string{"epic", "convoy"},
			MetadataFields:  map[string]string{"team": "platform"},
			HasMetadataKey:  "env",
		},
	},
}

func molTypePtr(mt types.MolType) *types.MolType { return &mt }
