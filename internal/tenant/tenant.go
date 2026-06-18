// Package tenant provides a shared context key and helpers for tenant-aware
// context propagation. It imports pkg/core for the ContextKey type so that
// context.WithValue and ctx.Value work across packages regardless of whether
// the caller uses tenant.Key directly or core.TenantContextKey.
//
// Go's context.Value uses interface{} equality, which compares both type and
// value: bare string("tenant_id") and MyType("tenant_id") are different keys.
// The shared ContextKey type (aliased from core.ContextKey) eliminates the mismatch.
package tenant

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// ContextKey is a type alias to core.ContextKey.
// core.ContextKey is the canonical definition (type ContextKey string).
type ContextKey = core.ContextKey

// Key is the tenant identification context key. Must match
// core.TenantContextKey: verified by TestTenantContextKey in
// internal/tracing.
const Key ContextKey = core.TenantContextKey

// ID extracts the tenant identifier from a context. Returns the empty string
// when no tenant is set (useful for backward-compatible single-tenant paths).
func ID(ctx context.Context) string {
	v := ctx.Value(Key)
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// WithID returns a child context carrying the given tenant identifier.
func WithID(parent context.Context, tenantID string) context.Context {
	return context.WithValue(parent, Key, tenantID)
}
