package domain

import "github.com/lyeve-labs/lyeve-core/pkg/core"

// The validation shape types live in pkg/core beside Schema, because they are
// part of a definition rather than part of validating one.

// ValidationRule is a single field-level constraint.
type ValidationRule = core.ValidationRule

// CrossFieldRule is a validation rule that references multiple fields.
type CrossFieldRule = core.CrossFieldRule

// CustomValidator is a user-defined validation function registered at runtime.
type CustomValidator = core.CustomValidator
