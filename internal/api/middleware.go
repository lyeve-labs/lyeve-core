package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/metrics"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"go.opentelemetry.io/otel/trace"
)

// dynamicCORSMu guards dynamicCORSProvider, which is set by the runtime when
// a plugin implementing CORSOriginProvider is active (for example, a plugin
// serving custom domains).
var (
	dynamicCORSMu       sync.RWMutex
	dynamicCORSProvider security.CORSOriginProvider
)

// SetDynamicCORSProvider installs or clears the dynamic CORS origin provider.
// Safe to call concurrently. Pass nil to clear.
func setDynamicCORSProvider(p security.CORSOriginProvider) {
	dynamicCORSMu.Lock()
	defer dynamicCORSMu.Unlock()
	dynamicCORSProvider = p
}

// accessLogExemptPaths is the only sanctioned way to keep a path out of the
// access log. It is empty on purpose.
//
// Excluding a route by leaving it above the logging middleware in the chain
// is indistinguishable at a glance from an oversight, and it silently widens:
// everything registered near the omitted route inherits the omission. An
// entry here is a decision someone made and someone else can read, and the
// structural test over the routing tree fails on any path that stops logging
// without appearing in this list.
//
// Health and readiness probes are deliberately NOT here. They are cheap to log
// and are the routes an operator most often needs to prove were answered.
var accessLogExemptPaths = map[string]bool{}

// accessLogExempt reports whether path is excused from the access log.
func accessLogExempt(path string) bool {
	if len(accessLogExemptPaths) == 0 {
		return false
	}
	return accessLogExemptPaths[path]
}

func structuredLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accessLogExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()

		// Tenancy is resolved further down the chain, on a context this frame
		// never sees, so the line reads the tenant from a slot the inner frame
		// fills. Without the slot every request line would carry no tenant,
		// and a tenant-scoped log search would find none of them.
		ctx, tenantSlot := core.WithTenantSlot(r.Context())
		// The route owner is decided at dispatch the same way, so the plugin
		// label on the request series below reads a slot too. Without it
		// every request would be recorded as the engine's own.
		ctx, _ = core.WithPluginSlot(ctx)
		r = r.WithContext(ctx)

		// Track in-flight requests for metrics.
		done := metrics.RecordInFlightStart(r)
		defer done()

		next.ServeHTTP(ww, r)
		dur := time.Since(start)

		// Extract OTel span context for trace correlation.
		span := trace.SpanFromContext(r.Context())
		traceID := span.SpanContext().TraceID().String()
		spanID := span.SpanContext().SpanID().String()

		logCtx := r.Context()
		if tid := tenantSlot.Get(); tid != "" {
			logCtx = core.WithTenantID(logCtx, tid)
		}

		slog.InfoContext(logCtx, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", dur.Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
			"trace_id", traceID,
			"span_id", spanID,
		)

		// Record Prometheus metrics.
		metrics.RecordRequest(r, ww.Status(), dur.Seconds())
	})
}

// CORSConfig holds the configuration for corsMiddleware.
type CORSConfig struct {
	Origins          []string
	AllowCredentials bool
	PreflightMaxAge  int    // seconds, default 3600 (1 hour)
	AllowMethods     string // comma-separated, default "GET, POST, PUT, DELETE, PATCH, OPTIONS"
	AllowHeaders     string // comma-separated, default "Content-Type, Authorization, X-API-Key, X-Tenant-ID, X-Correlation-ID"

	// ExposeHeaders names the response headers a cross-origin script is
	// allowed to read. The Fetch Standard exposes only the CORS-safelisted
	// ones by default, so without this a browser client cannot read the
	// request id it needs for a bug report, nor the rate-limit values it
	// needs to back off with, even though both are on the response.
	// Read from CORS_EXPOSE_HEADERS. Empty omits the header entirely.
	ExposeHeaders string // comma-separated

	// AllowedDomains is a suffix allowlist for dynamic CORS origins supplied
	// by CORSOriginProvider plugins (e.g. custom domains). When empty (the
	// default), ALL dynamic origins are REJECTED (fail-closed). An origin is
	// allowed if its host suffix matches an entry. Example: ["example.com",
	// "my-cms.io"] allows "https://tenant-a.example.com" and
	// "https://app.my-cms.io" but NOT "https://evil.com".
	// Read from the CORS_ALLOWED_DOMAINS env var.
	AllowedDomains []string
}

func corsMiddleware(cfg CORSConfig) func(http.Handler) http.Handler {
	// Validate wildcard+credentials: Fetch Standard forbids credentials with wildcard.
	// If origins contains "*" and credentials are enabled, log a startup warning and
	// ignore the wildcard so the net effect is "allow no origins with credentials."
	hasWildcard := false
	for _, o := range cfg.Origins {
		if o == "*" {
			hasWildcard = true
			break
		}
	}
	if hasWildcard && cfg.AllowCredentials {
		slog.Warn("CORS_ORIGINS contains '*' with credentials enabled, so the wildcard is ignored per the Fetch Standard. " +
			"Use explicit origins when AllowCredentials is true.")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			allOrigins := cfg.Origins
			dynamicCORSMu.RLock()
			dynamicProvider := dynamicCORSProvider
			dynamicCORSMu.RUnlock()
			origin := r.Header.Get("Origin")

			// SECURITY INTENT: Wildcard CORS without credentials is deliberate.
			//
			// When AllowCredentials is false, Access-Control-Allow-Origin: * is safe:
			// the browser will NOT attach cookies, Authorization headers, or TLS
			// client certs to cross-origin requests.  Any site can read the JSON
			// response, but only if the data is already public (unauthenticated
			// API endpoints).  This matches the Fetch Standard §3.2.3 invariant
			// that "*" + credentials is forbidden (enforced above at startup).
			//
			// This is acceptable for LyEve's public-facing API (content delivery,
			// health checks, open endpoints).  If the deployment requires restricting
			// which origins may read even public responses, configure CORS_ORIGINS
			// with explicit origin(s) instead of "*".  When AllowCredentials is true,
			// only explicitly listed origins are reflected back (never "*").
			//
			// Wildcard logic: if "*" is in origins and credentials are not enabled,
			// set Access-Control-Allow-Origin: * and skip Vary: Origin.
			// Per Fetch Standard §3.2.3, Vary: Origin is not needed when using "*".
			useWildcard := hasWildcard && !cfg.AllowCredentials

			if !useWildcard {
				// Per Fetch Standard §3.2.3: when Access-Control-Allow-Origin
				// is not '*' and varies by request, the response MUST include
				// Vary: Origin. Using Add preserves any Vary headers set by
				// downstream middleware (e.g. Accept-Encoding).
				w.Header().Add("Vary", "Origin")
			}

			matched := useWildcard
			if !matched {
				for _, o := range allOrigins {
					if o == origin {
						matched = true
						break
					}
				}
			}
			// A dynamic origin has to clear two gates. The operator's suffix
			// allowlist bounds it to a domain they control, and the provider
			// says it belongs to the same tenant as the host being addressed.
			// Both are required: the allowlist alone would let an origin
			// registered for one tenant be reflected on a request to another.
			if !matched && dynamicProvider != nil && origin != "" {
				if originWithinAllowedDomain(origin, cfg.AllowedDomains) &&
					dynamicProvider.OriginAllowedForHost(origin, r.Host) {
					matched = true
				}
			}

			if matched {
				if useWildcard {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
				w.Header().Set("Access-Control-Allow-Methods", cfg.AllowMethods)
				w.Header().Set("Access-Control-Allow-Headers", cfg.AllowHeaders)
				if cfg.ExposeHeaders != "" {
					w.Header().Set("Access-Control-Expose-Headers", cfg.ExposeHeaders)
				}
				if cfg.AllowCredentials {
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
				if r.Method == http.MethodOptions && cfg.PreflightMaxAge > 0 {
					w.Header().Set("Access-Control-Max-Age", strconv.Itoa(cfg.PreflightMaxAge))
				}
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			if r.Method == http.MethodOptions {
				// Unmatched origin preflight: 204 with NO CORS headers.
				// Browser enforces same-origin. No capability info is leaked.
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// filterAllowedDynamicOrigins filters a slice of origin URLs to only those
// whose host is a suffix match against the allowlist. If allowlist is empty,
// ALL origins are rejected (fail-closed). Each origin is parsed from URL form
// (e.g. "https://tenant.example.com:443") to extract host, then checked for
// suffix match against each domain in the allowlist.
func filterAllowedDynamicOrigins(origins, allowedDomains []string) []string {
	out := make([]string, 0, len(origins))
	for _, origin := range origins {
		if originWithinAllowedDomain(origin, allowedDomains) {
			out = append(out, origin)
		}
	}
	return out
}

// originWithinAllowedDomain reports whether a single origin's host is a suffix
// match against the operator's allowlist. An empty allowlist rejects
// everything, so an install that never configures CORS_ALLOWED_DOMAINS admits
// no dynamic origin at all. An origin that does not parse is rejected rather
// than let through.
func originWithinAllowedDomain(origin string, allowedDomains []string) bool {
	if len(allowedDomains) == 0 {
		return false
	}
	host := hostFromOrigin(origin)
	if host == "" {
		return false
	}
	for _, domain := range allowedDomains {
		if hostWithinDomain(host, domain) {
			return true
		}
	}
	return false
}

// hostWithinDomain reports whether host is domain itself or a subdomain of it.
//
// A plain suffix test treats the allowlist entry as the tail of the host
// rather than as a domain boundary, so an allowlist of example.com also
// matches evilexample.com, which an attacker can simply register. Reflecting
// it hands their page a credentialed cross-origin channel.
func hostWithinDomain(host, domain string) bool {
	if host == domain {
		return true
	}
	return strings.HasSuffix(host, "."+domain)
}

// hostFromOrigin extracts the host (without port) from a URL-style
// origin string like "https://tenant.example.com:443". Returns "" on parse
// failure so the caller can reject the origin.
func hostFromOrigin(origin string) string {
	// Origin headers may or may not include scheme:// prefix.
	// Normalize by stripping a known scheme prefix if present.
	// This avoids full URL parsing (which can be expensive at request time
	// with many origins). The function handles http://, https://, and no-scheme.
	var host string
	switch {
	case len(origin) > 8 && origin[:8] == "https://":
		host = origin[8:]
	case len(origin) > 7 && origin[:7] == "http://":
		host = origin[7:]
	case origin == "":
		return ""
	default:
		// No scheme. Treat the whole string as host:port.
		host = origin
	}
	// Strip optional port suffix (e.g. ":443") so suffix matching against
	// domain works correctly: "tenant-a.customer.com:443" matches "customer.com".
	if idx := strings.LastIndex(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	return host
}

// authSourceValue tracks where the JWT credentials came from, so CSRFCheck can
// tell a cookie-borne request (which a browser sends automatically, and which
// therefore needs a double-submit token) from a bearer one (which it does not).
// Authorization is never rejected on the strength of this alone: see requireRole.
type authSourceValue string

const (
	authSourceBearer authSourceValue = "bearer"
	authSourceCookie authSourceValue = "cookie"
)

type authSourceCtxKey struct{}

// sessionCookieValue returns the first non-empty session cookie among names.
func sessionCookieValue(r *http.Request, names []string) string {
	for _, name := range names {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// jwtAuth validates a JWT (bearer header or session cookie) and, if valid,
// injects its claims into the request context for downstream handlers.
// It does NOT reject the request if no token is present: use requireAuth for that.
// Only tokens carrying typ=session are accepted as session credentials.
// Tokens with typ=challenge, typ=magiclink, or a missing typ claim are silently
// dropped (no claims injected, requireAuth will reject downstream).
// secrets is the ordered list of active HMAC-SHA256 signing keys. All are tried
// for validation, enabling zero-downtime secret rotation.
// secure is the deployment's secure-cookie setting, which decides the names a
// session cookie is read from (security.SessionCookieNamesFor).
func jwtAuth(secrets []string, secure bool) func(http.Handler) http.Handler {
	cookieNames := security.SessionCookieNamesFor(secure)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var source authSourceValue
			token := bearerToken(r)
			if token == "" {
				if v := sessionCookieValue(r, cookieNames); v != "" {
					token = v
					source = authSourceCookie
				}
			} else {
				source = authSourceBearer
			}
			if token != "" {
				if claims, err := auth.ParseMulti(secrets, token); err == nil {
					// Only inject claims when typ=session.
					// Challenge tokens (typ=challenge) and magic-link tokens
					// (typ=magiclink) must not be usable as session credentials.
					if claims.TokenType == "session" {
						ctx := context.WithValue(r.Context(), auth.ClaimsKey, claims)
						ctx = context.WithValue(ctx, core.ClaimsKey, claims.AuthClaims())
						if source != "" {
							ctx = context.WithValue(ctx, authSourceCtxKey{}, source)
						}
						r = r.WithContext(ctx)
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireAuth rejects the request with 401 if no valid JWT or API key is
// present, or if the token is an MFA challenge token that has not been verified.
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := claimsFromCtx(r)
		if c != nil {
			if c.MFAPending {
				httpx.ErrorReq(w, r, http.StatusUnauthorized, "mfa verification required")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		// Fall through to core.AuthClaims (API keys).
		if ac := core.GetClaims(r.Context()); ac != nil {
			next.ServeHTTP(w, r)
			return
		}
		httpx.ErrorReq(w, r, http.StatusUnauthorized, "authentication required")
	})
}

// requireRole rejects the request with 403 if the authenticated user does not
// have at least one of the specified roles. Supports both JWT and API key auth.
//
// Credential shape is deliberately not checked here. It guards both routers:
// the /api/v1 content writes take a bearer token, and an admin UI may hold the
// session server-side and forward it as an Authorization header.
//
// Accepting a bearer token costs nothing in CSRF terms: a browser never
// attaches an Authorization header by itself, and CSRFCheck already keys off
// the presence of the session cookie, so cookie-borne requests are still
// paired with a double-submit token.
func requireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := claimsFromCtx(r)
			if claims != nil {
				for _, role := range roles {
					if claims.HasRole(role) {
						next.ServeHTTP(w, r)
						return
					}
				}
				httpx.ErrorReq(w, r, http.StatusForbidden, "insufficient permissions")
				return
			}
			// API key auth. The key must hold the role, exactly as a JWT must.
			// Scope is an additional gate that RequireScoped applies, never a
			// substitute for role, because the scope vocabulary has no
			// super-admin level.
			if ac := core.GetClaims(r.Context()); ac != nil {
				for _, role := range roles {
					for _, rl := range ac.Roles {
						if rl == role {
							next.ServeHTTP(w, r)
							return
						}
					}
				}
				httpx.ErrorReq(w, r, http.StatusForbidden, "insufficient permissions")
				return
			}
			httpx.ErrorReq(w, r, http.StatusUnauthorized, "authentication required")
		})
	}
}

// claimsFromCtx retrieves Claims injected by jwtAuth middleware.
func claimsFromCtx(r *http.Request) *auth.Claims {
	c, _ := r.Context().Value(auth.ClaimsKey).(*auth.Claims)
	return c
}

// requestClaims returns the caller's identity whichever credential carried it.
// A session or trusted-issuer token is read first, as requireAuth and
// requireRole read it, so every gate judges the same caller. An API key and an
// admin token write only the plugin-facing claims, so those are read next.
// Reading the session claims alone would answer nil for every API key, and a
// gate must never read nil as nothing to check.
func requestClaims(r *http.Request) *core.AuthClaims {
	if c := claimsFromCtx(r); c != nil {
		return c.AuthClaims()
	}
	return core.GetClaims(r.Context())
}

// metricsAuth checks for a static bearer token before falling through to
// requireRole("super_admin"). When MetricsToken is empty the middleware
// degrades to a plain requireRole("super_admin") gate.
//
// This enables Prometheus ServiceMonitors to scrape /metrics with a
// long-lived static token instead of an expiring JWT, while retaining
// JWT-based access for human operators.
func metricsAuth(metricsToken string) func(http.Handler) http.Handler {
	superAdmin := requireRole("super_admin")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if metricsToken != "" {
				if tok := bearerToken(r); tok != "" {
					if subtle.ConstantTimeCompare([]byte(tok), []byte(metricsToken)) == 1 {
						next.ServeHTTP(w, r)
						return
					}
					// Bearer token present but wrong: reject immediately.
					// Don't fall through to JWT auth. A wrong static token
					// should not accidentally succeed via stale credentials.
					httpx.ErrorReq(w, r, http.StatusUnauthorized, "invalid metrics token")
					return
				}
			}
			superAdmin(next).ServeHTTP(w, r)
		})
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// tokenVersionCheck rejects requests whose JWT's embedded token_version (tv)
// is less than the user's current token_version in the database. When a user
// logs out, their token_version is bumped, and all previously-issued access
// tokens become immediately invalid: even before their natural expiry.
//
// getTokenVersion is a function that queries the database for the current
// token_version of the user identified by userID. It MUST return 0 + nil
// when the user doesn't exist (the middleware will reject the token).
// Pass nil to disable the check (no-op).
func tokenVersionCheck(getTokenVersion func(ctx context.Context, userID string) (int, error)) func(http.Handler) http.Handler {
	if getTokenVersion == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := claimsFromCtx(r)
			if claims == nil {
				// No JWT claims (API key auth). Let the request through,
				// because API keys have their own revocation mechanism.
				next.ServeHTTP(w, r)
				return
			}
			currentVersion, err := getTokenVersion(r.Context(), claims.UserID)
			if err != nil {
				slog.WarnContext(r.Context(), "token version check failed, rejecting",
					"err", err, "user_id", claims.UserID)
				// A user who is gone, or a subject that is no user id, has no
				// session to keep. A database that cannot answer is an outage,
				// and answering 401 would sign every caller out through it.
				if errors.Is(err, domain.ErrNotFound) || errors.Is(err, errTokenSubject) {
					httpx.ErrorReq(w, r, http.StatusUnauthorized, "session invalidated")
					return
				}
				httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "session check unavailable")
				return
			}
			if claims.TokenVersion < currentVersion {
				slog.WarnContext(r.Context(), "token version mismatch: access token revoked",
					"user_id", claims.UserID,
					"token_version", claims.TokenVersion,
					"db_version", currentVersion)
				httpx.ErrorReq(w, r, http.StatusUnauthorized, "session invalidated")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// errTokenSubject marks a token whose subject is not a user id.
var errTokenSubject = errors.New("token subject is not a user id")

// tokenVersionStore abstracts a UUID-keyed token version lookup.
type tokenVersionStore interface {
	GetTokenVersion(ctx context.Context, id uuid.UUID) (int, error)
}

// tokenVersionGetter adapts a uuid.UUID-keyed store method to the
// string-keyed signature expected by tokenVersionCheck.
func tokenVersionGetter(s tokenVersionStore) func(ctx context.Context, userID string) (int, error) {
	return func(ctx context.Context, userID string) (int, error) {
		uid, err := uuid.Parse(userID)
		if err != nil {
			return 0, fmt.Errorf("tokenVersionGetter: parse user_id: %w: %w", errTokenSubject, err)
		}
		return s.GetTokenVersion(ctx, uid)
	}
}

// externalUsers is the store trustedIssuerAuth keeps issuers' users in.
type externalUsers interface {
	EnsureExternalUser(ctx context.Context, id uuid.UUID, email, passwordHash string, roles []string, tenantID string) (*domain.User, error)
	SessionsRevokedAt(ctx context.Context, id uuid.UUID) (*time.Time, error)
}

// trustedIssuerAuth runs after jwtAuth. If no claims are set yet and a Bearer
// token is present, it tries each issuer that has a policy: the token must be
// signed by that issuer's JWKS ({issuer}/.well-known/jwks.json), and the
// policy then decides the tenant and the roles. The caller acts as a local
// account tied to the issuer and subject, created on first sight. Disabling
// that account, or anything that ends its sessions (a logout, a password
// set, an erasure), refuses the issuer's tokens issued before it. A trusted
// issuer with no policy is never tried: taking the roles and tenant as the
// token states them would let the issuer mint super_admin anywhere. A no-op
// with no policies.
//
// A policy that grants an admin role makes the account an admin seat, so
// seats is asked first, and a refusal answers 402 rather than leaving the
// caller anonymous: the token is good and the install is full.
func trustedIssuerAuth(policies []auth.IssuerPolicy, users externalUsers, seats core.AdminSeatGuard) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(policies) == 0 || users == nil || claimsFromCtx(r) != nil {
				next.ServeHTTP(w, r)
				return
			}
			token := bearerToken(r)
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}
			for _, p := range policies {
				jwksURL := strings.TrimRight(p.Issuer, "/") + "/.well-known/jwks.json"
				ext, err := auth.ParseExternal(r.Context(), token, jwksURL, p.Issuer)
				if err != nil {
					continue
				}
				local, unavailable, seatErr := admitExternal(r.Context(), p, token, ext, users, seats)
				if seatErr != nil {
					admitAdminSeat(w, r, seatErr)
					return
				}
				if unavailable {
					httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "account store unavailable")
					return
				}
				if local != nil {
					// Plugins read the caller through core.ClaimsKey, so it is set
					// beside the engine's key the way a session token sets it.
					ctx := context.WithValue(r.Context(), auth.ClaimsKey, local)
					r = r.WithContext(context.WithValue(ctx, core.ClaimsKey, local.AuthClaims()))
				}
				break
			}
			next.ServeHTTP(w, r)
		})
	}
}

// admitExternal turns a verified issuer token into the claims of its local
// account, or nil when the policy or the account refuses it. A refusal leaves
// the request anonymous, so an authenticated route answers 401 as it does for
// any token the engine does not accept. unavailable reports a database that
// could not answer, which is an outage rather than a refusal. seatErr is the
// admin seat guard's refusal, or its failure to count, for a policy that
// grants an admin role.
func admitExternal(ctx context.Context, p auth.IssuerPolicy, token string, ext *auth.Claims, users externalUsers, seats core.AdminSeatGuard) (local *auth.Claims, unavailable bool, seatErr error) {
	roles, tenant, err := p.Admit(token, ext)
	if err != nil {
		slog.WarnContext(ctx, "trusted issuer token refused", "issuer", p.Issuer, "err", err)
		return nil, false, nil
	}
	id := auth.ExternalUserID(p.Issuer, ext.UserID)
	// The token's email labels the account and is unique across the install,
	// so an unverified one could squat an address a person will need. Without
	// a verified email, and for a machine token with none, a reserved domain
	// gives a unique address that can never collide with a person's.
	email := id.String() + "@external.invalid"
	if ext.Email != "" && auth.EmailVerified(token) {
		email = ext.Email
	}
	var u *domain.User
	seatErr, err = core.WriteWithAdminSeat(ctx, seats, id, roles, func(ctx context.Context) error {
		var err error
		u, err = users.EnsureExternalUser(ctx, id, email, auth.ExternalIssuerMarker, roles, tenant)
		return err
	})
	if seatErr != nil {
		return nil, false, seatErr
	}
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			// The driver's text names the email. The reason is enough here.
			slog.WarnContext(ctx, "trusted issuer account refused: the email or tenant belongs to another account", "issuer", p.Issuer)
			return nil, false, nil
		}
		slog.ErrorContext(ctx, "trusted issuer account unavailable", "issuer", p.Issuer)
		return nil, true, nil
	}
	if u.Disabled || (u.ExpiresAt != nil && time.Now().After(*u.ExpiresAt)) {
		return nil, false, nil
	}
	revokedAt, err := users.SessionsRevokedAt(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "trusted issuer account unavailable", "issuer", p.Issuer)
		return nil, true, nil
	}
	if revokedAt != nil && (ext.IssuedAt == nil || ext.IssuedAt.Before(*revokedAt)) {
		// Issued before the account's sessions last ended. A token with no
		// iat cannot show it came after, so it is refused too.
		return nil, false, nil
	}
	return &auth.Claims{
		UserID:           u.ID.String(),
		Email:            u.Email,
		Roles:            u.Roles,
		TenantID:         u.TenantID,
		TokenType:        "session",
		TokenVersion:     u.TokenVersion,
		RegisteredClaims: ext.RegisteredClaims,
	}, false, nil
}
