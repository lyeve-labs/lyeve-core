package middleware_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// TestCSPNonce_HappyPath verifies that CSPNonce generates a cryptographically
// random 16-byte nonce, stores it in context and the X-CSP-Nonce response
// header, and that it can be retrieved via CSPNonceFromContext.
func TestCSPNonce_HappyPath(t *testing.T) {
	var capturedNonce string
	var ctxNonce string

	r := chi.NewRouter()
	r.Use(apimw.CSPNonce())

	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		capturedNonce = w.Header().Get("X-CSP-Nonce")
		ctxNonce = apimw.CSPNonceFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.NotEmpty(t, capturedNonce, "X-CSP-Nonce header should be set")
	require.NotEmpty(t, ctxNonce, "CSPNonceFromContext should return a nonce")
	assert.Equal(t, capturedNonce, ctxNonce, "header and context nonce should match")

	// Verify it's a valid base64 encoding of 16 bytes (24 base64 chars).
	decoded, err := base64.StdEncoding.DecodeString(capturedNonce)
	require.NoError(t, err, "nonce should be valid base64")
	assert.Len(t, decoded, 16, "nonce should decode to 16 bytes")
}

// TestCSPNonce_ConsistentAcrossHandler verifies that the same nonce is visible
// throughout the handler chain: it's set once per request and doesn't mutate.
func TestCSPNonce_ConsistentAcrossHandler(t *testing.T) {
	var nonce1, nonce2 string

	r := chi.NewRouter()
	r.Use(apimw.CSPNonce())

	// Dummy middleware that reads the nonce before the handler.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nonce1 = apimw.CSPNonceFromContext(r.Context())
			next.ServeHTTP(w, r)
		})
	})

	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		nonce2 = apimw.CSPNonceFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.NotEmpty(t, nonce1)
	assert.Equal(t, nonce1, nonce2, "nonce should be consistent across the middleware chain")
}

// TestCSPNonce_WithoutMiddleware verifies that CSPNonceFromContext returns ""
// when CSPNonce middleware is NOT in the chain: no panic, no garbage value.
func TestCSPNonce_WithoutMiddleware(t *testing.T) {
	r := chi.NewRouter()

	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		nonce := apimw.CSPNonceFromContext(r.Context())
		assert.Empty(t, nonce, "should return empty string when middleware not active")
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestCSPNonce_ErrorLogging verifies that the happy path logs nothing at ERROR.
// The error path, a crypto/rand failure, is impractical to trigger in a unit
// test.
func TestCSPNonce_ErrorLogging(t *testing.T) {
	var logBuf bytes.Buffer
	logHandler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})

	logger := slog.New(logHandler)
	slog.SetDefault(logger)
	// Restore after the test. Concurrent tests would conflict, and -race on
	// the suite catches that.
	defer slog.SetDefault(slog.Default())

	r := chi.NewRouter()
	r.Use(apimw.CSPNonce())

	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	logOutput := logBuf.String()
	if logOutput != "" {
		// Happy path shouldn't emit error logs. If any log line appears
		// at ERROR level, the test fails.
		lines := bytes.Split(logBuf.Bytes(), []byte("\n"))
		for _, line := range lines {
			if len(line) == 0 {
				continue
			}
			var entry map[string]any
			if err := json.Unmarshal(line, &entry); err != nil {
				continue
			}
			level, _ := entry["level"].(string)
			assert.NotEqual(t, "ERROR", level,
				"CSPNonce happy path should not emit ERROR-level logs, got: %s", string(line))
		}
	}
}
