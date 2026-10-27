package domain

import (
	"fmt"
	"regexp"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

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

// The validation shape types live in pkg/core beside Schema, because they are
// part of a definition rather than part of validating one.

// ValidationRule is a single field-level constraint.
type ValidationRule = core.ValidationRule

// CrossFieldRule is a validation rule that references multiple fields.
type CrossFieldRule = core.CrossFieldRule

// CustomValidator is a user-defined validation function registered at runtime.
type CustomValidator = core.CustomValidator
