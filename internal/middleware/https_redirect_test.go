package middleware_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

func TestHTTPSRedirect_DisabledWhenSecureModeFalse(t *testing.T) {
	mw := apimw.HTTPSRedirect(false)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("expected handler to be called (no-op middleware), but it wasn't")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHTTPSRedirect_EnabledRedirectsHTTPTraffic(t *testing.T) {
	tests := []struct {
		name       string
		tls        bool
		proto      string // X-Forwarded-Proto header
		wantCalled bool
		wantCode   int
		wantTarget string
	}{
		{
			name:       "plain HTTP with no forwarded proto -> 301 redirect",
			tls:        false,
			proto:      "",
			wantCalled: false,
			wantCode:   http.StatusMovedPermanently,
			wantTarget: "https://example.com/api/items?page=1",
		},
		{
			name:       "TLS connection -> pass through",
			tls:        true,
			proto:      "",
			wantCalled: true,
			wantCode:   http.StatusOK,
		},
		{
			name:       "X-Forwarded-Proto: https -> pass through",
			tls:        false,
			proto:      "https",
			wantCalled: true,
			wantCode:   http.StatusOK,
		},
		{
			name:       "X-Forwarded-Proto: http -> redirect",
			tls:        false,
			proto:      "http",
			wantCalled: false,
			wantCode:   http.StatusMovedPermanently,
			wantTarget: "https://example.com/api/items?page=1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := apimw.HTTPSRedirect(true)
			called := false
			handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))

			// Use path-only target to match real HTTP server behavior
			// (httptest.NewRequest sets r.RequestURI to target, matching
			// the path+query that a real HTTP server populates).
			req := httptest.NewRequest(http.MethodGet, "/api/items?page=1", nil)
			req.Host = "example.com"
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}
			if tt.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tt.proto)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if called != tt.wantCalled {
				t.Errorf("handler called = %v, want %v", called, tt.wantCalled)
			}
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantTarget != "" {
				if got := rec.Header().Get("Location"); got != tt.wantTarget {
					t.Errorf("Location = %q, want %q", got, tt.wantTarget)
				}
			}
		})
	}
}

func TestHTTPSRedirect_PreservesRequestMethod(t *testing.T) {
	mw := apimw.HTTPSRedirect(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for redirect")
	}))

	req := httptest.NewRequest(http.MethodPost, "/api", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMovedPermanently)
	}
	loc := rec.Header().Get("Location")
	if loc != "https://example.com/api" {
		t.Errorf("Location = %q, want %q", loc, "https://example.com/api")
	}
}

// The image's HEALTHCHECK probes plaintext loopback and requires a literal
// 200, so a redirect here marks every production container unhealthy.
func TestHTTPSRedirect_HealthProbesServePlaintext(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz", "/startup"} {
		t.Run(path, func(t *testing.T) {
			mw := apimw.HTTPSRedirect(true)
			called := false
			handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Host = "example.com"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if !called {
				t.Errorf("%s was redirected instead of served", path)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}
		})
	}
}

// The exemption is an exact path match, not a prefix: a route that merely
// starts with a probe name must still redirect.
func TestHTTPSRedirect_HealthExemptionIsNotAPrefix(t *testing.T) {
	mw := apimw.HTTPSRedirect(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for redirect")
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz/secrets", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMovedPermanently)
	}
}

func TestHTTPSRedirect_RootPath(t *testing.T) {
	mw := apimw.HTTPSRedirect(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	want := "https://example.com/"
	if got := rec.Header().Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}
