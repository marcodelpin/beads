package issueops

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// CloneCreateBatchRequest returns a deep copy of request, so a body can
// normalize an attempt without writing through to the caller's items — and, in
// particular, without leaving the ID it assigned on the caller's issue.
func CloneCreateBatchRequest(request publicops.CreateBatchRequest) publicops.CreateBatchRequest {
	clone := request
	clone.Items = make([]publicops.BatchCreateItem, len(request.Items))
	for i, item := range request.Items {
		clone.Items[i] = publicops.BatchCreateItem{
			Issue:        clonePublicIssue(item.Issue),
			Dependencies: append([]publicops.CreateDependency(nil), item.Dependencies...),
		}
	}
	return clone
}

// ValidateCreateBatchRequest applies the request rules every BatchCreator
// implementation shares. It runs BEFORE any transaction opens: everything it
// checks is knowable from the request alone, so a batch refused here has
// provably written nothing.
//
// Per-item CONTENT rules are not here. They need the workspace's configured
// prefix, statuses and types, so they run inside the transaction through the
// same PreparePublicCreateRequest a single create runs.
func ValidateCreateBatchRequest(request publicops.CreateBatchRequest) error {
	if request.Actor == "" {
		return fmt.Errorf("%w: create batch requires an actor", storage.ErrValidation)
	}
	if len(request.Items) == 0 {
		return fmt.Errorf("%w: create batch requires at least one item", storage.ErrValidation)
	}
	for i, item := range request.Items {
		if item.Issue == nil {
			return fmt.Errorf("%w: create batch item %d requires an issue", storage.ErrValidation, i)
		}
	}
	return nil
}

// CreateBatchItemRequest projects one item onto the single-create request the
// shared preparation and validation speak. Both front doors and both bodies read
// an item through it, so no item restates CreateRequest's field rules.
func CreateBatchItemRequest(request publicops.CreateBatchRequest, item publicops.BatchCreateItem) publicops.CreateRequest {
	return publicops.CreateRequest{
		Actor:         request.Actor,
		Issue:         item.Issue,
		Dependencies:  item.Dependencies,
		ForceIDPrefix: request.ForceIDPrefix,
	}
}

// CreateBatchItemError names the item a batch refusal came from. The role
// promises the index appears in the message and nowhere else.
func CreateBatchItemError(index int, err error) error {
	return fmt.Errorf("create batch item %d: %w", index, err)
}

// CreateBatchCommitMessage is the history entry a batch records: the caller's
// own label when it supplied one, otherwise a default naming how much landed.
//
// IT NAMES A COUNT AND NEVER AN ID: a create batch's ids are new, there can be
// hundreds from one file, and an entry naming them all is the diff written twice.
//
// An all-ephemeral batch writes only to the dolt-ignored wisp tables, so the
// store-backed bodies stage nothing and record no entry whatever this returns —
// but the unit-of-work backend reads "" as "roll this attempt back", so a
// wisp-only batch must still hand it a message or the wisps it created are
// discarded. That is the same trap CloseBatchCommitMessage documents.
func CreateBatchCommitMessage(request publicops.CreateBatchRequest, result publicops.CreateBatchResult) string {
	durable, ephemeral := 0, 0
	for _, issue := range result.Issues {
		if IsWisp(issue) {
			ephemeral++
			continue
		}
		durable++
	}
	var fallback string
	switch {
	case durable > 0:
		fallback = fmt.Sprintf("bd: create %d issue(s)", durable)
	case ephemeral == 1:
		fallback = "bd: create 1 ephemeral item"
	case ephemeral > 1:
		fallback = fmt.Sprintf("bd: create %d ephemeral items", ephemeral)
	default:
		return ""
	}
	return HistoryEntry(request.Provenance, fallback)
}

// ExecuteCreateBatch creates every item in tx and reports the durable tables
// changed. It is the store-backed body behind the BatchCreator accessor.
//
// THE ITEMS ARE PREPARED FIRST AND WRITTEN ONCE. Each goes through the same
// PreparePublicCreateRequest a single create goes through, and each is assigned
// its id before any row is written, which is what lets an item's edge name an
// item created earlier in the same batch. The rows and their edges then land
// through ONE CreateIssuesInTxWithResult.
//
// ANY error returns, and the enclosing transaction rolls the whole batch back.
// That includes an edge the engine declined to write: the batch engine drops a
// dangling edge so a partial import can still land, and this role refuses.
func ExecuteCreateBatch(ctx context.Context, tx *sql.Tx, request publicops.CreateBatchRequest) (publicops.CreateBatchResult, ChangedTables, error) {
	attempt := CloneCreateBatchRequest(request)
	if err := ValidateCreateBatchRequest(attempt); err != nil {
		return publicops.CreateBatchResult{}, nil, err
	}
	options := storage.BatchCreateOptions{
		CreateOnly:           true,
		OrphanHandling:       storage.OrphanAllow,
		SkipPrefixValidation: attempt.ForceIDPrefix,
	}
	batch, err := NewBatchContext(ctx, tx, options)
	if err != nil {
		return publicops.CreateBatchResult{}, nil, err
	}
	createContext := PublicCreateContext{
		IssuePrefix:     batch.ConfigPrefix,
		AllowedPrefixes: batch.AllowedPrefixes,
		CustomStatuses:  batch.CustomStatuses,
		CustomTypes:     batch.CustomTypes,
	}
	infraTypes := ResolveInfraTypesInTx(ctx, tx)

	issues := make([]*types.Issue, len(attempt.Items))
	for i, item := range attempt.Items {
		prepared, err := PreparePublicCreateRequest(CreateBatchItemRequest(attempt, item), createContext)
		if err != nil {
			return publicops.CreateBatchResult{}, nil, CreateBatchItemError(i, err)
		}
		issue := prepared.Issue
		// Configured infra types live in the wisp tables, the same routing
		// ExecuteCreate applies. Mark the issue BEFORE its ID is assigned so ID
		// generation, the create-only guard and table routing all agree.
		if !issue.Ephemeral && !issue.NoHistory && infraTypes[string(issue.IssueType)] {
			issue.Ephemeral = true
		}
		if err := assignCreateIssueIDInTx(ctx, tx, batch, issue, attempt.Actor); err != nil {
			return publicops.CreateBatchResult{}, nil, CreateBatchItemError(i, ClassifyPublicCreateError(err))
		}
		issue.Dependencies = storage.CreatePublicCreateDependencies(issue.ID, prepared)
		issues[i] = issue
	}

	var skipped []skippedDependency
	options.OnSkippedDependency = func(issueID, dependsOnID, reason string) {
		skipped = append(skipped, skippedDependency{issueID: issueID, dependsOnID: dependsOnID, reason: reason})
	}
	created, err := CreateIssuesInTxWithResult(ctx, tx, issues, attempt.Actor, options)
	if err != nil {
		return publicops.CreateBatchResult{}, nil, ClassifyPublicCreateError(err)
	}
	if len(skipped) > 0 {
		return publicops.CreateBatchResult{}, nil, publicCreateValidationError(skippedDependencyError(skipped))
	}

	// CreateIssuesDirtyTables reports the child_counters advance itself, from
	// the reconciliation the batch engine already ran, so there is no separate
	// counter bookkeeping here.
	tables := ChangedTables{}
	tables.Merge(CreateIssuesDirtyTables(ctx, issues, created))
	result := publicops.CreateBatchResult{Issues: make([]*types.Issue, len(issues))}
	for i, issue := range issues {
		hydrated, err := HydrateIssueOperationResult(ctx, tx, issue.ID, false)
		if err != nil {
			return publicops.CreateBatchResult{}, nil, err
		}
		result.Issues[i] = hydrated
	}
	return result, tables, nil
}
