package core

import (
	"errors"
	"path/filepath"
	"strings"
)

// Sentinel errors
var (
	// ErrNotFound is returned when a resource is not found.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when a resource already exists.
	ErrConflict = errors.New("conflict")
	// ErrForbidden is returned when an action is not permitted.
	ErrForbidden = errors.New("forbidden")
	// ErrUnauth is returned when authentication is required.
	ErrUnauth = errors.New("unauthorized")
	// ErrInvalidPath is returned when a path traversal is detected.
	ErrInvalidPath = errors.New("invalid path")
	// ErrValidation is returned when input fails validation.
	ErrValidation = errors.New("validation error")
	// ErrCapDenied is returned by ScopedHost when a plugin attempts an
	// operation outside its declared capability set. Plugins can
	// check with errors.Is(err, core.ErrCapDenied).
	ErrCapDenied = errors.New("capability denied")

	// ErrServiceUnavailable signals an upstream dependency failure (database
	// unavailable, cache down, KV store unreachable, etc.). Plugins wrap the
	// root cause with this sentinel and the HTTP layer maps it to 503 when
	// the top-level error is errors.Is(err, ErrServiceUnavailable).
	ErrServiceUnavailable = errors.New("service unavailable")

	// ErrNotGranted says the license does not grant the capability an
	// operation needs. A plugin wraps it in a sentinel of its own, and the
	// HTTP layer maps it to 402, so a client learns that the license does not
	// cover the request rather than that the request failed.
	ErrNotGranted = errors.New("capability not granted")
)

// NotGrantedError is a refusal that names what the caller's license lacks. It
// wraps ErrNotGranted, so a reader asking errors.Is still sees a refusal, and
// it carries the plugin and the feature code a 402 body names, so a route the
// engine serves can answer with the same body the plugin's own routes do. The
// engine relays both fields and reads neither.
type NotGrantedError struct {
	// Plugin is the refusing plugin's Name.
	Plugin string
	// Feature is the code a 402 body carries, such as "feature:widgets_export".
	Feature string
}

func (e *NotGrantedError) Error() string {
	return "capability not granted: " + e.Feature
}

// Unwrap lets errors.Is(err, ErrNotGranted) hold for a NotGrantedError.
func (e *NotGrantedError) Unwrap() error { return ErrNotGranted }

// SafeJoin normalizes a relative path under a root directory and rejects
// any path that would escape the root via traversal sequences (e.g. "../").
// It is the canonical path-traversal guard for local storage drivers.
//
// Returns the absolute joined path on success, or an empty string and
// ErrInvalidPath if the resolved path would escape root.
func SafeJoin(root, rel string) (string, error) {
	// Reject an empty key: it would resolve to root itself, which is
	// never a valid storage key and can leak the root directory.
	if rel == "" {
		return "", ErrInvalidPath
	}
	// Clean the root before comparing. filepath.Join cleans its result, so a
	// root written the way operators usually write it ("./uploads", the default
	// for STORAGE_LOCAL_PATH) would never prefix-match the joined path, and
	// every upload would be rejected as a traversal attempt.
	root = filepath.Clean(root)
	clean := filepath.Clean("/" + rel)
	joined := filepath.Join(root, clean)
	// Verify the joined path is still under root by checking the prefix.
	// Append a path separator to root so sibling directories (e.g.
	// "/var/data" vs "/var/data-evil") are rejected. Also allow the
	// joined path to match root exactly (e.g. "." rel).
	// This matches the checkWriteJail pattern in internal/storage/local.go.
	if !strings.HasPrefix(joined, root+string(filepath.Separator)) && joined != root {
		return "", ErrInvalidPath
	}
	return joined, nil
}
