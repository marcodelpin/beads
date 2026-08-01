package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// TestMatchGatesToRuns_ScopesPerGateRepo is the core SF1 regression test:
// queryGitHubRuns previously built `gh run list` without --repo, and the
// discovery pass loaded every pending gh:run gate and matched those
// current-repo runs into all of them - a gate targeting another repository
// could be assigned a same-named workflow run from the current repo and
// become permanently undiscoverable (the persisted await_id pins it).
//
// matchGatesToRuns must instead query and match each gate only against runs
// from ITS OWN repo.
func TestMatchGatesToRuns_ScopesPerGateRepo(t *testing.T) {
	currentRepoGate := &types.Issue{
		ID:        "bd-local",
		AwaitType: "gh:run",
		AwaitID:   "release.yml",
		CreatedAt: time.Now(),
	}
	crossRepoGate := &types.Issue{
		ID:        "bd-cross",
		AwaitType: "gh:run",
		AwaitID:   "release.yml",
		CreatedAt: time.Now(),
		Metadata:  json.RawMessage(`{"repo":"other-owner/other-repo"}`),
	}

	queryCalls := map[string]int{}
	queryRuns := func(repo string) ([]GHWorkflowRun, error) {
		queryCalls[repo]++
		switch repo {
		case "":
			return []GHWorkflowRun{{DatabaseID: 111, Name: "release", WorkflowName: "release.yml", Status: "in_progress", CreatedAt: time.Now()}}, nil
		case "other-owner/other-repo":
			return []GHWorkflowRun{{DatabaseID: 222, Name: "release", WorkflowName: "release.yml", Status: "in_progress", CreatedAt: time.Now()}}, nil
		default:
			t.Fatalf("unexpected repo queried: %q", repo)
			return nil, nil
		}
	}

	results := matchGatesToRuns([]*types.Issue{currentRepoGate, crossRepoGate}, 30*time.Minute, queryRuns)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	for _, r := range results {
		if r.err != nil {
			t.Fatalf("gate %s: unexpected error: %v", r.gate.ID, r.err)
		}
		if r.run == nil {
			t.Fatalf("gate %s: expected a matched run", r.gate.ID)
		}
		switch r.gate.ID {
		case "bd-local":
			if r.run.DatabaseID != 111 {
				t.Errorf("bd-local matched run %d, want 111 (current repo's run)", r.run.DatabaseID)
			}
		case "bd-cross":
			if r.run.DatabaseID != 222 {
				t.Errorf("bd-cross matched run %d, want 222 (other-owner/other-repo's run) - a cross-repo gate must never be assigned the current repo's run", r.run.DatabaseID)
			}
		}
	}

	if queryCalls[""] != 1 {
		t.Errorf("queried current repo %d times, want 1 (per-repo caching)", queryCalls[""])
	}
	if queryCalls["other-owner/other-repo"] != 1 {
		t.Errorf("queried other-owner/other-repo %d times, want 1", queryCalls["other-owner/other-repo"])
	}
}

// TestMatchGatesToRuns_RejectsInvalidRepoMetadata covers the SF1/SF3
// interaction: a gate with malformed repo metadata must be reported as an
// error, never silently matched against the current repo's runs.
func TestMatchGatesToRuns_RejectsInvalidRepoMetadata(t *testing.T) {
	gate := &types.Issue{
		ID:        "bd-bad-repo",
		AwaitType: "gh:run",
		AwaitID:   "release.yml",
		CreatedAt: time.Now(),
		Metadata:  json.RawMessage(`{"repo":null}`),
	}

	queried := false
	queryRuns := func(repo string) ([]GHWorkflowRun, error) {
		queried = true
		return nil, nil
	}

	results := matchGatesToRuns([]*types.Issue{gate}, 30*time.Minute, queryRuns)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].err == nil {
		t.Fatal("expected an error for malformed repo metadata, got nil (silent fallback)")
	}
	if results[0].run != nil {
		t.Fatalf("expected no matched run for malformed repo metadata, got %+v", results[0].run)
	}
	if queried {
		t.Error("expected no GitHub query for a gate with malformed repo metadata")
	}
}

// TestMatchGatesToRuns_CachesQueryErrorPerRepo verifies a failing query for a
// repo is attempted once and its error applied to every gate sharing that
// repo, rather than re-querying (or silently matching against an empty run
// list) per gate.
func TestMatchGatesToRuns_CachesQueryErrorPerRepo(t *testing.T) {
	gateA := &types.Issue{ID: "bd-a", AwaitType: "gh:run", AwaitID: "release.yml", CreatedAt: time.Now()}
	gateB := &types.Issue{ID: "bd-b", AwaitType: "gh:run", AwaitID: "release.yml", CreatedAt: time.Now()}

	queryCalls := 0
	wantErr := errors.New("gh run list failed: rate limited")
	queryRuns := func(repo string) ([]GHWorkflowRun, error) {
		queryCalls++
		return nil, wantErr
	}

	results := matchGatesToRuns([]*types.Issue{gateA, gateB}, 30*time.Minute, queryRuns)
	if queryCalls != 1 {
		t.Errorf("queryRuns called %d times, want 1 (cached per repo)", queryCalls)
	}
	for _, r := range results {
		if !errors.Is(r.err, wantErr) {
			t.Errorf("gate %s: err = %v, want %v", r.gate.ID, r.err, wantErr)
		}
	}
}

func TestQueryGitHubRunsInRepoWithRunner(t *testing.T) {
	runs, err := queryGitHubRunsInRepoWithRunner(
		"main",
		10,
		"srobroek/agentic-packages",
		fakeGHRunner(t,
			`[{"databaseId":555,"name":"release","status":"completed","conclusion":"success","workflowName":"release.yml"}]`,
			"run", "list", "--json", "databaseId,displayTitle,headBranch,headSha,name,status,conclusion,createdAt,updatedAt,workflowName,url", "--limit", "10", "--branch", "main", "--repo", "srobroek/agentic-packages",
		),
	)
	if err != nil {
		t.Fatalf("queryGitHubRunsInRepoWithRunner returned error: %v", err)
	}
	if len(runs) != 1 || runs[0].DatabaseID != 555 {
		t.Fatalf("runs = %#v, want one run with database ID 555", runs)
	}
}

func TestQueryGitHubRunsInRepoWithRunner_OmitsRepoFlagForCurrentRepo(t *testing.T) {
	_, err := queryGitHubRunsInRepoWithRunner(
		"",
		10,
		"",
		fakeGHRunner(t,
			`[]`,
			"run", "list", "--json", "databaseId,displayTitle,headBranch,headSha,name,status,conclusion,createdAt,updatedAt,workflowName,url", "--limit", "10",
		),
	)
	if err != nil {
		t.Fatalf("queryGitHubRunsInRepoWithRunner returned error: %v", err)
	}
}
