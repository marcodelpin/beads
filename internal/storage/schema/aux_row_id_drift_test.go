package schema

import (
	"errors"
	"fmt"
	"testing"
)

// Fork-only (bda-53z): the aux re-key tolerates exactly the dolthub/dolt#11131
// schema-encoding-drift signature and nothing else.
func TestIsEncodingDriftErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			"server 1105 drift panic",
			errors.New(`events: Error 1105 (HY000): panic recovered: invalid hash length: 19`),
			true,
		},
		{
			"wrapped drift panic",
			fmt.Errorf("scan rows of events: %w",
				errors.New("Error 1105 (HY000): panic recovered: invalid hash length: 19")),
			true,
		},
		{"plain sql error", errors.New("Error 1062 (23000): duplicate entry"), false},
		{"connection drop", errors.New("invalid connection"), false},
		{"unrelated panic", errors.New("Error 1105 (HY000): panic recovered: index out of range"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isEncodingDriftErr(c.err); got != c.want {
				t.Fatalf("isEncodingDriftErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
