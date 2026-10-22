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

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// The admin router copies the operator's password settings into the users
// handler, whose own default is weaker. A password that meets the default but
// not the configured minimum has to be refused on both routes that set one, so
// losing that copy fails here instead of lowering the minimum in silence.
func TestAdminRouter_UsersRoutesHoldTheConfiguredPasswordPolicy(t *testing.T) {
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
			admin, err := users.Create(ctx, "policy-admin-"+uuid.NewString()[:8]+"@example.com",
				"$2a$10$placeholderhashforpolicyXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", []string{"super_admin"}, "default")
			require.NoError(t, err)
			target, err := users.Create(ctx, "policy-target-"+uuid.NewString()[:8]+"@example.com",
				"$2a$10$placeholderhashforpolicyXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", []string{"editor"}, "default")
			require.NoError(t, err)

			cfg := testConfig()
			cfg.DatabaseDriver = pool.Engine()
			cfg.PasswordHashAlgo = "bcrypt"
			cfg.PasswordMinLength = 20
			cfg.PasswordRequireComplexity = true
			cfg.PasswordCheckCommon = true
			router, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)))
			require.NoError(t, err)
			tok, err := auth.Sign(cfg.JWTSecret, 3600, admin.ID, admin.Email, admin.Roles, "default", admin.TokenVersion)
			require.NoError(t, err)

			// Fifteen characters with mixed case and digits: the default
			// policy takes it, the configured minimum of twenty does not.
			password := "Zq7vLm2pXr9tKw4"
			require.Len(t, password, 15)
			require.NoError(t, auth.ValidatePassword(auth.DefaultPasswordPolicy(), password))

			send := func(method, path, body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+tok)
				rr := httptest.NewRecorder()
				router.ServeHTTP(rr, r)
				return rr
			}
			requireTooShort := func(t *testing.T, rr *httptest.ResponseRecorder) {
				t.Helper()
				require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
				var out struct {
					Code string `json:"code"`
				}
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
				require.Equal(t, "AUTH_PASSWORD_TOO_SHORT", out.Code)
			}

			t.Run("user create", func(t *testing.T) {
				rr := send(http.MethodPost, "/api/admin/users",
					`{"email":"policy-new-`+uuid.NewString()[:8]+`@example.com","password":"`+password+`"}`)
				requireTooShort(t, rr)
			})
			t.Run("super admin sets a password", func(t *testing.T) {
				rr := send(http.MethodPut, "/api/admin/users/"+target.ID.String()+"/password",
					`{"password":"`+password+`"}`)
				requireTooShort(t, rr)
			})
		})
	}
}
