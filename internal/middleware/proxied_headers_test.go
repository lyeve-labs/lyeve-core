package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStripUntrustedProxyHeaders(t *testing.T) {
	_, trusted10, _ := net.ParseCIDR("10.0.0.0/8")

	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tls-Fingerprint-Received", r.Header.Get("X-TLS-Fingerprint"))
		w.Header().Set("True-Client-IP-Received", r.Header.Get("True-Client-IP"))
		w.Header().Set("X-Real-IP-Received", r.Header.Get("X-Real-IP"))
		w.Header().Set("X-Forwarded-For-Received", r.Header.Get("X-Forwarded-For"))
		w.Header().Set("RemoteAddr-Received", r.RemoteAddr)
		w.WriteHeader(http.StatusOK)
	})

	// X-TLS-Fingerprint

	t.Run("trusted peer passes header through", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-TLS-Fingerprint", "ja4=abc123")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Equal(t, "ja4=abc123", rec.Header().Get("X-Tls-Fingerprint-Received"))
	})

	t.Run("untrusted peer strips header", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.42:54321"
		req.Header.Set("X-TLS-Fingerprint", "ja4=spoofed")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("X-Tls-Fingerprint-Received"))
	})

	t.Run("untrusted peer with trusted CIDR but wrong range strips", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "172.16.0.1:8080"
		req.Header.Set("X-TLS-Fingerprint", "ja4=not-in-10-range")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("X-Tls-Fingerprint-Received"))
	})

	t.Run("no trusted CIDRs always strips (fail-safe)", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders(nil)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-TLS-Fingerprint", "ja4=should-be-stripped")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("X-Tls-Fingerprint-Received"))
	})

	t.Run("empty trusted CIDRs always strips (fail-safe)", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-TLS-Fingerprint", "ja4=also-stripped")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("X-Tls-Fingerprint-Received"))
	})

	t.Run("no header set on request is fine (empty passthrough)", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.5:9999"

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("X-Tls-Fingerprint-Received"))
	})

	t.Run("no header on untrusted request is fine", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.1:1111"

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("X-Tls-Fingerprint-Received"))
	})

	t.Run("multiple trusted CIDRs, peer in second range", func(t *testing.T) {
		_, trusted172, _ := net.ParseCIDR("172.16.0.0/12")
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10, trusted172})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "172.16.5.5:443"
		req.Header.Set("X-TLS-Fingerprint", "ja4=from-172-range")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Equal(t, "ja4=from-172-range", rec.Header().Get("X-Tls-Fingerprint-Received"))
	})

	t.Run("case-insensitive header strip", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders(nil)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		req.Header.Set("x-tls-fingerprint", "ja4=lowercase")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("X-Tls-Fingerprint-Received"))
	})

	// X-Forwarded-For, True-Client-IP, X-Real-IP

	t.Run("untrusted peer strips X-Forwarded-For", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.42:54321"
		req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("X-Forwarded-For-Received"),
			"X-Forwarded-For must be stripped from untrusted peers")
	})

	t.Run("trusted peer preserves X-Forwarded-For", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-Forwarded-For", "203.0.113.42, 10.0.0.1")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Equal(t, "203.0.113.42, 10.0.0.1", rec.Header().Get("X-Forwarded-For-Received"),
			"X-Forwarded-For must be preserved for trusted peers")
	})

	t.Run("untrusted peer strips True-Client-IP", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.42:54321"
		req.Header.Set("True-Client-IP", "1.2.3.4")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("True-Client-IP-Received"),
			"True-Client-IP must be stripped from untrusted peers")
	})

	t.Run("trusted peer preserves True-Client-IP", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("True-Client-IP", "203.0.113.42")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Equal(t, "203.0.113.42", rec.Header().Get("True-Client-IP-Received"),
			"True-Client-IP must be preserved for trusted peers")
	})

	t.Run("untrusted peer strips X-Real-IP", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.42:54321"
		req.Header.Set("X-Real-IP", "1.2.3.4")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("X-Real-IP-Received"),
			"X-Real-IP must be stripped from untrusted peers")
	})

	t.Run("trusted peer preserves X-Real-IP", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-Real-IP", "203.0.113.42")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Equal(t, "203.0.113.42", rec.Header().Get("X-Real-IP-Received"),
			"X-Real-IP must be preserved for trusted peers")
	})

	t.Run("no trusted CIDRs strips all IP headers", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders(nil)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		req.Header.Set("True-Client-IP", "5.6.7.8")
		req.Header.Set("X-Real-IP", "9.10.11.12")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("X-Forwarded-For-Received"))
		assert.Empty(t, rec.Header().Get("True-Client-IP-Received"))
		assert.Empty(t, rec.Header().Get("X-Real-IP-Received"))
	})

	// Attack scenario simulation
	// This simulates what happens at router level: StripUntrustedProxyHeaders
	// runs first, then chi RealIP would run next. Since we strip spoofed IP
	// headers from untrusted peers, chi RealIP finds nothing and leaves
	// RemoteAddr at its original value.

	t.Run("attack scenario: spoofed True-Client-IP is stripped, RealIP leaves RemoteAddr intact", func(t *testing.T) {
		// Simulate the middleware ordering: StripUntrustedProxyHeaders runs
		// before RealIP. Here, no trusted CIDRs (default deployment).
		mw := StripUntrustedProxyHeaders(nil)

		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		req.Header.Set("True-Client-IP", "10.0.0.99") // attacker spoof
		req.Header.Set("X-Forwarded-For", "10.0.0.99")
		req.Header.Set("X-Real-IP", "10.0.0.99")

		rec := httptest.NewRecorder()
		// The echo handler shows headers after stripping but BEFORE RealIP
		// (since RealIP is the NEXT middleware in the chain). We verify:
		// 1. IP headers are stripped from the *request* (not just the response)
		// 2. RemoteAddr is unchanged (original TCP source)
		mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// These should all be empty: stripped by middleware.
			assert.Empty(t, r.Header.Get("True-Client-IP"),
				"True-Client-IP header must be stripped before handler/RealIP sees it")
			assert.Empty(t, r.Header.Get("X-Forwarded-For"),
				"X-Forwarded-For header must be stripped before handler/RealIP sees it")
			assert.Empty(t, r.Header.Get("X-Real-IP"),
				"X-Real-IP header must be stripped before handler/RealIP sees it")
			// RemoteAddr must still be the original TCP source: RealIP has
			// no spoofed headers to override it with.
			assert.Equal(t, "192.168.1.1:12345", r.RemoteAddr,
				"RemoteAddr must remain the original TCP source after IP headers are stripped")
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("attack scenario: rotating spoofed IPs all counted against same RemoteAddr", func(t *testing.T) {
		// Attacker sends 3 requests with different spoofed True-Client-IP values.
		// Without trusted proxies, all should resolve to the same RemoteAddr.
		mw := StripUntrustedProxyHeaders(nil)

		spoofedIPs := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
		for i, spoof := range spoofedIPs {
			req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", nil)
			req.RemoteAddr = "192.168.1.1:12345"
			req.Header.Set("True-Client-IP", spoof)
			req.Header.Set("X-Forwarded-For", spoof)
			req.Header.Set("X-Real-IP", spoof)

			rec := httptest.NewRecorder()
			mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// RemoteAddr must be the same across all 3 requests
				// (same TCP connection, different spoofed headers).
				assert.Equal(t, "192.168.1.1:12345", r.RemoteAddr,
					"RemoteAddr must be consistent across request %d despite spoofed headers", i+1)
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)
		}
	})
}

// HTTPSRedirect treats X-Forwarded-Proto: https as "already secure", so a
// client that can set it skips its own redirect.
func TestStripUntrustedProxyHeaders_ForwardedProto(t *testing.T) {
	_, trusted10, _ := net.ParseCIDR("10.0.0.0/8")

	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Proto-Received", r.Header.Get("X-Forwarded-Proto"))
		w.WriteHeader(http.StatusOK)
	})

	t.Run("untrusted peer loses a spoofed proto", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.42:54321"
		req.Header.Set("X-Forwarded-Proto", "https")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Proto-Received"))
	})

	t.Run("trusted proxy keeps its proto", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders([]*net.IPNet{trusted10})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-Forwarded-Proto", "https")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Equal(t, "https", rec.Header().Get("Proto-Received"))
	})

	// With no declared topology a proxy is indistinguishable from a client, and
	// stripping the proto would loop every TLS-terminating deployment between
	// the redirect and the proxy.
	t.Run("no trusted CIDRs keeps the proto so proxies do not loop", func(t *testing.T) {
		mw := StripUntrustedProxyHeaders(nil)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.42:54321"
		req.Header.Set("X-Forwarded-Proto", "https")

		rec := httptest.NewRecorder()
		mw(echo).ServeHTTP(rec, req)

		assert.Equal(t, "https", rec.Header().Get("Proto-Received"))
	})
}
