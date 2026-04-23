package main

import "testing"

// TestSanitizeDBName verifies the sanitization logic for database names.
// Lives outside the cgo-tagged store_factory_test.go so the function remains
// testable in nocgo builds (GH#3402 workaround).
func TestSanitizeDBName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my-project", "my_project"},
		{"jtbot-core", "jtbot_core"},
		{"no-hyphens-here", "no_hyphens_here"},
		{"dots.and-hyphens", "dots_and_hyphens"},
		{"already_clean", "already_clean"},
		{"beads", "beads"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeDBName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeDBName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
