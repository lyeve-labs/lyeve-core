package domain

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// KnownFieldTypes is the set of valid field_type values for schema fields.
// Every value maps 1:1 to a column type in the schema engine's DDL generation. Unknown values must be
// rejected before they reach the DDL path so users get feedback instead of a
// silent fallback to TEXT.
var KnownFieldTypes = map[string]bool{
	"uid":       true,
	"text":      true,
	"rich_text": true,
	"email":     true,
	"url":       true,
	"media":     true,
	"number":    true,
	"boolean":   true,
	"date":      true,
	"datetime":  true,
	"json":      true,
	"relation":  true,
}

// validIdentRe matches safe SQL identifiers: start with letter or underscore,
// followed by letters, digits, or underscores. Max 63 chars (PG identifier limit).
var validIdentRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

const maxIdentifierLen = 63

// ValidateIdentifier returns an error if name is not a safe SQL identifier.
// Safe identifiers match [a-zA-Z_][a-zA-Z0-9_]* and are at most 63 chars.
// This is the primary defense against DDL injection: callers MUST validate
// before any identifier enters DDL. Quoting (QuoteIdentifier) is defense-in-depth.
func ValidateIdentifier(name string) error {
	if name == "" {
		return fmt.Errorf("%w: identifier must not be empty", ErrValidation)
	}
	if len(name) > maxIdentifierLen {
		return fmt.Errorf("%w: identifier %q exceeds %d character limit", ErrValidation, name, maxIdentifierLen)
	}
	if !validIdentRe.MatchString(name) {
		return fmt.Errorf("%w: identifier %q contains invalid characters (allowed: [a-zA-Z0-9_], must start with letter or underscore)", ErrValidation, name)
	}
	return nil
}

// allowedFieldTypes lists KnownFieldTypes in a stable order for error messages.
var allowedFieldTypes = []string{
	"uid", "text", "rich_text", "email", "url", "media",
	"number", "boolean", "date", "datetime", "json", "relation",
}

// validateFieldType returns an error if fieldType is not in KnownFieldTypes.
//
// The empty string is rejected rather than waved through. A request that spells
// the key anything but field_type leaves the type empty, and DDL generation
// falls back to text, so the typo silently lands a text column where the schema
// meant to declare a number, a date or a relation.
func validateFieldType(fieldType string) error {
	allowed := strings.Join(allowedFieldTypes, ", ")
	if fieldType == "" {
		return fmt.Errorf("%w: field_type is required (allowed: %s)", ErrValidation, allowed)
	}
	if !KnownFieldTypes[fieldType] {
		return fmt.Errorf("%w: unknown field_type %q (allowed: %s)", ErrValidation, fieldType, allowed)
	}
	return nil
}

// ValidateSchema checks that all identifiers in a schema definition are safe for
// use in DDL. This is the primary defense against DDL injection: every call site
// that accepts a schema definition from outside the engine must call this before
// the identifiers reach any DDL generation path.
//
// For System fields the name is still validated (System governs editability and
// column generation, not identifier safety), but relation attributes are skipped
// because legitimate System fields (auto-generated FK columns, timestamps) never
// carry relation metadata.
func ValidateSchema(s *Schema) error {
	if err := ValidateIdentifier(s.Name); err != nil {
		return fmt.Errorf("schema name: %w", err)
	}
	for _, f := range s.Fields {
		if err := ValidateIdentifier(f.Name); err != nil {
			return fmt.Errorf("field %q: %w", f.Name, err)
		}
		if f.System {
			continue
		}
		if err := validateFieldType(f.FieldType); err != nil {
			return fmt.Errorf("field %q: %w", f.Name, err)
		}
		if why := core.LocalizedRefusal(f); why != "" {
			return fmt.Errorf("field %q: %w: %s", f.Name, ErrValidation, why)
		}
		if why := core.IndexRefusal(f); why != "" {
			return fmt.Errorf("field %q: %w: %s", f.Name, ErrValidation, why)
		}
		if f.RelationTo != "" {
			if err := ValidateIdentifier(f.RelationTo); err != nil {
				return fmt.Errorf("field %q relation_to: %w", f.Name, err)
			}
		}
		if f.RelationFKName != "" {
			if err := ValidateIdentifier(f.RelationFKName); err != nil {
				return fmt.Errorf("field %q relation_fk_name: %w", f.Name, err)
			}
		}
		if f.RelationThrough != "" {
			if err := ValidateIdentifier(f.RelationThrough); err != nil {
				return fmt.Errorf("field %q relation_through: %w", f.Name, err)
			}
		}
	}
	for _, rule := range s.CrossFieldValidation {
		if why := core.CompareRuleRefusal(s, rule); why != "" {
			return fmt.Errorf("cross_field_validation: %w: %s", ErrValidation, why)
		}
	}
	if err := s.Transports.Validate(); err != nil {
		return fmt.Errorf("transports: %w", err)
	}
	return nil
}

// The validation shape types live in pkg/core beside Schema, because they are
// part of a definition rather than part of validating one.

// ValidationRule is a single field-level constraint.
type ValidationRule = core.ValidationRule

// CrossFieldRule is a validation rule that references multiple fields.
type CrossFieldRule = core.CrossFieldRule

// CustomValidator is a user-defined validation function registered at runtime.
type CustomValidator = core.CustomValidator
