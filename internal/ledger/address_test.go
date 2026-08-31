package ledger

import (
	"slices"
	"testing"
)

func TestValidateAddress(t *testing.T) {
	tests := []struct {
		addr string
		want bool
		why  string
	}{
		{"world", true, "single segment"},
		{"users:alice", true, "two segments"},
		{"users:alice:wallet", true, "three segments"},
		{"a:b", true, "short segments"},
		{"_reserved", true, "underscore first"},
		{"order-1001", true, "hyphen"},
		{"USERS:Alice", true, "case is not constrained"},
		{"users:1234", true, "digits"},

		{"", false, "empty"},
		{":", false, "two empty segments"},
		{"users:", false, "trailing colon leaves an empty segment"},
		{":users", false, "leading colon leaves an empty segment"},
		{"a::b", false, "empty middle segment"},
		{"world ", false, "trailing space, must not be read as world"},
		{" world", false, "leading space"},
		{"users alice", false, "inner space"},
		{"users.alice", false, "dot is not in the charset"},
		{"users/alice", false, "slash is not in the charset"},
		{"users:alice!", false, "punctuation"},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			if got := ValidateAddress(tc.addr); got != tc.want {
				t.Errorf("ValidateAddress(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

func TestSegments(t *testing.T) {
	tests := []struct {
		addr string
		want []string
	}{
		{"world", []string{"world"}},
		{"users:alice", []string{"users", "alice"}},
		{"users:alice:wallet", []string{"users", "alice", "wallet"}},
	}

	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			if got := Segments(tc.addr); !slices.Equal(got, tc.want) {
				t.Errorf("Segments(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
