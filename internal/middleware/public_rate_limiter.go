package middleware

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
)

// maxLRUBuckets is the eviction threshold for stale rate-limit buckets.
// When the bucket count exceeds this value, entries older than the cleanup
// interval are removed to bound memory under high-cardinality attack.
const maxLRUBuckets = 100

// PublicRateLimitConfig holds rate-limit parameters for a single public endpoint.
type PublicRateLimitConfig struct {
	// Rate is the sustained requests per second per IP.
	Rate float64
	// Burst is the maximum burst size (initial token count) per IP.
	Burst int
}

// PublicEndpointRateLimiterConfig holds configuration for the per-endpoint
// per-IP rate limiter.
type PublicEndpointRateLimiterConfig struct {
	// Rate limiter configs for individual endpoints.
	Configs map[string]PublicRateLimitConfig // method+pattern -> config
	// Global is the optional aggregate per-IP cap across all endpoints.
	Global *PublicRateLimitConfig
	// TrustedProxies defines reverse proxy CIDRs whose XFF header is trusted
	// for client IP extraction. nil means RemoteAddr is always used.
	TrustedProxies TrustedProxies

	// MaxIPs caps the number of in-memory IP buckets. When the cap is reached,
	// the least-recently-used bucket is evicted. Default: 10_000.
	MaxIPs int
	// CleanupInterval controls how often stale (unused for ≥1 interval)
	// IP buckets are purged. Zero means no background cleanup.
	CleanupInterval time.Duration
}

// PublicEndpointRateLimiter is a per-endpoint per-IP rate limiter designed
// for wrapping individual public route handlers.
type PublicEndpointRateLimiter struct {
	configs         map[string]PublicRateLimitConfig // method+pattern -> config
	global          *PublicRateLimitConfig           // optional aggregate cap
	trustedProxies  []*net.IPNet                     // nil -> RemoteAddr only
	maxIPs          int
	cleanupInterval time.Duration

	// wrapped tracks every method:pattern key passed to Wrap/WrapFunc.
	// Used by Verify() to assert that every rate-limited route has a
	// matching config entry: prevents prefix drift from silently
	// disabling per-endpoint brute-force protection.
	wrapped map[string]bool

	mu      sync.Mutex
	buckets *bucketLRU // "endpoint:ip" -> bucket, most recently used first

	stopCleanup func()
}

// TrustedProxies is a named slice of parsed CIDRs that defines which reverse
// proxy IPs are trusted for X-Forwarded-For header extraction. nil means
// RemoteAddr is always used (spoof-proof default).
type TrustedProxies []*net.IPNet

// NewPublicEndpointRateLimiter creates a limiter with per-endpoint configs.
//
// Deprecated: use NewPublicEndpointRateLimiterWithConfig. Sets MaxIPs to
// 10_000 and disables background cleanup.
func NewPublicEndpointRateLimiter(configs map[string]PublicRateLimitConfig, global *PublicRateLimitConfig, trustedProxies TrustedProxies) *PublicEndpointRateLimiter {
	return newPublicEndpointRateLimiterWithConfig(PublicEndpointRateLimiterConfig{
		Configs:        configs,
		Global:         global,
		TrustedProxies: trustedProxies,
	})
}

// NewPublicEndpointRateLimiterWithConfig creates a limiter with full config.
// MaxIPs defaults to 10_000. A non-zero CleanupInterval starts a background
// cleanup goroutine. Callers should defer .Stop().
func newPublicEndpointRateLimiterWithConfig(cfg PublicEndpointRateLimiterConfig) *PublicEndpointRateLimiter {
	if cfg.Configs == nil {
		cfg.Configs = make(map[string]PublicRateLimitConfig)
	}
	if cfg.MaxIPs <= 0 {
		cfg.MaxIPs = 10_000
	}
	l := &PublicEndpointRateLimiter{
		configs:         cfg.Configs,
		global:          cfg.Global,
		trustedProxies:  cfg.TrustedProxies,
		maxIPs:          cfg.MaxIPs,
		cleanupInterval: cfg.CleanupInterval,
		wrapped:         make(map[string]bool),
		buckets:         newBucketLRU(cfg.MaxIPs),
	}

	if cfg.CleanupInterval > 0 {
		cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
		l.stopCleanup = cleanupCancel
		engine.GoSafe("public-rate-limit-cleanup", func() { l.cleanupLoop(cleanupCtx) })
	}

	return l
}

// Stop terminates the background cleanup goroutine. Idempotent.
func (l *PublicEndpointRateLimiter) Stop() {
	if l.stopCleanup != nil {
		l.stopCleanup()
	}
}

// cleanupLoop periodically removes stale IP buckets.
func (l *PublicEndpointRateLimiter) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(l.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		l.mu.Lock()
		cutoff := time.Now().Add(-l.cleanupInterval)
		l.buckets.removeIf(func(b *tokenBucket) bool {
			b.mu.Lock()
			stale := b.lastFill.Before(cutoff)
			b.mu.Unlock()
			return stale && l.buckets.len() > maxLRUBuckets
		})
		l.mu.Unlock()
	}
}

// DefaultPublicRateLimits returns the engine's limits for the public routes
// it serves itself, and the global per-address cap. A route the engine does
// not serve states its own limit in its declaration, which the routers merge
// over these rows. An operator moves individual entries of either with
// PUBLIC_RATE_LIMITS and PUBLIC_RATE_LIMIT_GLOBAL, which merge over both.
// See ParsePublicRateLimits.
//
// Sign-in, token refresh and the MFA step: 5 req/s burst 10
// Content API token: 5 req/s burst 10
// Device sign-in start: 0.2 req/s burst 5
// DSAR export: 10 req/s burst 20, erase: 5 req/s burst 10
// Global per-IP cap: 50 req/s burst 100
func DefaultPublicRateLimits() (map[string]PublicRateLimitConfig, *PublicRateLimitConfig) {
	configs := map[string]PublicRateLimitConfig{
		// DSAR export and erase. Neither is public, but a subject-access request
		// reads every exporter and returns the whole of a person's data, so an
		// authenticated operator or a stolen session can drain every tenant one
		// identifier at a time. Bounded by generation cost, not by brute force.
		"POST:/api/admin/gdpr/export": {Rate: 10, Burst: 20},
		"POST:/api/admin/gdpr/erase":  {Rate: 5, Burst: 10},
		// Core auth endpoints: brute-force targets for credential stuffing
		"POST:/api/admin/auth/login":      {Rate: 5, Burst: 10},
		"POST:/api/admin/auth/refresh":    {Rate: 5, Burst: 10},
		"POST:/api/admin/auth/mfa-verify": {Rate: 5, Burst: 10},
		// Device sign-in. Starting one writes a row, so it is paced well below
		// login. The poll is the device's, every five seconds by default, and
		// the decision routes are a signed-in admin's, limited so a session
		// cannot walk the user code space.
		"POST:/api/admin/auth/device":                     {Rate: 0.2, Burst: 5},
		"POST:/api/admin/auth/device/token":               {Rate: 1, Burst: 10},
		"GET:/api/admin/auth/device/{user_code}":          {Rate: 1, Burst: 10},
		"POST:/api/admin/auth/device/{user_code}/approve": {Rate: 1, Burst: 10},
		"POST:/api/admin/auth/device/{user_code}/deny":    {Rate: 1, Burst: 10},
		// Token endpoint: brute-force target (password-based), same as login
		"POST:/api/v1/auth/token": {Rate: 5, Burst: 10},
	}
	global := &PublicRateLimitConfig{Rate: 50, Burst: 100}
	return configs, global
}

// credentialRoute matches the patterns of the routes that stand between a
// public caller and a credential: everything under an auth segment, on the
// admin API, the Content API or bare.
var credentialRoute = regexp.MustCompile(`^(/api/(admin|v1))?/auth(/|$)`)

// CredentialRoute reports whether pattern is a route that guards a
// credential. Such a route needs a limit of its own: the general cap is set
// for reading, not for guessing passwords, and no administrator may move its
// limit at runtime.
func CredentialRoute(pattern string) bool {
	return credentialRoute.MatchString(pattern)
}

// Verify checks that every wrapped route is limited as it should be. A route
// with no limit of its own runs on the global cap, the general limit every
// public route shares, which is the intended state for a route that guards no
// credential. Verify names a credential route with no limit of its own,
// global cap or not, and with no global cap every route with no limit of its
// own, because those run unlimited. Call before the server starts listening.
func (l *PublicEndpointRateLimiter) Verify() error {
	var credential, unlimited []string
	for key := range l.wrapped {
		method, pattern, _ := strings.Cut(key, ":")
		if _, _, ok := l.ownConfig(method, pattern); ok {
			continue
		}
		switch {
		case l.global == nil:
			unlimited = append(unlimited, key)
		case CredentialRoute(pattern):
			credential = append(credential, key)
		}
	}
	sort.Strings(credential)
	sort.Strings(unlimited)
	var problems []string
	if len(credential) > 0 {
		problems = append(problems, fmt.Sprintf("%d credential route(s) have no per-endpoint config: %s",
			len(credential), strings.Join(credential, ", ")))
	}
	if len(unlimited) > 0 {
		problems = append(problems, fmt.Sprintf("%d wrapped route(s) have no per-endpoint config and there is no global cap: %s",
			len(unlimited), strings.Join(unlimited, ", ")))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("public rate limiter: %s", strings.Join(problems, ". "))
}

// GeneralRoutes lists, sorted, the wrapped routes that run on the general
// limit: those with no limit of their own that guard no credential.
func (l *PublicEndpointRateLimiter) GeneralRoutes() []string {
	var out []string
	for key := range l.wrapped {
		method, pattern, _ := strings.Cut(key, ":")
		if _, _, ok := l.ownConfig(method, pattern); !ok && !CredentialRoute(pattern) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// ownConfig is the limit of its own a route runs on, and the key of the
// bucket it spends. HEAD does a GET's work, so a HEAD route with no entry
// follows its GET route's entry and spends that route's bucket.
func (l *PublicEndpointRateLimiter) ownConfig(method, pattern string) (string, PublicRateLimitConfig, bool) {
	key := method + ":" + pattern
	if cfg, ok := l.configs[key]; ok {
		return key, cfg, true
	}
	if method == http.MethodHead {
		key = http.MethodGet + ":" + pattern
		if cfg, ok := l.configs[key]; ok {
			return key, cfg, true
		}
	}
	return "", PublicRateLimitConfig{}, false
}

// Wrap applies per-IP rate limiting to an http.Handler keyed by
// method+pattern. Passes through unmodified when no config exists, making it
// safe to wrap all public routes.
func (l *PublicEndpointRateLimiter) Wrap(method, pattern string, next http.Handler) http.Handler {
	l.wrapped[method+":"+pattern] = true
	return l.wrapHandler(method, pattern, next)
}

// WrapFunc returns a chi-compatible middleware that applies per-IP rate
// limiting for the given method+pattern.
func (l *PublicEndpointRateLimiter) WrapFunc(method, pattern string) func(http.Handler) http.Handler {
	l.wrapped[method+":"+pattern] = true
	return func(next http.Handler) http.Handler {
		return l.wrapHandler(method, pattern, next)
	}
}

func (l *PublicEndpointRateLimiter) wrapHandler(method, pattern string, next http.Handler) http.Handler {
	endpointKey, cfg, ok := l.ownConfig(method, pattern)
	// HEAD does a GET's work. With no limit of its own and none on its GET
	// route, it follows the GET route's operator limit and spends the same
	// bucket, so lowering a route covers both methods.
	limitMethod := method
	if method == http.MethodHead {
		limitMethod = http.MethodGet
	}
	endpoint := limitMethod + " " + pattern
	customKey := "custom:" + limitMethod + ":" + pattern
	credential := CredentialRoute(pattern)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health probes are exempt: they are supposed to be frequent.
		//
		// Loopback is not exempt: a proxy on the same host (the single-box
		// deployment the docs recommend) makes every request arrive from
		// 127.0.0.1, and exempting it would switch brute-force protection off.
		ip := l.clientIP(r)
		if isHealthPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Per-endpoint check.
		if ok {
			bucketKey := endpointKey + ":" + ip
			allowed, remaining, resetAt := l.check(bucketKey, cfg.Rate, cfg.Burst)
			setPublicRateLimitHeaders(w, cfg.Burst, remaining, resetAt)
			if !allowed {
				writePublicRateLimitResponse(w, cfg.Burst, resetAt)
				return
			}
		} else if custom, set := core.ResolvePublicRouteLimit(endpoint); set && !credential {
			// An operator's limit on a route on the general limit replaces
			// the general limit there, so the global cap is not checked.
			// Its bucket is its own: the general one is not spent either.
			allowed, remaining, resetAt := l.check(customKey+":"+ip, custom.Rate, custom.Burst)
			setPublicRateLimitHeaders(w, custom.Burst, remaining, resetAt)
			if !allowed {
				writePublicRateLimitResponse(w, custom.Burst, resetAt)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Global aggregate check.
		if l.global != nil {
			globalKey := "__global__:" + ip
			allowed, remaining, resetAt := l.check(globalKey, l.global.Rate, l.global.Burst)
			setPublicRateLimitHeaders(w, l.global.Burst, remaining, resetAt)
			if !allowed {
				writePublicRateLimitResponse(w, l.global.Burst, resetAt)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// check tests and consumes a token from the bucket identified by key.
// Returns (allowed, remaining, resetAt).
func (l *PublicEndpointRateLimiter) check(key string, rate float64, burst int) (bool, int, time.Time) {
	l.mu.Lock()
	b, ok := l.buckets.get(key)
	if !ok {
		b = &tokenBucket{tokens: float64(burst), lastFill: time.Now()}
		l.buckets.add(key, b)
	}
	l.mu.Unlock()

	return b.allow(rate, burst)
}

// clientIP returns the address the limiter counts a request against.
//
// It resolves the client through the configured trusted proxies. Behind a
// load balancer RemoteAddr is the balancer, and counting against it would put
// every caller in one bucket, where one attacker exhausting it locks every
// legitimate caller out.
//
// With no trusted proxies configured this is RemoteAddr: X-Forwarded-For from
// an unknown hop is a header the client wrote.
func (l *PublicEndpointRateLimiter) clientIP(r *http.Request) string {
	return bucketAddress(reqparse.ClientIPTrusted(r, l.trustedProxies))
}

// healthPaths are paths that bypass public rate limiting.
var healthPaths = []string{"/healthz", "/readyz", "/startup", "/health", "/ready", "/metrics"}

// isHealthPath returns true if the path matches a health/ready endpoint.
func isHealthPath(path string) bool {
	for _, hp := range healthPaths {
		if strings.HasPrefix(path, hp) {
			return true
		}
	}
	return false
}

func setPublicRateLimitHeaders(w http.ResponseWriter, limit, remaining int, resetAt time.Time) {
	h := w.Header()
	limitStr := strconv.Itoa(limit)
	remainingStr := strconv.Itoa(remaining)
	resetStr := strconv.FormatInt(resetAt.Unix(), 10)

	h.Set("RateLimit-Limit", limitStr)
	h.Set("RateLimit-Remaining", remainingStr)
	h.Set("RateLimit-Reset", resetStr)
	h.Set("X-RateLimit-Limit", limitStr)
	h.Set("X-RateLimit-Remaining", remainingStr)
	h.Set("X-RateLimit-Reset", resetStr)
	h.Set("RateLimit-Policy", limitStr+";w=1")
}

func writePublicRateLimitResponse(w http.ResponseWriter, limit int, resetAt time.Time) {
	retryAfter := int(time.Until(resetAt).Seconds())
	if retryAfter < 1 {
		retryAfter = 1
	}
	h := w.Header()
	h.Set("Retry-After", strconv.Itoa(retryAfter))
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprintf(w,
		`{"error":"rate limit exceeded","remaining":0,"limit":%d}`,
		limit)
}
