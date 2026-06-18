// Package tenant_test: table-driven subtests for tenant context helpers.
//
// These tests use the table-driven + t.Run subtest pattern to enumerate all
// exported function paths explicitly. They run alongside the existing
// standalone tests in tenant_test.go without overlap.
package tenant_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Table-driven: tenant.ID

func TestID_TableDriven(t *testing.T) {
	longID := strings.Repeat("a", 4096)
	specialID := "tenant_123-_.~!@#$%^&*()_+=tenant"

	tests := []struct {
		name string
		ctx  context.Context // the input context
		want string          // expected tenant ID
	}{
		{
			name: "happy path - valid tenant string",
			ctx:  tenant.WithID(context.Background(), "acme_corp"),
			want: "acme_corp",
		},
		{
			name: "empty context - background context, no tenant",
			ctx:  context.Background(),
			want: "",
		},
		{
			name: "non-string value under tenant key",
			ctx:  context.WithValue(context.Background(), tenant.Key, 42),
			want: "",
		},
		{
			name: "nil interface value under tenant key",
			ctx:  context.WithValue(context.Background(), tenant.Key, nil),
			want: "",
		},
		{
			name: "explicitly set empty string",
			ctx:  tenant.WithID(context.Background(), ""),
			want: "",
		},
		{
			name: "long tenant ID (4096 chars)",
			ctx:  tenant.WithID(context.Background(), longID),
			want: longID,
		},
		{
			name: "unicode tenant ID with accented characters",
			ctx:  tenant.WithID(context.Background(), "café_ñuño_über"),
			want: "café_ñuño_über",
		},
		{
			name: "tenant ID with special characters",
			ctx:  tenant.WithID(context.Background(), specialID),
			want: specialID,
		},
		{
			name: "context with cancellation - tenant survives cancel",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel() // already canceled
				return tenant.WithID(ctx, "canceled_tenant")
			}(),
			want: "canceled_tenant",
		},
		{
			name: "context with deadline - tenant survives timeout",
			ctx: func() context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), 0)
				defer cancel()
				<-ctx.Done() // consume the deadline signal
				return tenant.WithID(ctx, "deadline_tenant")
			}(),
			want: "deadline_tenant",
		},
		{
			name: "nil context panics - documented contract",
			ctx:  nil, // special case: handled in run loop
			want: "PANIC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.ctx == nil {
				assert.Panics(t, func() { tenant.ID(nil) }, //nolint:staticcheck // intentional nil context test, verifies panic
					"ID(nil) must panic - nil context is programmer error")
				return
			}
			got := tenant.ID(tt.ctx)
			if got != tt.want {
				t.Errorf("tenant.ID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Table-driven: tenant.WithID

func TestWithID_TableDriven(t *testing.T) {
	t.Run("happy path - set and read back", func(t *testing.T) {
		ctx := tenant.WithID(context.Background(), "tracking_team")
		assert.Equal(t, "tracking_team", tenant.ID(ctx))
	})

	t.Run("empty string tenant ID is valid", func(t *testing.T) {
		ctx := tenant.WithID(context.Background(), "")
		assert.Empty(t, tenant.ID(ctx))
	})

	t.Run("returns new distinct context", func(t *testing.T) {
		parent := context.Background()
		child := tenant.WithID(parent, "subtenant")
		assert.NotEqual(t, parent, child,
			"WithID must return a new context object")
	})

	t.Run("does not mutate parent context", func(t *testing.T) {
		parent := context.Background()
		_ = tenant.WithID(parent, "child_only")
		assert.Empty(t, tenant.ID(parent),
			"parent must remain unmodified")
	})

	t.Run("overwrite replaces previous tenant", func(t *testing.T) {
		ctx := tenant.WithID(context.Background(), "first")
		ctx = tenant.WithID(ctx, "second")
		ctx = tenant.WithID(ctx, "third")
		assert.Equal(t, "third", tenant.ID(ctx))
	})

	t.Run("overwrite with empty string clears tenant", func(t *testing.T) {
		ctx := tenant.WithID(context.Background(), "will_be_cleared")
		ctx = tenant.WithID(ctx, "")
		assert.Empty(t, tenant.ID(ctx))
	})

	t.Run("chain through unrelated context layers", func(t *testing.T) {
		type keyA struct{}
		type keyB struct{}
		ctx := context.Background()
		ctx = context.WithValue(ctx, keyA{}, "value_a")
		ctx = tenant.WithID(ctx, "buried_tenant")
		ctx = context.WithValue(ctx, keyB{}, "value_b")
		ctx, cancel := context.WithTimeout(ctx, 60)
		defer cancel()
		assert.Equal(t, "buried_tenant", tenant.ID(ctx))
	})

	t.Run("with unicode tenant ID", func(t *testing.T) {
		ctx := tenant.WithID(context.Background(), "项目_alpha")
		assert.Equal(t, "项目_alpha", tenant.ID(ctx))
	})

	t.Run("with tenant ID containing slashes and dots", func(t *testing.T) {
		ctx := tenant.WithID(context.Background(), "org/repo.git@v1.0")
		assert.Equal(t, "org/repo.git@v1.0", tenant.ID(ctx))
	})

	t.Run("max plausible tenant ID length", func(t *testing.T) {
		id := strings.Repeat("x", 255)
		ctx := tenant.WithID(context.Background(), id)
		assert.Equal(t, id, tenant.ID(ctx))
	})

	t.Run("nil parent panics", func(t *testing.T) {
		assert.Panics(t, func() { tenant.WithID(nil, "acme") }, //nolint:staticcheck // intentional nil context test
			"WithID(nil, ...) must panic")
	})
}

// Table-driven: tenant.Key constant

func TestKey_TableDriven(t *testing.T) {
	t.Run("key is the string tenant_id", func(t *testing.T) {
		assert.Equal(t, "tenant_id", string(tenant.Key))
	})

	t.Run("key matches core.TenantContextKey", func(t *testing.T) {
		assert.Equal(t, string(core.TenantContextKey), string(tenant.Key),
			"drift between tenant.Key and core.TenantContextKey "+
				"breaks cross-package context propagation")
	})
}

// Cross-package round-trips (table-driven)

func TestCrossPackageRoundTrips_TableDriven(t *testing.T) {
	testCases := []struct {
		name     string
		tenantID string
	}{
		{name: "simple identifier", tenantID: "acme_corp"},
		{name: "empty string", tenantID: ""},
		{name: "with hyphens and dots", tenantID: "my-tenant.io"},
		{name: "unicode", tenantID: "über_project"},
		{name: "long (255 chars)", tenantID: strings.Repeat("z", 255)},
	}

	t.Run("core.WithTenantID -> tenant.ID", func(t *testing.T) {
		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				ctx := core.WithTenantID(context.Background(), tc.tenantID)
				assert.Equal(t, tc.tenantID, tenant.ID(ctx))
			})
		}
	})

	t.Run("tenant.WithID -> core.TenantIDFromCtx", func(t *testing.T) {
		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				ctx := tenant.WithID(context.Background(), tc.tenantID)
				assert.Equal(t, tc.tenantID, core.TenantIDFromCtx(ctx))
			})
		}
	})
}

// Concurrency (table-driven)

func TestConcurrency_TableDriven(t *testing.T) {
	t.Run("parallel reads on same context", func(t *testing.T) {
		ctx := tenant.WithID(context.Background(), "concurrent_read")
		var wg sync.WaitGroup
		errs := make(chan string, 64)

		for range 64 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got := tenant.ID(ctx); got != "concurrent_read" {
					errs <- fmt.Sprintf("got %q, want concurrent_read", got)
				}
			}()
		}
		wg.Wait()
		close(errs)

		for e := range errs {
			t.Error(e)
		}
	})

	t.Run("parallel writes - isolated context trees", func(t *testing.T) {
		var wg sync.WaitGroup
		tenants := []string{"alpha", "beta", "gamma", "delta"}

		for _, tid := range tenants {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				ctx := tenant.WithID(context.Background(), id)
				require.Equal(t, id, tenant.ID(ctx))
			}(tid)
		}
		wg.Wait()
	})
}

// Edge cases

func TestEdgeCases_TableDriven(t *testing.T) {
	t.Run("context carrying only WithID is not deadlined", func(t *testing.T) {
		ctx := tenant.WithID(context.Background(), "live_tenant")
		_, hasDeadline := ctx.Deadline()
		assert.False(t, hasDeadline,
			"tenant.WithID must not attach a deadline")
	})

	t.Run("context carrying WithID is not cancelable", func(t *testing.T) {
		ctx := tenant.WithID(context.Background(), "uncancelable")
		select {
		case <-ctx.Done():
			t.Error("tenant.WithID context must not be done immediately")
		default:
			// OK
		}
	})

	t.Run("ID on context with zero-value key", func(t *testing.T) {
		// An empty struct key stores no tenant: makes sure we don't
		// accidentally match on a zero-value when a caller puts an
		// empty struct{} under the string key.
		ctx := context.WithValue(context.Background(), tenant.Key, struct{}{})
		assert.Empty(t, tenant.ID(ctx),
			"struct{} value under tenant.Key must yield empty string")
	})

	t.Run("core.WithTenantID followed by tenant.WithID overwrites", func(t *testing.T) {
		ctx := core.WithTenantID(context.Background(), "from_core")
		ctx = tenant.WithID(ctx, "from_tenant")
		assert.Equal(t, "from_tenant", tenant.ID(ctx))
	})

	t.Run("tenant.WithID followed by core.WithTenantID overwrites", func(t *testing.T) {
		ctx := tenant.WithID(context.Background(), "from_tenant")
		ctx = core.WithTenantID(ctx, "from_core")
		assert.Equal(t, "from_core", core.TenantIDFromCtx(ctx))
	})
}
