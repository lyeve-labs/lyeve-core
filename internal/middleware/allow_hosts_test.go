package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllowedHosts(t *testing.T) {
	t.Run("noop when allowed list is empty", func(t *testing.T) {
		mw := AllowedHosts(nil)
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Host = "evil.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("rejects disallowed host with 421", func(t *testing.T) {
		mw := AllowedHosts([]string{"example.com"})
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Host = "evil.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusMisdirectedRequest, rec.Code)
	})

	t.Run("allows matching host", func(t *testing.T) {
		mw := AllowedHosts([]string{"example.com"})
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Host = "example.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("strips port before comparing", func(t *testing.T) {
		mw := AllowedHosts([]string{"example.com"})
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		testCases := []struct {
			name     string
			host     string
			expected int
		}{
			{"with port 443", "example.com:443", http.StatusOK},
			{"with port 3000", "example.com:3000", http.StatusOK},
			{"bare host", "example.com", http.StatusOK},
			{"mismatched host with port", "evil.com:443", http.StatusMisdirectedRequest},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, "/test", nil)
				req.Host = tc.host
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				assert.Equal(t, tc.expected, rec.Code)
			})
		}
	})

	t.Run("multiple allowed hosts", func(t *testing.T) {
		mw := AllowedHosts([]string{"a.example.com", "b.example.com"})
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		for _, host := range []string{"a.example.com", "b.example.com"} {
			t.Run(host, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, "/test", nil)
				req.Host = host
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusOK, rec.Code)
			})
		}

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Host = "c.example.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusMisdirectedRequest, rec.Code)
	})

	t.Run("IPv6 host with brackets", func(t *testing.T) {
		mw := AllowedHosts([]string{"::1", "127.0.0.1"})
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		// net.SplitHostPort strips brackets from "[::1]:3000" -> "::1".
		// AllowedHosts callers should list hostnames, not IPs.
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Host = "[::1]:3000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	})

	t.Run("passes request through to next handler on match", func(t *testing.T) {
		mw := AllowedHosts([]string{"example.com"})

		var capturedHost string
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedHost = r.Host
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/test?foo=bar", nil)
		req.Host = "example.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "example.com", capturedHost)
	})
}

func TestStripHostPort(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"example.com", "example.com"},
		{"example.com:443", "example.com"},
		{"example.com:3000", "example.com"},
		{"[::1]:8080", "::1"}, // net.SplitHostPort strips brackets
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"127.0.0.1:9090", "127.0.0.1"},
		{"127.0.0.1", "127.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := stripHostPort(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestAllowedHostsIntegration(t *testing.T) {
	// Simulate the full chain: AllowedHosts -> HTTPSRedirect
	mw := AllowedHosts([]string{"example.com"})

	var called bool
	handler := mw(HTTPSRedirect(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))

	t.Run("evil host blocked before redirect", func(t *testing.T) {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/path", nil)
		req.Host = "evil.com"
		// No TLS: HTTPSRedirect would normally fire
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusMisdirectedRequest, rec.Code,
			"evil host should be blocked by AllowedHosts before HTTPSRedirect can poison")
		assert.False(t, called, "downstream handler should never be called")
	})

	t.Run("good host passes through to redirect", func(t *testing.T) {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/path", nil)
		req.Host = "example.com"
		// No TLS: HTTPSRedirect fires
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		// Redirected, not the downstream handler
		assert.Equal(t, http.StatusMovedPermanently, rec.Code)
		require.NotEmpty(t, rec.Header().Get("Location"))
		assert.Contains(t, rec.Header().Get("Location"), "example.com",
			"redirect target should use the allowed host")
		assert.False(t, called, "redirect intercepts before handler")
	})
}
