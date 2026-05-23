package core

import (
	"context"
	"log/slog"
	"net/http"
)

// Optional plugin capabilities.
//
// The engine reaches a plugin's optional capability by type assertion: a
// plugin that implements the shape gets wired, one that does not is skipped.
// PreAuthMiddlewareProvider is read from every running plugin, and
// MiddlewareProvider from none. Each other shape here is a role
// one plugin holds: the activator reads it from the running plugin that
// implements it, and two running plugins implementing one stop the boot.
//
// The shapes are named here, and exported, so a plugin proves itself against
// the engine's own declaration. Two private restatements of one contract
// drift apart without any failure, because a plugin that does not implement a
// capability and a plugin that misspells a method are the same silence.
//
// A plugin pins itself with a compile-time assertion against the symbol here:
//
//	var _ core.MiddlewareProvider = (*Plugin)(nil)
//
// so a rename breaks the build in every consumer rather than unwiring one.

// MiddlewareProvider is a plugin's own middleware shape. The engine mounts
// nothing from it: a plugin places middleware in the chain through
// ChainMiddlewareProvider.
type MiddlewareProvider interface {
	Middleware() func(http.Handler) http.Handler
}

// PreAuthMiddlewareProvider is implemented by a plugin that needs to run
// middleware after CORS and before JWT authentication.
//
// It is the counterpart to MiddlewareProvider, which mounts below auth and
// tenancy. The engine mounts what this returns above both, in one fixed
// position, and that position is the capability: a plugin that resolves the
// tenant from the request itself, such as by mapping a custom domain in the
// Host header onto a slug, has to put the answer on the context with
// WithTenantID before TenantHeader looks for it. TenantHeader consults the
// context only after its header and claim branches come up empty, so anything
// mounted below it is too late to be read, and an anonymous request on a
// multi-tenant install names no tenant at all without this.
//
// Returning nil or an empty slice mounts nothing.
//
// Usage:
//
//	pp, ok := plugin.(core.PreAuthMiddlewareProvider)
//	if ok {
//	    mw := pp.PreAuthMiddleware()
//	}
type PreAuthMiddlewareProvider interface {
	// PreAuthMiddleware returns middleware to mount after CORS and before
	// JWT authentication, in the order given.
	PreAuthMiddleware() []func(http.Handler) http.Handler
}

// CaptchaMiddlewareProvider challenges suspected bots on the auth surfaces.
// It takes a context because the challenge threshold is read per request from
// the plugin's own store.
type CaptchaMiddlewareProvider interface {
	CaptchaMiddleware(ctx context.Context) func(http.Handler) http.Handler
}

// BruteForceMiddlewareProvider rate-limits per IP and account across the auth
// surfaces.
type BruteForceMiddlewareProvider interface {
	BruteForceMiddleware() func(http.Handler) http.Handler
}

// PIIMiddlewareProvider masks personal data in responses.
type PIIMiddlewareProvider interface {
	PIIMiddleware() func(http.Handler) http.Handler
}

// PIILogHandlerProvider masks personal data in log records.
type PIILogHandlerProvider interface {
	PIILogHandler() slog.Handler
}

// APIKeyAuditLogProvider records API key use.
type APIKeyAuditLogProvider interface {
	AuditLogFn() func(apiKeyID, method, path string, status int, ip string)
}

// APIKeyLookupProvider resolves a hashed API key to its claims.
type APIKeyLookupProvider interface {
	KeyLookupFn() func(ctx context.Context, hash string) (*AuthClaims, error)
}

// APIKeyUpgradeProvider re-hashes a key that authenticated under an older
// hash parameterization.
type APIKeyUpgradeProvider interface {
	UpgradeHashFn() func(ctx context.Context, oldHash, newHash string)
}
