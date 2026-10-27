package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateIdentifier_EdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty string", "", true},
		{"63 chars ok", strings.Repeat("a", 63), false},
		{"64 chars rejected", strings.Repeat("a", 64), true},
		{"null byte", "\x00", true},
		{"unicode: german", "täble", true},
		{"unicode: japanese", "日本語", true},
		{"unicode: cyrillic", "спутник", true},
		{"unicode: emoji", "🍕", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateIdentifier(tt.input)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tt.input)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if err != nil && !errors.Is(err, ErrValidation) {
				t.Errorf("error = %v, want ErrValidation", err)
			}
		})
	}
}
