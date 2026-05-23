package db

import (
	"context"
	"database/sql"
	"testing"
)

// Unit tests: no DB dependency

func TestLazyTenantConn_Acquired_BeforeAcquire(t *testing.T) {
	lt := NewLazyTenantConn(nil, &noopTenancy{})
	if c := lt.Acquired(); c != nil {
		t.Error("Acquired() should return nil before Acquire() is called")
	}
}

func TestWithLazyTenantConn_RoundTrip(t *testing.T) {
	lt := NewLazyTenantConn(nil, &noopTenancy{})
	ctx := WithLazyTenantConn(context.Background(), lt)

	got := LazyTenantConnFromCtx(ctx)
	if got != lt {
		t.Error("LazyTenantConnFromCtx returned unexpected value")
	}
}

func TestLazyTenantConnFromCtx_NoTenant(t *testing.T) {
	if c := LazyTenantConnFromCtx(context.Background()); c != nil {
		t.Error("LazyTenantConnFromCtx should return nil on bare context")
	}
}

func TestTenantConn_NoTenantScope(t *testing.T) {
	conn, err := tenantConn(context.Background())
	if conn != nil || err != nil {
		t.Errorf("tenantConn() = (%v, %v), want (nil, nil)", conn, err)
	}
}

func TestTenantConn_EagerAlreadySet(t *testing.T) {
	// When TenantConn(ctx) already has a conn, tenantConn returns it without
	// touching LazyTenantConn. We test with a nil conn to verify the fast path.
	ctx := WithTenantConn(context.Background(), nil)
	// A nil *sql.Conn stored in context is still "set" - TenantConn returns nil.
	// tenantConn should still hit TenantConn first and get nil.
	conn, err := tenantConn(ctx)
	if conn != nil || err != nil {
		t.Errorf("tenantConn(with nil conn) = (%v, %v), want (nil, nil)", conn, err)
	}
}

// noopTenancy: satisfies Tenancy without any DB calls

type noopTenancy struct{}

func (n *noopTenancy) Apply(_ context.Context, _ *sql.Conn) error { return nil }
func (n *noopTenancy) Reset(_ context.Context, _ *sql.Conn) error { return nil }
