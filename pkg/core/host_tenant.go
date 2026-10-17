package core

import "context"

type hostTenantCtxKey struct{}

// WithHostTenantID records the tenant the request's host resolved to before
// any credential was read. The engine's tenant middleware sets it.
func WithHostTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, hostTenantCtxKey{}, tenantID)
}

// HostTenantIDFromCtx returns the tenant the request's host resolved to, or ""
// when the host resolved none.
//
// It can differ from TenantIDFromCtx. A credential names its own tenant, and
// that tenant wins wherever the credential is used, so a session of one tenant
// presented on another tenant's mapped domain acts in the session's tenant. A
// sign-in completed there has to act in the host's tenant, and this is the only
// place a handler can still read which tenant that is.
func HostTenantIDFromCtx(ctx context.Context) string {
	s, _ := ctx.Value(hostTenantCtxKey{}).(string)
	return s
}

type fallbackTenantCtxKey struct{}

// WithFallbackTenantID puts tenantID on the context the way WithTenantID does,
// for a pre-auth resolver that picks a tenant the request's host does not name,
// such as the only tenant an install has. The request acts in that tenant all
// the same. The difference is HostTenantIDFromCtx, which then reports no host
// tenant: the host belongs to no tenant in particular, so a sign-in on it is not
// held to the one the resolver picked.
//
// A resolver that maps the host to its tenant, such as a custom domain, calls
// WithTenantID instead.
func WithFallbackTenantID(ctx context.Context, tenantID string) context.Context {
	ctx = WithTenantID(ctx, tenantID)
	return context.WithValue(ctx, fallbackTenantCtxKey{}, tenantID)
}

// IsFallbackTenant reports whether the tenant on ctx was put there by
// WithFallbackTenantID. A later WithTenantID naming another tenant ends it.
func IsFallbackTenant(ctx context.Context) bool {
	s, _ := ctx.Value(fallbackTenantCtxKey{}).(string)
	return s != "" && s == TenantIDFromCtx(ctx)
}
