package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// bcrypt reads at most 72 bytes and refuses a longer password outright. Each
// route that sets a password answers that as a policy violation the caller
// can fix, never as a server fault. The refusal comes before any store call,
// so these handlers run without a database.
func TestPasswordRoutes_RefusePasswordOverBcryptLimit(t *testing.T) {
	t.Parallel()
	// 73 bytes that pass every other rule of the default policy.
	long := "Aa1" + strings.Repeat("x", 70)
	require.Len(t, long, 73)
	body := `{"email":"long@ex.test","password":"` + long + `","setup_token":"operator-setup-token-0123"}`

	assertRefused := func(t *testing.T, rr *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		var out struct {
			Code string `json:"code"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
		require.Equal(t, "AUTH_PASSWORD_TOO_LONG", out.Code)
	}

	t.Run("first-run setup", func(t *testing.T) {
		t.Parallel()
		h := NewAuthHandler(&fakeUserStore{}, nil, nil, "secret", 3600, false)
		h.WithSetupToken(NewSetupTokenFromEnv("operator-setup-token-0123"))
		rr := httptest.NewRecorder()
		h.Setup(rr, httptest.NewRequest(http.MethodPost, "/api/admin/setup", strings.NewReader(body)))
		assertRefused(t, rr)
	})

	// The admin router builds its own policy from the configuration, so the
	// limit has to survive that wiring, not only the handler default.
	t.Run("first-run setup through the admin router", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig()
		cfg.PasswordHashAlgo = "bcrypt"
		cfg.SetupToken = "operator-setup-token-0123"
		router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)))
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodPost, "/api/admin/setup", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, r)
		assertRefused(t, rr)
	})

	t.Run("user create", func(t *testing.T) {
		t.Parallel()
		h := NewUsersHandler(nil)
		r := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(body))
		r = r.WithContext(core.WithTenantID(r.Context(), "default"))
		rr := httptest.NewRecorder()
		h.Create(rr, r)
		assertRefused(t, rr)
	})

	t.Run("super admin sets a password", func(t *testing.T) {
		t.Parallel()
		h := NewUsersHandler(nil)
		id := uuid.NewString()
		r := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+id+"/password", strings.NewReader(body))
		r = chiCtx(r, map[string]string{"id": id})
		rr := httptest.NewRecorder()
		h.SetPassword(rr, r)
		assertRefused(t, rr)
	})
}
