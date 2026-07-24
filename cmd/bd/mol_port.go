package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/utils"
)

type molReader interface {
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
	GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error)
	GetIssuesByLabel(ctx context.Context, label string) ([]*types.Issue, error)
	GetDependents(ctx context.Context, issueID string) ([]*types.Issue, error)
	GetDependentsWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error)
	GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error)
	GetDependencyRecordsForIssues(ctx context.Context, issueIDs []string) (map[string][]*types.Dependency, error)
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
	GetReadyWork(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error)
	GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error)
	GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error)
	GetLabels(ctx context.Context, issueID string) ([]string, error)
	GetConfig(ctx context.Context, key string) (string, error)
	IsInfraTypeCtx(ctx context.Context, t types.IssueType) bool
	GetCustomStatusesDetailed(ctx context.Context) ([]types.CustomStatus, error)
	GetMoleculeProgress(ctx context.Context, moleculeID string) (*types.MoleculeProgressStats, error)
	GetMoleculeLastActivity(ctx context.Context, moleculeID string) (*types.MoleculeLastActivity, error)
	FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error)
}

var _ molReader = storage.DoltStorage(nil)

type molConfigWriter interface {
	molReader
	SetConfig(ctx context.Context, key, value string) error
}

var _ molConfigWriter = storage.DoltStorage(nil)

type molWriter interface {
	molReader
	CreateIssue(ctx context.Context, issue *types.Issue, actor string) error
	AddDependency(ctx context.Context, dep *types.Dependency, actor string) error
	AddLabel(ctx context.Context, issueID, label, actor string) error
	UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error
	CloseIssue(ctx context.Context, id, reason, actor string) error
	DeleteIssue(ctx context.Context, id, actor string) error
	SetConfig(ctx context.Context, key, value string) error
	ClaimStepIfOpen(ctx context.Context, id, actor string) error
}

type storeMolWriter struct {
	storage.DoltStorage
	tx storage.Transaction
}

func (w storeMolWriter) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	return w.tx.CreateIssue(ctx, issue, actor)
}

func (w storeMolWriter) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return w.tx.AddDependency(ctx, dep, actor)
}

func (w storeMolWriter) AddLabel(ctx context.Context, issueID, label, actor string) error {
	return w.tx.AddLabel(ctx, issueID, label, actor)
}

func (w storeMolWriter) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return w.tx.UpdateIssue(ctx, id, updates, actor)
}

func (w storeMolWriter) CloseIssue(ctx context.Context, id, reason, actor string) error {
	return w.tx.CloseIssue(ctx, id, reason, actor, "")
}

func (w storeMolWriter) DeleteIssue(ctx context.Context, id, _ string) error {
	return w.tx.DeleteIssue(ctx, id)
}

func (w storeMolWriter) SetConfig(ctx context.Context, key, value string) error {
	return w.tx.SetConfig(ctx, key, value)
}

func (w storeMolWriter) ClaimStepIfOpen(ctx context.Context, id, actor string) error {
	return w.DoltStorage.RunInTransaction(ctx, fmt.Sprintf("bd: advance to step %s", id), func(tx storage.Transaction) error {
		current, err := tx.GetIssue(ctx, id)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("step %s not found", id)
		}
		if current.Status != types.StatusOpen {
			return fmt.Errorf("step %s already claimed (status: %s)", id, current.Status)
		}
		return tx.UpdateIssue(ctx, id, map[string]interface{}{"status": types.StatusInProgress}, actor)
	})
}

func newStandaloneStoreMolWriter(store storage.DoltStorage) storeMolWriter {
	return storeMolWriter{DoltStorage: store}
}

type uowMolReader struct {
	uw uow.UnitOfWork
}

func (r uowMolReader) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	issue, isWisp := proxiedResolveIssueOrWisp(ctx, r.uw, id)
	if issue == nil {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	var labels []string
	var err error
	if isWisp {
		labels, err = r.uw.LabelUseCase().GetWispLabels(ctx, id)
	} else {
		labels, err = r.uw.LabelUseCase().GetLabels(ctx, id)
	}
	if err == nil {
		issue.Labels = labels
	}
	return issue, nil
}

func (r uowMolReader) GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error) {
	issues, err := r.uw.IssueUseCase().GetIssuesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	if labelMap, err := r.uw.LabelUseCase().GetLabelsForIssues(ctx, ids); err == nil {
		for _, issue := range issues {
			issue.Labels = labelMap[issue.ID]
		}
	}

	wisps, err := r.uw.IssueUseCase().GetWispsByIDs(ctx, ids)
	if err != nil {
		wisps = nil //nolint:staticcheck // wisps table may not exist; issues result still valid
	}
	if len(wisps) > 0 {
		if labelMap, err := r.uw.LabelUseCase().GetLabelsForWisps(ctx, ids); err == nil {
			for _, wisp := range wisps {
				wisp.Labels = labelMap[wisp.ID]
			}
		}
	}

	return append(issues, wisps...), nil
}

func (r uowMolReader) GetIssuesByLabel(ctx context.Context, label string) ([]*types.Issue, error) {
	page, err := r.uw.IssueUseCase().SearchIssues(ctx, "", types.IssueFilter{Labels: []string{label}})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

func (r uowMolReader) GetDependents(ctx context.Context, issueID string) ([]*types.Issue, error) {
	withMeta, err := r.GetDependentsWithMetadata(ctx, issueID)
	if err != nil {
		return nil, err
	}
	out := make([]*types.Issue, len(withMeta))
	for i, m := range withMeta {
		issue := m.Issue
		out[i] = &issue
	}
	return out, nil
}

func (r uowMolReader) GetDependentsWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error) {
	filter := domain.DepListFilter{Direction: domain.DepDirectionIn}
	issueDeps, err := r.uw.DependencyUseCase().ListWithIssueMetadata(ctx, issueID, filter)
	if err != nil {
		return nil, err
	}
	wispDeps, err := r.uw.DependencyUseCase().ListWispWithIssueMetadata(ctx, issueID, filter)
	if err != nil {
		return issueDeps, nil //nolint:nilerr // wisp_dependencies table may not exist
	}
	return append(issueDeps, wispDeps...), nil
}

func (r uowMolReader) GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error) {
	out, err := r.uw.DependencyUseCase().GetForIssueIDs(ctx, []string{issueID})
	if err != nil {
		return nil, err
	}
	return out[issueID], nil
}

func (r uowMolReader) GetDependencyRecordsForIssues(ctx context.Context, issueIDs []string) (map[string][]*types.Dependency, error) {
	return r.uw.DependencyUseCase().GetForIssueIDs(ctx, issueIDs)
}

func (r uowMolReader) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	page, err := r.uw.IssueUseCase().SearchIssues(ctx, query, filter)
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

func (r uowMolReader) GetReadyWork(ctx context.Context, filter types.WorkFilter) ([]*types.Issue, error) {
	page, err := r.uw.IssueUseCase().GetReadyWork(ctx, filter)
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

func (r uowMolReader) GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	return r.uw.IssueUseCase().GetBlockedIssues(ctx, filter)
}

func (r uowMolReader) GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error) {
	return r.uw.IssueUseCase().GetEpicsEligibleForClosure(ctx)
}

func (r uowMolReader) GetLabels(ctx context.Context, issueID string) ([]string, error) {
	return r.uw.LabelUseCase().GetLabels(ctx, issueID)
}

func (r uowMolReader) GetConfig(ctx context.Context, key string) (string, error) {
	return r.uw.ConfigUseCase().GetConfig(ctx, key)
}

func (r uowMolReader) IsInfraTypeCtx(ctx context.Context, t types.IssueType) bool {
	ok, err := r.uw.ConfigUseCase().IsInfraTypeCtx(ctx, t)
	return err == nil && ok
}

func (r uowMolReader) GetCustomStatusesDetailed(ctx context.Context) ([]types.CustomStatus, error) {
	return r.uw.ConfigUseCase().GetCustomStatuses(ctx)
}

func (r uowMolReader) GetMoleculeProgress(ctx context.Context, moleculeID string) (*types.MoleculeProgressStats, error) {
	progress, err := getMoleculeProgress(ctx, r, moleculeID)
	if err != nil {
		return nil, err
	}
	stats := &types.MoleculeProgressStats{
		MoleculeID:    progress.MoleculeID,
		MoleculeTitle: progress.MoleculeTitle,
		Total:         progress.Total,
		Completed:     progress.Completed,
	}
	if progress.CurrentStep != nil {
		stats.CurrentStepID = progress.CurrentStep.ID
		stats.InProgress = 1
	}
	return stats, nil
}

func (r uowMolReader) GetMoleculeLastActivity(ctx context.Context, moleculeID string) (*types.MoleculeLastActivity, error) {
	root, err := r.GetIssue(ctx, moleculeID)
	if err != nil {
		return nil, err
	}
	subgraph, err := loadTemplateSubgraph(ctx, r, moleculeID)
	if err != nil {
		return nil, err
	}
	last := &types.MoleculeLastActivity{
		MoleculeID:   moleculeID,
		LastActivity: root.UpdatedAt,
		Source:       "molecule_updated",
	}
	for _, issue := range subgraph.Issues {
		if issue.ID == moleculeID {
			continue
		}
		if issue.UpdatedAt.After(last.LastActivity) {
			last.LastActivity = issue.UpdatedAt
			last.Source = "step_updated"
			last.SourceStepID = issue.ID
		}
		if issue.ClosedAt != nil && issue.ClosedAt.After(last.LastActivity) {
			last.LastActivity = *issue.ClosedAt
			last.Source = "step_closed"
			last.SourceStepID = issue.ID
		}
	}
	return last, nil
}

func (r uowMolReader) FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error) {
	return r.uw.IssueUseCase().FindWispDependentsRecursive(ctx, ids)
}

type uowMolWriter struct {
	uowMolReader
	wispIDs map[string]bool
}

func newUOWMolWriter(uw uow.UnitOfWork) *uowMolWriter {
	return &uowMolWriter{uowMolReader: uowMolReader{uw: uw}, wispIDs: make(map[string]bool)}
}

func (w *uowMolWriter) isWisp(ctx context.Context, id string) bool {
	if w.wispIDs[id] {
		return true
	}
	if _, err := w.uw.IssueUseCase().GetWisp(ctx, id); err == nil {
		return true
	}
	return false
}

func (w *uowMolWriter) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	params := domain.CreateIssueParams{Issue: issue, ExplicitID: issue.ID}
	var err error
	if issue.Ephemeral || issue.NoHistory {
		_, err = w.uw.IssueUseCase().CreateWisp(ctx, params, actor)
		if err == nil {
			w.wispIDs[issue.ID] = true
		}
	} else {
		_, err = w.uw.IssueUseCase().CreateIssue(ctx, params, actor)
	}
	return err
}

func (w *uowMolWriter) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	if w.isWisp(ctx, dep.IssueID) {
		return w.uw.DependencyUseCase().AddWispDependency(ctx, dep, actor)
	}
	return w.uw.DependencyUseCase().AddDependency(ctx, dep, actor)
}

func (w *uowMolWriter) AddLabel(ctx context.Context, issueID, label, actor string) error {
	if w.isWisp(ctx, issueID) {
		return w.uw.LabelUseCase().AddWispLabel(ctx, issueID, label, actor)
	}
	return w.uw.LabelUseCase().AddLabel(ctx, issueID, label, actor)
}

func (w *uowMolWriter) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	if w.isWisp(ctx, id) {
		return w.uw.IssueUseCase().UpdateWisp(ctx, id, updates, actor)
	}
	return w.uw.IssueUseCase().UpdateIssue(ctx, id, updates, actor)
}

func (w *uowMolWriter) CloseIssue(ctx context.Context, id, reason, actor string) error {
	params := domain.CloseIssueParams{Reason: reason}
	var err error
	if w.isWisp(ctx, id) {
		_, err = w.uw.IssueUseCase().CloseWisp(ctx, id, params, actor)
	} else {
		_, err = w.uw.IssueUseCase().CloseIssue(ctx, id, params, actor)
	}
	return err
}

func (w *uowMolWriter) DeleteIssue(ctx context.Context, id, actor string) error {
	var err error
	if w.isWisp(ctx, id) {
		_, err = w.uw.IssueUseCase().DeleteWisp(ctx, id, actor)
	} else {
		_, err = w.uw.IssueUseCase().DeleteIssue(ctx, id, actor)
	}
	return err
}

func (w *uowMolWriter) SetConfig(ctx context.Context, key, value string) error {
	return w.uw.ConfigUseCase().SetConfig(ctx, key, value)
}

func (w *uowMolWriter) ClaimStepIfOpen(ctx context.Context, id, actor string) error {
	if w.isWisp(ctx, id) {
		_, err := w.uw.IssueUseCase().ClaimWispIfOpen(ctx, id, actor)
		return err
	}
	_, err := w.uw.IssueUseCase().ClaimIssueIfOpen(ctx, id, actor)
	return err
}

func resolveMolID(ctx context.Context, r molReader, input string) (string, error) {
	if issues, err := r.SearchIssues(ctx, "", types.IssueFilter{IDs: []string{input}}); err == nil && len(issues) > 0 {
		return issues[0].ID, nil
	}

	prefix, err := r.GetConfig(ctx, "issue_prefix")
	if err != nil || prefix == "" {
		prefix = "bd"
	}
	prefixWithHyphen := prefix
	if !strings.HasSuffix(prefix, "-") {
		prefixWithHyphen += "-"
	}

	normalized := input
	if !strings.HasPrefix(input, prefixWithHyphen) && utils.ExtractIssuePrefix(input) == "" {
		normalized = prefixWithHyphen + input
	}

	if issues, err := r.SearchIssues(ctx, "", types.IssueFilter{IDs: []string{normalized}}); err == nil && len(issues) > 0 {
		return issues[0].ID, nil
	}

	hashPart := strings.TrimPrefix(normalized, prefixWithHyphen)
	if hashPart == "" {
		return "", fmt.Errorf("no issue found matching %q", input)
	}

	issues, err := r.SearchIssues(ctx, hashPart, types.IssueFilter{})
	if err != nil {
		return "", fmt.Errorf("searching for %q: %w", input, err)
	}

	var matches []string
	var exactMatch string
	for _, issue := range issues {
		if issue.ID == input || issue.ID == normalized {
			exactMatch = issue.ID
			break
		}
		hash := issue.ID
		if p := utils.ExtractIssuePrefix(issue.ID); p != "" {
			hash = strings.TrimPrefix(issue.ID, p+"-")
		}
		if hash == hashPart {
			exactMatch = issue.ID
		}
		if strings.Contains(hash, hashPart) {
			matches = append(matches, issue.ID)
		}
	}
	if exactMatch != "" {
		return exactMatch, nil
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no issue found matching %q", input)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous ID %q matches %d issues: %v", input, len(matches), matches)
	}
	return matches[0], nil
}
