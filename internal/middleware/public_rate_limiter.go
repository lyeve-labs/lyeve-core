package middleware

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
	"strconv"
	"strings"
	"sync"
	"time"
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
		go l.cleanupLoop(cleanupCtx)
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

// Verify checks that every route wrapped via Wrap/WrapFunc has a matching
// per-endpoint rate-limit config. Returns an error listing unconfigured
// routes. Call before the server starts listening.
func (l *PublicEndpointRateLimiter) Verify() error {
	var missing []string
	for key := range l.wrapped {
		if _, ok := l.configs[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	globalNote := " (no global cap)"
	if l.global != nil {
		globalNote = " (global cap present but no per-endpoint config)"
	}
	return fmt.Errorf("public rate limiter: %d wrapped route(s) have no per-endpoint config%s: %s",
		len(missing), globalNote, strings.Join(missing, ", "))
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
	cfg, ok := l.configs[method+":"+pattern]
	if !ok && l.global == nil {
		// No per-endpoint config and no global cap: pass through.
		return next
	}

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

		endpointKey := method + ":" + pattern

		// Per-endpoint check.
		if ok {
			bucketKey := endpointKey + ":" + ip
			allowed, remaining, resetAt := l.check(bucketKey, cfg.Rate, cfg.Burst)
			setPublicRateLimitHeaders(w, cfg.Burst, remaining, resetAt)
			if !allowed {
				writePublicRateLimitResponse(w, cfg.Burst, resetAt)
				return
			}
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
