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

Examples:
  bd remember "always run tests with -race flag"
  bd remember "Dolt phantom DBs hide in three places" --key dolt-phantoms
  bd remember "auth module uses JWT not sessions" --key auth-jwt
  bd remember "feature flag X enabled for beta" --valid-for=30d
  bd remember "TLS cert expires" --valid-until=2026-12-31 --expire-policy=notify
  bd remember "temp workaround for upstream bug" --valid-for=2w --expire-policy=delete`,
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

		// Generate or use provided key
		key := memoryKeyFlag
		if key == "" {
			key = slugify(insight)
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

		if err := store.SetConfig(ctx, storageKey, storedValue); err != nil {
			FatalErrorRespectJSON("storing memory: %v", err)
		}
		if _, err := store.CommitPending(ctx, getActor()); err != nil {
			WarnError("failed to commit memory: %v", err)
		}

		if jsonOutput {
			result := map[string]string{
				"key":    key,
				"value":  insight,
				"action": strings.ToLower(verb),
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
		// new state.
		var gcKeys []string
		if memoriesGCFlag {
			kept := all[:0]
			for _, m := range all {
				if m.expired && m.envelope.effectivePolicy() == policyDelete {
					storageKey := fullPrefix + m.key
					if err := store.DeleteConfig(ctx, storageKey); err != nil {
						FatalErrorRespectJSON("deleting expired memory %q: %v", m.key, err)
					}
					gcKeys = append(gcKeys, m.key)
					continue
				}
				kept = append(kept, m)
			}
			all = kept
			if len(gcKeys) > 0 {
				if _, err := store.CommitPending(ctx, getActor()); err != nil {
					WarnError("failed to commit gc: %v", err)
				}
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
