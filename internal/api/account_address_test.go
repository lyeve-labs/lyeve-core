//go:build !mutest

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

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// First-boot setup and the admin create answer 422 for a value that is not an
// address, and store an address in its composed form whichever way it was
// typed, so the two spellings of one address cannot make two accounts.
func TestAccountCreation_AddressIsCheckedAndStoredInOneForm(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}
	if testing.Short() {
		t.Skip("integration test")
	}
	compliance.RegisterAuditWriter(&recordingAuditWriter{})
	t.Cleanup(compliance.ClearAuditWriter)
	pool := testdb.Postgres(t)
	users := db.NewUserStore(pool)
	const password = "CorrectHorseBatteryStaple1!"
	const token = "operator-setup-token-0123"

	setup := func(email string) *httptest.ResponseRecorder {
		h := NewAuthHandler(users, nil, nil, "secret", 3600, false)
		h.WithSetupToken(NewSetupTokenFromEnv(token))
		body := `{"email":"` + email + `","password":"` + password + `","setup_token":"` + token + `"}`
		r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/setup", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.Setup(rr, r)
		return rr
	}
	rr := setup("Admin <admin@example.com>")
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "VALIDATION_EMAIL")
	rr = setup("José-admin@example.com")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	admin, err := users.GetByEmail(context.Background(), "josé-admin@example.com")
	require.NoError(t, err)
	assert.Equal(t, "josé-admin@example.com", admin.Email)

	h := NewUsersHandler(users)
	h.host = auditOnlyHost{}
	create := func(email string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
		ctx := core.WithTenantID(r.Context(), "acme")
		ctx = context.WithValue(ctx, core.ClaimsKey, &core.AuthClaims{UserID: uuid.NewString(), Roles: []string{"super_admin"}, TenantID: "acme"})
		rr := httptest.NewRecorder()
		h.Create(rr, r.WithContext(ctx))
		return rr
	}
	rr = create("not-an-address")
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "VALIDATION_EMAIL")
	rr = create("zoë@example.com")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var created map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &created))
	assert.Equal(t, "zoë@example.com", created["email"])
	rr = create("ZOË@example.com")
	assert.Equal(t, http.StatusConflict, rr.Code, "the other spelling is the same address: %s", rr.Body.String())
}
