package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The JWKS endpoint is how an external service verifies a CMS-issued token
// without a shared secret, so it is mounted and public on both listeners.
func TestAPIRouter_JWKSIsMountedAndPublic(t *testing.T) {
	t.Parallel()

	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /.well-known/jwks.json: status = %d, want 200 (no auth); body=%q",
			w.Code, w.Body.String())
	}

	// An empty keyset is valid JWKS and is what a build with no Ed25519 key
	// returns, so the shape is what matters here, not the key count.
	var body struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v; body=%q", err, w.Body.String())
	}
	if body.Keys == nil {
		t.Error(`response has no "keys" member; RFC 7517 requires it`)
	}
}
