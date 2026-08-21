package main

// Fork seam (sys-iogddl, codex-review HIGH finding on fdaab2d34): memory.go's
// classic route keeps the fork's envelope KV flow (dedup/provenance/tags/
// expiry/GC); the proxied-server route here speaks upstream's memoryops role,
// which the merged tree's memory_proxied_integration_test.go exercises end to
// end. The runners and print helpers below carry upstream memory.go's logic
// (gastownhall/main 2632d57e0) adapted to dispatch-from-the-fork-RunE form.
// Fork-only flags have no role transport yet - they are refused loudly rather
// than silently dropped (memoryops-role redesign follow-up in sys-iogddl).

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/memoryapi"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/memoryops"
)

// proxiedMemories hands back the guarded persistent-memory surface for this
// invocation's proxied-server provider, through the provider's OWN capability
// accessor - the same two-step proxiedWorkspaceConfig performs.
func proxiedMemories() (memoryops.Memories, error) {
	if uowProvider == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	return memoriesFromProvider(uowProvider)
}

// runRememberProxied is the proxied-server route of `bd remember`.
func runRememberProxied(cmd *cobra.Command, args []string) error {
	CheckReadonly("remember")

	if memoryNoDedupFlag || memoryNoProvenanceFlag || len(memoryTagsFlag) > 0 ||
		memoryScopeFlag != "" || memoryValidForFlag != "" || memoryValidUntilFlag != "" ||
		memoryExpirePolicyFlag != "" {
		return HandleErrorRespectJSON("fork-only remember flags (--no-dedup, --no-provenance, --tag, --scope, --valid-for, --valid-until, --expire-policy) are not supported in proxied-server mode yet")
	}

	evt := metrics.NewCommandEvent("remember")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	insight := args[0]

	// Front-door guard, same contract as the classic route (GH#4401): a single
	// bare word matching a command name is a mistyped command, not content.
	if memoryKeyFlag == "" {
		if name, ok := matchesKnownCommand(cmd, insight); ok {
			return HandleErrorWithHintRespectJSON(
				fmt.Sprintf("%q looks like a command, not something to remember", insight),
				fmt.Sprintf("Did you mean 'bd %s'? To store %q as a memory anyway, give it an explicit key: bd remember %q --key <key>", name, insight, insight),
			)
		}
	}

	memories, err := proxiedMemories()
	if err != nil {
		return HandleError("%v", err)
	}

	// Desire path: `bd remember <bare-existing-key>` reads instead of writing;
	// a bare slug naming nothing is refused. DeriveKey("") is "" - the
	// derived != "" clause keeps empty content on the write path, where the
	// role refuses it with its own validation sentence.
	derived := memoryapi.DeriveKey(insight)
	if memoryKeyFlag == "" && derived != "" && derived == insight {
		recalled, rerr := memories.Recall(rootCtx, memoryops.RecallRequest{Key: derived})
		if rerr != nil {
			return HandleErrorRespectJSON("recalling memory: %v", rerr)
		}
		return rememberBareKeyPath(derived, insight, recalled.Value)
	}

	result, err := memories.Remember(rootCtx, memoryops.RememberRequest{Key: memoryKeyFlag, Content: insight})
	if err != nil {
		// The role's validation refusals ARE the shipped sentences - print
		// them as themselves instead of rewording matchable output.
		if errors.Is(err, memoryops.ErrValidation) {
			return HandleErrorRespectJSON("%s", strings.TrimPrefix(err.Error(), memoryops.ErrValidation.Error()+": "))
		}
		return HandleErrorRespectJSON("storing memory: %v", err)
	}

	// Remembered versus Updated is Replaced, observed in the SAME transaction
	// as the write. No commandDidWrite flag here: a proxied write already
	// committed inside the role's unit of work.
	verb := "Remembered"
	if result.Replaced {
		verb = "Updated"
	}
	return printRememberResult(verb, result.Key, result.Value)
}

// runMemoriesProxied is the proxied-server route of `bd memories`.
func runMemoriesProxied(args []string) error {
	if memoriesIncludeExpired || memoriesGCFlag || memoriesGCPlan || memoriesGCOnly != "" ||
		memoriesNoMemoryBackup || len(memoriesTagsFilter) > 0 || memoriesScopeFilter != "" {
		return HandleErrorRespectJSON("fork-only memories flags (--include-expired, --gc, --gc-plan, --gc-only, --no-memory-backup, --tag, --scope) are not supported in proxied-server mode yet")
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
	return printForgetResult(result.Key, result.Value)
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
	return printRecallResult(result.Key, result.Value)
}

// rememberBareKeyPath implements the desire-path / footgun guard for
// `bd remember <bare-slug>` (no --key): a bare slug naming an EXISTING memory
// is recalled instead of stored; a bare slug naming nothing is refused.
func rememberBareKeyPath(key, insight, existing string) error {
	if existing != "" {
		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"key":    key,
				"value":  existing,
				"found":  true,
				"action": "recalled",
			})
		}
		fmt.Fprintf(os.Stderr,
			"(recalled %q -- a bare existing key READS. To overwrite: `bd remember \"<new content>\" --key %s`)\n",
			key, key)
		fmt.Printf("%s\n", existing)
		return nil
	}
	return HandleErrorRespectJSON(
		"no memory named %q to recall -- and refusing to store a bare key-like token as its own content. "+
			"`bd remember` WRITES (its positional arg is CONTENT, not a key). "+
			"To store it anyway: `bd remember %q --key %s`. To browse keys: `bd memories`",
		key, insight, key)
}

// printRememberResult renders the `bd remember` success output.
func printRememberResult(verb, key, insight string) error {
	if jsonOutput {
		return outputJSON(map[string]string{
			"key":    key,
			"value":  insight,
			"action": strings.ToLower(verb),
		})
	}
	fmt.Printf("%s [%s]: %s\n", verb, key, truncateMemory(insight, 80))
	return nil
}

// printMemoriesResult renders the `bd memories` output.
func printMemoriesResult(memories map[string]string, search string) error {
	if jsonOutput {
		return outputJSON(memories)
	}

	if len(memories) == 0 {
		if search != "" {
			fmt.Printf("No memories matching %q\n", search)
		} else {
			fmt.Println("No memories stored. Use 'bd remember \"insight\"' to add one.")
		}
		return nil
	}

	keys := make([]string, 0, len(memories))
	for k := range memories {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if search != "" {
		fmt.Printf("Memories matching %q:\n\n", search)
	} else {
		fmt.Printf("Memories (%d):\n\n", len(memories))
	}
	for _, k := range keys {
		v := memories[k]
		fmt.Printf("  %s\n", k)
		fmt.Printf("    %s\n\n", truncateMemory(v, 120))
	}
	return nil
}

// printForgetNotFound renders the `bd forget` missing-key output (including
// the SilentExit contract).
func printForgetNotFound(key string) error {
	if jsonOutput {
		if jerr := outputJSON(map[string]string{
			"key":   key,
			"found": "false",
		}); jerr != nil {
			return jerr
		}
		return SilentExit()
	}
	fmt.Fprintf(os.Stderr, "No memory with key %q\n", key)
	return SilentExit()
}

// printForgetResult renders the `bd forget` success output.
func printForgetResult(key, existing string) error {
	if jsonOutput {
		return outputJSON(map[string]string{
			"key":     key,
			"deleted": "true",
		})
	}
	fmt.Printf("Forgot [%s]: %s\n", key, truncateMemory(existing, 80))
	return nil
}

// printRecallResult renders the `bd recall` output (including the not-found
// SilentExit contract).
func printRecallResult(key, value string) error {
	if jsonOutput {
		if jerr := outputJSON(map[string]interface{}{
			"key":   key,
			"value": value,
			"found": value != "",
		}); jerr != nil {
			return jerr
		}
		if value == "" {
			return SilentExit()
		}
		return nil
	}
	if value == "" {
		fmt.Fprintf(os.Stderr, "No memory with key %q\n", key)
		return SilentExit()
	}
	fmt.Printf("%s\n", value)
	return nil
}
