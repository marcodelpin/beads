//go:build cgo

package embeddeddolt

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
)

var (
	cacheMu sync.Mutex
	cache   = make(map[string]*cacheEntry) // keyed by absolute dataDir
)

type cacheEntry struct {
	store    *EmbeddedDoltStore
	refCount int // guarded by cacheMu
}

// Open returns a cached EmbeddedDoltStore for the given data directory, creating
// one via New if no cached instance exists. Subsequent calls with the same
// resolved dataDir return the existing store and increment a reference count.
//
// Each Open must be paired with a Close. The underlying store is only truly
// closed (flock released, resources freed) when the last reference calls Close.
//
// This prevents the same-process deadlock that occurs when two code paths open
// connectors against the same data directory: the embedded Dolt driver's
// internal engine lock combined with infinite-backoff retry means the second
// connector spins forever waiting for the first to release.
func Open(ctx context.Context, beadsDir, database, branch string, opts ...Option) (*EmbeddedDoltStore, error) {
	key, err := cacheKey(beadsDir)
	if err != nil {
		return nil, err
	}

	cacheMu.Lock()
	if entry, ok := cache[key]; ok {
		entry.refCount++
		cacheMu.Unlock()
		return entry.store, nil
	}
	cacheMu.Unlock()

	// Slow path: create a new store outside the lock. New() acquires the
	// flock and initializes the schema, which can take significant time.
	s, err := newStore(ctx, beadsDir, database, branch, opts...)
	if err != nil {
		return nil, err
	}

	cacheMu.Lock()
	// Double-check: another goroutine may have inserted while we created.
	if entry, ok := cache[key]; ok {
		cacheMu.Unlock()
		// Discard the store we just created; use the cached one.
		_ = s.closeUnderlying()
		entry.refCount++
		return entry.store, nil
	}
	cache[key] = &cacheEntry{store: s, refCount: 1}
	cacheMu.Unlock()
	return s, nil
}

// closeCached decrements the reference count for a cached store.
// Returns true when the cache absorbed the close (refs remain, suppress real
// close). Returns false when the caller must run closeUnderlying — either the
// entry was evicted (last ref) or the store was never cached.
func closeCached(s *EmbeddedDoltStore) bool {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	for key, entry := range cache {
		if entry.store == s {
			entry.refCount--
			if entry.refCount <= 0 {
				delete(cache, key)
				// Actual close happens after releasing cacheMu (via caller).
				return false
			}
			// Other references remain — suppress the real close.
			return true
		}
	}
	// Not in cache — let the caller close normally.
	return false
}

// cacheKey resolves beadsDir to an absolute dataDir path for use as a cache key.
func cacheKey(beadsDir string) (string, error) {
	absBeadsDir, err := filepath.Abs(beadsDir)
	if err != nil {
		return "", fmt.Errorf("embeddeddolt: resolving beads dir for cache key: %w", err)
	}
	return filepath.Join(absBeadsDir, "embeddeddolt"), nil
}
