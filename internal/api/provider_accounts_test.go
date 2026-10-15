package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// eachProviderDialect runs fn against every dialect the run covers.
func eachProviderDialect(t *testing.T, fn func(t *testing.T, pool db.DB)) {
	t.Helper()
	dialects := []struct {
		name string
		pool func(*testing.T) db.DB
	}{
		{"postgres", testdb.Postgres},
		{"mysql", testdb.MySQL},
		{"mssql", testdb.MSSQL},
	}
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			fn(t, d.pool(t))
		})
	}
}

// A trusted issuer's first sign-in answers the same whether another tenant
// holds the verified address or nobody does. The account it creates belongs
// to the issuer's tenant alone, so the other tenant's account decides nothing.
func TestTrustedIssuer_AddressHeldInAnotherTenantAnswersAsAFreeOne(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		iss := newTestIssuer(t)
		policy := auth.IssuerPolicy{Issuer: iss.srv.URL, Tenant: "agency", DefaultRoles: []string{"viewer"}}

		taken := "taken-" + uuid.NewString()[:8] + "@agency.example"
		free := "free-" + uuid.NewString()[:8] + "@agency.example"
		_, err := users.Create(ctx, taken, "$2a$10$placeholderhashXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", []string{"editor"}, "elsewhere")
		require.NoError(t, err)

		serve := func(sub, email string) (int, string) {
			var seenEmail string
			h := trustedIssuerAuth([]auth.IssuerPolicy{policy}, users, nil)(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenEmail = claimsFromCtx(r).Email
				w.WriteHeader(http.StatusOK)
			})))
			req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
			req.Header.Set("Authorization", "Bearer "+iss.sign(t, map[string]any{"sub": sub, "email": email, "email_verified": true}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec.Code, seenEmail
		}

		takenCode, takenEmail := serve("sub-taken", taken)
		freeCode, freeEmail := serve("sub-free", free)
		assert.Equal(t, http.StatusOK, freeCode, "a free address is admitted")
		assert.Equal(t, freeCode, takenCode, "an address another tenant holds is answered as a free one")
		assert.Equal(t, taken, takenEmail, "the session carries the address the issuer verified")
		assert.Equal(t, free, freeEmail)

		other, err := users.GetByEmail(ctx, taken)
		require.NoError(t, err)
		assert.Equal(t, "elsewhere", other.TenantID, "the other tenant's account is untouched")
	})
}
