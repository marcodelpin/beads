package main

// Fork seam for `bd remember` (bda-nchm, path c "front-door composition").
//
// rememberCmd's RunE in memory.go is upstream's, through the memoryops role on
// every route, with a handful of hook lines that call into this file. The fork
// adds, on the DIRECT route only:
//
//   - fact validity windows (--valid-for, --valid-until, --expire-policy),
//   - provenance capture (session id, git HEAD, cwd; --no-provenance),
//   - classification (--tag, --scope),
//   - auto-key dedup by content fingerprint (--no-dedup),
//
// all carried in the memory envelope (memory_envelope.go) that the role stores
// verbatim as the memory's content. On the role routes (proxied server, remote
// backend) the fork flags are refused, as before: those routes have no
// envelope transport decided yet (sys-iogddl).
//
// Keeping the fork logic here, behind an upstream RunE, is what keeps memory.go
// close to upstream: memory.go conflicted in 10 of 12 upstream syncs while the
// fork carried its own remember route.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/memoryapi"
	"github.com/steveyegge/beads/memoryops"
)

// errForkRememberFlagsOnRole is the refusal for the fork-only flags on a role
// route (proxied server or remote backend). The sentence is shipped output.
var errForkRememberFlagsOnRole = errors.New("fork-only remember flags (--no-dedup, --no-provenance, --tag, --scope, --valid-for, --valid-until, --expire-policy) are not supported in proxied-server or remote-backend mode yet")

// forkMemoryProvenance captures the provenance a direct-route write records.
// It is a variable so the in-process tests that drive upstream's RunE through
// a fake role can run without it (memory_fork_seams_test.go): those tests pin
// the stored content byte for byte, and provenance is captured automatically.
var forkMemoryProvenance = captureMemoryProvenance

// forkRemember is one `bd remember` invocation's fork state.
type forkRemember struct {
	active   bool   // direct route: the fork envelope path is on
	envelope string // the stored value when an envelope is needed, else ""
	validity bool   // a validity flag was given
	intent   bool   // a fork write-intent flag was given (validity, tags, scope)
	dedup    bool   // the key came from a content-fingerprint match
}

// anyForkRememberFlag reports whether a fork-only remember flag was given.
func anyForkRememberFlag() bool {
	return memoryNoDedupFlag || memoryNoProvenanceFlag || len(memoryTagsFlag) > 0 ||
		memoryScopeFlag != "" || memoryValidForFlag != "" || memoryValidUntilFlag != "" ||
		memoryExpirePolicyFlag != ""
}

// forkRememberPrecheck gives the refusals the fork's routes gave before the
// command guard, in the same order: on a role route the fork flags; on the
// direct route empty content, an explicit key that names nothing, and content
// that derives no key.
func forkRememberPrecheck(insight string) error {
	if usesMemoryRole() {
		if anyForkRememberFlag() {
			return errForkRememberFlagsOnRole
		}
		return nil
	}
	if strings.TrimSpace(insight) == "" {
		return errors.New("memory content cannot be empty")
	}
	if memoryKeyFlag != "" {
		if _, err := memoryapi.ValidateKey(memoryKeyFlag); err != nil {
			return errors.New(strings.TrimPrefix(err.Error(), memoryops.ErrValidation.Error()+": "))
		}
		return nil
	}
	if memoryapi.DeriveKey(insight) == "" && memoryFingerprint(insight) == "" {
		return errors.New("could not generate key from content; use --key to specify one")
	}
	return nil
}

// newForkRemember parses the fork flags and, on the direct route, builds the
// envelope the role will store. It refuses before anything is opened or read.
func newForkRemember(insight string) (*forkRemember, error) {
	f := &forkRemember{}
	if usesMemoryRole() {
		return f, nil
	}
	f.active = true

	var (
		validFor   time.Duration
		validUntil time.Time
	)
	if strings.TrimSpace(memoryValidForFlag) != "" {
		d, err := parseValidFor(memoryValidForFlag)
		if err != nil {
			return nil, fmt.Errorf("invalid --valid-for: %v", err)
		}
		validFor = d
	}
	if strings.TrimSpace(memoryValidUntilFlag) != "" {
		t, err := parseValidUntil(memoryValidUntilFlag)
		if err != nil {
			return nil, fmt.Errorf("invalid --valid-until: %v", err)
		}
		validUntil = t
	}
	if err := validatePolicy(memoryExpirePolicyFlag); err != nil {
		return nil, err
	}
	f.validity = validFor > 0 || !validUntil.IsZero() || memoryExpirePolicyFlag != ""

	// Provenance is captured automatically (bda-97j), so it does NOT count as
	// write intent for the bare-key read path.
	var prov memoryProvenance
	if !memoryNoProvenanceFlag {
		prov = forkMemoryProvenance()
	}
	hasProvenance := prov.SessionID != "" || prov.Commit != "" || prov.Path != ""

	var tags []string
	for _, t := range memoryTagsFlag {
		if s := strings.TrimSpace(t); s != "" {
			tags = append(tags, s)
		}
	}
	scope := strings.TrimSpace(memoryScopeFlag)
	f.intent = f.validity || len(tags) > 0 || scope != ""

	// Plain text stays plain (backward compatibility): an envelope only when
	// something is there to carry.
	if f.validity || hasProvenance || len(tags) > 0 || scope != "" {
		env, err := buildMemoryEnvelope(insight, time.Now(), validFor, validUntil, memoryExpirePolicyFlag, prov, tags, scope)
		if err != nil {
			return nil, err
		}
		f.envelope = env
	}
	return f, nil
}

// writeIntent reports whether a fork flag states that this is a write, which
// takes a bare-key-shaped insight off the read path.
func (f *forkRemember) writeIntent() bool { return f.intent }

// storedContent is the value the role stores: the envelope, or the insight.
func (f *forkRemember) storedContent(insight string) string {
	if f.envelope != "" {
		return f.envelope
	}
	return insight
}

// resolveKey returns the key the write is requested under. Off the direct
// route, or with an explicit --key, it is --key unchanged. Otherwise a
// content-fingerprint match reuses the existing key (dedup), and an envelope
// write names the key derived from the INSIGHT: the role would otherwise
// derive it from the envelope's JSON.
func (f *forkRemember) resolveKey(memories memoryops.Memories, insight string) (string, error) {
	if !f.active || memoryKeyFlag != "" {
		return memoryKeyFlag, nil
	}
	if !memoryNoDedupFlag {
		// Best effort, as on the old direct route: a failed read only means
		// no dedup for this write.
		if listed, err := memories.List(rootCtx, memoryops.ListRequest{}); err == nil {
			if key, ok := findDuplicateMemoryKey(prefixedMemoryPlane(listed.Memories), insight); ok && key != "" {
				f.dedup = true
				return key, nil
			}
		}
	}
	if f.envelope == "" {
		return "", nil
	}
	key := memoryapi.DeriveKey(insight)
	if key == "" {
		return "", errors.New("could not generate key from content; use --key to specify one")
	}
	return key, nil
}

// readKey is the key a bare-key read looks up: the dedup match when there is
// one (the fork's route recalled the memory the content fingerprint named),
// else the read's own key.
func (f *forkRemember) readKey(own, resolved string) string {
	if f.dedup {
		return resolved
	}
	return own
}

// recalledContent is what a bare-key read prints: on the direct route the
// envelope's content, elsewhere the stored value unchanged (as upstream).
func (f *forkRemember) recalledContent(stored string) string {
	if f.active {
		return parseStoredMemory(stored).Content
	}
	return stored
}

// prefixedMemoryPlane turns a role listing (user key -> stored value) into the
// config-table shape findDuplicateMemoryKey reads (kv.memory.<key> -> value).
func prefixedMemoryPlane(plane map[string]string) map[string]string {
	out := make(map[string]string, len(plane))
	for k, v := range plane {
		out[kvPrefix+memoryPrefix+k] = v
	}
	return out
}

// printResult reports the write. Off the direct route it is upstream's
// printRememberResult; on it, the fork's verbs and validity details.
func (f *forkRemember) printResult(verb, key, insight, stored string, commitErr error) error {
	if !f.active {
		return printRememberResult(verb, key, stored, commitErr)
	}
	action := strings.ToLower(verb)
	if f.dedup {
		verb = "Deduped (updated)"
		action = "deduped"
	}
	env := parseStoredMemory(stored)
	if jsonOutput {
		result := map[string]interface{}{
			"key":    key,
			"value":  insight,
			"action": action,
		}
		memoryCommitFields(result, commitErr)
		if f.validity {
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
	if f.validity {
		if env.ValidUntil != "" {
			suffix = fmt.Sprintf(" (valid until %s, policy=%s)", env.ValidUntil, env.effectivePolicy())
		} else if env.ExpirePolicy != "" {
			suffix = fmt.Sprintf(" (policy=%s)", env.effectivePolicy())
		}
	}
	fmt.Printf("%s [%s]: %s%s\n", verb, key, truncateMemory(insight, 80), suffix)
	return nil
}
