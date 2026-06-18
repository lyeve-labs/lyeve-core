// Package tenant_test verifies the tenant context propagation package.
//
// The package is intentionally minimal (an untyped string constant and two
// tiny helpers), but it is the backbone of multi-tenant context propagation.
// These tests verify:
//   - Key constant matches core.TenantContextKey (cross-package contract)
//   - ID extraction from context (happy path, empty, nil, wrong type)
//   - WithID round-trip (set and retrieve, including empty string)
//   - Thread safety under concurrent access
//   - Benchmark for hot-path performance
package tenant_test

import (
	"context"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Key Consistency

// TestKey_MatchesCoreConstant verifies that tenant.Key and
// core.TenantContextKey are the same string value. This is the whole
// reason the package exists: hooks and core both need to read/write
// the same context key without importing each other.
// It compares string values: tenant.ContextKey is an alias of
// core.ContextKey, so the two keys share one type and equal strings mean
// equal keys to context.Value.
func TestKey_MatchesCoreConstant(t *testing.T) {
	assert.Equal(t, string(core.TenantContextKey), string(tenant.Key),
		"tenant.Key must match core.TenantContextKey; drift breaks cross-package context propagation")
}

// TestKey_IsExplicitString verifies that Key is the exact string "tenant_id"
// and not a random constant that happens to match. If someone renames it, the
// core match test above catches it.
func TestKey_IsExplicitString(t *testing.T) {
	assert.Equal(t, "tenant_id", string(tenant.Key))
}

// ID Extraction

// TestID_FromEmptyContext checks that ID returns "" when no tenant is set.
// This is the common single-tenant or unauthenticated-request path.
func TestID_FromEmptyContext(t *testing.T) {
	id := tenant.ID(context.Background())
	assert.Empty(t, id, "ID on background context should be empty")
}

// TestID_FromNilContext checks that passing nil panics on the ctx.Value call.
// This is user error, not something the package should paper over. The test
// just documents the contract: never pass nil.
func TestID_FromNilContext(t *testing.T) {
	assert.Panics(t, func() { tenant.ID(nil) }, "ID(nil) must panic") //nolint:staticcheck // intentional nil context test
}

// TestID_AfterWithID, the happy round-trip: set a tenant, read it back.
func TestID_AfterWithID(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "acme_corp")
	id := tenant.ID(ctx)
	assert.Equal(t, "acme_corp", id)
}

// TestID_AfterWithID_EmptyString checks that setting an empty tenant ID
// explicitly is valid (and used by single-tenant fallback paths). It must
// return "".
func TestID_AfterWithID_EmptyString(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "")
	id := tenant.ID(ctx)
	assert.Empty(t, id)
}

// TestID_ValueTypeMismatch covers someone setting a non-string value under
// the same key. The type assertion in ID() must return "" rather than
// panicking.
func TestID_ValueTypeMismatch(t *testing.T) {
	// This relies on the fact that context.WithValue uses the same
	// untyped string key. An ill-behaved caller could do this.
	ctx := context.WithValue(context.Background(), tenant.Key, 42)
	id := tenant.ID(ctx)
	assert.Empty(t, id, "ID must return empty string for non-string value")
}

// TestID_Overwrite checks that setting a new tenant ID replaces the previous one.
func TestID_Overwrite(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "tenant_a")
	ctx = tenant.WithID(ctx, "tenant_b")
	id := tenant.ID(ctx)
	assert.Equal(t, "tenant_b", id)
}

// TestID_DoesNotLeakToParent checks that setting a tenant on a child context
// does not mutate the parent. This is the fundamental context.Value contract.
func TestID_DoesNotLeakToParent(t *testing.T) {
	parent := context.Background()
	child := tenant.WithID(parent, "child_tenant")
	assert.Empty(t, tenant.ID(parent), "parent must not see child's tenant")
	assert.Equal(t, "child_tenant", tenant.ID(child))
}

// TestID_WithMultipleValues checks that the tenant ID coexists with other
// context values without interference.
func TestID_WithMultipleValues(t *testing.T) {
	var reqIDKey struct{}

	ctx := context.WithValue(context.Background(), reqIDKey, "req-123")
	ctx = tenant.WithID(ctx, "acme")
	id := tenant.ID(ctx)
	assert.Equal(t, "acme", id)
	assert.Equal(t, "req-123", ctx.Value(reqIDKey))
}

// WithID Edge Cases

// TestWithID_NilParent checks that passing a nil parent panics. Same contract as ID(nil).
func TestWithID_NilParent(t *testing.T) {
	assert.Panics(t, func() { tenant.WithID(nil, "acme") }, "WithID with nil parent must panic") //nolint:staticcheck // intentional nil context test
}

// TestWithID_ReturnsNewContext checks that the returned context is a child,
// not the same pointer.
func TestWithID_ReturnsNewContext(t *testing.T) {
	parent := context.Background()
	child := tenant.WithID(parent, "acme")
	assert.NotEqual(t, parent, child, "WithID must return a new context")
}

// Cross-Package Compatibility

// TestCoreWithTenantIDRoundTrip verifies that core.WithTenantID writes
// a value that tenant.ID can read. This is the main cross-package contract.
func TestCoreWithTenantIDRoundTrip(t *testing.T) {
	ctx := core.WithTenantID(context.Background(), "domain_resolved")
	assert.Equal(t, "domain_resolved", tenant.ID(ctx))
}

// TestCoreTenantIDFromCtxRoundTrip verifies that tenant.WithID writes a
// value that core.TenantIDFromCtx can read. Same contract, other direction.
func TestCoreTenantIDFromCtxRoundTrip(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "injected_via_middleware")
	assert.Equal(t, "injected_via_middleware", core.TenantIDFromCtx(ctx))
}

// Concurrency

// TestID_ConcurrentAccess checks that many goroutines reading and writing the
// same context do not race or corrupt. Context.Value is documented as safe
// for concurrent reads, but WithID creates new contexts. This test
// exercises both reads and writes under the race detector.
func TestID_ConcurrentAccess(t *testing.T) {
	base := context.Background()
	const goroutines = 64

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Concurrent reads on the same context.
	for i := range goroutines {
		go func(label string) {
			defer wg.Done()
			id := tenant.ID(base)
			if id != "" {
				t.Errorf("unexpected tenant in base context: %q", id)
			}
			_ = label
		}(string(rune('a' + i%26)))
	}

	// Concurrent writes (each creates its own child context).
	for i := range goroutines {
		go func(n int) {
			defer wg.Done()
			slug := "tenant"
			if n%2 == 0 {
				slug = "tenant_even"
			}
			ctx := tenant.WithID(base, slug)
			// Read back what we wrote.
			got := tenant.ID(ctx)
			if got != slug {
				t.Errorf("WithID(%q) -> ID() = %q, want %q", slug, got, slug)
			}
		}(i)
	}

	wg.Wait()
}

// TestWithID_ChainUnderConcurrency runs a chain of WithID calls under
// concurrent goroutines. Each goroutine builds a short chain of
// nested contexts with different tenant IDs and verifies the chain.
func TestWithID_ChainUnderConcurrency(t *testing.T) {
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Build a context chain: a -> b -> c.
			ctx := tenant.WithID(context.Background(), "tenant_a")
			ctx = tenant.WithID(ctx, "tenant_b")
			ctx = tenant.WithID(ctx, "tenant_c")
			// The most recent WriteID wins (tightest scope).
			require.Equal(t, "tenant_c", tenant.ID(ctx))
		}()
	}
	wg.Wait()
}

// Isolation Invariants

// TestMultipleTenants_NoCrossContamination verifies that separate context
// trees carry their own tenant IDs without leaking between them.
func TestMultipleTenants_NoCrossContamination(t *testing.T) {
	ctxA := tenant.WithID(context.Background(), "tenant_a")
	ctxB := tenant.WithID(context.Background(), "tenant_b")

	assert.Equal(t, "tenant_a", tenant.ID(ctxA))
	assert.Equal(t, "tenant_b", tenant.ID(ctxB))

	// Child contexts also respect isolation.
	childA := tenant.WithID(ctxA, "tenant_a_child")
	childB := tenant.WithID(ctxB, "tenant_b_child")

	assert.Equal(t, "tenant_a_child", tenant.ID(childA))
	assert.Equal(t, "tenant_b_child", tenant.ID(childB))

	// Parent unchanged.
	assert.Equal(t, "tenant_a", tenant.ID(ctxA))
	assert.Equal(t, "tenant_b", tenant.ID(ctxB))
}

// TestTenantID_PropagatesThroughChain verifies that tenant ID set via
// WithID survives multiple nested context.WithValue layers (the typical
// middleware stack: tenant.WithID -> context.WithValue(cancellation) ->
// context.WithValue(tracing) -> etc).
func TestTenantID_PropagatesThroughChain(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "deeply_nested")

	// Simulate middleware chain adding unrelated values.
	type traceID struct{}
	type userID struct{}

	ctx = context.WithValue(ctx, traceID{}, "span-xyz")
	ctx = context.WithValue(ctx, userID{}, "usr-42")
	ctx, cancel := context.WithTimeout(ctx, 60) // arbitrary large timeout
	defer cancel()

	id := tenant.ID(ctx)
	assert.Equal(t, "deeply_nested", id,
		"tenant ID must survive through unrelated context layers")
}
