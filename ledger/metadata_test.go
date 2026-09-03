package ledger

import (
	"errors"
	"strings"
	"testing"
)

func TestMetadataValidate(t *testing.T) {
	tests := []struct {
		why  string
		m    Metadata
		want error
	}{
		{"empty is valid, emptiness is the caller's problem", Metadata{}, nil},
		{"ordinary", Metadata{"orderId": "ord_1", "provider": "stripe"}, nil},
		{"an empty value is fine, only the key must be present", Metadata{"k": ""}, nil},
		{"at the key limit", full(MaxMetadataKeys), nil},
		{"a key at its length limit", Metadata{strings.Repeat("k", MaxMetadataKeyLength): "v"}, nil},
		{"a value at its length limit", Metadata{"k": strings.Repeat("v", MaxMetadataValueLength)}, nil},

		{"an empty key", Metadata{"": "v"}, ErrEmptyMetadataKey},
		{"one key too many", full(MaxMetadataKeys + 1), ErrTooManyMetadataKeys},
		{"a key one byte too long", Metadata{strings.Repeat("k", MaxMetadataKeyLength+1): "v"}, ErrMetadataKeyTooLong},
		{"a value one byte too long", Metadata{"k": strings.Repeat("v", MaxMetadataValueLength+1)}, ErrMetadataValueTooLong},
	}

	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			err := tc.m.Validate()
			if tc.want == nil {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// an oversized key would otherwise be echoed back in full inside the error it
// caused, which is a way to get a kilobyte of attacker text into a log line.
func TestOversizedKeysAreNotEchoedInFull(t *testing.T) {
	key := strings.Repeat("k", MaxMetadataKeyLength+1)
	err := Metadata{key: "v"}.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the whole key was echoed back: %d bytes", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "...") {
		t.Errorf("expected the key to be truncated: %v", err)
	}
}

func full(n int) Metadata {
	m := Metadata{}
	for i := range n {
		m[string(rune('a'+i%26))+string(rune('0'+i/26))] = "v"
	}
	return m
}
