// Package middleware provides pluggable HTTP middleware for both CMS routers.
// Each function returns a chi-compatible middleware (func(http.Handler) http.Handler).
//
// Usage in cmd/lyeve/main.go:
//
//	import apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
//
//	router := api.NewAPIRouter(pool, cfg, hookReg,
//	    api.WithMiddleware(
//	        apimw.RateLimiter(50, 100),
//	        apimw.ResponseTime(),
//	    ),
//	)
package middleware

import (
	"context"
	"crypto/subtle"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// StripForwardedHeaders removes X-Forwarded-For and X-Real-IP from incoming
// requests. Place FIRST in the middleware chain. Defense-in-depth: ensures
// IP-based logic uses r.RemoteAddr even if future code reads proxy headers.
func StripForwardedHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Real-IP")
			next.ServeHTTP(w, r)
		})
	}
}

// ResponseTime adds an X-Response-Time header with handler duration in microseconds.
func ResponseTime() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			w.Header().Set("X-Response-Time", strconv.FormatInt(time.Since(start).Microseconds(), 10)+"µs")
		})
	}
}

// MaxBodySize limits the request body to maxBytes. Larger bodies receive
// HTTP 413. A maxBytes of 0 disables the limit.
//
// The declared Content-Length is refused before the body is read. Wrapping the
// reader alone only catches a caller who actually sends the bytes: one who
// declares 10 GiB and then sends a kilobyte would leave the handler waiting on
// a body that never arrives, until the server's read timeout closes the
// connection with no response at all. A client cannot tell that from a network
// fault, so it retries: with the same large upload. Reading the header costs
// nothing and turns a hung connection into an answer.
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return MaxBodySizeFor(maxBytes, nil)
}

// MaxBodySizeFor is MaxBodySize with a per-route ceiling for the few routes
// that carry more than an ordinary JSON document. overrides is keyed
// "METHOD /path" and matched exactly.
//
// The global limit is mounted ahead of routing, so it decides before chi knows
// which handler will run. A route that legitimately accepts a larger body,
// such as a bulk import carrying a base64 file, would be refused here whatever
// it declared downstream, and the limit it published would be one it could
// never honor. Matching the exact method and path is enough to lift the
// ceiling for those routes alone: no pattern matching, no wildcards, and every
// other route keeps the global limit unchanged.
func MaxBodySizeFor(maxBytes int64, overrides map[string]int64) func(http.Handler) http.Handler {
	if maxBytes <= 0 && len(overrides) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit := maxBytes
			if len(overrides) > 0 {
				if n, ok := overrides[r.Method+" "+routeKeyPath(r.URL.Path)]; ok && n > limit {
					limit = n
				}
			}
			if limit <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			if r.ContentLength > limit {
				tooLarge(w, r, limit)
				return
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// routeKeyPath normalizes a request path for override lookup: one trailing
// slash is ignored, so "/api/admin/imports/" matches the route declared
// without it, the way chi resolves them.
func routeKeyPath(path string) string {
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		return strings.TrimRight(path, "/")
	}
	return path
}

// BodyLimitKey builds the overrides key for a method and path.
func BodyLimitKey(method, path string) string {
	return strings.ToUpper(method) + " " + routeKeyPath(path)
}

// tooLarge answers a refused body in the envelope every other response uses.
//
// An oversized upload is one of the commonest refusals an API issues, so the
// client has to be able to parse the answer and find its request id, which a
// text/plain body would not give it.
//
// Connection: close because the caller is mid-send: without it the server tries
// to drain a body it has already refused, which for a large upload means
// reading the whole thing to say it was too big.
func tooLarge(w http.ResponseWriter, r *http.Request, maxBytes int64) {
	w.Header().Set("Connection", "close")
	httpx.ErrorReq(w, r, http.StatusRequestEntityTooLarge,
		fmt.Sprintf("Request body exceeds the %d byte limit.", maxBytes))
}

// ContentLengthLimit enforces a per-route-group body-size limit of maxBytes,
// returning HTTP 413. Soft guard for JSON routes (e.g. 1 MiB) under the
// global MaxBodySize hard limit (e.g. 10 MiB for uploads). Never mounted on
// upload routes. Rejects early when Content-Length exceeds maxBytes and wraps
// r.Body in http.MaxBytesReader so chunked bodies cannot bypass the check.
// A maxBytes of 0 disables the limit.
func ContentLengthLimit(maxBytes int64) func(http.Handler) http.Handler {
	if maxBytes <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxBytes {
				tooLarge(w, r, maxBytes)
				return
			}
			// Cap bytes read so chunked bodies (Content-Length -1/0) can't bypass the header check.
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// JSONContentLengthLimit applies the JSON body cap to every request except a
// multipart upload, which is left to the global MaxBodySize hard limit.
//
// ContentLengthLimit wraps r.Body in http.MaxBytesReader, so mounting it on a
// route group that also carries upload routes would cap those uploads at the
// JSON limit. Multipart uploads are left to the global MaxBodySize, so the
// JSON cap never limits an upload route.
func JSONContentLengthLimit(maxBytes int64) func(http.Handler) http.Handler {
	if maxBytes <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	limit := ContentLengthLimit(maxBytes)
	return func(next http.Handler) http.Handler {
		guarded := limit(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isMultipartUpload(r) {
				next.ServeHTTP(w, r)
				return
			}
			guarded.ServeHTTP(w, r)
		})
	}
}

// isMultipartUpload reports whether the request carries a multipart body.
func isMultipartUpload(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && strings.HasPrefix(mt, "multipart/")
}

// IPAllowlist blocks requests from IPs not in the provided CIDR ranges.
// Empty cidrs allows all traffic (no-op). Returns an error for malformed CIDRs.
func IPAllowlist(cidrs []string) (func(http.Handler) http.Handler, error) {
	if len(cidrs) == 0 {
		return func(next http.Handler) http.Handler { return next }, nil
	}

	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("middleware: invalid CIDR %q: %w", cidr, err)
		}
		nets = append(nets, n)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ipStr, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				ipStr = r.RemoteAddr
			}
			ip := net.ParseIP(ipStr)
			if ip == nil {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			for _, n := range nets {
				if n.Contains(ip) {
					next.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		})
	}, nil
}

type tenantKey struct{}

// TenantIDFromCtx retrieves the tenant slug set by TenantHeader.
func TenantIDFromCtx(r *http.Request) string {
	return TenantIDFromContext(r.Context())
}

// TenantIDFromContext retrieves the tenant slug from a context.Context.
// Low-level variant for DB pool hooks that lack *http.Request.
func TenantIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tenantKey{}).(string); ok {
		return v
	}
	return ""
}

// tenantValidate runs each validator against the slug. Returns true when
// the slug is empty or all validators accept it.
func tenantValidate(validators []core.TenantValidatorFunc, ctx context.Context, slug string) bool {
	if slug == "" {
		return true // empty means no override: not a validation failure
	}
	// A slug the engine would never have issued is not a tenant, whatever the
	// roster answers. MySQL and MSSQL collate their string columns case
	// insensitively, so the lookup below returns the row for `acme` when asked
	// for `Acme` and the request then runs under the spelling the caller sent
	// rather than the one the engine stored: a tenant addressable under many
	// names, splitting every structure keyed by it, and behaving differently
	// from PostgreSQL where the same comparison is case sensitive.
	if !core.IsValidTenantSlug(slug) {
		return false
	}
	for _, v := range validators {
		if v == nil {
			continue
		}
		if !v(ctx, slug) {
			return false
		}
	}
	return true
}

// ImplicitTenant is core.DefaultTenantSlug, re-exported so tests and callers in
// this package name the engine's implicit tenant rather than spelling it.
//
// It is a real tenant to every query in the process and to nothing in the
// roster, which is why the header override below compares against the tenant
// that was resolved rather than against the JWT claim.
const ImplicitTenant = core.DefaultTenantSlug

// TenantHeader resolves the active tenant and injects it into the request
// context. No-op when multiTenant is false.
//
// Resolution order (most-trusted first):
//  1. JWT claim tenant_id: set by the auth server, cannot be spoofed.
//  2. X-Tenant-ID header: only honored for super_admin (ops/tooling
//     override). Validated via TenantValidatorFunc args. Invalid slugs
//     receive HTTP 404. A header naming the tenant already resolved in step 1
//     is not an override and is not validated.
//  3. core.TenantContextKey: injected by domain routing when a custom
//     domain resolves to a tenant.
func TenantHeader(multiTenant bool, validators ...core.TenantValidatorFunc) func(http.Handler) http.Handler {
	// super_admin X-Tenant-ID overrides are honored even when multiTenant
	// is false: explicit intent from an authorized caller. JWT and API-key
	// tenant claims are gated behind multiTenant.
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID := ""

			if claims, ok := r.Context().Value(auth.ClaimsKey).(*auth.Claims); ok && claims != nil {
				// JWT tenant claims: always honored. In single-tenant mode,
				// still extracted so RequireTenantID has a tenant context.
				if multiTenant || claims.TenantID != "" {
					tenantID = claims.TenantID
				} else {
					// Single-tenant: the JWT carries no tenant_id claim, so the
					// request runs as the tenant the engine names implicitly,
					// whether or not the token has a subject, the same as an
					// API key.
					tenantID = ImplicitTenant
				}

				// super_admin header override for ops/tooling. A header naming
				// the tenant the request already resolved to asks for nothing,
				// so it skips the roster check: the slug the engine itself just
				// chose cannot be a slug that does not exist.
				if claims.HasRole("super_admin") {
					if h := r.Header.Get("X-Tenant-ID"); h != "" {
						if h != tenantID {
							// Check the override against the roster. With no
							// roster registered the shape check still runs, so a
							// slug the engine could never have issued is refused
							// either way.
							if !tenantValidate(validators, r.Context(), h) {
								w.Header().Set("Content-Type", "application/json")
								w.WriteHeader(http.StatusNotFound)
								fmt.Fprintf(w, `{"error":"tenant not found"}`)
								return
							}
						}
						tenantID = h
					}
				}
			}

			// API key claims. Stored at a distinct context key (typed, no
			// collision with JWT claims). An API key's tenant claim resolves
			// exactly like a JWT's, on single-tenant and multi-tenant installs
			// alike.
			if tenantID == "" {
				if ac := core.GetClaims(r.Context()); ac != nil {
					if multiTenant || ac.TenantID != "" {
						tenantID = ac.TenantID
					} else {
						tenantID = ImplicitTenant
					}
					for _, role := range ac.Roles {
						if role == "super_admin" {
							if h := r.Header.Get("X-Tenant-ID"); h != "" && h != tenantID {
								if !tenantValidate(validators, r.Context(), h) {
									w.Header().Set("Content-Type", "application/json")
									w.WriteHeader(http.StatusNotFound)
									fmt.Fprintf(w, `{"error":"tenant not found"}`)
									return
								}
								tenantID = h
							} else if h := r.Header.Get("X-Tenant-ID"); h != "" {
								tenantID = h
							}
							break
						}
					}
				}
			}

			// Domain-routing fallback (custom domains), for a request that
			// carries no credential and for a super admin. A credential names
			// its own tenant above, and an account whose credential names none
			// is misconfigured: taking the host's tenant would hand it the
			// tenant of whatever domain it was used on, so it falls through to
			// the refusal below. A super admin on a mapped domain works in that
			// domain's tenant, and this key has to agree with the core key the
			// host resolver already set, which handlers and plugins read.
			// Leaving it empty would split one request across two scopes, and
			// the archived-tenant guard would read the empty one.
			if tenantID == "" && (!hasCredential(r.Context()) || isSuperAdmin(r.Context())) {
				tenantID = core.TenantIDFromCtx(r.Context())
			}

			// Unauthenticated request. Public plugin routes are mounted in
			// this same chain and carry no credential of any kind, so every
			// branch above resolved nothing. Their stores still gate on
			// core.RequireTenantID, correctly: sys_* tables are shared and
			// isolated by column, so that guard is the only thing keeping a
			// query inside one tenant.
			//
			// A single-tenant install has exactly one answer, so give it. That
			// is the same slug an authenticated request on the same install
			// already resolved to, so a public read matches the rows the admin
			// API wrote.
			//
			// A multi-tenant install is deliberately left unresolved here. The
			// only thing an anonymous caller could offer is X-Tenant-ID, and
			// honoring it would mean checking the slug against the roster:
			// 200 for a tenant that exists and 404 for one that does not turns
			// every public route into a tenant enumeration oracle, and it
			// would break first-run setup, which has to answer before any
			// tenant exists. Domain routing is the supported answer there, and
			// it has already been consulted above. Without it the request
			// genuinely does not name a tenant and the store is right to
			// refuse.
			if tenantID == "" && !multiTenant && !hasCredential(r.Context()) {
				tenantID = ImplicitTenant
			}

			if tenantID == "" {
				// An authenticated request that resolves to no tenant is
				// refused. The empty tenant is the cross-tenant scope (see
				// core.ResolveTenantScope): a store that branches on it drops
				// its tenant_id predicate, and sys_* tables are isolated by
				// that predicate alone, so the empty scope has to be held
				// rather than arrived at.
				//
				// An account stored with the empty tenant has a JWT that
				// names no tenant. The log names the account, because the
				// fix is to give it a tenant and a bare 403 does not say
				// which account to fix.
				//
				// Not gated on multiTenant. A single-tenant install resolves
				// the implicit tenant for every credential above, so nothing
				// legitimate reaches this point. Anything that does has a
				// credential and no scope, and passing it through would hand it
				// the one view that reads across tenants.
				{
					if claims, ok := r.Context().Value(auth.ClaimsKey).(*auth.Claims); ok && claims != nil && !claims.HasRole("super_admin") {
						slog.WarnContext(r.Context(), "refusing request that resolves to no tenant",
							"user_id", claims.UserID, "path", r.URL.Path,
							"hint", "account carries no tenant_id; set one on its sys_users row")
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, `{"error":"request carries no tenant scope"}`)
						return
					}
					if ac := core.GetClaims(r.Context()); ac != nil && !ac.HasRole("super_admin") {
						slog.WarnContext(r.Context(), "refusing api-key request that resolves to no tenant",
							"path", r.URL.Path,
							"hint", "api key carries no tenant_id; set one on its sys_api_keys row")
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, `{"error":"request carries no tenant scope"}`)
						return
					}
				}
				next.ServeHTTP(w, r)
				return
			}
			// Report it to any middleware that wrapped the whole chain: this
			// context is new, so an outer frame keeps the one it was given and
			// would otherwise record the request as belonging to no tenant.
			core.TenantSlotFrom(r.Context()).Set(tenantID)

			ctx := context.WithValue(r.Context(), tenantKey{}, tenantID)
			// Inject via core key so tracing (which cannot import
			// internal/middleware) can read the tenant.
			ctx = core.WithTenantID(ctx, tenantID)
			// Defense-in-depth: normalize raw header to the validated tenant.
			// Context is canonical. This ensures code reading X-Tenant-ID
			// sees the resolved value, never a client-supplied one.
			r.Header.Set("X-Tenant-ID", tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// isSuperAdmin reports whether the request's credential, a session or a key,
// holds the super_admin role.
func isSuperAdmin(ctx context.Context) bool {
	if claims, ok := ctx.Value(auth.ClaimsKey).(*auth.Claims); ok && claims != nil && claims.HasRole("super_admin") {
		return true
	}
	if ac := core.GetClaims(ctx); ac != nil {
		for _, role := range ac.Roles {
			if role == "super_admin" {
				return true
			}
		}
	}
	return false
}

// hasCredential reports whether the request carried any credential at all: a
// JWT or an API key. It separates "authenticated but scoped to no tenant",
// which is a misconfigured account and is refused below, from "anonymous",
// which is an ordinary public request and is resolved above.
func hasCredential(ctx context.Context) bool {
	if claims, ok := ctx.Value(auth.ClaimsKey).(*auth.Claims); ok && claims != nil {
		return true
	}
	return core.GetClaims(ctx) != nil
}

// TenancyConn stores a LazyTenantConn on the context when a tenant slug is
// present. DB acquisition is deferred until the first query. Requests that
// never touch the database never acquire a pool connection. Cleanup runs in
// a defer only if a conn was acquired. Pass tenancy = nil to disable.
func TenancyConn(database db.DB, tenancy db.Tenancy) func(http.Handler) http.Handler {
	if database == nil || tenancy == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if TenantIDFromContext(r.Context()) == "" {
				next.ServeHTTP(w, r)
				return
			}
			ctx := r.Context()
			lc := db.NewLazyTenantConn(database.SQLDB(), tenancy)
			ctx = db.WithLazyTenantConn(ctx, lc)
			defer func() {
				conn := lc.Acquired()
				if conn == nil {
					return // never touched DB: nothing to clean up
				}
				// Bounded context for cleanup so it runs even when the
				// request context was canceled mid-handler.
				resetCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := tenancy.Reset(resetCtx, conn); err != nil {
					// Close does not close anything: it hands the connection
					// back to the pool with its session state intact. A reset
					// that failed leaves search_path (or the selected database)
					// still naming this tenant, so the next request to borrow
					// this connection reads that tenant's rows under its own
					// credential, and nothing in the request path would notice.
					//
					// Returning ErrBadConn from Raw is what keeps it out of the
					// pool: database/sql closes a connection marked bad rather
					// than reusing it. Losing a connection is the cheap outcome.
					slog.ErrorContext(resetCtx, "tenant isolation reset failed - discarding the connection rather than returning it to the pool",
						"err", err, "tenant", TenantIDFromContext(r.Context()))
					_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				}
				_ = conn.Close() // err suppressed: best-effort cleanup in defer
			}()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ReadOnlyArchived blocks mutating methods (POST/PUT/PATCH/DELETE) when the
// tenant is archived. GET/HEAD/OPTIONS pass through. No-op when fn is nil.
func ReadOnlyArchived(fn core.TenantArchivedFunc) func(http.Handler) http.Handler {
	if fn == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}

			slug := TenantIDFromCtx(r)
			if slug == "" {
				next.ServeHTTP(w, r)
				return
			}

			archived, found := fn(r.Context(), slug)
			if !found || !archived {
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusLocked)
			fmt.Fprintf(w, `{"error":"tenant is archived - read-only"}`)
		})
	}
}

// csrfCookieName is the production cookie. The __Host- prefix is only legal
// with Secure set, and browsers reject a __Host- cookie that lacks it, so a
// plaintext development server has to use the unprefixed name instead. See
// CSRFCookieNameFor.
const csrfCookieName = "__Host-csrf"

// csrfCookieNameInsecure is the development fallback. Production validation
// requires SECURE_COOKIE=true, so a deployment never lands here.
const csrfCookieNameInsecure = "csrf"

const csrfHeaderName = "X-CSRF-Token"

// CSRFCookieNameFor returns the cookie name valid for the connection's scheme.
// Over plain HTTP the __Host- prefix cannot be used: it mandates Secure, and a
// browser discards any __Host- cookie sent without it, so a plaintext server
// uses the unprefixed name.
func CSRFCookieNameFor(secure bool) string {
	if secure {
		return csrfCookieName
	}
	return csrfCookieNameInsecure
}

// csrfCookieValue reads whichever CSRF cookie the server set. Checking both
// keeps the middleware independent of the handler's secure-cookie setting, and
// tolerates a session that outlives a change to it.
func csrfCookieValue(r *http.Request) (string, bool) {
	if c, err := r.Cookie(csrfCookieName); err == nil && c.Value != "" {
		return c.Value, true
	}
	if c, err := r.Cookie(csrfCookieNameInsecure); err == nil && c.Value != "" {
		return c.Value, true
	}
	return "", false
}

// hasSessionCookie reports whether the request carries a session cookie under
// either name. It ignores the secure-cookie setting on purpose: a request is
// held to the double-submit check whenever a browser could have attached a
// session to it, whichever name the authenticator would accept.
func hasSessionCookie(r *http.Request) bool {
	for _, name := range []string{security.SessionCookieName, security.SessionCookieNameInsecure} {
		if _, err := r.Cookie(name); err == nil {
			return true
		}
	}
	return false
}

// csrfSafeMethods is the set of HTTP methods that do not require CSRF validation.
var csrfSafeMethods = map[string]bool{
	"GET":     true,
	"HEAD":    true,
	"OPTIONS": true,
	"TRACE":   true,
}

// CSRFCheck validates CSRF tokens via the double-submit cookie pattern.
// Session-cookie requests must include a matching X-CSRF-Token header.
//
// Skips: safe methods (GET/HEAD/OPTIONS/TRACE), no session cookie (Bearer
// auth, no CSRF risk), no __Host-csrf cookie. Rejects with 403 when session
// cookie is present but the header is missing or mismatches. Uses
// crypto/subtle.ConstantTimeCompare to prevent timing side channels.
func CSRFCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if csrfSafeMethods[r.Method] {
			next.ServeHTTP(w, r)
			return
		}

		// Bearer token auth: no CSRF risk.
		if !hasSessionCookie(r) {
			next.ServeHTTP(w, r)
			return
		}

		cookieValue, ok := csrfCookieValue(r)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"csrf token required"}`))
			return
		}

		csrfHeader := strings.TrimSpace(r.Header.Get(csrfHeaderName))
		if csrfHeader == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"csrf token required"}`))
			return
		}

		if subtle.ConstantTimeCompare([]byte(cookieValue), []byte(csrfHeader)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"csrf token mismatch"}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// healthProbePaths never redirect to HTTPS. Container and orchestrator probes
// speak plaintext to the loopback port and do not follow redirects: the image's
// own HEALTHCHECK requires a literal 200, so redirecting these marks every
// production container permanently unhealthy. Production requires
// SECURE_COOKIE=true, which is what arms the redirect, so this is not a corner
// case. The responses carry probe names, pass or fail, and a failure reason
// only where the probe declared one safe to publish.
var healthProbePaths = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
	"/startup": true,
}

// HTTPSRedirect redirects HTTP to HTTPS via 301. No-op when secureMode is
// false, and never applied to the health probe endpoints. Detects both direct
// TLS and X-Forwarded-Proto for reverse-proxy deployments. Place BEFORE
// SecurityHeaders.
func HTTPSRedirect(secureMode bool) func(http.Handler) http.Handler {
	if !secureMode {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" && !healthProbePaths[r.URL.Path] {
				target := "https://" + r.Host + r.RequestURI
				http.Redirect(w, r, target, http.StatusMovedPermanently) //nolint:gosec // G710 false positive: redirect preserves r.Host (same origin)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
