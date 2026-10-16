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
