package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/execx"
	"github.com/steveyegge/beads/internal/memoryapi"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/remedy"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/kvkeys"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/memoryops"
)

// memoryPrefix is prepended (after kvPrefix) to all memory keys.
const memoryPrefix = kvkeys.MemoryPrefix

// memoryKeyFlag allows explicit key override for bd remember.
var memoryKeyFlag string

// memoryNoDedupFlag opts out of fork-only auto-key dedup on bd remember.
// When auto-key generation is in effect (no --key passed) and the new
// insight has the same normalized fingerprint as an existing memory's
// content, we reuse that memory's key instead of creating a sibling
// entry under a slightly different slug. --no-dedup forces a new key.
var memoryNoDedupFlag bool

// memoryNoProvenanceFlag opts out of fork-only provenance capture on
// bd remember (bda-97j). Default false (capture ON when CLAUDE_SESSION_ID
// is set or cwd is a git repo).
var memoryNoProvenanceFlag bool

// memoryTagsFlag is the fork-only --tag flag (repeatable) on bd remember.
// Tags are arbitrary classification labels, stored as the envelope's Tags
// field (bda-5to). Use --tag multiple times for multi-tag entries.
var memoryTagsFlag []string

// memoryScopeFlag is the fork-only --scope flag on bd remember (bda-5to).
// Stored as the envelope's Scope field (e.g. "machine", "project", "global").
var memoryScopeFlag string

// memoriesTagsFilter is the fork-only --tag flag on bd memories (bda-5to).
// AND-semantics: a memory must have all listed tags to match.
var memoriesTagsFilter []string

// memoriesScopeFilter is the fork-only --scope flag on bd memories
// (bda-5to). Exact-match on the envelope's Scope field.
var memoriesScopeFilter string

// Fact validity window flags for `bd remember`.
var (
	memoryValidForFlag     string
	memoryValidUntilFlag   string
	memoryExpirePolicyFlag string
)

// Query/gc flags for `bd memories`.
var (
	memoriesIncludeExpired bool
	memoriesGCFlag         bool
	memoriesGCPlan         bool
	memoriesGCOnly         string
	// memoriesNoMemoryBackup disables the fork-only pre-delete JSONL backup
	// written by pruneExpiredMemories. Default false (backup ON).
	memoriesNoMemoryBackup bool
)

// memoryFingerprintWS collapses runs of whitespace into a single space.
var memoryFingerprintWS = regexp.MustCompile(`\s+`)

// memoryFingerprintNonAlnum strips characters that are not lowercase
// alphanumeric or space. Applied AFTER whitespace collapse so tabs and
// newlines have already been turned into spaces.
var memoryFingerprintNonAlnum = regexp.MustCompile(`[^a-z0-9 ]+`)

// memoryFingerprint normalizes a memory string for dedup comparison.
// Pipeline:
//
//  1. lowercase
//  2. collapse all whitespace runs (spaces, tabs, newlines, CRs) to a
//     single ASCII space
//  3. strip every character that is not lowercase alphanumeric or space
//  4. trim leading/trailing space
//
// Two strings that differ only in case, punctuation, or whitespace
// produce the same fingerprint. Fork-only — used by bd remember to
// detect content-equal memories that would otherwise live under
// sibling keys produced by slugify().
func memoryFingerprint(s string) string {
	s = strings.ToLower(s)
	s = memoryFingerprintWS.ReplaceAllString(s, " ")
	s = memoryFingerprintNonAlnum.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// findDuplicateMemoryKey scans existing memories for one whose CONTENT (after
// envelope unwrap) has the same fingerprint as the new insight. Returns the
// matching key (without the kv.memory. prefix) and true on hit.
//
// Fork-only — invoked from rememberCmd when auto-key generation is in effect
// and --no-dedup is not set.
func findDuplicateMemoryKey(allConfig map[string]string, insight string) (string, bool) {
	target := memoryFingerprint(insight)
	if target == "" {
		return "", false
	}
	fullPrefix := kvPrefix + memoryPrefix
	for k, v := range allConfig {
		if !strings.HasPrefix(k, fullPrefix) {
			continue
		}
		// Unwrap envelope so plain-text and validity-windowed memories
		// can dedup against each other.
		existingContent := parseStoredMemory(v).Content
		if existingContent == "" {
			existingContent = v
		}
		if memoryFingerprint(existingContent) == target {
			return strings.TrimPrefix(k, fullPrefix), true
		}
	}
	return "", false
}

// slugify converts a string to a URL-friendly slug for use as a memory key.
// Takes the first ~8 words, lowercases, replaces non-alphanumeric with hyphens.
func slugify(s string) string {
	s = strings.ToLower(s)
	// Replace non-alphanumeric chars with hyphens
	re := regexp.MustCompile(`[^a-z0-9]+`)
	s = re.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")

	// Limit to first ~8 "words" (hyphen-separated segments)
	parts := strings.SplitN(s, "-", 10)
	if len(parts) > 8 {
		parts = parts[:8]
	}
	slug := strings.Join(parts, "-")

	// Cap total length
	if len(slug) > 60 {
		slug = slug[:60]
		// Don't end on a hyphen
		slug = strings.TrimRight(slug, "-")
	}
	return slug
}

// matchesKnownCommand reports whether insight is a single bare word that
// matches the name or an alias of a top-level bd command. It is used to catch
// `bd remember <subcommand>` mistakes before they become accidental memories.
// Multi-word insights (the normal case) always pass, since they contain
// whitespace and so cannot be a single command token.
func matchesKnownCommand(cmd *cobra.Command, insight string) (string, bool) {
	word := strings.TrimSpace(insight)
	if word == "" || strings.ContainsAny(word, " \t\r\n") {
		return "", false
	}
	for _, c := range cmd.Root().Commands() {
		if strings.EqualFold(c.Name(), word) {
			return c.Name(), true
		}
		for _, alias := range c.Aliases {
			if strings.EqualFold(alias, word) {
				return c.Name(), true
			}
		}
	}
	return "", false
}

// memoryForceFlag is `bd remember --force`: write even when the corpus budget
// would be crossed. It only has meaning when memories.budget-chars is set.
var memoryForceFlag bool

// memoryConfigInt reads an integer config key (stubbable for tests), the way
// primeConfigInt does for the prime-side injection caps.
var memoryConfigInt = func(key string) int {
	return config.GetInt(key)
}

// memoryCorpusBudget resolves the memories.budget-chars ceiling. 0 — the
// default — means OFF, and so does a negative value: the budget is opt-in, and
// a nonsense setting must not turn into a refusal the operator never asked for.
func memoryCorpusBudget() int {
	budget := memoryConfigInt("memories.budget-chars")
	if budget < 0 {
		return 0
	}
	return budget
}

// memoryCorpusChars sums len(key)+len(content) over a corpus.
//
// The unit is BYTES (Go len), deliberately the same unit prime's
// --max-memory-chars cap counts in, so the two numbers are comparable. They are
// not the same MEASURE: prime counts each entry as it renders it — the key and
// value plus a few bytes of heading or bullet markup per memory, and the
// compact form truncates long values — so a corpus sitting exactly at a budget
// equal to prime.max-memory-chars can still have memories elided at injection.
// A budget meant to keep prime from eliding anything has to sit below that cap.
func memoryCorpusChars(memories map[string]string) int {
	total := 0
	for k, v := range memories {
		total += len(k) + len(v)
	}
	return total
}

// projectedMemoryCorpusChars is that sum AFTER a write of content under key
// would apply. An OVERWRITE replaces the key's old content in the sum instead
// of adding to it — the delta, not the sum — which is what makes editing an
// existing memory possible at all once a corpus is near its ceiling.
func projectedMemoryCorpusChars(existing map[string]string, key, content string) int {
	total := memoryCorpusChars(existing)
	if old, ok := existing[key]; ok {
		total -= len(key) + len(old)
	}
	return total + len(key) + len(content)
}

// memoryBudgetLine renders the ONE stderr line the budget check emits, and
// says whether the write must be refused.
//
// Three outcomes, in order of severity:
//   - over budget, no --force: refuse, naming the override.
//   - over budget with --force: write, but SAY SO — a forced crossing that
//     printed nothing would make the budget invisible exactly when it matters.
//   - at or above 80% of budget and within it: write, warn. That is the whole
//     point of a budget over a tripwire: the warning arrives while there is
//     still room to act on it.
//
// Below 80%, and whenever the budget is off, it returns "" and the command is
// silent — byte-identical to the pre-budget behavior.
//
// The percentage FLOORS within the budget and CEILINGS over it, and the
// rounding is not cosmetic: floor(201*100/200) is 100, and "100%" is exactly
// the at-budget reading this code deliberately ALLOWS — so a floored refusal
// would report the number of the case it is not. Ceiling above the line makes
// every over-budget line read 101% or more, and the split is on the VALUE, not
// on --force, so the same projection never reports two different percentages
// depending on a flag. Both forms are int64 arithmetic, so a large budget
// cannot overflow the comparison.
func memoryBudgetLine(projected, budget int, force bool) (line string, refuse bool) {
	if budget <= 0 {
		return "", false
	}
	pct := int(int64(projected) * 100 / int64(budget))
	if projected > budget {
		pct = int((int64(projected)*100 + int64(budget) - 1) / int64(budget))
	}
	switch {
	case projected > budget && !force:
		return fmt.Sprintf("bd remember: memory corpus would be %d chars, budget is %d (%d%%) — refused; use --force to override", projected, budget, pct), true
	case projected > budget:
		return fmt.Sprintf("bd remember: memory corpus at %d of %d chars (%d%%) — over budget, written anyway (--force)", projected, budget, pct), false
	case int64(projected)*5 >= int64(budget)*4:
		return fmt.Sprintf("bd remember: memory corpus at %d of %d chars (%d%%)", projected, budget, pct), false
	}
	return "", false
}

// rememberBudgetVerdict is the whole budget decision as ONE pure function:
// given the corpus as it stands, the write about to be made, the budget and
// --force, it answers with the line to print (empty = say nothing) and whether
// the write must be refused. The command around it only has to fetch the
// corpus and obey.
//
// Keeping it pure is what lets every branch — off, boundary, overwrite delta,
// force, warn — be pinned by a test that needs neither a database nor cgo.
func rememberBudgetVerdict(existing map[string]string, key, content string, budget int, force bool) (line string, refuse bool) {
	if budget <= 0 {
		return "", false
	}
	return memoryBudgetLine(projectedMemoryCorpusChars(existing, key, content), budget, force)
}

// rememberBareKeyPath implements the desire-path / footgun guard for
// `bd remember <bare-slug>` (no --key): a bare slug naming an EXISTING memory
// is recalled instead of stored; a bare slug naming nothing is refused. The
// caller only invokes it when memoryKeyFlag == "" and the insight round-trips
// through memoryapi.DeriveKey unchanged, having already read the key. As in
// printRecallResult, presence is found (row existence), never existing != "":
// a bare slug naming a memory stored as the empty string recalls it.
func rememberBareKeyPath(key, insight, existing string, found bool) error {
	if found {
		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"key":    key,
				"value":  existing,
				"found":  true,
				"action": "recalled",
			})
		}
		keyArg, keyNote := remedy.CommandArg(runtime.GOOS, key, "<key>")
		fmt.Fprintf(os.Stderr,
			"(recalled %q -- a bare existing key READS. To overwrite: bd remember \"<new content>\" --key %s%s)\n",
			key, keyArg, keyNote)
		fmt.Printf("%s\n", existing)
		return nil
	}
	keyArg, keyNote := remedy.CommandArg(runtime.GOOS, key, "<key>")
	return HandleErrorRespectJSON(
		"no memory named %q to recall -- and refusing to store a bare key-like token as its own content. "+
			"bd remember WRITES (its positional arg is CONTENT, not a key). "+
			"To browse keys: bd memories. To store it anyway, run: bd remember %q --key %s%s",
		key, insight, keyArg, keyNote)
}

// printRememberResult renders the `bd remember` success output. commitErr is
// the memory commit's failure, if any; the memory is stored either way.
func printRememberResult(verb, key, insight string, commitErr error) error {
	if jsonOutput {
		payload := map[string]interface{}{
			"key":    key,
			"value":  insight,
			"action": strings.ToLower(verb),
		}
		memoryCommitFields(payload, commitErr)
		return outputJSON(payload)
	}
	fmt.Printf("%s [%s]: %s\n", verb, key, truncateMemory(insight, 80))
	return nil
}

// memoryCommitFields tells a `--json` consumer that the write landed but its
// Dolt commit did not, which the exit code no longer says (see the commit
// sites). The fields are added only then, so a committed write's payload keeps
// its shipped shape. dirty_config_keys names the internal keys the kv.* screen
// refused, which is what the user has to commit or revert. commit_indeterminate
// marks an error after which the store cannot say whether the commit landed
// (storage.ErrCommitIndeterminate, such as a connection lost during
// DOLT_COMMIT). committed stays false beside it, so a consumer that reads only
// committed never takes a memory that may be uncommitted for a committed one;
// `bd dolt commit` settles it either way.
func memoryCommitFields(payload map[string]interface{}, commitErr error) {
	if commitErr == nil {
		return
	}
	payload["committed"] = false
	payload["commit_error"] = commitErr.Error()
	if errors.Is(commitErr, storage.ErrCommitIndeterminate) {
		payload["commit_indeterminate"] = true
	}
	var refused *storage.ConfigCommitRefusedError
	if errors.As(commitErr, &refused) {
		payload["dirty_config_keys"] = refused.Keys
	}
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

// printForgetResult renders the `bd forget` success output. commitErr is as
// for printRememberResult.
func printForgetResult(key, existing string, commitErr error) error {
	if jsonOutput {
		payload := map[string]interface{}{
			"key":     key,
			"deleted": "true",
		}
		memoryCommitFields(payload, commitErr)
		return outputJSON(payload)
	}
	fmt.Printf("Forgot [%s]: %s\n", key, truncateMemory(existing, 80))
	return nil
}

// printRecallResult renders the `bd recall` output (including the not-found
// SilentExit contract). Presence comes from found — ROW EXISTENCE, per the
// role's RecallResult.Found — never from value != "": a memory stored as the
// empty string is found and recalls as "", and only an absent key is a miss.
func printRecallResult(key, value string, found bool) error {
	if jsonOutput {
		if jerr := outputJSON(map[string]interface{}{
			"key":   key,
			"value": value,
			"found": found,
		}); jerr != nil {
			return jerr
		}
		if !found {
			return SilentExit()
		}
		return nil
	}
	if !found {
		fmt.Fprintf(os.Stderr, "No memory with key %q\n", key)
		return SilentExit()
	}
	fmt.Printf("%s\n", value)
	return nil
}

// rememberCmd stores a memory.
var rememberCmd = &cobra.Command{
	Use:   `remember "<insight>"`,
	Short: "Store a persistent memory",
	Long: `Store a memory that persists across sessions and account rotations.

Memories are injected at prime time (bd prime) so you have them
in every session without manual loading.

The positional arg is the memory CONTENT (the key is auto-generated from it
unless --key is given). As a convenience (upstream #4545), if the arg is a bare
key naming an existing memory, it is RECALLED instead of stored (same as
'bd recall'); a bare key naming nothing is refused. Use --key to store
slug-like content.

Fact validity windows (mempalace pattern):
  --valid-for=<dur>       memory expires <dur> from now (e.g. 30d, 2w, 1y, 72h)
  --valid-until=<date>    memory expires at absolute date (YYYY-MM-DD or RFC3339)
  --expire-policy=<p>     what happens after expiry: hide|notify|delete (default: hide)

  hide    — hidden from default 'bd memories' listings; use --include-expired to see
  notify  — still listed, but marked EXPIRED next to the key
  delete  — hidden from listings and removed by 'bd memories --gc'

Auto-key dedup (fork-only, on by default):
  When --key is NOT given, the new insight is fingerprinted (lowercase,
  punctuation-stripped, whitespace-collapsed) and compared against every
  existing memory. If a fingerprint match is found, the existing key is
  reused — preventing sibling keys for content that differs only in
  punctuation, case, or wording-equivalent rephrasing. Pass --no-dedup
  to disable and always create a new key from slugify.

Corpus budget (opt-in, off by default): set the memories.budget-chars config
key to a byte ceiling for the whole memory corpus (sum of len(key)+len(content)).
A write that would cross it is refused, and --force overrides; a write that
lands at or above 80% of the budget warns. With the key unset nothing changes.
bd prime's --max-memory-chars also counts bytes, but of each memory as
injected, formatting included, so a budget equal to that cap does not
guarantee prime injects every memory; set the budget below it for that.

Examples:
  bd remember "always run tests with -race flag"
  bd remember "Dolt phantom DBs hide in three places" --key dolt-phantoms
  bd remember "auth module uses JWT not sessions" --key auth-jwt
  bd remember "feature flag X enabled for beta" --valid-for=30d
  bd remember "TLS cert expires" --valid-until=2026-12-31 --expire-policy=notify
  bd remember "temp workaround for upstream bug" --valid-for=2w --expire-policy=delete
  bd remember "always run tests with -race"            # deduped onto first entry
  bd remember "always run tests with -race"  --no-dedup  # creates a sibling key
  bd remember dolt-phantoms        # bare existing key: reads it (= bd recall)`,
	GroupID:       "setup",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runRememberProxied(cmd, args)
		}
		CheckReadonly("remember") // also covers the migration freeze check (dc-6jaq)

		evt := metrics.NewCommandEvent("remember")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if err := ensureDirectMode("remember requires direct database access"); err != nil {
			return HandleError("%v", err)
		}

		insight := args[0]
		if strings.TrimSpace(insight) == "" {
			return HandleErrorRespectJSON("memory content cannot be empty")
		}
		// An explicit key must name something: the same rule recall and forget
		// apply (upstream memoryapi.ValidateKey), so a whitespace-only --key
		// cannot mint a row no memory operation can name again.
		if memoryKeyFlag != "" {
			if _, err := memoryapi.ValidateKey(memoryKeyFlag); err != nil {
				return HandleErrorRespectJSON("%s", strings.TrimPrefix(err.Error(), memoryops.ErrValidation.Error()+": "))
			}
		}

		// Guard against a subcommand-like first argument being silently stored
		// as memory content. `bd remember` is a leaf command, so a mistaken
		// `bd remember recall` (or any bare bd command name) would otherwise
		// store the word "recall" as a memory instead of doing what the user
		// intended (GH#4401). A genuine insight is a phrase, so only a single
		// bare word that matches a known command is treated as suspect, and an
		// explicit --key signals deliberate intent and bypasses the guard.
		if memoryKeyFlag == "" {
			if name, ok := matchesKnownCommand(cmd, insight); ok {
				return HandleErrorWithHintRespectJSON(
					fmt.Sprintf("%q looks like a command, not something to remember", insight),
					fmt.Sprintf("Did you mean 'bd %s'? To store %q as a memory anyway, give it an explicit key: bd remember %q --key <key>", name, insight, insight),
				)
			}
		}

		// Generate or use provided key.
		// Fork-only dedup: when no --key is provided and --no-dedup is not set,
		// look for an existing memory whose normalized content matches the new
		// insight. If found, reuse its key so we update in place instead of
		// creating a sibling entry under a new slug.
		key := memoryKeyFlag
		dedupHit := false
		if key == "" {
			if !memoryNoDedupFlag {
				if all, err := store.GetAllConfig(rootCtx); err == nil {
					if existingKey, ok := findDuplicateMemoryKey(all, insight); ok {
						key = existingKey
						dedupHit = true
					}
				}
			}
			if key == "" {
				key = slugify(insight)
			}
		}
		if key == "" {
			return HandleErrorRespectJSON("could not generate key from content; use --key to specify one")
		}

		// Parse fact-validity flags. Any flag set means we'll store an
		// envelope instead of plain text. Mutually exclusive: --valid-for vs
		// --valid-until.
		var (
			validFor   time.Duration
			validUntil time.Time
		)
		if strings.TrimSpace(memoryValidForFlag) != "" {
			d, err := parseValidFor(memoryValidForFlag)
			if err != nil {
				return HandleErrorRespectJSON("invalid --valid-for: %v", err)
			}
			validFor = d
		}
		if strings.TrimSpace(memoryValidUntilFlag) != "" {
			t, err := parseValidUntil(memoryValidUntilFlag)
			if err != nil {
				return HandleErrorRespectJSON("invalid --valid-until: %v", err)
			}
			validUntil = t
		}
		if err := validatePolicy(memoryExpirePolicyFlag); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		hasValidity := validFor > 0 || !validUntil.IsZero() || memoryExpirePolicyFlag != ""

		// Capture fork-only provenance (bda-97j): session_id from env, git
		// HEAD if cwd is a repo, and the cwd itself. All best-effort —
		// missing fields stay empty.
		var prov memoryProvenance
		if !memoryNoProvenanceFlag {
			prov = captureMemoryProvenance()
		}
		hasProvenance := prov.SessionID != "" || prov.Commit != "" || prov.Path != ""

		// Fork-only classification (bda-5to): trim tags/scope; empties drop.
		var tagsClean []string
		for _, t := range memoryTagsFlag {
			if s := strings.TrimSpace(t); s != "" {
				tagsClean = append(tagsClean, s)
			}
		}
		scopeClean := strings.TrimSpace(memoryScopeFlag)
		hasTags := len(tagsClean) > 0
		hasScope := scopeClean != ""

		// Default storage is plain text for backward compatibility — only
		// build an envelope when the user has asked for validity semantics
		// OR when fork-only provenance was captured OR tags/scope are set.
		storedValue := insight
		if hasValidity || hasProvenance || hasTags || hasScope {
			v, err := buildMemoryEnvelope(insight, time.Now(), validFor, validUntil, memoryExpirePolicyFlag, prov, tagsClean, scopeClean)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			storedValue = v
		}

		storageKey := kvPrefix + memoryPrefix + key

		ctx := rootCtx

		existing, found, _ := directMemoryLookup(ctx, storageKey)
		verb := "Remembered"
		if found {
			verb = "Updated"
		}
		if dedupHit {
			verb = "Deduped (updated)"
		}

		// Desire path + footgun guard (upstream #4545): `bd remember <x>` is a WRITE whose
		// positional arg is the CONTENT, not a key -- but "remember X" reads as a getter in
		// English, so agents routinely type `bd remember some-key` meaning "do you remember X?".
		// The tell-tale of a mistyped read is content that round-trips through slugify unchanged
		// (a bare slug); real prose insights never do. When that happens and no explicit --key
		// was given AND no fork write-intent flag (validity/tags/scope) was set:
		//   - the key EXISTS  -> pave the desire path: recall it instead of writing
		//   - no such key     -> refuse; storing a key-like token as its own content would
		//                        create a junk memory that hides the mistake
		// Passing --key, or any --valid-*/--tags/--scope flag, states write intent and bypasses
		// both branches (provenance is auto-captured, so it does NOT count as write intent).
		if memoryKeyFlag == "" && slugify(insight) == insight && !hasValidity && !hasTags && !hasScope {
			if found {
				existing = parseStoredMemory(existing).Content
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

		// Corpus budget (memories.budget-chars, upstream #6508), ported onto
		// the fork's direct-store write path. OPT-IN, 0 = off: when off the
		// whole block is skipped and behavior is byte-identical to before.
		// The key credited for an overwrite is the key this write lands under
		// (explicit --key, a dedup hit, or the slug). The corpus is measured on
		// memory CONTENT, the envelope (validity, provenance, tags) unwrapped,
		// so the unit matches upstream's len(key)+len(content). ADVISORY, not
		// atomic: the read and the write below are separate operations.
		var budgetLine string
		if budget := memoryCorpusBudget(); budget > 0 {
			all, err := store.GetAllConfig(ctx)
			if err != nil {
				return HandleErrorRespectJSON("reading the memory corpus for the budget check: %v", err)
			}
			line, refuse := rememberBudgetVerdict(directMemoryCorpus(all), key, insight, budget, memoryForceFlag)
			if refuse {
				// The budget line IS the refusal sentence: said once, on one
				// stream, with the same hint on both routes (upstream #6508).
				const hint = "raise memories.budget-chars, forget a memory, or pass --force"
				if jsonOutput {
					jsonStdoutError(line, hint)
				} else {
					fmt.Fprintln(os.Stderr, line)
					fmt.Fprintf(os.Stderr, "Hint: %s\n", hint)
				}
				return SilentExit()
			}
			budgetLine = line
		}

		// Fork: write the (possibly envelope-wrapped) value, not the raw insight.
		if err := store.SetConfig(ctx, storageKey, storedValue); err != nil {
			return HandleErrorRespectJSON("storing memory: %v", err)
		}
		commandDidWrite.Store(true)
		// The memory is stored by now, so a commit the kv.* screen refuses or
		// that fails is a warning, not the command's failure (upstream #7608);
		// --json gets the same news in memoryCommitFields.
		commitErr := commitMemoryWrite(ctx, store, "remember")
		if commitErr != nil {
			WarnError("%v", commitErr)
		}

		if budgetLine != "" {
			fmt.Fprintln(os.Stderr, budgetLine)
		}

		if jsonOutput {
			action := "remembered"
			if dedupHit {
				action = "deduped"
			} else if found {
				action = "updated"
			}
			result := map[string]interface{}{
				"key":    key,
				"value":  insight,
				"action": action,
			}
			memoryCommitFields(result, commitErr)
			if hasValidity {
				env := parseStoredMemory(storedValue)
				if env.ValidUntil != "" {
					result["valid_until"] = env.ValidUntil
				}
				if env.ExpirePolicy != "" {
					result["expire_policy"] = env.ExpirePolicy
				}
			}
			return outputJSON(result)
		}
		suffix := ""
		if hasValidity {
			env := parseStoredMemory(storedValue)
			if env.ValidUntil != "" {
				suffix = fmt.Sprintf(" (valid until %s, policy=%s)", env.ValidUntil, env.effectivePolicy())
			} else if env.ExpirePolicy != "" {
				suffix = fmt.Sprintf(" (policy=%s)", env.effectivePolicy())
			}
		}
		fmt.Printf("%s [%s]: %s%s\n", verb, key, truncateMemory(insight, 80), suffix)
		return nil
	},
}

// expiredMemoryCandidate describes a memory that is eligible for deletion
// by the prune flow. Used by --plan mode to surface candidates without
// modifying anything.
type expiredMemoryCandidate struct {
	Key          string `json:"key"`
	Content      string `json:"content"`
	ValidUntil   string `json:"valid_until,omitempty"`
	ExpirePolicy string `json:"expire_policy,omitempty"`
}

// listExpiredMemoryCandidates returns the set of memories that the prune
// flow WOULD delete (expired + effective policy == delete). Read-only;
// safe to call from --plan mode.
func listExpiredMemoryCandidates(now time.Time) ([]expiredMemoryCandidate, error) {
	allConfig, err := store.GetAllConfig(rootCtx)
	if err != nil {
		return nil, fmt.Errorf("listing memories: %w", err)
	}
	fullPrefix := kvPrefix + memoryPrefix
	var out []expiredMemoryCandidate
	for k, v := range allConfig {
		if !strings.HasPrefix(k, fullPrefix) {
			continue
		}
		env := parseStoredMemory(v)
		if !env.isExpired(now) {
			continue
		}
		if env.effectivePolicy() != policyDelete {
			continue
		}
		out = append(out, expiredMemoryCandidate{
			Key:          strings.TrimPrefix(k, fullPrefix),
			Content:      env.Content,
			ValidUntil:   env.ValidUntil,
			ExpirePolicy: env.effectivePolicy(),
		})
	}
	return out, nil
}

// pruneExpiredMemories deletes every memory whose envelope has policy=delete
// and a valid_until in the past relative to `now`. If allowlist is non-nil,
// only entries whose key appears in the allowlist are deleted (others are
// skipped silently). Pass nil to delete every eligible candidate (legacy
// `bd memories --gc` behavior).
//
// Returns the list of keys that were removed (without the kv.memory. prefix).
//
// Fork-only — extracted from bd memories --gc so that bd gc (the global
// maintenance command) can prune stale facts in its decay phase as well.
// Caller is responsible for CommitPending() on the store after a non-empty
// return.
//
// skipBackup disables the pre-delete JSONL backup (.beads/.gc-memory-backup-<unix>.jsonl).
// Mirrors writeGCBackup in cmd/bd/gc.go for closed-issue decay (sys-979he / bda-td3).
func pruneExpiredMemories(now time.Time, allowlist map[string]bool, skipBackup bool) ([]string, error) {
	candidates, err := listExpiredMemoryCandidates(now)
	if err != nil {
		return nil, err
	}

	// Build the to-delete set up-front so we can write a backup that
	// reflects exactly what is about to be removed (after allowlist filter).
	var toDelete []expiredMemoryCandidate
	for _, c := range candidates {
		if allowlist != nil && !allowlist[c.Key] {
			continue
		}
		toDelete = append(toDelete, c)
	}

	// Pre-delete backup (default ON, fork-only safeguard).
	if !skipBackup && len(toDelete) > 0 {
		path, err := writeMemoryGCBackup(toDelete)
		if err != nil {
			return nil, fmt.Errorf("writing memory gc backup: %w", err)
		}
		fmt.Fprintf(os.Stderr, "memory gc: pre-delete backup written to %s (%d candidates)\n", path, len(toDelete))
	}

	fullPrefix := kvPrefix + memoryPrefix
	var deleted []string
	for _, c := range toDelete {
		if err := store.DeleteConfig(rootCtx, fullPrefix+c.Key); err != nil {
			return deleted, fmt.Errorf("deleting expired memory %q: %w", c.Key, err)
		}
		deleted = append(deleted, c.Key)
	}
	return deleted, nil
}

// writeMemoryGCBackup serializes the given memory candidates to a JSONL file
// inside .beads/ before any DeleteConfig call. Returns the absolute path of
// the backup on success. Mirrors writeGCBackup in cmd/bd/gc.go (issue decay).
//
// File: .beads/.gc-memory-backup-<unix>.jsonl   mode 0o600   fsync'd
//
// Fork-only safeguard motivated by upstream gastownhall/beads#3543 (closed-issue
// decay data loss); bda-3vg extends parity to memory prune.
func writeMemoryGCBackup(candidates []expiredMemoryCandidate) (string, error) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("locate beads dir or cwd: %w", err)
		}
		beadsDir = cwd
	}
	name := fmt.Sprintf(".gc-memory-backup-%d.jsonl", time.Now().Unix())
	path := filepath.Join(beadsDir, name)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // contains no secrets, only memory content
	if err != nil {
		return "", fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, c := range candidates {
		if err := enc.Encode(c); err != nil {
			return "", fmt.Errorf("encode memory %q: %w", c.Key, err)
		}
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("fsync %s: %w", path, err)
	}
	return path, nil
}

// memoryDisplay is what we render per memory after envelope parsing.
type memoryDisplay struct {
	key      string
	content  string
	envelope memoryEnvelope
	expired  bool
}

// memoriesCmd lists and searches memories.
var memoriesCmd = &cobra.Command{
	Use:   "memories [search]",
	Short: "List or search persistent memories",
	Long: `List all memories, or search by keyword.

By default, memories whose fact validity window has expired are hidden
(policy=hide or policy=delete) or marked EXPIRED (policy=notify).

Flags:
  --include-expired   show every memory, including those past valid_until
  --gc                garbage-collect: delete memories with expire-policy=delete
                      whose valid_until is in the past
  --gc-plan           emit a JSON plan of GC candidates without deleting (mutex with --gc)
  --gc-only=CSV       allowlist of keys to delete with --gc (skip everything else)

Consent flow (recommended):
  bd memories --gc-plan                       # JSON of expired/policy=delete memories
  # orchestrator (e.g. Claude AskUserQuestion) curates the keys
  bd memories --gc --gc-only=key1,key2        # delete ONLY the curated subset

Examples:
  bd memories              # list all memories
  bd memories dolt         # search for memories about dolt
  bd memories "race flag"  # search for a phrase
  bd memories --include-expired
  bd memories --gc-plan
  bd memories --gc --gc-only=stale-1,stale-2`,
	GroupID:       "setup",
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runMemoriesProxied(args)
		}
		evt := metrics.NewCommandEvent("memories")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if memoriesGCFlag {
			CheckReadonly("memories --gc")
		}

		if err := ensureDirectMode("memories requires direct database access"); err != nil {
			return HandleError("%v", err)
		}

		ctx := rootCtx
		allConfig, err := store.GetAllConfig(ctx)
		if err != nil {
			return HandleErrorRespectJSON("listing memories: %v", err)
		}

		// Filter for kv.memory.* keys and parse envelopes.
		fullPrefix := kvPrefix + memoryPrefix
		now := time.Now()

		// Fork-only classification filters (bda-5to). Cleaned up-front so
		// the loop body stays a tight predicate.
		var tagFilter []string
		for _, t := range memoriesTagsFilter {
			if s := strings.TrimSpace(t); s != "" {
				tagFilter = append(tagFilter, s)
			}
		}
		scopeFilter := strings.TrimSpace(memoriesScopeFilter)

		// hasAllTags: every wanted tag is present in have (AND-semantic).
		hasAllTags := func(have []string) bool {
			if len(tagFilter) == 0 {
				return true
			}
			set := make(map[string]struct{}, len(have))
			for _, t := range have {
				set[t] = struct{}{}
			}
			for _, want := range tagFilter {
				if _, ok := set[want]; !ok {
					return false
				}
			}
			return true
		}

		var all []memoryDisplay
		for k, v := range allConfig {
			if !strings.HasPrefix(k, fullPrefix) {
				continue
			}
			userKey := strings.TrimPrefix(k, fullPrefix)
			env := parseStoredMemory(v)
			// Apply fork-only tag/scope filters.
			if !hasAllTags(env.Tags) {
				continue
			}
			if scopeFilter != "" && env.Scope != scopeFilter {
				continue
			}
			all = append(all, memoryDisplay{
				key:      userKey,
				content:  env.Content,
				envelope: env,
				expired:  env.isExpired(now),
			})
		}

		// Garbage-collect pass: delete expired memories with policy=delete,
		// regardless of --include-expired. This is a destructive mode so we
		// run it before any filtering/display so the summary reflects the
		// new state. The actual deletion logic lives in pruneExpiredMemories
		// so bd gc (decay phase) can call it too.
		var gcKeys []string
		// --gc-plan: JSON list of expired memory candidates, no DB changes.
		// Mutually exclusive with --gc.
		if memoriesGCPlan {
			if memoriesGCFlag {
				return HandleErrorRespectJSON("--gc-plan and --gc are mutually exclusive (use --gc-plan first to inspect, then --gc --gc-only=<csv> to delete)")
			}
			cands, err := listExpiredMemoryCandidates(now)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			plan := map[string]interface{}{
				"now":          now.UTC().Format(time.RFC3339),
				"memories":     cands,
				"memory_count": len(cands),
			}
			if len(cands) == 0 {
				plan["hint_next_step"] = "Nothing to do (no expired memories with policy=delete)."
			} else {
				plan["hint_next_step"] = "Pass approved keys via 'bd memories --gc --gc-only=<csv>' to delete only the curated subset."
			}
			return outputJSON(plan)
		}

		if memoriesGCFlag {
			// Parse --gc-only allowlist if set.
			var allowlist map[string]bool
			if strings.TrimSpace(memoriesGCOnly) != "" {
				allowlist = make(map[string]bool)
				for _, s := range strings.Split(memoriesGCOnly, ",") {
					s = strings.TrimSpace(s)
					if s != "" {
						allowlist[s] = true
					}
				}
			}
			deleted, err := pruneExpiredMemories(now, allowlist, memoriesNoMemoryBackup)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			gcKeys = deleted
			if len(gcKeys) > 0 {
				if _, err := store.CommitPending(ctx, getActor()); err != nil {
					WarnError("failed to commit gc: %v", err)
				}
				// Drop deleted entries from the in-memory display list.
				deletedSet := make(map[string]struct{}, len(gcKeys))
				for _, k := range gcKeys {
					deletedSet[k] = struct{}{}
				}
				kept := all[:0]
				for _, m := range all {
					if _, dropped := deletedSet[m.key]; dropped {
						continue
					}
					kept = append(kept, m)
				}
				all = kept
			}
		}

		// Apply search filter (matches key and content).
		var search string
		if len(args) > 0 {
			search = strings.ToLower(args[0])
		}
		if search != "" {
			filtered := all[:0]
			for _, m := range all {
				if strings.Contains(strings.ToLower(m.key), search) ||
					strings.Contains(strings.ToLower(m.content), search) {
					filtered = append(filtered, m)
				}
			}
			all = filtered
		}

		// Apply default expiration filter. The rules are:
		//   - --include-expired: show everything
		//   - policy=notify: show even if expired, with EXPIRED marker
		//   - policy=hide  : hide if expired
		//   - policy=delete: hide if expired (gc will remove next run)
		visible := all[:0]
		hiddenExpired := 0
		for _, m := range all {
			if !m.expired {
				visible = append(visible, m)
				continue
			}
			if memoriesIncludeExpired {
				visible = append(visible, m)
				continue
			}
			if m.envelope.effectivePolicy() == policyNotify {
				visible = append(visible, m)
				continue
			}
			hiddenExpired++
		}

		// Sort by key for stable output.
		sort.Slice(visible, func(i, j int) bool {
			return visible[i].key < visible[j].key
		})

		if jsonOutput {
			out := make([]map[string]interface{}, 0, len(visible))
			for _, m := range visible {
				entry := map[string]interface{}{
					"key":     m.key,
					"value":   m.content,
					"expired": m.expired,
				}
				if m.envelope.ValidUntil != "" {
					entry["valid_until"] = m.envelope.ValidUntil
				}
				if m.envelope.ExpirePolicy != "" {
					entry["expire_policy"] = m.envelope.ExpirePolicy
				}
				if m.envelope.CreatedAt != "" {
					entry["created_at"] = m.envelope.CreatedAt
				}
				out = append(out, entry)
			}
			payload := map[string]interface{}{
				"memories":       out,
				"hidden_expired": hiddenExpired,
				"gc_deleted":     gcKeys,
			}
			return outputJSON(payload)
		}

		if memoriesGCFlag && len(gcKeys) > 0 {
			sort.Strings(gcKeys)
			fmt.Printf("Garbage-collected %d expired memories (policy=delete): %s\n\n",
				len(gcKeys), strings.Join(gcKeys, ", "))
		}

		if len(visible) == 0 {
			if search != "" {
				fmt.Printf("No memories matching %q", search)
			} else {
				fmt.Print("No memories stored. Use 'bd remember \"insight\"' to add one.")
			}
			if hiddenExpired > 0 {
				fmt.Printf(" (%d expired hidden — use --include-expired to show)", hiddenExpired)
			}
			fmt.Println()
			return nil
		}

		if search != "" {
			fmt.Printf("Memories matching %q:\n\n", search)
		} else {
			fmt.Printf("Memories (%d", len(visible))
			if hiddenExpired > 0 {
				fmt.Printf(", %d expired hidden", hiddenExpired)
			}
			fmt.Printf("):\n\n")
		}
		for _, m := range visible {
			marker := ""
			if m.expired {
				marker = " [EXPIRED]"
			} else if m.envelope.ValidUntil != "" {
				marker = fmt.Sprintf(" [valid until %s]", m.envelope.ValidUntil)
			}
			fmt.Printf("  %s%s\n", m.key, marker)
			fmt.Printf("    %s\n\n", truncateMemory(m.content, 120))
		}
		return nil
	},
}

// forgetCmd removes a memory.
var forgetCmd = &cobra.Command{
	Use:   "forget <key>",
	Short: "Remove a persistent memory",
	Long: `Remove a memory by its key.

Use 'bd memories' to see available keys.

Examples:
  bd forget dolt-phantoms
  bd forget auth-jwt`,
	GroupID:       "setup",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runForgetProxied(args)
		}
		CheckReadonly("forget")

		evt := metrics.NewCommandEvent("forget")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if err := ensureDirectMode("forget requires direct database access"); err != nil {
			return HandleError("%v", err)
		}

		key := args[0]
		storageKey := kvPrefix + memoryPrefix + key

		ctx := rootCtx

		existing, found, _ := directMemoryLookup(ctx, storageKey)
		if !found {
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

		if err := store.DeleteConfig(ctx, storageKey); err != nil {
			return HandleErrorRespectJSON("forgetting memory: %v", err)
		}
		commandDidWrite.Store(true)
		// As for remember: the delete landed, so a refused or failed commit
		// warns and the command still succeeds (upstream #7608).
		commitErr := commitMemoryWrite(ctx, store, "forget")
		if commitErr != nil {
			WarnError("%v", commitErr)
		}

		if jsonOutput {
			payload := map[string]interface{}{
				"key":     key,
				"deleted": "true",
			}
			memoryCommitFields(payload, commitErr)
			return outputJSON(payload)
		}
		// The stored value may be the fork's memory envelope; print its content.
		fmt.Printf("Forgot [%s]: %s\n", key, truncateMemory(parseStoredMemory(existing).Content, 80))
		return nil
	},
}

// recallCmd retrieves a specific memory by key.
var recallCmd = &cobra.Command{
	Use:   "recall <key>",
	Short: "Retrieve a specific memory",
	Long: `Retrieve the full content of a memory by its key.

Examples:
  bd recall dolt-phantoms
  bd recall auth-jwt`,
	GroupID:       "setup",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return runRecallProxied(args)
		}
		evt := metrics.NewCommandEvent("recall")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if err := ensureDirectMode("recall requires direct database access"); err != nil {
			return HandleError("%v", err)
		}

		key := args[0]
		storageKey := kvPrefix + memoryPrefix + key

		ctx := rootCtx
		value, found, err := directMemoryLookup(ctx, storageKey)
		if err != nil {
			return HandleErrorRespectJSON("recalling memory: %v", err)
		}

		// Decode envelope so recall always returns the user-facing content
		// and not the raw JSON wrapper.
		var (
			content      string
			validUntil   string
			expirePolicy string
			createdAt    string
			expired      bool
		)
		if value != "" {
			env := parseStoredMemory(value)
			content = env.Content
			validUntil = env.ValidUntil
			expirePolicy = env.ExpirePolicy
			createdAt = env.CreatedAt
			expired = env.isExpired(time.Now())
		}

		if jsonOutput {
			result := map[string]interface{}{
				"key":   key,
				"value": content,
				"found": found,
			}
			if validUntil != "" {
				result["valid_until"] = validUntil
				result["expired"] = expired
			}
			if expirePolicy != "" {
				result["expire_policy"] = expirePolicy
			}
			if createdAt != "" {
				result["created_at"] = createdAt
			}
			if jerr := outputJSON(result); jerr != nil {
				return jerr
			}
			if !found {
				return SilentExit()
			}
			return nil
		}
		if !found {
			fmt.Fprintf(os.Stderr, "No memory with key %q\n", key)
			return SilentExit()
		}
		fmt.Printf("%s\n", content)
		return nil
	},
}

// directMemoryLookup reads a memory row on the direct route and reports its
// presence by ROW EXISTENCE, never by value != "" (upstream #5964): a memory
// stored as the empty string is present. GetConfig answers "" for an absent
// key too, so presence comes from the config map.
func directMemoryLookup(ctx context.Context, storageKey string) (value string, found bool, err error) {
	all, err := store.GetAllConfig(ctx)
	if err != nil {
		return "", false, err
	}
	value, found = all[storageKey]
	return value, found, nil
}

// truncateMemory shortens a string to maxLen for display.
func truncateMemory(s string, maxLen int) string {
	// Replace newlines with spaces for single-line display
	s = strings.ReplaceAll(s, "\n", " ")
	return truncate(s, maxLen)
}

func init() {
	rememberCmd.Flags().StringVar(&memoryKeyFlag, "key", "", "Explicit key for the memory (auto-generated from content if not set). If a memory with this key already exists, it will be updated in place")
	rememberCmd.Flags().BoolVar(&memoryNoDedupFlag, "no-dedup", false, "Disable fork-only auto-key content dedup (always create a new key from slugify even if normalized content matches an existing memory)")
	rememberCmd.Flags().BoolVar(&memoryNoProvenanceFlag, "no-provenance", false, "Disable fork-only provenance capture (CLAUDE_SESSION_ID + git HEAD + cwd) on bd remember")
	rememberCmd.Flags().StringSliceVar(&memoryTagsFlag, "tag", nil, "Tag the memory (repeatable; fork-only). Example: --tag=machine:mdp-home --tag=project:beads")
	rememberCmd.Flags().StringVar(&memoryScopeFlag, "scope", "", "Scope label for the memory (fork-only). Common values: machine, project, global")
	rememberCmd.Flags().StringVar(&memoryValidForFlag, "valid-for", "", "Relative validity window for this memory (e.g. 30d, 2w, 1y, 72h). Mutually exclusive with --valid-until.")
	rememberCmd.Flags().StringVar(&memoryValidUntilFlag, "valid-until", "", "Absolute expiration timestamp (YYYY-MM-DD or RFC3339). Mutually exclusive with --valid-for.")
	rememberCmd.Flags().StringVar(&memoryExpirePolicyFlag, "expire-policy", "", "What to do after expiration: hide (default), notify, delete")

	rememberCmd.Flags().BoolVar(&memoryForceFlag, "force", false, "Store even when the write would cross the memories.budget-chars corpus budget (no effect when the budget is unset)")

	memoriesCmd.Flags().BoolVar(&memoriesIncludeExpired, "include-expired", false, "Include memories whose fact validity window has expired")
	memoriesCmd.Flags().BoolVar(&memoriesGCFlag, "gc", false, "Delete expired memories with expire-policy=delete (combine with --gc-only to curate)")
	memoriesCmd.Flags().BoolVar(&memoriesGCPlan, "gc-plan", false, "Emit a JSON plan of the expired memories that --gc WOULD delete, without modifying anything (mutex with --gc, fork-only)")
	memoriesCmd.Flags().StringVar(&memoriesGCOnly, "gc-only", "", "Comma-separated allowlist of memory keys; when combined with --gc, deletes ONLY items in this list (fork-only)")
	memoriesCmd.Flags().BoolVar(&memoriesNoMemoryBackup, "no-memory-backup", false, "Skip pre-delete JSONL backup written to .beads/.gc-memory-backup-<unix>.jsonl (fork-only — backup is on by default)")
	memoriesCmd.Flags().StringSliceVar(&memoriesTagsFilter, "tag", nil, "Filter memories by tag (repeatable; AND-semantic — memory must have ALL listed tags). Fork-only.")
	memoriesCmd.Flags().StringVar(&memoriesScopeFilter, "scope", "", "Filter memories by scope (exact match on Scope field). Fork-only.")

	rootCmd.AddCommand(rememberCmd)
	rootCmd.AddCommand(memoriesCmd)
	rootCmd.AddCommand(forgetCmd)
	rootCmd.AddCommand(recallCmd)
}

// captureMemoryProvenance returns best-effort provenance for the
// current `bd remember` invocation (bda-97j). All fields are optional;
// missing data stays empty and is omitted from the envelope.
//
// Captures:
//   - SessionID: from $CLAUDE_SESSION_ID env (set by Claude Code harness)
//   - Commit:    git rev-parse HEAD in cwd, if cwd is in a git repo
//   - Path:      os.Getwd()
func captureMemoryProvenance() memoryProvenance {
	var prov memoryProvenance
	prov.SessionID = strings.TrimSpace(os.Getenv("CLAUDE_SESSION_ID"))
	if cwd, err := os.Getwd(); err == nil {
		prov.Path = cwd
	}
	// Best-effort git HEAD; ignore errors (not in a repo, git missing, etc.)
	if out, err := execx.GitCommand("rev-parse", "HEAD").Output(); err == nil {
		prov.Commit = strings.TrimSpace(string(out))
	}
	return prov
}

// memoriesFromProvider is that accessor step for a provider the caller names.
//
// It takes the provider rather than reading the global one because `bd prime`
// opens a provider SCOPED to its read — prime is in noDbCommands, so the root
// pre-run opens nothing — and one spelling of "ask this provider for the memory
// surface" is the whole point of having an accessor at all.
func memoriesFromProvider(provider uow.UnitOfWorkProvider) (memoryops.Memories, error) {
	src, ok := provider.(uow.MemoriesSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the persistent-memory surface", provider)
	}
	return src.Memories()
}

// directMemoryCorpus maps every stored memory key (prefix stripped) to its
// content, the envelope unwrapped, for the corpus budget (upstream #6508).
func directMemoryCorpus(allConfig map[string]string) map[string]string {
	fullPrefix := kvPrefix + memoryPrefix
	corpus := make(map[string]string)
	for k, v := range allConfig {
		if !strings.HasPrefix(k, fullPrefix) {
			continue
		}
		corpus[strings.TrimPrefix(k, fullPrefix)] = parseStoredMemory(v).Content
	}
	return corpus
}
