package storage

// NonCommitGraphBackend is an affirmative *negative* marker: it is implemented
// only by backends whose Commit/GetCurrentCommit/Push do NOT participate in a
// real version-control commit graph (currently SQLite). cmd/bd's PostRun
// skips its Dolt-only maintenance tail (auto-commit, auto-export, auto-backup,
// auto-push) when the active store implements this and returns true.
//
// The marker is negative and lives only on non-Dolt backends by design: a store
// that does NOT implement it is assumed to be Dolt, so the default path runs the
// full maintenance tail unchanged and stays byte-identical. A future non-Dolt
// implementation would opt out of Dolt maintenance by implementing this marker.
type NonCommitGraphBackend interface {
	CommitGraphUnsupported() bool
}
