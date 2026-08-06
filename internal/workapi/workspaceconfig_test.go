package workapi

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/issueops"
)

// The settings-write rules, checked without a database.
//
// The conformance contract pins what only a real backend can show — that a row
// and its projection land together, and that a refusal leaves both alone. What
// is here is the part all three implementations share, so a rule broken here
// would otherwise fail three suites at once.

func TestValidateSettingKeyRefusesOnlyTheEmptyKey(t *testing.T) {
	for _, key := range []string{"", " ", "\t\n"} {
		if _, err := ValidateSettingKey(key); !errors.Is(err, issueops.ErrValidation) {
			t.Errorf("ValidateSettingKey(%q) error = %v, want ErrValidation", key, err)
		}
	}
	// Everything else is a key, including the shapes a front door routes
	// elsewhere: which SOURCE owns a key is not this validator's question.
	for _, key := range []string{
		"custom.anything",
		"status.custom",
		"export.auto", // yaml-only at the front door, but not this plane's call
		"beads.role",  // git config at the front door, likewise
		"  padded  ",  // stored verbatim, untrimmed
		"Mixed.Case",  // no case folding
	} {
		got, err := ValidateSettingKey(key)
		if err != nil {
			t.Errorf("ValidateSettingKey(%q) = %v, want it accepted", key, err)
		}
		if got != key {
			t.Errorf("ValidateSettingKey(%q) returned %q; the key is used verbatim", key, got)
		}
	}
}

func TestValidateSettingWriteRefusesTheProtectedKeyInBothSpellings(t *testing.T) {
	for _, key := range []string{"issue_prefix", "issue-prefix"} {
		_, err := ValidateSettingWrite(key, "bd")
		if !errors.Is(err, issueops.ErrValidation) {
			t.Fatalf("ValidateSettingWrite(%q) error = %v, want ErrValidation", key, err)
		}
		// The message names the commands that DO own the prefix, so a caller is
		// not left hunting for a flag on the verb that just refused them.
		for _, want := range []string{"bd init --prefix", "bd bootstrap", "bd rename-prefix"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ValidateSettingWrite(%q) message %q does not name %q", key, err, want)
			}
		}
	}
}

func TestValidateSettingWriteParsesTheCustomStatusValue(t *testing.T) {
	for _, value := range []string{
		"awaiting_review",
		"awaiting_review:active,awaiting_docs:wip",
		// Empty CLEARS the set, and clearing is not a value to parse. Refusing
		// it here would make the key impossible to unset by writing.
		"",
	} {
		if _, err := ValidateSettingWrite("status.custom", value); err != nil {
			t.Errorf("ValidateSettingWrite(status.custom, %q) = %v, want it accepted", value, err)
		}
	}
	for _, value := range []string{
		"open",              // collides with a built-in status
		"Awaiting Review",   // spaces and capitals are outside the name shape
		"awaiting_review:",  // trailing colon, empty category
		"awaiting:nonsense", // category outside the closed set
	} {
		if _, err := ValidateSettingWrite("status.custom", value); !errors.Is(err, issueops.ErrValidation) {
			t.Errorf("ValidateSettingWrite(status.custom, %q) error = %v, want ErrValidation", value, err)
		}
	}
}

// TestValidateSettingWriteReturnsTheValueUnchanged is the machine half of
// issueops.SetSettingResult.Value's promise: a successful write stores what the
// caller sent. Nothing normalizes today, and this is what would fail if
// something started to.
func TestValidateSettingWriteReturnsTheValueUnchanged(t *testing.T) {
	for _, test := range []struct{ key, value string }{
		{"custom.thing", "  spaced  "},
		{"custom.thing", "a,b,c"},
		{"types.custom", "research,session"},
		{"status.custom", "awaiting_review:active"},
		{"claim.pools", ""},
	} {
		got, err := ValidateSettingWrite(test.key, test.value)
		if err != nil {
			t.Fatalf("ValidateSettingWrite(%q, %q): %v", test.key, test.value, err)
		}
		if got != test.value {
			t.Errorf("ValidateSettingWrite(%q, %q) returned %q, want the value unchanged", test.key, test.value, got)
		}
	}
}
