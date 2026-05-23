package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func noSchemaEngineRouter(t *testing.T) (http.Handler, string) {
	t.Helper()
	ctx := t.Context()
	pool := testdb.Postgres(t)

	cfg := &config.Config{
		JWTSecrets:       []string{testSecret},
		JWTSecret:        testSecret,
		JWTExpirySecs:    3600,
		CORSOrigins:      []string{"*"},
		DatabaseDriver:   "postgres",
		MaxJSONBodyBytes: 1 << 20,
	}

	router, err := NewAPIRouter(pool, cfg, nil,
		WithSchemaSource(core.AbsentSchemaSource{}),
		WithLifetime(testLifetime(t)),
	)
	require.NoError(t, err)

	userID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles, token_version) VALUES ($1, $2, $3, $4, $5)`,
		userID, "super@test.com", "$2a$10$placeholder-hash-for-test", `{"super_admin"}`, 1)
	require.NoError(t, err, "seed user")

	return router, "Bearer " + adminJWT(userID, "super@test.com", []string{"super_admin"})
}

// An install with no schema engine keeps serving, but it must never answer a
// content route with an empty page. An empty list is what a caller sees after
// its data is deleted, so reporting emptiness for a missing engine publishes a
// data-loss reading on every route at once.
func TestContentAPI_WithoutASchemaEngine_SaysSoRatherThanAnsweringEmpty(t *testing.T) {
	router, token := noSchemaEngineRouter(t)

	for _, tc := range []struct{ name, method, path, body string }{
		{"list", http.MethodGet, "/api/v1/content/notes", ""},
		{"read", http.MethodGet, "/api/v1/content/notes/" + uuid.New().String(), ""},
		{"create", http.MethodPost, "/api/v1/content/notes", `{"data":{"title":"x"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			} else {
				body = strings.NewReader("")
			}
			r := httptest.NewRequest(tc.method, tc.path, body)
			r.Header.Set("Authorization", token)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)

			require.NotEqual(t, http.StatusOK, w.Code,
				"answered 200 with no schema engine installed: %s", w.Body.String())
			require.Contains(t, strings.ToLower(w.Body.String()), "no schema engine",
				"the refusal does not say why: %s", w.Body.String())
		})
	}
}

// The kernel stays in rotation, so readiness is a 200. It must not call itself
// ready, because an operator reading a bare "ready" would have no way to learn
// the install cannot serve content.
func TestReadiness_WithoutASchemaEngine_ReportsDegraded(t *testing.T) {
	router, token := noSchemaEngineRouter(t)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil)
	r.Header.Set("Authorization", token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code, "the kernel should keep serving: %s", w.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, "degraded", got["status"], "readiness claimed a state it is not in: %s", w.Body.String())
	require.Equal(t, "absent", got["schema_engine"])
	require.NotEmpty(t, got["limitations"], "degraded readiness says nothing about what is lost")
}
