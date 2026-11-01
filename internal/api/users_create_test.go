package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// An account holds only the roles its creator names. A create that names none
// is refused with a 422 naming the field and stores nothing, and one that
// names a role creates an account holding exactly that role.
func TestAdminRouter_UserCreateRequiresRoles(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	for _, d := range []struct {
		name string
		open func(*testing.T) db.DB
	}{
		{"postgres", testdb.Postgres},
		{"mysql", testdb.MySQL},
		{"mssql", testdb.MSSQL},
	} {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("%s not enabled for this run", d.name)
			}
			pool := d.open(t)
			users := db.NewUserStore(pool)
			ctx := context.Background()
			admin, err := users.Create(ctx, "roles-admin-"+uuid.NewString()[:8]+"@example.com",
				"$2a$10$placeholderhashforrolesXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", []string{"super_admin"}, "default")
			require.NoError(t, err)

			cfg := testConfig()
			cfg.DatabaseDriver = pool.Engine()
			cfg.PasswordHashAlgo = "bcrypt"
			router, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)))
			require.NoError(t, err)
			tok, err := auth.Sign(cfg.JWTSecret, 3600, admin.ID, admin.Email, admin.Roles, "default", admin.TokenVersion)
			require.NoError(t, err)

			send := func(body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+tok)
				rr := httptest.NewRecorder()
				router.ServeHTTP(rr, r)
				return rr
			}
			const password = "CorrectHorseBatteryStaple1!"

			for _, tc := range []struct {
				name  string
				roles string
			}{
				{"no roles key", ``},
				{"roles null", `,"roles":null`},
				{"roles empty", `,"roles":[]`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					email := "roles-none-" + uuid.NewString()[:8] + "@example.com"
					rr := send(`{"email":"` + email + `","password":"` + password + `"` + tc.roles + `}`)
					require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
					var out struct {
						Error string `json:"error"`
						Code  string `json:"code"`
					}
					require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
					assert.Equal(t, "VALIDATION_REQUIRED", out.Code)
					assert.Equal(t, "The field roles is required.", out.Error)
					_, err := users.GetByEmail(ctx, email)
					require.ErrorIs(t, err, domain.ErrNotFound, "a refused create stores no account")
				})
			}

			t.Run("a named role", func(t *testing.T) {
				email := "roles-viewer-" + uuid.NewString()[:8] + "@example.com"
				rr := send(`{"email":"` + email + `","password":"` + password + `","roles":["viewer"]}`)
				require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
				u, err := users.GetByEmail(ctx, email)
				require.NoError(t, err)
				assert.Equal(t, []string{"viewer"}, u.Roles)
			})
		})
	}
}
