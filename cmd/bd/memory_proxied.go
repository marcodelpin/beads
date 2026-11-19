package main

// Fork seam (sys-iogddl, codex-review HIGH finding on fdaab2d34): memory.go's
// classic route keeps the fork's envelope KV flow (dedup/provenance/tags/
// expiry/GC); the proxied-server route here speaks upstream's memoryops role,
// which the merged tree's memory_proxied_integration_test.go exercises end to
// end. The runners and print helpers below carry upstream memory.go's logic
// (gastownhall/main 2632d57e0) adapted to dispatch-from-the-fork-RunE form.
// Fork-only flags have no role transport yet - they are refused loudly rather
// than silently dropped (memoryops-role redesign follow-up in sys-iogddl).
// `bd remember` no longer dispatches here: its RunE in memory.go is upstream's
// on every route, with the fork seam in memory_fork.go (bda-nchm).
// A remote (HTTP) backend takes this route too (usesMemoryRole, bda-tfok.4.4).

import (
	"errors"
	"strings"

	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/memoryops"
)

// usesMemoryRole reports whether the memory commands take the memoryops role
// route below instead of memory.go's direct config-table route: in
// proxied-server mode, and on a remote (HTTP) backend (bda-tfok.4.4, codex F5).
// The HTTP store serves memories ONLY through its Memories accessor: its
// settings document omits kv.memory.* and it refuses SetConfig, so the direct
// route would list nothing, miss every key and fail every write.
func usesMemoryRole() bool {
	return usesProxiedServer() || storeIsRemoteBackend(getStore())
}

// proxiedMemories hands back the guarded persistent-memory surface for this
// invocation: the proxied-server provider's, through the provider's OWN
// capability accessor (the same two-step proxiedWorkspaceConfig performs), or
// else the store's own accessor, which is the remote backend's memory API.
func proxiedMemories() (memoryops.Memories, error) {
	if !usesProxiedServer() {
		st := getStore()
		if st == nil {
			return nil, errors.New("memory role: no store open")
		}
		return st.Memories()
	}
	if uowProvider == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	return memoriesFromProvider(uowProvider)
}

// runMemoriesProxied is the proxied-server route of `bd memories`.
func runMemoriesProxied(args []string) error {
	if memoriesIncludeExpired || memoriesGCFlag || memoriesGCPlan || memoriesGCOnly != "" ||
		memoriesNoMemoryBackup || len(memoriesTagsFilter) > 0 || memoriesScopeFilter != "" {
		return HandleErrorRespectJSON("fork-only memories flags (--include-expired, --gc, --gc-plan, --gc-only, --no-memory-backup, --tag, --scope) are not supported in proxied-server or remote-backend mode yet")
	}

	evt := metrics.NewCommandEvent("memories")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	var search string
	if len(args) > 0 {
		search = args[0]
	}

	memories, err := proxiedMemories()
	if err != nil {
		return HandleError("%v", err)
	}
	// The term goes to the role RAW: case folding is List's, so the two routes
	// cannot come to disagree about what matches.
	result, err := memories.List(rootCtx, memoryops.ListRequest{Search: search})
	if err != nil {
		return HandleErrorRespectJSON("listing memories: %v", err)
	}
	return printMemoriesResult(result.Memories, strings.ToLower(search))
}

// runForgetProxied is the proxied-server route of `bd forget`.
func runForgetProxied(args []string) error {
	CheckReadonly("forget")

	evt := metrics.NewCommandEvent("forget")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	memories, err := proxiedMemories()
	if err != nil {
		return HandleError("%v", err)
	}
	// No pre-read, deliberately: the value printed below is the one the role's
	// transaction actually deleted.
	result, err := memories.Forget(rootCtx, memoryops.ForgetRequest{Key: args[0]})
	if err != nil {
		return HandleErrorRespectJSON("forgetting memory: %v", err)
	}
	if !result.Found {
		return printForgetNotFound(result.Key)
	}
	return printForgetResult(result.Key, result.Value, nil)
}

// runRecallProxied is the proxied-server route of `bd recall`.
func runRecallProxied(args []string) error {
	evt := metrics.NewCommandEvent("recall")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	memories, err := proxiedMemories()
	if err != nil {
		return HandleError("%v", err)
	}
	result, err := memories.Recall(rootCtx, memoryops.RecallRequest{Key: args[0]})
	if err != nil {
		return HandleErrorRespectJSON("recalling memory: %v", err)
	}
	return printRecallResult(result.Key, result.Value, result.Found)
}
