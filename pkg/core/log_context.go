package core

import "context"

// LogContextAbout returns the context to log a line about tenant on. A line
// about the context's own tenant keeps it. A line about any other tenant, or
// about every tenant at once (an empty tenant), gets LogContextWithoutTenant,
// because the engine's log handler files every record under its context's
// tenant and that tenant's admins read every record filed under it. Log calls
// only: a query on the returned context can lose its tenant scope.
func LogContextAbout(ctx context.Context, tenant string) context.Context {
	if tenant == TenantIDFromCtx(ctx) {
		return ctx
	}
	return LogContextWithoutTenant(ctx)
}

// LogContextWithoutTenant returns ctx with no tenant, for a line about tenants
// other than the one the request acts in. The engine's log handler then files
// the line under no tenant, so only an operator reading the whole log sees it,
// and the level is the install's rather than one a tenant admin raised. The
// request id and trace stay, so the line still correlates with its request.
// Log calls only: a query on this context loses its tenant scope.
func LogContextWithoutTenant(ctx context.Context) context.Context {
	return WithTenantID(ctx, "")
}
