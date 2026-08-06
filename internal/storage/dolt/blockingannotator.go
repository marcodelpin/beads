package dolt

import (
	"context"
	"database/sql"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/issueops"
)

// BlockingAnnotator returns the guarded blocking-decoration surface for this
// store.
func (s *DoltStore) BlockingAnnotator() (issueops.BlockingAnnotator, error) {
	return newBlockingAnnotator(s)
}

// newBlockingAnnotator returns guarded blocking annotations backed by store.
//
// It is unexported for the reason newEdgeReader beside it is: a command holds
// the storage.DoltStorage interface and reaches the role through the accessor
// above, which is where each decorator adds its layer. There is no shared
// constructor for the cmd-bd-role-constructors depguard rule to deny here
// because the shared body is an InTx function — it needs a transaction this
// store owns, so no front door can reach it at all.
func newBlockingAnnotator(store *DoltStore) (issueops.BlockingAnnotator, error) {
	if store == nil {
		return nil, &storage.ErrUnsupported{Op: "newBlockingAnnotator", Backend: "nil"}
	}
	return &blockingAnnotator{store: store}, nil
}

type blockingAnnotator struct{ store *DoltStore }

var _ issueops.BlockingAnnotator = (*blockingAnnotator)(nil)

// AnnotateBlocking runs the outbound read, the inbound read and the status
// lookups in ONE read transaction, so no answer can report a blocker open and
// closed at once.
func (a *blockingAnnotator) AnnotateBlocking(ctx context.Context, request issueops.BlockingRequest) (issueops.BlockingResult, error) {
	if err := storageissueops.ValidateBlockingRequest(request); err != nil {
		return issueops.BlockingResult{}, err
	}
	var result issueops.BlockingResult
	err := a.store.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = storageissueops.ExecuteBlockingAnnotation(ctx, tx, request)
		return err
	})
	if err != nil {
		return issueops.BlockingResult{}, err
	}
	return result, nil
}
