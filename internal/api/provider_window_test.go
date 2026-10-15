package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The window decides who can sign in, so only a signed-in session reads or
// changes it. An admin token holding every grant and an API key are refused
// with 403. An API key holding an admin role never reaches the route: the
// admin API refuses it with 401. No refused call changes the stored window.
func TestProviderWindow_RefusesAPIKeysAndAdminTokens(t *testing.T) {
	asKey := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			roles := []string{"editor"}
			if key == "admin-key" {
				roles = []string{"admin", "super_admin"}
			}
			claims := &core.AuthClaims{
				UserID:   uuid.NewString(),
				Roles:    roles,
				Scopes:   []string{"*"},
				TenantID: "default",
				IsAPIKey: true,
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}
	rg := newTokenRig(t, WithAPIKeyAuth(asKey))
	_, session := rg.user("admin")

	var grants []string
	for _, g := range core.AdminGrants() {
		grants = append(grants, g.Name)
	}
	token, _ := rg.issue(session, grants, nil)

	const path = "/api/admin/auth/provider-window"
	require.Equal(t, http.StatusOK, rg.get(path, session), "the route is mounted and a session reaches it")

	for _, c := range []struct {
		name   string
		bearer string
		header map[string]string
		want   int
	}{
		{"api key", "", map[string]string{"X-API-Key": "editor-key"}, http.StatusForbidden},
		{"api key with an admin role", "", map[string]string{"X-API-Key": "admin-key"}, http.StatusUnauthorized},
		{"admin token", token, nil, http.StatusForbidden},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			rec := rg.do(tokenCall{method: method, path: path, bearer: c.bearer, header: c.header, body: map[string]any{"days": 0}})
			assert.Equal(t, c.want, rec.Code, "%s %s: %s", c.name, method, rec.Body.String())
		}
	}

	days, err := config.NewProviderWindows(rg.pool).ProviderWindowDays(context.Background(), "default")
	require.NoError(t, err)
	assert.Equal(t, core.ProviderWindowDefaultDays, days, "no refused call changed the window")
}
