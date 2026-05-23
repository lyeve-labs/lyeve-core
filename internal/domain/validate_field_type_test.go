package domain

import (
	"errors"
	"testing"
)

func TestValidateFieldType_Valid(t *testing.T) {
	tests := []struct {
		fieldType string
	}{
		{"uid"},
		{"text"},
		{"rich_text"},
		{"email"},
		{"url"},
		{"media"},
		{"number"},
		{"boolean"},
		{"date"},
		{"datetime"},
		{"json"},
		{"relation"},
	}
	for _, tt := range tests {
		t.Run(tt.fieldType, func(t *testing.T) {
			if err := validateFieldType(tt.fieldType); err != nil {
				t.Errorf("validateFieldType(%q) unexpected error: %v", tt.fieldType, err)
			}
		})
	}
}

func TestValidateFieldType_Invalid(t *testing.T) {
	tests := []struct {
		name      string
		fieldType string
	}{
		{"empty", ""},
		{"uppercase_variant", "INTEGER"},
		{"malicious_sqli", "DROP TABLE users; --"},
		{"precision_hint", "NUMERIC(10,2)"},
		{"typo", "booleen"},
		{"unknown", "made_up_float16"},
		{"case_mismatch", "Text"},
		{"relation_typo", "Belongs_To"},
		{"schema", "articles"}, // valid identifier name but not a field type
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFieldType(tt.fieldType)
			if err == nil {
				t.Errorf("validateFieldType(%q) expected error, got nil", tt.fieldType)
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("validateFieldType(%q) error = %v, want ErrValidation", tt.fieldType, err)
			}
		})
	}
}

// The type-to-SQL mapping belongs to the schema engine. This checks the map
// holds no empty key.
func TestKnownFieldTypes_HasNoEmptyKey(t *testing.T) {
	for ft := range KnownFieldTypes {
		if ft == "" {
			t.Error("KnownFieldTypes contains an empty key")
		}
	}
}
