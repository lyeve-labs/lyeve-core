package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
)

// A client revalidates a content read with the tag it was served. The tag
// comes from the cache middleware on the route, so the test goes through the
// router the engine mounts. The entry has many fields because Go walks a map
// in a different order on each pass, and the tag has to describe the entry,
// not the walk.
func TestContentAPI_ConditionalGetAnswersNotModified(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Postgres(t)

	fields := make([]map[string]any, 0, 12)
	data := map[string]any{}
	for i := range 12 {
		name := fmt.Sprintf("field_%02d", i)
		fields = append(fields, map[string]any{"name": name, "field_type": "text"})
		data[name] = fmt.Sprintf("value %d", i)
	}
	def, err := json.Marshal(map[string]any{"name": "tagged", "display_name": "Tagged", "fields": fields})
	require.NoError(t, err)
	eng := schemaengine.New(testhost.New(pool))
	require.NoError(t, eng.Apply(ctx, "tagged", def))

	cfg := &config.Config{
		JWTSecrets:       []string{testSecret},
		JWTSecret:        testSecret,
		JWTExpirySecs:    3600,
		CORSOrigins:      []string{"*"},
		DatabaseDriver:   "postgres",
		MaxJSONBodyBytes: 1 << 20,
	}
	router, err := NewAPIRouter(pool, cfg, nil,
		WithSchemaSource(eng.SchemaSource()),
		WithContentRules(eng),
		WithLifetime(testLifetime(t)),
	)
	require.NoError(t, err)

	userID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles, token_version) VALUES ($1, $2, $3, $4, $5)`,
		userID, "tags@test.com", "$2a$10$placeholder-hash-for-test", `{"super_admin"}`, 1)
	require.NoError(t, err, "seed user")
	token := "Bearer " + adminJWT(userID, "tags@test.com", []string{"super_admin"})

	do := func(method, path, body, ifNoneMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	body, err := json.Marshal(map[string]any{"data": data})
	require.NoError(t, err)
	created := do(http.MethodPost, "/api/v1/content/tagged", string(body), "")
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var entry struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &entry))

	for _, path := range []string{"/api/v1/content/tagged", "/api/v1/content/tagged/" + entry.ID} {
		first := do(http.MethodGet, path, "", "")
		require.Equal(t, http.StatusOK, first.Code, first.Body.String())
		tag := first.Header().Get("ETag")
		require.NotEmpty(t, tag, "GET %s served no tag", path)

		for i := range 20 {
			again := do(http.MethodGet, path, "", tag)
			require.Equal(t, http.StatusNotModified, again.Code,
				"GET %s with the tag it served answered %d on read %d", path, again.Code, i)
			assert.Empty(t, again.Body.String(), "GET %s sent a body with its 304", path)
		}

		stale := do(http.MethodGet, path, "", `"not-the-tag"`)
		assert.Equal(t, http.StatusOK, stale.Code, "GET %s with a stale tag", path)
	}
}
