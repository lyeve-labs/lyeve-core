package domain

import (
	"encoding/json"
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

func TestValidateSchema_NullVsEmptyFieldType(t *testing.T) {
	tests := []struct {
		name    string
		fields  []SchemaField
		wantErr bool
	}{
		// A field carrying no type would reach DDL generation, whose default
		// arm makes a text column, so it is refused here like a misspelled
		// type.
		{"empty field_type is refused", []SchemaField{{Name: "title", FieldType: ""}}, true},
		{"valid json field_type", []SchemaField{{Name: "meta", FieldType: "json"}}, false},
		{"optional json with required=false", []SchemaField{{Name: "meta", FieldType: "json", Required: false}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &Schema{Name: "articles", Fields: tt.fields}
			err := ValidateSchema(sc)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestValidateSchema_DeeplyNestedValidationRules(t *testing.T) {
	// ValidateSchema checks identifiers and types, not param structure depth.
	// 100-level nested params should not cause a validation failure.
	inner := map[string]any{"leaf": "value"}
	for range 99 {
		inner = map[string]any{"nested": inner}
	}
	raw, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal deep params: %v", err)
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal deep params: %v", err)
	}

	sc := &Schema{
		Name: "articles",
		Fields: []SchemaField{{
			Name:       "title",
			FieldType:  "text",
			Validation: []ValidationRule{{Rule: "custom", Params: inner}},
		}},
	}
	if err := ValidateSchema(sc); err != nil {
		t.Errorf("unexpected error with 100-level nested params: %v", err)
	}
}

func TestValidateSchema_CircularRelationReferences(t *testing.T) {
	// Cross-schema circular relations are resolved at the DDL/FK level,
	// not by ValidateSchema. A schema referencing another by valid identifier
	// passes validation.
	sc := &Schema{
		Name: "articles",
		Fields: []SchemaField{
			{Name: "author", FieldType: "relation", RelationTo: "authors", RelationType: RelBelongsTo},
			{Name: "co_author", FieldType: "relation", RelationTo: "authors", RelationType: RelBelongsTo},
		},
	}
	if err := ValidateSchema(sc); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
