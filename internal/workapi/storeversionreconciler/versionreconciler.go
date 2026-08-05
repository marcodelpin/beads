// Package storeversionreconciler holds the store-backed implementation of
// issueops.VersionReconciler: one shared body that every store-shaped backend's
// VersionReconciler accessor hands back.
//
// It is a package of its own for the reason internal/workapi/storereader and
// internal/workapi/storeworkspaceconfig are — see those packages' docs. A
// constructor sitting in internal/workapi would be a one-line drop-in for
// store.VersionReconciler() from any front door, and one that silently skips
// the decorators, because a decorator adds its layer in its own accessor. Down
// here the only importers are the two Dolt store packages, and the
// cmd-bd-role-constructors depguard rule in .golangci.yml makes a front door
// importing it a lint failure rather than a review comment.
//
// The accessor is the door. This is the thing behind it.
package storeversionreconciler

import (
	"context"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// New returns the version-marker surface backed by a store handle. *DoltStore
// and *EmbeddedDoltStore answer identically because the difference between them
// is below storage.DoltStorage, not above it.
func New(store storage.DoltStorage) (issueops.VersionReconciler, error) {
	if store == nil {
		return nil, &issueops.ErrUnsupported{Op: "storeversionreconciler.New", Backend: "nil"}
	}
	return &storeVersionReconciler{store: store}, nil
}

type storeVersionReconciler struct{ store storage.DoltStorage }

var _ issueops.VersionReconciler = (*storeVersionReconciler)(nil)

func (r *storeVersionReconciler) RecordedVersion(ctx context.Context, _ issueops.RecordedVersionRequest) (issueops.RecordedVersionResult, error) {
	recorded, mark, err := r.readMarkers(ctx)
	if err != nil {
		return issueops.RecordedVersionResult{}, err
	}
	return issueops.RecordedVersionResult{Recorded: recorded, HighWaterMark: mark}, nil
}

// ReconcileVersion reads the markers, decides through the shared planner and
// writes only what the plan asks for.
//
// THE TWO WRITES ARE NOT ONE TRANSACTION on this backend, and that is stated
// rather than hidden: storage.DoltStorage publishes methods, not transactions,
// so a failure between them leaves the marker moved and the mark behind. The
// consequence is bounded and benign — a mark below the marker only ever means
// the next older binary is refused by the marker instead of by the mark, which
// is the same refusal — and buying atomicity would mean a transaction-scoped
// body for two writes to a dolt-ignored plane that no other reader shares.
func (r *storeVersionReconciler) ReconcileVersion(ctx context.Context, req issueops.VersionReconcileRequest) (issueops.VersionReconcileResult, error) {
	// Validate before reading anything: a refused reconciliation should cost no
	// round trip, and this one runs on every startup.
	if _, err := workapi.ValidateReconcileVersion(req.CLIVersion); err != nil {
		return issueops.VersionReconcileResult{}, err
	}

	recorded, mark, err := r.readMarkers(ctx)
	if err != nil {
		return issueops.VersionReconcileResult{}, err
	}
	plan, err := workapi.PlanVersionReconcile(req.CLIVersion, recorded, mark)
	if err != nil {
		return issueops.VersionReconcileResult{}, err
	}

	if plan.RecordVersion {
		if err := r.store.SetLocalMetadata(ctx, workapi.MetadataKeyVersion, plan.Result.Current); err != nil {
			return issueops.VersionReconcileResult{}, err
		}
	}
	if plan.RecordHighWaterMark {
		if err := r.store.SetLocalMetadata(ctx, workapi.MetadataKeyVersionMax, plan.Result.Current); err != nil {
			return issueops.VersionReconcileResult{}, err
		}
	}
	return plan.Result, nil
}

// readMarkers reads both markers, which is the whole cost of the path that
// changes nothing: two point reads of a dolt-ignored table, no configuration
// load, no second connection and no lock. Both are read even when the first one
// already decides the outcome, because RecordedVersion publishes the pair and a
// second shape of this helper would be a second thing to keep true.
func (r *storeVersionReconciler) readMarkers(ctx context.Context) (recorded, mark string, err error) {
	recorded, err = r.store.GetLocalMetadata(ctx, workapi.MetadataKeyVersion)
	if err != nil {
		return "", "", err
	}
	mark, err = r.store.GetLocalMetadata(ctx, workapi.MetadataKeyVersionMax)
	if err != nil {
		return "", "", err
	}
	return recorded, mark, nil
}
