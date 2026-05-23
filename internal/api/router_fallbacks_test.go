package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// An unrouted path and a wrong method are the two responses the router itself
// writes, and chi's defaults answer them with plain-text "404 page not found"
// and an empty 405. Both break the envelope every handler returns, so a client
// that parses {"error", "code", "request_id"} uniformly chokes on exactly the
// two mistakes it is most likely to make against an unfamiliar API: with no
// request id on either to look up afterwards.
func fallbackRouter(t *testing.T) *chi.Mux {
	t.Helper()
	r := chi.NewRouter()
	applyBuiltinMiddleware(r, &config.Config{MaxBodyBytes: 1 << 20}, nil)
	r.Get("/api/v1/known", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Post("/api/v1/known", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	r.Route("/api/v1/sub", func(sr chi.Router) {
		sr.Get("/thing", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	})
	return r
}

func doFallback(t *testing.T, r *chi.Mux, method, path string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body),
		"%s %s answered %q, which is not the JSON envelope", method, path, rec.Body.String())
	return rec, body
}

func TestRouterFallbacks_UnknownPathIsTheJSONEnvelope(t *testing.T) {
	rec, body := doFallback(t, fallbackRouter(t), http.MethodGet, "/api/v1/no-such-endpoint")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", body["code"])
	assert.NotEmpty(t, body["error"])
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	assert.Contains(t, body, "request_id", "a 404 must be correlatable with the log")
}

func TestRouterFallbacks_WrongMethodIsTheJSONEnvelope(t *testing.T) {
	rec, body := doFallback(t, fallbackRouter(t), http.MethodDelete, "/api/v1/known")

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "method_not_allowed", body["code"])
	assert.NotEmpty(t, body["error"], "an empty 405 body tells a client nothing")
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
}

// Chi copies both handlers onto each subrouter as it is mounted, but only for
// subrouters mounted after they were set. Registering the fallbacks first is
// what makes that hold. This pins it so a later reordering of the chain cannot
// quietly return half the tree to the defaults.
func TestRouterFallbacks_ReachSubroutersMountedLater(t *testing.T) {
	r := fallbackRouter(t)

	rec, body := doFallback(t, r, http.MethodGet, "/api/v1/sub/missing")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", body["code"])

	rec, body = doFallback(t, r, http.MethodPost, "/api/v1/sub/thing")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "method_not_allowed", body["code"])
}

// chi carries the methods a 405 path accepts, but only its own handler reads
// them, so the envelope handler sets Allow itself. RFC 9110 requires it on a
// 405.
func TestRouterFallbacks_WrongMethodSaysWhichMethodsAreAllowed(t *testing.T) {
	rec, _ := doFallback(t, fallbackRouter(t), http.MethodDelete, "/api/v1/known")

	allow := rec.Header().Get("Allow")
	require.NotEmpty(t, allow, "a 405 that does not say what is allowed is a guess with no next step")
	assert.Contains(t, allow, http.MethodGet)
	assert.Contains(t, allow, http.MethodPost)
	assert.NotContains(t, allow, http.MethodDelete, "the method that was refused is not allowed")
}

// Allow is answered from the routing tree, so a subrouter mounted later reports
// its own methods rather than the parent's.
func TestRouterFallbacks_AllowIsPerPath(t *testing.T) {
	rec, _ := doFallback(t, fallbackRouter(t), http.MethodPost, "/api/v1/sub/thing")

	allow := rec.Header().Get("Allow")
	assert.Contains(t, allow, http.MethodGet)
	assert.NotContains(t, allow, http.MethodPost)
}

// The fallbacks must not shadow a route that does exist.
func TestRouterFallbacks_LeaveARealRouteAlone(t *testing.T) {
	rec := httptest.NewRecorder()
	fallbackRouter(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/known", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
}
