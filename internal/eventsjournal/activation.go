// Package eventsjournal resolves and applies durable events-journal activation
// to one storage instance.
//
// It is a package rather than a helper inside cmd/bd because two binaries'
// worth of code opens stores that mutate beads: the command surface itself, and
// bd doctor's repair handlers, which live in their own package and cannot
// import package main. Journal coverage has to hold for both — a repair that
// deletes stale issues or imports a JSONL backlog is an ordinary bead mutation,
// and a consumer whose mirror silently diverges because `bd doctor --fix` ran
// is the failure this feature exists to prevent.
//
// Activation is applied by the FACTORIES that construct a store or provider,
// never by the commands that use one. See
// cmd/bd/events_journal_construction_test.go for the structural guard that
// keeps it that way, and the note at the top of cmd/bd/events_journal.go for
// the three ways it was got wrong before.
package eventsjournal

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
)

// ConfigKey is the workspace setting that turns the journal on.
const ConfigKey = "events-journal"

// EnvVar is the environment override for ConfigKey, matching viper's BD_ prefix
// and hyphen-to-underscore mapping.
const EnvVar = "BD_EVENTS_JOURNAL"

// EnabledFor reports activation for the workspace rooted at beadsDir.
//
// Precedence, highest first:
//
//  1. An explicitly set BD_EVENTS_JOURNAL. Environment beats file, as it does
//     everywhere else in bd's configuration, and it is the documented way to
//     turn the journal on for a process without editing a workspace — including
//     a process that writes into several workspaces.
//  2. The TARGET workspace's own config.yaml. The journal records THAT
//     workspace's mutations, so that workspace decides whether they are
//     recorded: a routed `bd create --repo ../other` must neither journal into
//     a project that never asked for it nor skip journaling one that did. The
//     launching workspace's file is deliberately never consulted for a target.
//  3. The default, off.
//
// beadsDir is empty only where no workspace has been resolved yet; there is no
// target to consult, so the process-merged value is the only answer available.
func EnabledFor(beadsDir string) bool {
	if enabled, ok := parseBool(os.LookupEnv(EnvVar)); ok {
		return enabled
	}
	if beadsDir == "" {
		return config.GetBool(ConfigKey)
	}
	if enabled, ok := parseBool(config.WorkspaceYamlValue(beadsDir, ConfigKey)); ok {
		return enabled
	}
	return false
}

func parseBool(raw string, present bool) (bool, bool) {
	if !present {
		return false, false
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, false
	}
	return value, true
}

// ActivateStore applies beadsDir's configured activation to a freshly opened
// store. It is written to be used as a deferred rewrite of a factory's named
// results:
//
//	func newX(...) (s storage.DoltStorage, err error) {
//	    defer func() { s, err = eventsjournal.ActivateStore(beadsDir, s, err) }()
//	    ... open and return ...
//	}
//
// The shape is deliberate. It keeps the open and its activation in ONE function
// body, so "constructs a store" and "activates the journal on it" are the same
// syntactic unit — which is what lets the construction guard check the property
// structurally instead of chasing a call graph. A failed open passes through
// untouched; a failed activation closes the store rather than returning one
// that would mutate unrecorded.
func ActivateStore(beadsDir string, s storage.DoltStorage, err error) (storage.DoltStorage, error) {
	if err != nil || s == nil {
		return s, err
	}
	configurer, _ := storage.UnwrapStore(s).(storage.EventsJournalConfigurer)
	if cfgErr := Apply(configurer, EnabledFor(beadsDir)); cfgErr != nil {
		_ = s.Close()
		return nil, cfgErr
	}
	return s, nil
}

// Apply binds a resolved activation to one storage instance. Activation is per
// instance rather than process-global: a process can hold several plumbings at
// once (a routed create holds two stores), and enabling one must not enable the
// rest.
//
// A nil configurer means the plumbing cannot journal. An enabled workspace
// FAILS there rather than running with a silently empty journal: the point of
// the journal is that a consumer can trust its cursor, and a plumbing that
// records nothing while reporting success is the one outcome that breaks that
// trust invisibly. A disabled workspace accepts any plumbing.
func Apply(configurer storage.EventsJournalConfigurer, enabled bool) error {
	if configurer == nil {
		if enabled {
			return fmt.Errorf("storage backend does not support the events journal")
		}
		return nil
	}
	configurer.SetEventsJournalEnabled(enabled)
	return nil
}
