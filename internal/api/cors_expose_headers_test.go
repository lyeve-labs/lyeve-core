package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A cross-origin script reads only the CORS-safelisted response headers unless
// the server names the others. The engine sets X-Request-Id on every response
// and RateLimit-* on throttled ones, and a browser client needs both named to
// quote a request id in a bug report or read the values it backs off with.
func corsHandler(t *testing.T, cfg CORSConfig) http.Handler {
	t.Helper()
	return corsMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestCORS_ExposeHeadersNamesTheHeadersAClientMustRead(t *testing.T) {
	h := corsHandler(t, CORSConfig{
		Origins:       []string{"https://app.example.com"},
		AllowMethods:  "GET, POST",
		AllowHeaders:  "Content-Type",
		ExposeHeaders: "X-Request-Id, RateLimit-Remaining",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	req.Header.Set("Origin", "https://app.example.com")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	require.Equal(t, "https://app.example.com", res.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "X-Request-Id, RateLimit-Remaining", res.Header().Get("Access-Control-Expose-Headers"))
}

func TestCORS_PreflightCarriesExposeHeaders(t *testing.T) {
	h := corsHandler(t, CORSConfig{
		Origins:       []string{"https://app.example.com"},
		AllowMethods:  "GET, POST",
		AllowHeaders:  "Content-Type",
		ExposeHeaders: "X-Request-Id",
	})

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/thing", nil)
	req.Header.Set("Origin", "https://app.example.com")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	require.Equal(t, http.StatusNoContent, res.Code)
	assert.Equal(t, "X-Request-Id", res.Header().Get("Access-Control-Expose-Headers"))
}

// An unmatched origin gets no CORS headers at all, exposure included.
func TestCORS_UnmatchedOriginIsNotToldWhatItMayRead(t *testing.T) {
	h := corsHandler(t, CORSConfig{
		Origins:       []string{"https://app.example.com"},
		AllowMethods:  "GET",
		AllowHeaders:  "Content-Type",
		ExposeHeaders: "X-Request-Id",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	req.Header.Set("Origin", "https://evil.example.net")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	assert.Empty(t, res.Header().Get("Access-Control-Expose-Headers"))
}

// Empty config omits the header rather than sending an empty one.
func TestCORS_NoExposeHeadersConfiguredOmitsTheHeader(t *testing.T) {
	h := corsHandler(t, CORSConfig{
		Origins:      []string{"https://app.example.com"},
		AllowMethods: "GET",
		AllowHeaders: "Content-Type",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	req.Header.Set("Origin", "https://app.example.com")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	_, present := res.Header()["Access-Control-Expose-Headers"]
	assert.False(t, present, "an unset ExposeHeaders must not emit an empty header")
}
