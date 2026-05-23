package plugin

import (
	"context"
	"log/slog"
	"net/http"
	"sort"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// The accessors below read the roles the runtime hands the routers. A role
// one plugin holds is read from the running plugin that implements its
// interface, and is nil when none does. It is nil as well when two do,
// because CheckRoles refuses that boot. The generic middleware is read by
// phase, through ChainMiddleware.

// MFAStore returns the MFA store of the running plugin that implements
// security.MFAStoreProvider, or nil. Used by the runtime to wire the admin
// router.
func (a *Activator) MFAStore() security.MFAStore {
	if p, ok := owner[security.MFAStoreProvider](a); ok {
		return p.MFAStore()
	}
	return nil
}

// RiskAssessor returns the device risk assessor of the running plugin that
// implements security.DeviceRiskAssessorProvider, or nil.
func (a *Activator) RiskAssessor() security.DeviceRiskAssessor {
	if p, ok := owner[security.DeviceRiskAssessorProvider](a); ok {
		return p.RiskAssessor()
	}
	return nil
}

// BruteForceMiddleware returns the brute-force limiting middleware of the
// running plugin that implements core.BruteForceMiddlewareProvider, or nil.
// The runtime mounts it on both routers.
func (a *Activator) BruteForceMiddleware() func(http.Handler) http.Handler {
	if p, ok := owner[core.BruteForceMiddlewareProvider](a); ok {
		return p.BruteForceMiddleware()
	}
	return nil
}

// CaptchaMiddleware returns the challenge middleware of the running plugin
// that implements core.CaptchaMiddlewareProvider, or nil. The runtime mounts
// it on both routers, where it can challenge a login.
func (a *Activator) CaptchaMiddleware(ctx context.Context) func(http.Handler) http.Handler {
	if p, ok := owner[core.CaptchaMiddlewareProvider](a); ok {
		return p.CaptchaMiddleware(ctx)
	}
	return nil
}

// ChainMiddleware returns the entries the running plugins contribute to
// router's chain through core.ChainMiddlewareProvider, ordered by phase, then
// by plugin name, then in the order a plugin lists them. An entry with no
// middleware is dropped.
//
// core.MiddlewareProvider alone places nothing in the chain, whatever the
// plugin is called, because a plugin may implement it for another purpose.
func (a *Activator) ChainMiddleware(router core.ChainRouter) []core.ChainEntry {
	var out []core.ChainEntry
	for _, cp := range Providers[core.ChainMiddlewareProvider](a) {
		for _, e := range cp.ChainMiddleware() {
			if e.Middleware != nil && e.Routers&router != 0 {
				out = append(out, e)
			}
		}
	}
	// Stable, so the name order the running plugins came in holds within a
	// phase, and so does each plugin's own order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Phase < out[j].Phase })
	return out
}

// RequestSampler returns the request sampling middleware of the running
// plugin that implements core.RequestSamplerProvider, or nil. The runtime
// mounts it on both routers in the one position core.RequestSamplerProvider
// documents, so what the sampler times is the same on every install that runs
// one.
func (a *Activator) RequestSampler() func(http.Handler) http.Handler {
	if p, ok := owner[core.RequestSamplerProvider](a); ok {
		return p.RequestSampler()
	}
	return nil
}

// IdempotencyMiddleware returns the Idempotency-Key enforcement middleware of
// the running plugin that implements core.IdempotencyMiddlewareProvider, or
// nil. The runtime mounts it on both routers after every other chain-level
// middleware and before the routes, the position the interface documents.
func (a *Activator) IdempotencyMiddleware() func(http.Handler) http.Handler {
	if p, ok := owner[core.IdempotencyMiddlewareProvider](a); ok {
		return p.IdempotencyMiddleware()
	}
	return nil
}

// CaptureSink returns the capture sink of the running plugin that implements
// observability.CaptureSinkProvider, or nil. The runtime wires it into the
// WithRequestCapture router option, so the middleware hands intercepted
// request and response pairs to the plugin.
func (a *Activator) CaptureSink() observability.CaptureSink {
	if p, ok := owner[observability.CaptureSinkProvider](a); ok {
		return p.CaptureSink()
	}
	return nil
}

// CapturePolicy returns the capture policy of the running plugin that
// implements core.CapturePolicyProvider, or nil. The runtime wires it into the
// WithCapturePolicy router option, so the capture middleware asks it which
// requests to keep and for how long.
func (a *Activator) CapturePolicy() core.CapturePolicy {
	if p, ok := owner[core.CapturePolicyProvider](a); ok {
		return p.CapturePolicy()
	}
	return nil
}

// TenantRegionResolver returns the tenant region resolver of the running
// plugin that implements core.TenantRegionResolverProvider, or nil. The
// runtime hands the activator itself to the host, which asks this on every
// call, so a plugin reading a tenant's region always reaches the plugin that
// holds the role now, whatever order the plugins started in.
func (a *Activator) TenantRegionResolver() core.TenantRegionResolver {
	if p, ok := owner[core.TenantRegionResolverProvider](a); ok {
		return p.TenantRegionResolver()
	}
	return nil
}

// DSARAuditWriter returns the DSAR audit writer of the running plugin that
// implements compliance.DSARAuditWriterProvider, or nil. The runtime wires it
// into the WithDSARAudit router option, so the GDPR DSAR endpoints record
// audit entries through it.
func (a *Activator) DSARAuditWriter() compliance.DSARAuditWriter {
	if p, ok := owner[compliance.DSARAuditWriterProvider](a); ok {
		return p.DSARAuditWriter()
	}
	return nil
}

// APIKeyAuditLogFn returns the fire-and-forget API key audit function of the
// running plugin that implements core.APIKeyAuditLogProvider, or nil. The
// runtime wires it into the API key audit middleware. The function takes
// (apiKeyID, method, path, status, ip) and must not depend on the request
// context, so an entry survives the request's cancellation.
func (a *Activator) APIKeyAuditLogFn() func(apiKeyID, method, path string, status int, ip string) {
	if p, ok := owner[core.APIKeyAuditLogProvider](a); ok {
		return p.AuditLogFn()
	}
	return nil
}

// APIKeyLookupFn returns the key lookup function of the running plugin that
// implements core.APIKeyLookupProvider, or nil. The runtime wires it into the
// APIKeyAuth middleware. The function takes (ctx, keyHash) and returns
// *AuthClaims or nil.
func (a *Activator) APIKeyLookupFn() func(ctx context.Context, hash string) (*core.AuthClaims, error) {
	if p, ok := owner[core.APIKeyLookupProvider](a); ok {
		return p.KeyLookupFn()
	}
	return nil
}

// APIKeyUpgradeFn returns the fire-and-forget hash-upgrade function of the
// running plugin that implements core.APIKeyUpgradeProvider, or nil. The
// runtime wires it into the API key auth middleware, so a key whose stored
// hash is plain SHA-256 has it rewritten to the peppered HMAC form on first
// use.
// The function takes (ctx, oldHash, newHash) and must outlive the request
// context, so the rewrite survives the request's cancellation.
func (a *Activator) APIKeyUpgradeFn() func(ctx context.Context, oldHash, newHash string) {
	if p, ok := owner[core.APIKeyUpgradeProvider](a); ok {
		return p.UpgradeHashFn()
	}
	return nil
}

// PIIMiddleware returns the response-body masking middleware of the running
// plugin that implements core.PIIMiddlewareProvider, or nil.
func (a *Activator) PIIMiddleware() func(http.Handler) http.Handler {
	if p, ok := owner[core.PIIMiddlewareProvider](a); ok {
		return p.PIIMiddleware()
	}
	return nil
}

// PIILogHandler returns the PII-masking slog handler of the running plugin
// that implements core.PIILogHandlerProvider, or nil. The runtime calls
// slog.SetDefault with the wrapped handler so all log output is sanitized
// before hitting stdout/stderr.
func (a *Activator) PIILogHandler() slog.Handler {
	if p, ok := owner[core.PIILogHandlerProvider](a); ok {
		return p.PIILogHandler()
	}
	return nil
}

// PreAuthMiddleware returns the middleware every running plugin contributes
// through core.PreAuthMiddlewareProvider, for the runtime to mount above JWT
// auth and tenancy on both routers. It returns nil when no running plugin
// implements the capability, so an install without one mounts nothing extra.
//
// Any number of plugins may contribute. They are asked in name order, so two
// plugins that both resolve a tenant mount in the same order on every boot.
func (a *Activator) PreAuthMiddleware() []func(http.Handler) http.Handler {
	var out []func(http.Handler) http.Handler
	for _, provider := range Providers[core.PreAuthMiddlewareProvider](a) {
		for _, mw := range provider.PreAuthMiddleware() {
			if mw != nil {
				out = append(out, mw)
			}
		}
	}
	return out
}
