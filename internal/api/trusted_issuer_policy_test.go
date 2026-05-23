package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/ssrf"
)

// fakeExternalUsers keeps issuers' accounts in memory.
type fakeExternalUsers struct {
	mu      sync.Mutex
	users   map[uuid.UUID]*domain.User
	revoked map[uuid.UUID]time.Time
}

func (f *fakeExternalUsers) SessionsRevokedAt(_ context.Context, id uuid.UUID) (*time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at, ok := f.revoked[id]; ok {
		return &at, nil
	}
	return nil, nil
}

func (f *fakeExternalUsers) EnsureExternalUser(_ context.Context, id uuid.UUID, email, hash string, roles []string, tenant string) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.users == nil {
		f.users = map[uuid.UUID]*domain.User{}
	}
	u, ok := f.users[id]
	if !ok {
		u = &domain.User{ID: id, Email: email, PasswordHash: hash, TenantID: tenant, TokenVersion: 1}
		f.users[id] = u
	}
	if u.TenantID != tenant {
		return nil, domain.ErrConflict
	}
	u.Roles = append([]string(nil), roles...)
	cp := *u
	return &cp, nil
}

type testIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	require.NoError(t, ssrf.AddAllowlist("127.0.0.1/32"))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	iss := &testIssuer{key: key}
	iss.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(iss.srv.Close)
	return iss
}

func (i *testIssuer) sign(t *testing.T, extra map[string]any) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": i.srv.URL, "aud": []string{security.JWTAudience}, "sub": "idp-user-7",
		"email": "person@agency.example", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
	for k, v := range extra {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(i.key)
	require.NoError(t, err)
	return s
}

// An issuer's token acts as a local account in the pinned tenant, holding the
// roles the policy maps from the issuer's groups and nothing it claims beyond
// them.
func TestTrustedIssuerAuth_AppliesThePolicy(t *testing.T) {
	iss := newTestIssuer(t)
	policy := auth.IssuerPolicy{
		Issuer: iss.srv.URL, Tenant: "agency", RolesClaim: "groups",
		RoleMap:      map[string]string{"cms-editors": "editor", "cms-admins": "admin"},
		DefaultRoles: []string{"viewer"},
	}
	users := &fakeExternalUsers{}

	serve := func(policies []auth.IssuerPolicy, token string) (*auth.Claims, int) {
		var seen *auth.Claims
		h := trustedIssuerAuth(policies, users, nil)(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = claimsFromCtx(r)
			w.WriteHeader(http.StatusOK)
		})))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return seen, rec.Code
	}

	t.Run("roles are mapped and capped, the tenant is pinned", func(t *testing.T) {
		got, code := serve([]auth.IssuerPolicy{policy}, iss.sign(t, map[string]any{
			"groups": []string{"cms-editors", "finance"},
			"roles":  []string{"super_admin"},
		}))
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, []string{"editor", "viewer"}, got.Roles, "unmapped groups and the token's own roles claim grant nothing")
		assert.Equal(t, "agency", got.TenantID)
		assert.Equal(t, auth.ExternalUserID(iss.srv.URL, "idp-user-7").String(), got.UserID, "the caller is the local account")
		assert.Equal(t, 1, got.TokenVersion, "the account's own version")
	})

	t.Run("plugins see the same caller", func(t *testing.T) {
		var seen *core.AuthClaims
		h := trustedIssuerAuth([]auth.IssuerPolicy{policy}, users, nil)(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = core.GetClaims(r.Context())
			w.WriteHeader(http.StatusOK)
		})))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/realtime/events", nil)
		req.Header.Set("Authorization", "Bearer "+iss.sign(t, map[string]any{"groups": []string{"cms-editors"}}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.NotNil(t, seen, "a plugin handler reads the caller through core.GetClaims")
		assert.Equal(t, auth.ExternalUserID(iss.srv.URL, "idp-user-7").String(), seen.UserID)
		assert.Equal(t, "agency", seen.TenantID)
		assert.Equal(t, []string{"editor", "viewer"}, seen.Roles)
	})

	t.Run("the roles follow the issuer on the next request", func(t *testing.T) {
		got, code := serve([]auth.IssuerPolicy{policy}, iss.sign(t, map[string]any{"groups": []string{"cms-admins"}}))
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, []string{"admin", "viewer"}, got.Roles)
	})

	t.Run("a token naming another tenant is refused", func(t *testing.T) {
		_, code := serve([]auth.IssuerPolicy{policy}, iss.sign(t, map[string]any{"tenant_id": "someone_else"}))
		assert.Equal(t, http.StatusUnauthorized, code)
	})

	t.Run("an issuer without a policy is not trusted", func(t *testing.T) {
		_, code := serve(nil, iss.sign(t, map[string]any{"roles": []string{"super_admin"}}))
		assert.Equal(t, http.StatusUnauthorized, code)
	})

	t.Run("an unverified email does not label the account", func(t *testing.T) {
		got, code := serve([]auth.IssuerPolicy{policy}, iss.sign(t, map[string]any{"email_verified": false}))
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, auth.ExternalUserID(iss.srv.URL, "idp-user-7").String()+"@external.invalid", got.Email)
	})

	t.Run("a token issued before the account's sessions ended is refused", func(t *testing.T) {
		id := auth.ExternalUserID(iss.srv.URL, "idp-user-7")
		users.mu.Lock()
		users.revoked = map[uuid.UUID]time.Time{id: time.Now()}
		users.mu.Unlock()
		_, code := serve([]auth.IssuerPolicy{policy}, iss.sign(t, map[string]any{"iat": time.Now().Add(-time.Minute).Unix()}))
		assert.Equal(t, http.StatusUnauthorized, code, "a logout, a password set or an erasure ends the issuer's earlier tokens")
		_, code = serve([]auth.IssuerPolicy{policy}, iss.sign(t, map[string]any{"iat": nil}))
		assert.Equal(t, http.StatusUnauthorized, code, "a token with no iat cannot show it came after")
		_, code = serve([]auth.IssuerPolicy{policy}, iss.sign(t, map[string]any{"iat": time.Now().Add(time.Second).Unix()}))
		assert.Equal(t, http.StatusOK, code, "a token issued after it is a new session")
		users.mu.Lock()
		users.revoked = nil
		users.mu.Unlock()
	})

	t.Run("an issuer the policies do not cover is not trusted", func(t *testing.T) {
		other := auth.IssuerPolicy{Issuer: "https://other-idp.example.com", Tenant: "agency"}
		_, code := serve([]auth.IssuerPolicy{other}, iss.sign(t, nil))
		assert.Equal(t, http.StatusUnauthorized, code)
	})

	t.Run("a subject spelled twice is refused", func(t *testing.T) {
		_, code := serve([]auth.IssuerPolicy{policy}, iss.sign(t, map[string]any{"Sub": "someone-else"}))
		assert.Equal(t, http.StatusUnauthorized, code)
	})

	t.Run("an expired account is refused", func(t *testing.T) {
		past := time.Now().Add(-time.Hour)
		users.mu.Lock()
		users.users[auth.ExternalUserID(iss.srv.URL, "idp-user-7")].ExpiresAt = &past
		users.mu.Unlock()
		_, code := serve([]auth.IssuerPolicy{policy}, iss.sign(t, nil))
		assert.Equal(t, http.StatusUnauthorized, code)
		users.mu.Lock()
		users.users[auth.ExternalUserID(iss.srv.URL, "idp-user-7")].ExpiresAt = nil
		users.mu.Unlock()
	})

	t.Run("a disabled account is refused", func(t *testing.T) {
		users.mu.Lock()
		users.users[auth.ExternalUserID(iss.srv.URL, "idp-user-7")].Disabled = true
		users.mu.Unlock()
		_, code := serve([]auth.IssuerPolicy{policy}, iss.sign(t, map[string]any{"groups": []string{"cms-editors"}}))
		assert.Equal(t, http.StatusUnauthorized, code)
	})
}
