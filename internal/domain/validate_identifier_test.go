package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateIdentifier_Valid(t *testing.T) {
	tests := []struct {
		name string
	}{
		{"articles"},
		{"_internal"},
		{"my_table_123"},
		{"a"},
		{"_"},
		{"ABC_def_123"},
		{"created_at"},
		{"_status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateIdentifier(tt.name); err != nil {
				t.Errorf("ValidateIdentifier(%q) unexpected error: %v", tt.name, err)
			}
		})
	}
}

func TestValidateIdentifier_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"space", "my table"},
		{"semicolon", "articles; DROP TABLE"},
		{"quote", `art"cles`},
		{"backtick", "art`cles"},
		{"bracket", "art[cles"},
		{"dash", "my-table"},
		{"dot", "my.table"},
		{"leading digit", "1table"},
		{"unicode", "täble"},
		{"sqli", "x'; DROP TABLE users --"},
		{"backslash", `my\ttable`},
		{"null byte", "col\x00name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateIdentifier(tt.input)
			if err == nil {
				t.Errorf("ValidateIdentifier(%q) expected error, got nil", tt.input)
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("ValidateIdentifier(%q) error = %v, want ErrValidation", tt.input, err)
			}
		})
	}
}

func TestValidateIdentifier_MaxLength(t *testing.T) {
	// 63 chars = OK
	name63 := strings.Repeat("a", 63)
	if err := ValidateIdentifier(name63); err != nil {
		t.Errorf("ValidateIdentifier(63 chars) unexpected error: %v", err)
	}
	// 64 chars = too long
	name64 := strings.Repeat("a", 64)
	if err := ValidateIdentifier(name64); err == nil {
		t.Error("ValidateIdentifier(64 chars) expected error, got nil")
	}
}
