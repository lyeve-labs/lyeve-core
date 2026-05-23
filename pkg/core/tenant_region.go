package core

import "context"

// The tenant region slot.
//
// A plugin that decides where a tenant's data lives answers which region a
// tenant belongs to by implementing TenantRegionResolverProvider, which one
// plugin holds. Another plugin that has to honor that answer, such as one
// that picks where a tenant's files are written, reads it through the host:
// type-assert the host to TenantRegionResolverProvider and ask it at the time
// of the operation. The host looks the holder up on every call, so the
// answer follows the plugin that holds the role as it starts and stops, and a
// reader never has to know which plugin that is.
//
// A nil resolver from the host means no running plugin holds the role. The
// reader then falls back to whatever it did without one.
//
// Usage in the plugin that answers:
//
//	var _ core.TenantRegionResolverProvider = (*Plugin)(nil)
//
//	func (p *Plugin) TenantRegionResolver() core.TenantRegionResolver { return p }
//
//	func (p *Plugin) TenantRegion(ctx context.Context, tenantID string) (string, bool, error) {
//	    return p.store.RegionOf(ctx, tenantID)
//	}
//
// Usage in a plugin that reads it:
//
//	if rp, ok := host.(core.TenantRegionResolverProvider); ok {
//	    if r := rp.TenantRegionResolver(); r != nil {
//	        region, assigned, err := r.TenantRegion(ctx, core.TenantIDFromCtx(ctx))
//	        ...
//	    }
//	}

// TenantRegionResolver answers which region a tenant's data belongs in.
type TenantRegionResolver interface {
	// TenantRegion returns the region tenantID is assigned to. tenantID is
	// the tenant as the engine carries it on a request context. assigned is
	// false, with no error, when the tenant has no region. An error means the
	// answer could not be read, which a caller enforcing residency must not
	// read as no region. It must be safe for concurrent use.
	TenantRegion(ctx context.Context, tenantID string) (region string, assigned bool, err error)
}

// TenantRegionResolverProvider is implemented by the plugin that holds the
// tenant region role, and by the host, which hands that plugin's resolver to
// every other plugin. Returning nil means no region is known for any tenant.
type TenantRegionResolverProvider interface {
	TenantRegionResolver() TenantRegionResolver
}
