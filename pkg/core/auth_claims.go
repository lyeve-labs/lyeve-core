package core

import (
	"context"
	"net/http"
)

// ContextKey is a typed string for context.WithValue keys so that
// different context entries (tenant, claims, etc.) don't collide.
// internal/tenant.ContextKey is a type alias to this type.
type ContextKey string

// ClaimsKey is the context key for auth claims. The JWT middleware stores
// claims under this key. Plugins read from it and may write synthetic
// claims (e.g. API key auth).
const ClaimsKey ContextKey = "claims"

// TenantContextKey is the context key for tenant identification injected
// before the TenantHeader middleware runs. A plugin that resolves the tenant
// from the Host header sets it. TenantHeader checks this key as a fallback
// when no JWT-based tenant is present.
const TenantContextKey ContextKey = "tenant_id"

// WithTenantID returns a copy of ctx with the given tenant ID set.
// Plugins (e.g. domain routing) call this to inject the tenant before
// TenantHeader middleware runs. The value is later consumed by
// TenantIDFromCtx or TenantIDFromContext.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, TenantContextKey, tenantID)
}

// TenantIDFromCtx extracts the tenant ID set via WithTenantID or
// TenantHeader middleware. Returns "" when no tenant is present.
func TenantIDFromCtx(ctx context.Context) string {
	v := ctx.Value(TenantContextKey)
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// ResolveTenantScope returns the tenant a request acts in, and whether the
// caller is entitled to the scope it resolved to.
//
// It prefers the context tenant TenantHeader sets, then the JWT claim. An empty
// result is the cross-tenant scope: a store that branches on the tenant being
// empty drops its tenant_id predicate and reads or writes every tenant's rows.
// sys_* tables are not replicated per tenant, so that predicate is the only
// isolation there is, and the empty scope has to be earned rather than fallen
// into. In multi-tenant mode TenantHeader assigns claims.TenantID verbatim, so
// a JWT carrying no tenant claim reaches the store as the empty string, and
// an account whose sys_users.tenant_id was never set carries exactly that,
// because it is the column's default.
//
// Callers that get ok == false must refuse the request. Substituting some
// particular tenant instead is a guess, and on a write path the guess is
// stamped onto the new row.
func ResolveTenantScope(ctx context.Context) (tenantID string, ok bool) {
	if t := TenantIDFromCtx(ctx); t != "" {
		return t, true
	}
	if claims := GetClaims(ctx); claims != nil {
		if claims.TenantID != "" {
			return claims.TenantID, true
		}
		return "", claims.HasRole("super_admin")
	}
	return "", false
}

// AuthClaims carries auth identity for the current request. Plugins that need
// the full JWT claims (including TenantID, MFAPending, etc.) should use
// the internal/auth package. This type covers the fields plugins commonly
// create or consume.
type AuthClaims struct {
	UserID string   `json:"sub"`
	Email  string   `json:"email,omitempty"`
	Roles  []string `json:"roles,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
	// Schemas limits the content schemas the caller may reach. Empty limits
	// nothing, so a session and an API key issued without the list read
	// every schema their roles and scopes allow. AllowsSchema reads it.
	Schemas    []string `json:"schemas,omitempty"`
	TenantID   string   `json:"tenant_id,omitempty"`
	MFAPending bool     `json:"mfa_pending,omitempty"`
	IsAPIKey   bool     `json:"-"`                    // set by APIKeyAuth middleware
	APIKeyID   string   `json:"api_key_id,omitempty"` // set by APIKeyAuth middleware
	TokenType  string   `json:"typ,omitempty"`        // "session", "challenge", or a type a plugin issues

	// AdminTokenID is set when an admin token is the caller. UserID is then
	// the token's owner, Roles the owner's current roles in the token's
	// tenant, and TenantID the tenant the token is bound to.
	AdminTokenID string `json:"admin_token_id,omitempty"`
}

// HasRole reports whether the claims include the given role. Safe to call on nil.
func (c *AuthClaims) HasRole(role string) bool {
	if c == nil {
		return false
	}
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// AllowsSchema reports whether the claims may reach the named content schema.
// An empty Schemas list allows every schema, and a non-empty one allows only
// the names it holds, compared exactly. Nil claims allow nothing.
func (c *AuthClaims) AllowsSchema(name string) bool {
	if c == nil {
		return false
	}
	if len(c.Schemas) == 0 {
		return true
	}
	for _, s := range c.Schemas {
		if s == name {
			return true
		}
	}
	return false
}

// HasScope reports whether the claims grant the requested resource:action pair.
// Supports wildcard scopes: "*:*" grants everything, "content:*" grants all actions
// on content, "*:read" grants read on all resources.
func (c *AuthClaims) HasScope(resource, action string) bool {
	if c == nil {
		return false
	}
	return ScopeGrants(c.Scopes, resource, action)
}

// GetClaims extracts auth claims from a request context. Works with both
// *core.AuthClaims (external plugins) and *auth.Claims (internal code)
// via the AuthClaims() interface method.
func GetClaims(ctx context.Context) *AuthClaims {
	v := ctx.Value(ClaimsKey)
	if ac, ok := v.(*AuthClaims); ok {
		return ac
	}
	type claimsProvider interface {
		AuthClaims() *AuthClaims
	}
	if cp, ok := v.(claimsProvider); ok {
		return cp.AuthClaims()
	}
	return nil
}

// SkipPIIMaskKey is the context key for the request-scoped signal that tells
// a response-masking middleware to detect PII (for the access log) but not
// rewrite the body. The runtime sets it on the admin identity paths
// (/api/admin/users, /api/admin/auth, /api/admin/gdpr, /api/admin/invitations)
// where the email/phone IS the record under administration and masking it
// would leave the console unable to name the account it is acting on.
const SkipPIIMaskKey ContextKey = "skip_pii_mask"

// WithSkipPIIMask returns a copy of ctx flagged so a response-masking
// middleware records access without masking the response body.
func WithSkipPIIMask(ctx context.Context) context.Context {
	return context.WithValue(ctx, SkipPIIMaskKey, true)
}

// SkipPIIMask reports whether a response-masking middleware should record
// access without masking the response body for this request.
func SkipPIIMask(ctx context.Context) bool {
	v, _ := ctx.Value(SkipPIIMaskKey).(bool)
	return v
}

// RequireSuperAdmin is a defense-in-depth check for handlers that already have
// middleware gating. The GroupSuperAdmin middleware enforces super_admin-only
// access, but this second gating protects against internal callers or future
// refactors that could bypass middleware. Handlers for credential mutation
// (OAuth/SAML/SCIM provider config, cert rollover, tenant delete) call this
// before executing their logic.
func RequireSuperAdmin(r *http.Request) bool {
	claims := GetClaims(r.Context())
	return claims != nil && claims.HasRole("super_admin")
}
