package plugin

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Valid names from spec §5.5 plus boundary cases.
		{"a", true},
		{"my-plugin", true},
		{"acme.tools", true},
		{"lint3r", true},
		{"0num", true},
		{"a1.b-c", true},
		{strings.Repeat("a", 64), true},
		// Invalid names from spec §5.5 plus boundary cases.
		{"", false},
		{strings.Repeat("a", 65), false},
		{"My-Plugin", false},
		{"-start", false},
		{"end-", false},
		{".start", false},
		{"end.", false},
		{"has--double", false},
		{"too.many..dots", false},
		{"under_score", false},
		{"spa ce", false},
		{"with/slash", false},
		{"emoji-π", false},
		{"new\nline", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.name), func(t *testing.T) {
			if got := ValidName(tc.name); got != tc.want {
				t.Errorf("ValidName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
