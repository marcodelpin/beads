package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
)

// TestGenerateListFilterGoldenFromOldBuilder records what the pre-extraction
// buildListFilter produced for a table of inputs, so the extracted
// workapi.BuildListFilter can be proven byte-identical against it.
//
// This generator is throwaway scaffolding for the bd-fv4 extraction: it is run
// once, against the OLD builder, and deleted in the commit that removes
// cmd/bd/list_filter.go. Its params->listInput conversion cannot launder a
// mistake into a false pass — the golden records the ListParams, so a
// mis-mapped field here makes the replay test fail, not succeed.
//
//	BD_WRITE_LIST_FILTER_GOLDEN=1 go test ./cmd/bd -run TestGenerateListFilterGoldenFromOldBuilder
func TestGenerateListFilterGoldenFromOldBuilder(t *testing.T) {
	if os.Getenv("BD_WRITE_LIST_FILTER_GOLDEN") != "1" {
		t.Skip("set BD_WRITE_LIST_FILTER_GOLDEN=1 to regenerate the list-filter golden")
	}

	type goldenCase struct {
		Name   string          `json:"name"`
		Params json.RawMessage `json:"params"`
		Config json.RawMessage `json:"config"`
		Filter json.RawMessage `json:"filter,omitempty"`
		Error  string          `json:"error,omitempty"`
	}

	out := make([]goldenCase, 0, len(listFilterGoldenCases))
	for _, c := range listFilterGoldenCases {
		filter, err := buildListFilter(oldListInput(c.params), oldListFilterConfig(c.config))
		gc := goldenCase{Name: c.name}
		if err != nil {
			gc.Error = err.Error()
		}
		for _, m := range []struct {
			dst  *json.RawMessage
			src  any
			what string
		}{
			{&gc.Params, c.params, "params"},
			{&gc.Config, c.config, "config"},
		} {
			// Params and config are inputs: pruning their zero-valued fields
			// keeps the golden readable and decodes back to the same struct.
			blob, mErr := marshalWithoutZeroFields(m.src)
			if mErr != nil {
				t.Fatalf("marshal %s for %q: %v", m.what, c.name, mErr)
			}
			*m.dst = blob
		}
		// The filter is the oracle, so it is recorded in full and compared
		// byte-for-byte - no pruning helper the replay side has to mirror.
		blob, mErr := json.Marshal(filter)
		if mErr != nil {
			t.Fatalf("marshal filter for %q: %v", c.name, mErr)
		}
		gc.Filter = blob
		out = append(out, gc)
	}

	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	blob = append(blob, '\n')

	path := filepath.Join("..", "..", "internal", "workapi", "testdata", "list_filter_golden.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
	t.Logf("wrote %d cases to %s", len(out), path)
}

// marshalWithoutZeroFields marshals v and drops every top-level key whose
// value is the zero value for its type. Decoding the result back into the
// original struct reproduces it exactly.
func marshalWithoutZeroFields(v any) ([]byte, error) {
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

// oldListFilterConfig rebuilds the pre-extraction config struct.
func oldListFilterConfig(c workapi.ListConfig) listFilterConfig {
	return listFilterConfig{
		customStatuses: c.CustomStatuses,
		customTypes:    c.CustomTypes,
		infraSet:       c.InfraSet,
	}
}

// oldListInput rebuilds the pre-extraction input struct.
func oldListInput(p workapi.ListParams) listInput {
	return listInput{
		status:      p.Status,
		issueType:   p.IssueType,
		assignee:    p.Assignee,
		titleSearch: p.TitleSearch,
		specPrefix:  p.SpecPrefix,
		idFilter:    p.IDFilter,

		labels:        p.Labels,
		labelsAny:     p.LabelsAny,
		excludeLabels: p.ExcludeLabels,
		labelPattern:  p.LabelPattern,
		labelRegex:    p.LabelRegex,

		titleContains:    p.TitleContains,
		descContains:     p.DescContains,
		notesContains:    p.NotesContains,
		externalContains: p.ExternalContains,
		externalRef:      p.ExternalRef,

		createdBefore: p.CreatedBefore,
		createdAfter:  p.CreatedAfter,
		updatedAfter:  p.UpdatedAfter,
		updatedBefore: p.UpdatedBefore,
		closedAfter:   p.ClosedAfter,
		closedBefore:  p.ClosedBefore,
		deferAfter:    p.DeferAfter,
		deferBefore:   p.DeferBefore,
		dueAfter:      p.DueAfter,
		dueBefore:     p.DueBefore,

		emptyDesc:  p.EmptyDesc,
		noAssignee: p.NoAssignee,
		noLabels:   p.NoLabels,
		skipLabels: p.SkipLabels,

		priority:       p.Priority,
		prioritySet:    p.PrioritySet,
		priorityMin:    p.PriorityMin,
		priorityMinSet: p.PriorityMinSet,
		priorityMax:    p.PriorityMax,
		priorityMaxSet: p.PriorityMaxSet,

		pinnedFlag:       p.PinnedFlag,
		noPinnedFlag:     p.NoPinnedFlag,
		includeTemplates: p.IncludeTemplates,
		includeGates:     p.IncludeGates,
		includeInfra:     p.IncludeInfra,
		excludeTypeStrs:  p.ExcludeTypeStrs,

		parentID: p.ParentID,
		noParent: p.NoParent,
		molType:  p.MolType,
		wispType: p.WispType,

		deferredFlag: p.DeferredFlag,
		overdueFlag:  p.OverdueFlag,

		metadataFields: p.MetadataFields,
		hasMetadataKey: p.HasMetadataKey,

		allFlag:   p.AllFlag,
		readyFlag: p.ReadyFlag,

		sortBy:  p.SortBy,
		reverse: p.Reverse,

		sqlLimit: p.SQLLimit,
		offset:   p.Offset,
	}
}

func goldenTime(day int) *time.Time {
	t := time.Date(2024, time.March, day, 4, 5, 6, 0, time.UTC)
	return &t
}

var listFilterGoldenCases = []struct {
	name   string
	params workapi.ListParams
	config workapi.ListConfig
}{
	{
		name:   "defaults",
		params: workapi.ListParams{SQLLimit: workapi.DefaultListLimit},
	},
	{
		name:   "all_flag",
		params: workapi.ListParams{AllFlag: true},
	},
	{
		name:   "ready_flag",
		params: workapi.ListParams{ReadyFlag: true, SQLLimit: 20},
	},
	{
		name:   "status_single",
		params: workapi.ListParams{Status: " in_progress "},
	},
	{
		name:   "status_multi",
		params: workapi.ListParams{Status: "open, blocked"},
	},
	{
		name:   "status_all_keyword",
		params: workapi.ListParams{Status: "all"},
	},
	{
		name:   "status_pinned_keyword",
		params: workapi.ListParams{Status: "pinned"},
	},
	{
		name:   "status_hooked_keyword",
		params: workapi.ListParams{Status: "hooked"},
	},
	{
		name:   "status_invalid",
		params: workapi.ListParams{Status: "not-a-status"},
	},
	{
		name:   "status_custom",
		params: workapi.ListParams{Status: "in_review"},
		config: workapi.ListConfig{CustomStatuses: []types.CustomStatus{{Name: "in_review", Category: types.CategoryActive}}},
	},
	{
		name:   "custom_done_and_frozen_excluded_by_default",
		params: workapi.ListParams{},
		config: workapi.ListConfig{CustomStatuses: []types.CustomStatus{
			{Name: "in_review", Category: types.CategoryActive},
			{Name: "shipped", Category: types.CategoryDone},
			{Name: "on_ice", Category: types.CategoryFrozen},
		}},
	},
	{
		name:   "pinned_flag",
		params: workapi.ListParams{PinnedFlag: true},
	},
	{
		name:   "no_pinned_flag_with_all",
		params: workapi.ListParams{NoPinnedFlag: true, AllFlag: true},
	},
	{
		name:   "no_pinned_flag",
		params: workapi.ListParams{NoPinnedFlag: true},
	},
	{
		name:   "ready_flag_overrides_status",
		params: workapi.ListParams{ReadyFlag: true, Status: "closed"},
	},
	{
		name:   "status_with_all_flag",
		params: workapi.ListParams{Status: "closed", AllFlag: true},
	},
	{
		name:   "include_templates_and_gates",
		params: workapi.ListParams{IncludeTemplates: true, IncludeGates: true},
	},
	{
		name:   "type_gate",
		params: workapi.ListParams{IssueType: "gate"},
	},
	{
		name:   "type_invalid",
		params: workapi.ListParams{IssueType: "banana"},
		config: workapi.ListConfig{CustomTypes: []string{"spike", "saga"}},
	},
	{
		name:   "type_custom_valid",
		params: workapi.ListParams{IssueType: "spike"},
		config: workapi.ListConfig{CustomTypes: []string{"spike", "saga"}},
	},
	{
		name:   "include_infra",
		params: workapi.ListParams{IncludeInfra: true},
	},
	{
		name:   "include_infra_with_infra_type",
		params: workapi.ListParams{IncludeInfra: true, IssueType: "message"},
	},
	{
		name:   "infra_type_default_set",
		params: workapi.ListParams{IssueType: "message"},
	},
	{
		name:   "infra_type_not_valid_without_custom_types",
		params: workapi.ListParams{IssueType: "agent"},
	},
	{
		name:   "infra_type_valid_via_custom_types",
		params: workapi.ListParams{IssueType: "agent"},
		config: workapi.ListConfig{CustomTypes: []string{"agent"}},
	},
	{
		name:   "infra_type_custom_set",
		params: workapi.ListParams{IssueType: "robot"},
		config: workapi.ListConfig{CustomTypes: []string{"robot"}, InfraSet: map[string]bool{"robot": true}},
	},
	{
		name:   "non_infra_type_under_custom_set",
		params: workapi.ListParams{IssueType: "message"},
		config: workapi.ListConfig{InfraSet: map[string]bool{"robot": true}},
	},
	{
		name:   "exclude_type_strings",
		params: workapi.ListParams{ExcludeTypeStrs: []string{"bug, Feature", "", " chore "}},
	},
	{
		name: "labels_and_text",
		params: workapi.ListParams{
			Labels:           []string{"alpha", "beta"},
			LabelsAny:        []string{"gamma"},
			ExcludeLabels:    []string{"delta"},
			LabelPattern:     "tech-*",
			LabelRegex:       "tech-(debt|legacy)",
			TitleSearch:      "search me",
			TitleContains:    "contains",
			DescContains:     "desc",
			NotesContains:    "notes",
			ExternalContains: "ext",
			ExternalRef:      "GH#1",
			SpecPrefix:       "spec-",
			IDFilter:         " bd-1 , BD-2 ,, bd-3 ",
		},
	},
	{
		name: "empties_and_negations",
		params: workapi.ListParams{
			EmptyDesc:  true,
			NoAssignee: true,
			NoLabels:   true,
			SkipLabels: true,
			NoParent:   true,
		},
	},
	{
		name: "priority_ranges",
		params: workapi.ListParams{
			Priority: 1, PrioritySet: true,
			PriorityMin: 0, PriorityMinSet: true,
			PriorityMax: 3, PriorityMaxSet: true,
		},
	},
	{
		name: "time_ranges",
		params: workapi.ListParams{
			CreatedAfter:  goldenTime(1),
			CreatedBefore: goldenTime(2),
			UpdatedAfter:  goldenTime(3),
			UpdatedBefore: goldenTime(4),
			ClosedAfter:   goldenTime(5),
			ClosedBefore:  goldenTime(6),
			DeferAfter:    goldenTime(7),
			DeferBefore:   goldenTime(8),
			DueAfter:      goldenTime(9),
			DueBefore:     goldenTime(10),
			DeferredFlag:  true,
			OverdueFlag:   true,
		},
	},
	{
		name: "mol_and_wisp_types",
		params: workapi.ListParams{
			MolType:  molTypePtr(types.MolTypeSwarm),
			WispType: wispTypePtr(types.WispTypeHeartbeat),
		},
	},
	{
		name: "metadata_and_parent",
		params: workapi.ListParams{
			MetadataFields: map[string]string{"team": "core"},
			HasMetadataKey: "owner",
			ParentID:       "bd-parent",
			Assignee:       "alice",
		},
	},
	{
		name: "sort_and_paging",
		params: workapi.ListParams{
			SortBy:   "created",
			Reverse:  true,
			SQLLimit: 7,
			Offset:   14,
		},
	},
}

func molTypePtr(v types.MolType) *types.MolType    { return &v }
func wispTypePtr(v types.WispType) *types.WispType { return &v }
