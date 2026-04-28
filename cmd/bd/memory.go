package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// memoryPrefix is prepended (after kvPrefix) to all memory keys.
const memoryPrefix = "memory."

// memoryKeyFlag allows explicit key override for bd remember.
var memoryKeyFlag string

// memoryNoDedupFlag opts out of fork-only auto-key dedup on bd remember.
// When auto-key generation is in effect (no --key passed) and the new
// insight has the same normalized fingerprint as an existing memory's
// content, we reuse that memory's key instead of creating a sibling
// entry under a slightly different slug. --no-dedup forces a new key.
var memoryNoDedupFlag bool

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

// rememberCmd stores a memory.
var rememberCmd = &cobra.Command{
	Use:   `remember "<insight>"`,
	Short: "Store a persistent memory",
	Long: `Store a memory that persists across sessions and account rotations.

Memories are injected at prime time (bd prime) so you have them
in every session without manual loading.

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

Examples:
  bd remember "always run tests with -race flag"
  bd remember "Dolt phantom DBs hide in three places" --key dolt-phantoms
  bd remember "auth module uses JWT not sessions" --key auth-jwt
  bd remember "feature flag X enabled for beta" --valid-for=30d
  bd remember "TLS cert expires" --valid-until=2026-12-31 --expire-policy=notify
  bd remember "temp workaround for upstream bug" --valid-for=2w --expire-policy=delete
  bd remember "always run tests with -race"            # deduped onto first entry
  bd remember "always run tests with -race"  --no-dedup  # creates a sibling key`,
	GroupID: "setup",
	Args:    cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		CheckReadonly("remember")

		if err := ensureDirectMode("remember requires direct database access"); err != nil {
			FatalError("%v", err)
		}

		insight := args[0]
		if strings.TrimSpace(insight) == "" {
			FatalErrorRespectJSON("memory content cannot be empty")
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
			FatalErrorRespectJSON("could not generate key from content; use --key to specify one")
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
				FatalErrorRespectJSON("invalid --valid-for: %v", err)
			}
			validFor = d
		}
		if strings.TrimSpace(memoryValidUntilFlag) != "" {
			t, err := parseValidUntil(memoryValidUntilFlag)
			if err != nil {
				FatalErrorRespectJSON("invalid --valid-until: %v", err)
			}
			validUntil = t
		}
		if err := validatePolicy(memoryExpirePolicyFlag); err != nil {
			FatalErrorRespectJSON("%v", err)
		}

		hasValidity := validFor > 0 || !validUntil.IsZero() || memoryExpirePolicyFlag != ""

		// Default storage is plain text for backward compatibility — only
		// build an envelope when the user has asked for validity semantics.
		storedValue := insight
		if hasValidity {
			v, err := buildMemoryEnvelope(insight, time.Now(), validFor, validUntil, memoryExpirePolicyFlag)
			if err != nil {
				FatalErrorRespectJSON("%v", err)
			}
			storedValue = v
		}

		storageKey := kvPrefix + memoryPrefix + key

		ctx := rootCtx

		// Check if updating an existing memory
		existing, _ := store.GetConfig(ctx, storageKey)
		verb := "Remembered"
		if existing != "" {
			verb = "Updated"
		}
		if dedupHit {
			verb = "Deduped (updated)"
		}

		if err := store.SetConfig(ctx, storageKey, storedValue); err != nil {
			FatalErrorRespectJSON("storing memory: %v", err)
		}
		if _, err := store.CommitPending(ctx, getActor()); err != nil {
			WarnError("failed to commit memory: %v", err)
		}

		if jsonOutput {
			action := "remembered"
			if dedupHit {
				action = "deduped"
			} else if existing != "" {
				action = "updated"
			}
			result := map[string]string{
				"key":    key,
				"value":  insight,
				"action": action,
			}
			if hasValidity {
				env := parseStoredMemory(storedValue)
				if env.ValidUntil != "" {
					result["valid_until"] = env.ValidUntil
				}
				if env.ExpirePolicy != "" {
					result["expire_policy"] = env.ExpirePolicy
				}
			}
			outputJSON(result)
		} else {
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
		}
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
func pruneExpiredMemories(now time.Time, allowlist map[string]bool) ([]string, error) {
	candidates, err := listExpiredMemoryCandidates(now)
	if err != nil {
		return nil, err
	}
	fullPrefix := kvPrefix + memoryPrefix
	var deleted []string
	for _, c := range candidates {
		if allowlist != nil && !allowlist[c.Key] {
			continue
		}
		if err := store.DeleteConfig(rootCtx, fullPrefix+c.Key); err != nil {
			return deleted, fmt.Errorf("deleting expired memory %q: %w", c.Key, err)
		}
		deleted = append(deleted, c.Key)
	}
	return deleted, nil
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

Examples:
  bd memories              # list all memories
  bd memories dolt         # search for memories about dolt
  bd memories "race flag"  # search for a phrase
  bd memories --include-expired
  bd memories --gc`,
	GroupID: "setup",
	Args:    cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if memoriesGCFlag {
			CheckReadonly("memories --gc")
		}
		if err := ensureDirectMode("memories requires direct database access"); err != nil {
			FatalError("%v", err)
		}

		ctx := rootCtx
		allConfig, err := store.GetAllConfig(ctx)
		if err != nil {
			FatalErrorRespectJSON("listing memories: %v", err)
		}

		// Filter for kv.memory.* keys and parse envelopes.
		fullPrefix := kvPrefix + memoryPrefix
		now := time.Now()
		var all []memoryDisplay
		for k, v := range allConfig {
			if !strings.HasPrefix(k, fullPrefix) {
				continue
			}
			userKey := strings.TrimPrefix(k, fullPrefix)
			env := parseStoredMemory(v)
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
		if memoriesGCFlag {
			deleted, err := pruneExpiredMemories(now, nil)
			if err != nil {
				FatalErrorRespectJSON("%v", err)
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
			outputJSON(payload)
			return
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
			return
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
	GroupID: "setup",
	Args:    cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		CheckReadonly("forget")

		if err := ensureDirectMode("forget requires direct database access"); err != nil {
			FatalError("%v", err)
		}

		key := args[0]
		storageKey := kvPrefix + memoryPrefix + key

		ctx := rootCtx

		// Check if it exists first
		existing, _ := store.GetConfig(ctx, storageKey)
		if existing == "" {
			if jsonOutput {
				outputJSON(map[string]string{
					"key":   key,
					"found": "false",
				})
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "No memory with key %q\n", key)
			os.Exit(1)
		}

		if err := store.DeleteConfig(ctx, storageKey); err != nil {
			FatalErrorRespectJSON("forgetting memory: %v", err)
		}
		if _, err := store.CommitPending(ctx, getActor()); err != nil {
			WarnError("failed to commit forget: %v", err)
		}

		if jsonOutput {
			outputJSON(map[string]string{
				"key":     key,
				"deleted": "true",
			})
		} else {
			fmt.Printf("Forgot [%s]: %s\n", key, truncateMemory(existing, 80))
		}
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
	GroupID: "setup",
	Args:    cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if err := ensureDirectMode("recall requires direct database access"); err != nil {
			FatalError("%v", err)
		}

		key := args[0]
		storageKey := kvPrefix + memoryPrefix + key

		ctx := rootCtx
		value, err := store.GetConfig(ctx, storageKey)
		if err != nil {
			FatalErrorRespectJSON("recalling memory: %v", err)
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
				"found": value != "",
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
			outputJSON(result)
			if value == "" {
				os.Exit(1)
			}
		} else {
			if value == "" {
				fmt.Fprintf(os.Stderr, "No memory with key %q\n", key)
				os.Exit(1)
			}
			fmt.Printf("%s\n", content)
		}
	},
}

// truncateMemory shortens a string to maxLen for display.
func truncateMemory(s string, maxLen int) string {
	// Replace newlines with spaces for single-line display
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func init() {
	rememberCmd.Flags().StringVar(&memoryKeyFlag, "key", "", "Explicit key for the memory (auto-generated from content if not set). If a memory with this key already exists, it will be updated in place")
	rememberCmd.Flags().BoolVar(&memoryNoDedupFlag, "no-dedup", false, "Disable fork-only auto-key content dedup (always create a new key from slugify even if normalized content matches an existing memory)")
	rememberCmd.Flags().StringVar(&memoryValidForFlag, "valid-for", "", "Relative validity window for this memory (e.g. 30d, 2w, 1y, 72h). Mutually exclusive with --valid-until.")
	rememberCmd.Flags().StringVar(&memoryValidUntilFlag, "valid-until", "", "Absolute expiration timestamp (YYYY-MM-DD or RFC3339). Mutually exclusive with --valid-for.")
	rememberCmd.Flags().StringVar(&memoryExpirePolicyFlag, "expire-policy", "", "What to do after expiration: hide (default), notify, delete")

	memoriesCmd.Flags().BoolVar(&memoriesIncludeExpired, "include-expired", false, "Include memories whose fact validity window has expired")
	memoriesCmd.Flags().BoolVar(&memoriesGCFlag, "gc", false, "Delete expired memories with expire-policy=delete")

	rootCmd.AddCommand(rememberCmd)
	rootCmd.AddCommand(memoriesCmd)
	rootCmd.AddCommand(forgetCmd)
	rootCmd.AddCommand(recallCmd)
}
