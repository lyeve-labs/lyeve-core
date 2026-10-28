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
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// An account create and a first-admin claim with no email answer 422 with a
// message that names the email field, in the caller's language, instead of
// the catalog's unfilled template.
func TestAdminRouter_MissingEmailNamesTheField(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	const setupToken = "required-email-setup-token-0123"
	const password = "CorrectHorseBatteryStaple1!"
	languages := []struct {
		header string
		want   string
	}{
		{"en", "The field email is required."},
		{"fr", "Le champ email est obligatoire."},
	}
	bodies := []struct {
		name string
		body string
	}{
		{"email empty", `"email":"",`},
		{"email absent", ``},
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
			cfg := testConfig()
			cfg.DatabaseDriver = pool.Engine()
			cfg.PasswordHashAlgo = "bcrypt"
			cfg.SetupToken = setupToken
			router, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)))
			require.NoError(t, err)

			send := func(t *testing.T, path, body, bearer, language, want string) {
				t.Helper()
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Accept-Language", language)
				if bearer != "" {
					r.Header.Set("Authorization", "Bearer "+bearer)
				}
				rr := httptest.NewRecorder()
				router.ServeHTTP(rr, r)
				require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
				var out struct {
					Error string `json:"error"`
					Code  string `json:"code"`
				}
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
				assert.Equal(t, "VALIDATION_REQUIRED", out.Code)
				assert.Equal(t, want, out.Error)
			}

			// Setup is open only while no account exists, so it goes first.
			for _, b := range bodies {
				for _, lang := range languages {
					t.Run("setup/"+b.name+"/"+lang.header, func(t *testing.T) {
						send(t, "/api/admin/setup",
							`{`+b.body+`"password":"`+password+`","setup_token":"`+setupToken+`"}`, "", lang.header, lang.want)
					})
				}
			}
			users := db.NewUserStore(pool)
			n, err := users.Count(context.Background())
			require.NoError(t, err)
			require.Zero(t, n, "a refused claim created an account")

			admin, err := users.Create(context.Background(), "email-admin-"+uuid.NewString()[:8]+"@example.com",
				"$2a$10$placeholderhashforemailXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", []string{"super_admin"}, "default")
			require.NoError(t, err)
			tok, err := auth.Sign(cfg.JWTSecret, 3600, admin.ID, admin.Email, admin.Roles, "default", admin.TokenVersion)
			require.NoError(t, err)
			for _, b := range bodies {
				for _, lang := range languages {
					t.Run("create/"+b.name+"/"+lang.header, func(t *testing.T) {
						send(t, "/api/admin/users", `{`+b.body+`"password":"`+password+`","roles":["viewer"]}`, tok, lang.header, lang.want)
					})
				}
			}
		})
	}
}
