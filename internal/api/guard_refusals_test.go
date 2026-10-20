package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The host check, the IP allowlist and the key scope check refuse before any
// handler runs. A client meets them through the same code that reads every
// other refusal, so each answers in the error envelope the handlers use, as
// JSON, with the request id the response header carries. The security headers
// run ahead of the guards and still mark the body nosniff.
func TestRouters_GuardRefusalsAnswerTheErrorEnvelope(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.AllowedHosts = []string{"cms.example.com"}
	// httptest.NewRequest sends from 192.0.2.1, inside this range.
	cfg.IPAllowlist = []string{"192.0.2.0/24"}

	contentKey := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") == "" {
				next.ServeHTTP(w, r)
				return
			}
			claims := &core.AuthClaims{
				UserID:   uuid.NewString(),
				Roles:    []string{"viewer"},
				Scopes:   []string{"content:read"},
				IsAPIKey: true,
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}

	pool := &fakeDB{engine: "postgres"}
	admin, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)), WithAPIKeyAuth(contentKey))
	require.NoError(t, err)
	apiRouter, err := NewAPIRouter(pool, cfg, nil, WithLifetime(testLifetime(t)), WithAPIKeyAuth(contentKey))
	require.NoError(t, err)

	type refusal struct {
		name    string
		router  http.Handler
		method  string
		path    string
		host    string
		remote  string
		key     bool
		status  int
		message string
		code    string
	}
	var cases []refusal
	for _, rt := range []struct {
		name        string
		router      http.Handler
		scopedRoute string
		scopeMethod string
	}{
		{"admin", admin, "/api/admin/users", http.MethodPost},
		{"api", apiRouter, "/api/v1/schemas", http.MethodGet},
	} {
		cases = append(cases,
			refusal{rt.name + " host it does not serve", rt.router, http.MethodGet, rt.scopedRoute, "evil.example", "", false,
				http.StatusMisdirectedRequest, "misdirected request", "http_421"},
			refusal{rt.name + " address outside the allowlist", rt.router, http.MethodGet, rt.scopedRoute, "cms.example.com", "203.0.113.9:4000", false,
				http.StatusForbidden, "forbidden", "forbidden"},
			refusal{rt.name + " key without the scope", rt.router, rt.scopeMethod, rt.scopedRoute, "cms.example.com", "", true,
				http.StatusForbidden, "insufficient scope", "forbidden"},
		)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Host = tc.host
			if tc.remote != "" {
				req.RemoteAddr = tc.remote
			}
			if tc.key {
				req.Header.Set("X-API-Key", "lyeve_test_key")
			}
			if tc.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			tc.router.ServeHTTP(rec, req)

			assert.Equal(t, tc.status, rec.Code, "body = %q", rec.Body.String())
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

			var body map[string]string
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body = %q", rec.Body.String())
			id := rec.Header().Get("X-Request-ID")
			require.NotEmpty(t, id)
			assert.Equal(t, map[string]string{
				"error":      tc.message,
				"code":       tc.code,
				"request_id": id,
			}, body)
		})
	}
}
