// Package domain defines the core domain types and the sentinel error set
// (ErrNotFound, ErrConflict, ErrValidation, ...) shared across the engine.
// Handlers map these errors to HTTP status codes.
package domain

import "errors"

// Sentinel errors for domain-layer failures. Handlers map these to HTTP status codes.
var (
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
	ErrForbidden  = errors.New("forbidden")
	ErrUnauth     = errors.New("unauthorized")
	ErrValidation = errors.New("validation error")
	ErrBadRequest = errors.New("bad request")
)
