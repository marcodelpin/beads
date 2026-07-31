package dolt

import (
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// IssueReader returns the guarded issue-query surface for this store.
func (s *DoltStore) IssueReader() (issueops.Reader, error) {
	return NewIssueReader(s)
}

// NewIssueReader returns guarded issue queries backed by store.
//
// The implementation is the shared one: the two Dolt-backed stores differ
// below storage.DoltStorage, not above it, so a second copy here would be a
// copy of nothing but the constructor.
func NewIssueReader(store *DoltStore) (issueops.Reader, error) {
	if store == nil {
		return nil, &storage.ErrUnsupported{Op: "NewIssueReader", Backend: "nil"}
	}
	return workapi.NewStoreReader(store)
}
