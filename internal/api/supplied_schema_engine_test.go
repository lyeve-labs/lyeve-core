package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
)

// The kernel claims an extension point: someone brings their own
// schema engine, builds the engine, and serves content through it. This drives
// that claim end to end with an engine that imports nothing but pkg/core.
func TestContentAPI_ServesFromASuppliedSchemaEngine(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Postgres(t)

	eng := schemaengine.New(testhost.New(pool))
	def, err := json.Marshal(map[string]any{
		"name":         "notes",
		"display_name": "Notes",
		"fields":       []map[string]any{{"name": "title", "field_type": "text"}},
	})
	require.NoError(t, err)
	require.NoError(t, eng.Apply(ctx, "notes", def), "the supplied engine could not create its table")

	cfg := &config.Config{
		JWTSecrets:       []string{testSecret},
		JWTSecret:        testSecret,
		JWTExpirySecs:    3600,
		CORSOrigins:      []string{"*"},
		SecureCookie:     false,
		DatabaseDriver:   "postgres",
		MaxJSONBodyBytes: 1 << 20,
	}

	router, err := NewAPIRouter(pool, cfg, nil,
		WithSchemaSource(eng.SchemaSource()),
		WithLifetime(testLifetime(t)),
	)
	require.NoError(t, err)

	// The token version check reads sys_users for the caller, so the caller
	// has to exist before the request is made.
	userID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles, token_version) VALUES ($1, $2, $3, $4, $5)`,
		userID, "super@test.com", "$2a$10$placeholder-hash-for-test", `{"super_admin"}`, 1)
	require.NoError(t, err, "seed user")

	token := "Bearer " + adminJWT(userID, "super@test.com", []string{"super_admin"})

	body := strings.NewReader(`{"data":{"title":"written through a supplied engine"}}`)
	cr := httptest.NewRequest(http.MethodPost, "/api/v1/content/notes", body)
	cr.Header.Set("Authorization", token)
	cr.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, cr)
	require.Equal(t, http.StatusCreated, cw.Code,
		"the content API could not write to a table the supplied engine created: %s", cw.Body.String())

	lr := httptest.NewRequest(http.MethodGet, "/api/v1/content/notes", nil)
	lr.Header.Set("Authorization", token)
	lw := httptest.NewRecorder()
	router.ServeHTTP(lw, lr)
	require.Equal(t, http.StatusOK, lw.Code,
		"the content API did not serve a schema the supplied engine defined: %s", lw.Body.String())
	require.Contains(t, lw.Body.String(), "written through a supplied engine",
		"the entry written through the supplied engine did not come back")
}
