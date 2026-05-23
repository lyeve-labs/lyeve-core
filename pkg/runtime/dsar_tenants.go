package runtime

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// dsarTenantLister adapts the plugin's roster enumeration to what the
// compliance package asks for, for a cross-tenant DSAR export.
//
// The roster belongs to whichever plugin supplies core.TenantSlugsFunc, so the
// engine asks for the slugs rather than reading them. Returns nil when nothing
// supplies them, which makes the endpoint refuse a cross-tenant request rather
// than answer a narrower one: an export that covered fewer tenants than it
// claimed would be worse than no export.
func dsarTenantLister(slugs core.TenantSlugsFunc) compliance.TenantLister {
	if slugs == nil {
		return nil
	}
	return func(ctx context.Context) ([]string, error) {
		return slugs(ctx)
	}
}

// dsarTenantScope binds a context to one tenant for the duration of that
// tenant's slice of an export.
//
// This has to go through the engine's own AcquireTenantConn rather than just
// setting the context value. On MySQL and MSSQL a tenant is a separate
// database reached with USE, so a query issued on an unscoped connection reads
// the wrong one however the context is labeled.
func dsarTenantScope(host core.Host) compliance.TenantScopeFunc {
	provider, ok := host.(core.TenancyConnProvider)
	if !ok {
		return nil
	}
	return func(ctx context.Context, tenant string) (context.Context, func(), error) {
		return provider.AcquireTenantConn(ctx, tenant)
	}
}
