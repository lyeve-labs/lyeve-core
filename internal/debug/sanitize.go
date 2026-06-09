package debug

import (
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/pii"
)

// SanitizeArgs returns a deep copy of args with each element's string
// representation run through pii.Sanitize to redact PII (email addresses,
// etc.) before serialization in debug reports.
//
// When args is nil, returns nil. The returned slice is always a fresh
// allocation: the caller's original args are never mutated.
func SanitizeArgs(args []any) []any {
	if args == nil {
		return nil
	}
	sanitized := make([]any, len(args))
	for i, arg := range args {
		sanitized[i] = pii.Sanitize(fmt.Sprint(arg))
	}
	return sanitized
}
