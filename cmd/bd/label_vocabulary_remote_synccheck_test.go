//go:build cgo

package main

// bda-tfok.4.4 (codex round 1 F8): labels.vocabulary=warn on a backend that
// does not serve the vocabulary registry (the HTTP store) must say the check
// did not run, instead of accepting an undefined label in silence.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

type vocabularyRegistryStore struct {
	storage.DoltStorage
	mode    string
	listErr error
}

func (s *vocabularyRegistryStore) GetConfig(_ context.Context, key string) (string, error) {
	if key == labelsVocabularyConfigKey {
		return s.mode, nil
	}
	return "", nil
}

func (s *vocabularyRegistryStore) ListLabelDefinitions(context.Context) ([]types.LabelDefinition, error) {
	return nil, s.listErr
}

func runVocabularyCheck(t *testing.T, fake *vocabularyRegistryStore) (string, error) {
	t.Helper()
	saveAndRestoreGlobals(t)
	saveStorageMode(t)
	store = fake
	setStoreActive(true)
	var err error
	_, stderr := captureStdio(t, func() { err = checkLabelVocabulary(context.Background(), []string{"undefined-label"}) })
	return stderr, err
}

func TestLabelVocabularyWarnSaysSoWhenRegistryUnsupported(t *testing.T) {
	unsupported := &storage.ErrUnsupported{Op: "ListLabelDefinitions", Backend: "http"}
	for _, mode := range []string{labelsVocabularyWarn, labelsVocabularyEnforce} {
		t.Run(mode, func(t *testing.T) {
			stderr, err := runVocabularyCheck(t, &vocabularyRegistryStore{mode: mode, listErr: unsupported})
			if err != nil {
				t.Errorf("checkLabelVocabulary(%s, registry unsupported) = %v, want nil (fail open)", mode, err)
			}
			if strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, labelsVocabularyConfigKey+"="+mode) ||
				!strings.Contains(stderr, "not checked") {
				t.Errorf("stderr = %q, want exactly one line naming %s=%s and saying the labels were not checked", stderr, labelsVocabularyConfigKey, mode)
			}
		})
	}
}

// Controls: open mode, and a transient (non-unsupported) registry read error,
// stay silent - the documented fail-open contract for a transient read.
func TestLabelVocabularySilentWhenOpenOrTransient(t *testing.T) {
	unsupported := &storage.ErrUnsupported{Op: "ListLabelDefinitions", Backend: "http"}
	for name, fake := range map[string]*vocabularyRegistryStore{
		"open-unsupported": {mode: labelsVocabularyOpen, listErr: unsupported},
		"warn-transient":   {mode: labelsVocabularyWarn, listErr: errors.New("connection reset by peer")},
	} {
		t.Run(name, func(t *testing.T) {
			stderr, err := runVocabularyCheck(t, fake)
			if err != nil || stderr != "" {
				t.Errorf("checkLabelVocabulary = %v, stderr %q, want nil and silence", err, stderr)
			}
		})
	}
}
