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
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/core/enginehost"
)

// A plugin registers its engine at Start, which is after the host was built
// and after the content store was built from it. The content path has to
// follow that registration, or the authoring routes serve one registry while
// the content routes serve another.
//
// The sibling test supplies the reader directly. This one supplies nothing and
// registers the engine the way a plugin does, which is the path the runtime
// wires.
func TestContentAPI_FollowsAnEngineRegisteredAfterTheHostWasBuilt(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Postgres(t)

	host := enginehost.NewHost(pool, pool.SQLDB(), &config.Config{}, nil, "test")
	source := core.LazySchemaSource(host.(core.SchemaSourceProvider).SchemaSource)

	// Nothing has registered, and this host was never given the built-in
	// engine either, so the reader has to say there is no engine rather than
	// report an install with no content types.
	require.True(t, core.NoSchemaEngine(source),
		"a host with no engine must read as having none")

	eng := schemaengine.New(testhost.New(pool))
	def, err := json.Marshal(map[string]any{
		"name":         "memos",
		"display_name": "Memos",
		"fields":       []map[string]any{{"name": "title", "field_type": "text"}},
	})
	require.NoError(t, err)
	require.NoError(t, eng.Apply(ctx, "memos", def))

	cfg := &config.Config{
		JWTSecrets:       []string{testSecret},
		JWTSecret:        testSecret,
		JWTExpirySecs:    3600,
		CORSOrigins:      []string{"*"},
		SecureCookie:     false,
		DatabaseDriver:   "postgres",
		MaxJSONBodyBytes: 1 << 20,
	}

	// The router is built before the registration, exactly as the runtime
	// builds it.
	router, err := NewAPIRouter(pool, cfg, nil,
		WithSchemaSource(source),
		WithLifetime(testLifetime(t)),
	)
	require.NoError(t, err)

	host.(core.SchemaEngineRegistrar).RegisterSchemaEngine(eng)
	require.False(t, core.NoSchemaEngine(source),
		"the reader did not follow the registration")

	userID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles, token_version) VALUES ($1, $2, $3, $4, $5)`,
		userID, "registrar@test.com", "$2a$10$placeholder-hash-for-test", `{"super_admin"}`, 1)
	require.NoError(t, err, "seed user")
	token := "Bearer " + adminJWT(userID, "registrar@test.com", []string{"super_admin"})

	body := strings.NewReader(`{"data":{"title":"written after the engine registered"}}`)
	cr := httptest.NewRequest(http.MethodPost, "/api/v1/content/memos", body)
	cr.Header.Set("Authorization", token)
	cr.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	router.ServeHTTP(cw, cr)
	require.Equal(t, http.StatusCreated, cw.Code,
		"the content API could not write through an engine registered after it was built: %s", cw.Body.String())

	lr := httptest.NewRequest(http.MethodGet, "/api/v1/content/memos", nil)
	lr.Header.Set("Authorization", token)
	lw := httptest.NewRecorder()
	router.ServeHTTP(lw, lr)
	require.Equal(t, http.StatusOK, lw.Code, lw.Body.String())
	require.Contains(t, lw.Body.String(), "written after the engine registered")

	// A plugin stopping deregisters with nil. The reader has to go back to
	// reporting no engine, not to reporting an empty world.
	host.(core.SchemaEngineRegistrar).RegisterSchemaEngine(nil)
	require.True(t, core.NoSchemaEngine(source),
		"the reader did not follow the deregistration")
}
