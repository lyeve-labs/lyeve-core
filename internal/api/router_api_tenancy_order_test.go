package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// Plugin middleware supplied through WithMiddleware runs on the API router and
// must see the tenant a write resolved to. A write guard at the residency
// phase reads it with core.TenantIDFromCtx and passes a request that names no
// tenant, so mounting the plugin middleware above tenancy would switch the
// guard off without a word.
func TestAPIRouter_ExtraMiddlewareSeesResolvedTenant(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		roles  []string
		claim  string
		header string
		want   string
	}{
		{name: "the tenant a token claims", roles: []string{"editor"}, claim: "beta", want: "beta"},
		{name: "the tenant a super admin names", roles: []string{"super_admin"}, header: "acme", want: "acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()

			var seen string
			var ran bool
			router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, cfg, nil, WithLifetime(testLifetime(t)),
				WithMiddleware(tenantProbe(&seen, &ran)))
			require.NoError(t, err)

			token, err := auth.Sign(cfg.JWTSecret, 3600, uuid.New(), "writer@test.com", tc.roles, tc.claim, 1)
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/content/posts", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			if tc.header != "" {
				req.Header.Set("X-Tenant-ID", tc.header)
			}
			router.ServeHTTP(httptest.NewRecorder(), req)

			require.True(t, ran, "extra middleware never ran on a write")
			assert.Equal(t, tc.want, seen, "tenancy resolves below the plugin middleware in the chain")
		})
	}
}
