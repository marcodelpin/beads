package workapi

import (
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The single definition of what a write to the workspace's durable settings
// plane is allowed to be, beside BuildListFilter and BuildCountFilter.
//
// It lives here for the reason those do: three implementations of
// issueops.WorkspaceConfig have to agree about it, and it is checkable without
// a database. The conformance contract is left to pin what only a real backend
// can show — that the row and its projection land together.
//
// It is deliberately NOT the whole of `bd config set`. Which SOURCE a key
// belongs to (config.yaml, git config, this plane) is resolved by the front
// door; see issueops/workspaceconfig.go.

// ValidateSettingKey checks a key a caller wants to READ or REMOVE, and
// returns it unchanged.
//
// The only rule at this end is that a key has to name something: an empty key
// answered with the empty value an unset key returns would report "not set"
// for a question nobody asked.
func ValidateSettingKey(key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("%w: config key must not be empty", issueops.ErrValidation)
	}
	return key, nil
}

// ValidateSettingWrite checks a key and value a caller wants to STORE, and
// returns the value as it will be stored, so that a body cannot store a
// different string from the one that was checked.
func ValidateSettingWrite(key, value string) (string, error) {
	if _, err := ValidateSettingKey(key); err != nil {
		return "", err
	}
	// The prefix is owned by bd init --prefix, bd bootstrap and bd
	// rename-prefix. Refused HERE rather than at the front door because `bd
	// config set` is not the only door that reaches this plane: before this
	// role existed `bd config set-many issue_prefix=x` walked past the guard
	// and re-prefixed the workspace.
	if key == issueops.SettingKeyIssuePrefix || key == "issue-prefix" {
		return "", fmt.Errorf("%w: %q is set by bd init --prefix, bd bootstrap or bd rename-prefix, not by a config write: "+
			"storing it here would leave existing ids under the old prefix with nothing to reconcile them",
			issueops.ErrValidation, key)
	}
	// status.custom is PROJECTED into custom_statuses, which reads consult
	// first, so a value that cannot be projected must not become a row.
	// Checking here rather than leaving it to SyncCustomStatusesTable is what
	// makes the refusal a validation error rather than a storage failure.
	if key == issueops.SettingKeyStatusCustom && value != "" {
		if _, err := types.ParseCustomStatusConfig(value); err != nil {
			return "", fmt.Errorf("%w: invalid %s value: %v", issueops.ErrValidation, key, err)
		}
	}
	return value, nil
}
