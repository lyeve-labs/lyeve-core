package middleware

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	lastFill time.Time
	lastSeen time.Time // updated on every allow() call for LRU eviction
}

// allow checks whether a request is permitted.
func (b *tokenBucket) allow(rate float64, burst int) (allowed bool, remaining int, resetAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.lastSeen = now
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens = math.Min(float64(burst), b.tokens+elapsed*rate)
	b.lastFill = now

	rem := int(math.Floor(b.tokens))
	if rem > burst {
		rem = burst
	}

	if b.tokens < 1 {
		tokensNeeded := 1.0 - b.tokens
		resetIn := tokensNeeded / rate
		return false, 0, now.Add(time.Duration(resetIn * float64(time.Second)))
	}
	b.tokens--

	tokensToFull := float64(burst) - b.tokens
	return true, rem, now.Add(time.Duration(tokensToFull / rate * float64(time.Second)))
}

// RateLimiter returns a per-IP token-bucket rate limiter. rate is sustained
// requests/second. Burst is the initial token count. Exceeding clients
// receive HTTP 429 with Retry-After and standard rate-limit headers.
func RateLimiter(rate float64, burst int) func(http.Handler) http.Handler {
	return perKeyRateLimiter(rate, burst, 0, func(r *http.Request) string {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		return bucketAddress(ip)
	})
}

// PerTenantRateLimiter returns a per-tenant+IP token-bucket rate limiter.
// Bucket key is "tenant:ip" when the context carries a tenant ID (set by
// TenantHeader). Falls back to IP alone otherwise.
func PerTenantRateLimiter(rate float64, burst int) func(http.Handler) http.Handler {
	return perKeyRateLimiter(rate, burst, 0, func(r *http.Request) string {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		ip = bucketAddress(ip)
		tid := TenantIDFromCtx(r)
		if tid == "" {
			return ip
		}
		return tid + ":" + ip
	})
}

// DefaultMaxKeys is the default maximum number of rate-limit buckets in the
// perKeyRateLimiter map. Once reached, the least-recently-used bucket is
// evicted on each new-key insert, preventing unbounded memory growth.
const DefaultMaxKeys = 10_000

// perKeyRateLimiter is the shared rate-limit implementation. maxKeys caps
// concurrent buckets. At the cap the least recently used bucket is evicted.
func perKeyRateLimiter(rate float64, burst int, maxKeys int, keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	var (
		mu        sync.Mutex
		buckets   = newBucketLRU(maxKeys)
		limitStr  = strconv.Itoa(burst)
		policyStr = strconv.Itoa(burst) + ";w=1"
	)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyFn(r)
			mu.Lock()
			b, ok := buckets.get(key)
			if !ok {
				b = &tokenBucket{tokens: float64(burst), lastFill: time.Now(), lastSeen: time.Now()}
				buckets.add(key, b)
			}
			mu.Unlock()

			allowed, remaining, resetAt := b.allow(rate, burst)

			// strconv.Itoa avoids per-request allocations vs fmt.Sprintf.
			h := w.Header()
			remStr := strconv.Itoa(remaining)
			resetStr := strconv.FormatInt(resetAt.Unix(), 10)
			h.Set("RateLimit-Limit", limitStr)
			h.Set("RateLimit-Remaining", remStr)
			h.Set("RateLimit-Reset", resetStr)
			h.Set("X-RateLimit-Limit", limitStr)
			h.Set("X-RateLimit-Remaining", remStr)
			h.Set("X-RateLimit-Reset", resetStr)
			h.Set("RateLimit-Policy", policyStr)

			if !allowed {
				retryAfter := int(time.Until(resetAt).Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				h.Set("Retry-After", strconv.Itoa(retryAfter))
				h.Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprintf(w, `{"error":"rate limit exceeded","remaining":0,"limit":%d}`, burst)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
