package domain

import (
	"errors"
	"testing"
)

func TestValidateSchema_Valid(t *testing.T) {
	sc := &Schema{
		Name: "articles",
		Fields: []SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "author", FieldType: "relation", RelationTo: "authors", RelationType: RelBelongsTo},
			{Name: "tags", FieldType: "relation", RelationTo: "tags", RelationType: RelManyToMany, RelationThrough: "articles_tags"},
			{Name: "created_at", FieldType: "datetime", System: true},
			{Name: "updated_at", FieldType: "datetime", System: true},
		},
	}
	if err := ValidateSchema(sc); err != nil {
		t.Errorf("ValidateSchema unexpected error: %v", err)
	}
}

func TestValidateSchema_InvalidName(t *testing.T) {
	sc := &Schema{
		Name:   "drop table; --",
		Fields: []SchemaField{{Name: "x", FieldType: "text"}},
	}
	err := ValidateSchema(sc)
	if err == nil {
		t.Fatal("expected error for invalid schema name, got nil")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

func TestValidateSchema_InvalidFieldName(t *testing.T) {
	sc := &Schema{
		Name:   "articles",
		Fields: []SchemaField{{Name: "x' OR 1=1--", FieldType: "text"}},
	}
	err := ValidateSchema(sc)
	if err == nil {
		t.Fatal("expected error for invalid field name, got nil")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

func TestValidateSchema_InvalidRelationTo(t *testing.T) {
	sc := &Schema{
		Name: "articles",
		Fields: []SchemaField{{
			Name: "author", FieldType: "relation", RelationTo: "sink'; DROP--",
		}},
	}
	err := ValidateSchema(sc)
	if err == nil {
		t.Fatal("expected error for invalid relation_to, got nil")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

// ValidateSchema rejects a field with System:true AND malicious relation attributes.
// The validation loop checks every field's name regardless of System. The API layer
// strips System before calling Upsert, but when the engine calls ValidateSchema
// directly, System fields still get their Name validated. Relation attributes are
// skipped because legitimate System fields never carry them.
func TestValidateSchema_SystemFieldNameStillValidated(t *testing.T) {
	sc := &Schema{
		Name: "articles",
		Fields: []SchemaField{
			{Name: "created_at", FieldType: "datetime", System: true},
			{Name: "x' OR 1=1--", FieldType: "text", System: true},
		},
	}
	err := ValidateSchema(sc)
	if err == nil {
		t.Fatal("expected error for invalid System field name, got nil")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

// ValidateSchema does NOT validate RelationTo on System fields (legitimate System
// fields like author_id never carry relation metadata). The API layer strips
// client-supplied System flags before calling this. When the engine calls it,
// the schema is already sanitized by injectFKFields.
func TestValidateSchema_SystemFieldBypassesRelationValidation(t *testing.T) {
	// A field claiming System:true with a malicious relation_to. The Name is
	// validated, but the relation_to is skipped because System governs column
	// generation and System fields never carry relation metadata.
	sc := &Schema{
		Name: "articles",
		Fields: []SchemaField{{
			Name: "safe_name", FieldType: "relation",
			RelationTo: "drop;--", System: true,
		}},
	}
	err := ValidateSchema(sc)
	if err != nil {
		t.Errorf("ValidateSchema unexpected error on System field with malicious relation_to: %v", err)
	}
}

func TestValidateSchema_UnknownFieldType(t *testing.T) {
	sc := &Schema{
		Name:   "articles",
		Fields: []SchemaField{{Name: "title", FieldType: "bogus"}},
	}
	err := ValidateSchema(sc)
	if err == nil {
		t.Fatal("expected error for unknown field type, got nil")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

func TestValidateSchema_InvalidRelationFKName(t *testing.T) {
	sc := &Schema{
		Name: "articles",
		Fields: []SchemaField{{
			Name: "author", FieldType: "relation", RelationTo: "authors",
			RelationFKName: "evi;--l",
		}},
	}
	err := ValidateSchema(sc)
	if err == nil {
		t.Fatal("expected error for invalid relation_fk_name, got nil")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

func TestValidateSchema_InvalidRelationThrough(t *testing.T) {
	sc := &Schema{
		Name: "articles",
		Fields: []SchemaField{{
			Name: "tags", FieldType: "relation", RelationTo: "tags",
			RelationType:    RelManyToMany,
			RelationThrough: "dro;p table--",
		}},
	}
	err := ValidateSchema(sc)
	if err == nil {
		t.Fatal("expected error for invalid relation_through, got nil")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

func TestValidateSchema_SystemFieldWithoutATypeIsAllowed(t *testing.T) {
	sc := &Schema{
		Name:   "articles",
		Fields: []SchemaField{{Name: "created_at", System: true}},
	}
	if err := ValidateSchema(sc); err != nil {
		t.Errorf("system field without a type must stay allowed, got %v", err)
	}
}
