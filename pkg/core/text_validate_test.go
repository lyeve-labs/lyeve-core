package core

import "testing"

func TestContainsControlChars(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"plain text", "provider-one", false},
		{"unicode text", "provedor-über-1", false},
		{"emoji", "name 🎯", false},
		{"null byte", "bad\x00name", true},
		{"tab", "bad\tname", true},
		{"newline", "bad\nname", true},
		{"carriage return", "bad\rname", true},
		{"escape", "bad\x1bname", true},
		{"del", "bad\x7fname", true},
		{"empty", "", false},
		{"space is not a control char", "two words", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainsControlChars(tc.in); got != tc.want {
				t.Errorf("ContainsControlChars(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
