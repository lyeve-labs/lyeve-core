// Package uuidx provides UUID parsing helpers for plugin handlers and stores,
// with convenience wrappers around net/http path-parameter extraction.
//
// Deprecated: use reqparse.ParseUUID instead.
package uuidx

import (
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// Parse delegates to uuid.Parse so callers need not import google/uuid.
func Parse(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}

// Param reads name from r's path values and parses it as a UUID.
// Returns an error when the parameter is missing or not a valid UUID.
func Param(r *http.Request, name string) (uuid.UUID, error) {
	raw := r.PathValue(name)
	if raw == "" {
		return uuid.Nil, fmt.Errorf("missing path parameter: %s", name)
	}
	return uuid.Parse(raw)
}
