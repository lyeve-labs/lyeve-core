package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
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

// A trusted issuer's first sign-in answers the same status, body and timing
// class whether another tenant holds the verified address or nobody does. The
// account it creates belongs to the issuer's tenant alone, so the other
// tenant's account decides nothing.
func TestTrustedIssuer_AddressHeldInAnotherTenantAnswersAsAFreeOne(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		iss := newTestIssuer(t)
		policy := auth.IssuerPolicy{Issuer: iss.srv.URL, Tenant: "agency", DefaultRoles: []string{"viewer"}}
		h := trustedIssuerAuth([]auth.IssuerPolicy{policy}, users, nil)(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(claimsFromCtx(r).Email))
		})))

		serve := func(sub, email string) (int, string, time.Duration) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
			req.Header.Set("Authorization", "Bearer "+iss.sign(t, map[string]any{"sub": sub, "email": email, "email_verified": true}))
			rec := httptest.NewRecorder()
			start := time.Now()
			h.ServeHTTP(rec, req)
			return rec.Code, rec.Body.String(), time.Since(start)
		}

		const iterations = 10
		var takenTimes, freeTimes []time.Duration
		for i := 0; i < iterations; i++ {
			taken := "taken-" + uuid.NewString()[:8] + "@agency.example"
			free := "free-" + uuid.NewString()[:8] + "@agency.example"
			_, err := users.Create(ctx, taken, "$2a$10$placeholderhashXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", []string{"editor"}, "elsewhere")
			require.NoError(t, err)

			freeCode, freeBody, freeTook := serve("free-"+uuid.NewString(), free)
			takenCode, takenBody, takenTook := serve("taken-"+uuid.NewString(), taken)
			require.Equal(t, http.StatusOK, freeCode, "a free address is admitted")
			require.Equal(t, freeCode, takenCode, "an address another tenant holds is answered as a free one")
			assert.Equal(t, free, freeBody, "the session carries the address the issuer verified")
			assert.Equal(t, taken, takenBody)
			freeTimes, takenTimes = append(freeTimes, freeTook), append(takenTimes, takenTook)

			other, err := users.GetByEmail(ctx, taken)
			require.NoError(t, err)
			assert.Equal(t, "elsewhere", other.TenantID, "the other tenant's account is untouched")
		}
		ratio := float64(median(takenTimes)) / float64(median(freeTimes))
		assert.True(t, ratio >= 0.5 && ratio <= 2.0, "timing ratio taken/free %.2f, free %v", ratio, median(freeTimes))
	})
}

const providerTestPassword = "correct-horse-battery-staple"

// providerRig is the admin and content API routers over one database on a
// multi-tenant install, with the audit trail recorded.
type providerRig struct {
	t     *testing.T
	pool  db.DB
	cfg   *config.Config
	admin http.Handler
	api   http.Handler
	users *db.UserStore
	audit *recordingAuditWriter
	hash  string
}

func newProviderRig(t *testing.T, pool db.DB) *providerRig {
	t.Helper()
	audit := &recordingAuditWriter{}
	compliance.RegisterAuditWriter(audit)
	t.Cleanup(compliance.ClearAuditWriter)
	cfg := testConfig()
	cfg.MultiTenant = true
	admin, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)), WithScalingHost(auditOnlyHost{}))
	require.NoError(t, err)
	apiRouter, err := NewAPIRouter(pool, cfg, nil, WithLifetime(testLifetime(t)))
	require.NoError(t, err)
	hash, err := auth.HashPassword("bcrypt", providerTestPassword)
	require.NoError(t, err)
	return &providerRig{t: t, pool: pool, cfg: cfg, admin: admin, api: apiRouter, users: db.NewUserStore(pool), audit: audit, hash: hash}
}

// session creates a password account in tenant and signs a session for it.
func (rg *providerRig) session(tenant string, roles ...string) string {
	rg.t.Helper()
	u, err := rg.users.Create(context.Background(), "person-"+uuid.NewString()[:8]+"@example.com", rg.hash, roles, tenant)
	require.NoError(rg.t, err)
	token, err := auth.Sign(rg.cfg.JWTSecret, 3600, u.ID, u.Email, u.Roles, tenant, u.TokenVersion)
	require.NoError(rg.t, err)
	return token
}

// call sends a request with a JSON body, which may be raw text.
func (rg *providerRig) call(h http.Handler, method, path, bearer string, body any, header map[string]string) *httptest.ResponseRecorder {
	rg.t.Helper()
	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		require.NoError(rg.t, err)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	// Every call comes from its own address, so the public limiter never
	// paces a case.
	req.RemoteAddr = fmt.Sprintf("198.51.100.%d:4000", rand.Intn(250)+1)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (rg *providerRig) window(bearer string, header map[string]string) int {
	rg.t.Helper()
	rec := rg.call(rg.admin, http.MethodGet, "/api/admin/auth/provider-window", bearer, nil, header)
	require.Equal(rg.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Days int `json:"days"`
	}
	require.NoError(rg.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Days
}

func (rg *providerRig) setWindow(bearer string, body any, header map[string]string) *httptest.ResponseRecorder {
	rg.t.Helper()
	return rg.call(rg.admin, http.MethodPut, "/api/admin/auth/provider-window", bearer, body, header)
}

func (rg *providerRig) windowAudits() []compliance.AuditEntry {
	rg.audit.mu.Lock()
	defer rg.audit.mu.Unlock()
	var out []compliance.AuditEntry
	for _, e := range rg.audit.entries {
		if e.Action == "auth.provider_window.update" {
			out = append(out, e)
		}
	}
	return out
}

// The window is a per-tenant admin setting: 30 days until set, a whole number
// from 0 to 365, changed by the tenant's own admin or by a super admin naming
// the tenant, and every change leaves one audit row with both values.
func TestProviderWindow_Setting(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		rg := newProviderRig(t, pool)
		acmeAdmin := rg.session("acme", "admin")
		brandAdmin := rg.session("brand", "admin")
		acmeEditor := rg.session("acme", "editor")
		superAdmin := rg.session("default", "super_admin")

		// The engine settings row may hold other keys, and they survive.
		ctx := tenant.WithID(context.Background(), "acme")
		store := db.NewPluginConfigStore(pool)
		require.NoError(t, store.Set(ctx, config.CoreSettingsOwner, map[string]any{"cors_origins": "https://acme.example"}, nil))

		assert.Equal(t, 30, rg.window(acmeAdmin, nil), "no row reads 30")

		for _, days := range []int{0, 365} {
			rec := rg.setWindow(acmeAdmin, map[string]any{"days": days}, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, days, rg.window(acmeAdmin, nil))
		}
		for name, body := range map[string]any{
			"negative":       map[string]any{"days": -1},
			"past the limit": map[string]any{"days": 366},
			"a fraction":     map[string]any{"days": 1.5},
			"a string":       map[string]any{"days": "30"},
			"absent":         map[string]any{"weeks": 2},
		} {
			rec := rg.setWindow(acmeAdmin, body, nil)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "%s: %s", name, rec.Body.String())
		}
		assert.Equal(t, http.StatusBadRequest, rg.setWindow(acmeAdmin, "not json", nil).Code)
		assert.Equal(t, 365, rg.window(acmeAdmin, nil), "a refused change stores nothing")

		assert.Equal(t, http.StatusForbidden, rg.setWindow(acmeEditor, map[string]any{"days": 1}, nil).Code, "a member who is not an admin")
		assert.Equal(t, http.StatusForbidden, rg.call(rg.admin, http.MethodGet, "/api/admin/auth/provider-window", acmeEditor, nil, nil).Code)

		rec := rg.setWindow(acmeAdmin, map[string]any{"days": 7}, map[string]string{"X-Tenant-ID": "brand"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, 7, rg.window(acmeAdmin, nil), "a tenant admin changes their own tenant")
		assert.Equal(t, 30, rg.window(brandAdmin, nil), "and no other, whatever the header names")

		rec = rg.setWindow(superAdmin, map[string]any{"days": 12}, map[string]string{"X-Tenant-ID": "brand"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, 12, rg.window(brandAdmin, nil), "a super admin changes the tenant it names")
		assert.Equal(t, 7, rg.window(acmeAdmin, nil))

		raw, err := store.Get(ctx, config.CoreSettingsOwner)
		require.NoError(t, err)
		assert.Equal(t, "https://acme.example", raw["cors_origins"], "the tenant's other settings are kept")

		audits := rg.windowAudits()
		require.Len(t, audits, 4, "one row per change, none for a refusal")
		want := []struct {
			tenant        string
			before, after int
		}{{"acme", 30, 0}, {"acme", 0, 365}, {"acme", 365, 7}, {"brand", 30, 12}}
		for i, w := range want {
			assert.Equal(t, w.tenant, audits[i].TenantID)
			assert.Equal(t, "tenant", audits[i].ResourceType)
			assert.Equal(t, w.tenant, audits[i].ResourceID)
			assert.JSONEq(t, fmt.Sprintf(`{"tenant_id":%q,"days":%d}`, w.tenant, w.before), audits[i].BeforeJSON)
			assert.JSONEq(t, fmt.Sprintf(`{"tenant_id":%q,"days":%d}`, w.tenant, w.after), audits[i].AfterJSON)
			assert.NotNil(t, audits[i].UserID, "the actor is named")
		}
	})
}

// login posts a password login and returns the recorder.
func (rg *providerRig) login(email, tenantSlug string) *httptest.ResponseRecorder {
	rg.t.Helper()
	body := map[string]string{"email": email, "password": providerTestPassword}
	if tenantSlug != "" {
		body["tenant"] = tenantSlug
	}
	return rg.call(rg.admin, http.MethodPost, "/api/admin/auth/login", "", body, nil)
}

// token posts to the token endpoint and returns the recorder.
func (rg *providerRig) token(email, tenantSlug string) *httptest.ResponseRecorder {
	rg.t.Helper()
	body := map[string]string{"email": email, "password": providerTestPassword}
	if tenantSlug != "" {
		body["tenant"] = tenantSlug
	}
	return rg.call(rg.api, http.MethodPost, "/api/v1/auth/token", "", body, nil)
}

// seenAgo sets when the account's provider last signed it in.
func (rg *providerRig) seenAgo(id uuid.UUID, ago time.Duration) {
	rg.t.Helper()
	_, err := rg.pool.Exec(context.Background(), `UPDATE sys_users SET provider_seen_at = $1 WHERE id = $2`, time.Now().UTC().Add(-ago), id)
	require.NoError(rg.t, err)
}

// A provider account signs in by password only from a request naming its
// tenant, and only inside its tenant's window. A lowered window refuses the
// next sign-in, with no restart, and a raised one admits it again.
func TestProviderWindow_PasswordLoginFollowsTheWindow(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		rg := newProviderRig(t, pool)
		ctx := context.Background()
		admin := rg.session("acme", "admin")
		address := "sso-" + uuid.NewString()[:8] + "@agency.example"
		acct, _, err := rg.users.CreateTenantLocal(ctx, "acme", address, rg.hash, []string{"editor"}, true)
		require.NoError(t, err)
		rg.seenAgo(acct.ID, 10*24*time.Hour)

		rec := rg.login(address, "acme")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out struct {
			Token string         `json:"token"`
			User  map[string]any `json:"user"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		assert.Equal(t, address, out.User["email"], "the console sees the address, not the key")
		assert.Equal(t, true, out.User["tenant_local"])
		claims, err := auth.Parse(rg.cfg.JWTSecret, out.Token)
		require.NoError(t, err)
		assert.Equal(t, address, claims.Email, "the session carries the address")
		assert.Equal(t, "acme", claims.TenantID)

		assert.Equal(t, http.StatusUnauthorized, rg.login(address, "").Code, "a request naming no tenant reaches only the install-wide email")
		assert.Equal(t, http.StatusUnauthorized, rg.login(address, "brand").Code)
		assert.Equal(t, http.StatusOK, rg.token(address, "acme").Code)

		require.Equal(t, http.StatusOK, rg.setWindow(admin, map[string]any{"days": 5}, nil).Code)
		refused := rg.login(address, "acme")
		assert.Equal(t, http.StatusUnauthorized, refused.Code, "the next login after the window is lowered")
		assert.Contains(t, refused.Body.String(), "AUTH_INVALID_CREDENTIALS", "answered as a wrong password")
		assert.Equal(t, http.StatusUnauthorized, rg.token(address, "acme").Code)

		require.Equal(t, http.StatusOK, rg.setWindow(admin, map[string]any{"days": 11}, nil).Code)
		assert.Equal(t, http.StatusOK, rg.login(address, "acme").Code, "one day inside the window is served")

		require.Equal(t, http.StatusOK, rg.setWindow(admin, map[string]any{"days": 0}, nil).Code)
		assert.Equal(t, http.StatusUnauthorized, rg.login(address, "acme").Code, "0 admits no provider account")

		mailbox := "link-" + uuid.NewString()[:8] + "@agency.example"
		linked, _, err := rg.users.CreateTenantLocal(ctx, "acme", mailbox, rg.hash, nil, false)
		require.NoError(t, err)
		assert.Nil(t, linked.ProviderSeenAt)
		assert.Equal(t, http.StatusOK, rg.login(mailbox, "acme").Code, "the window covers only accounts a provider vouches for")

		require.Equal(t, http.StatusOK, rg.setWindow(admin, map[string]any{"days": 365}, nil).Code)
		disabled := true
		_, err = rg.users.SetAccountState(ctx, acct.ID, &disabled, nil)
		require.NoError(t, err)
		assert.Equal(t, http.StatusUnauthorized, rg.login(address, "acme").Code)
		assert.Equal(t, http.StatusUnauthorized, rg.token(address, "acme").Code, "the token endpoint refuses a disabled account as login does")
	})
}

// Users.Create refuses an address a tenant-local account holds in the same
// tenant, and creates it in any other.
func TestUsersCreate_RefusesAnAddressATenantLocalAccountHolds(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		rg := newProviderRig(t, pool)
		superAdmin := rg.session("default", "super_admin")
		address := "held-" + uuid.NewString()[:8] + "@agency.example"
		_, _, err := rg.users.CreateTenantLocal(context.Background(), "acme", address, "", nil, true)
		require.NoError(t, err)

		body := map[string]any{"email": address, "password": "A-long-enough-passphrase-42", "roles": []string{"editor"}}
		rec := rg.call(rg.admin, http.MethodPost, "/api/admin/users", superAdmin, body, map[string]string{"X-Tenant-ID": "acme"})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		rec = rg.call(rg.admin, http.MethodPost, "/api/admin/users", superAdmin, body, map[string]string{"X-Tenant-ID": "brand"})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
}

// The engine's password doors answer the same status, body and timing class
// for an address another tenant holds and a free one, when the request names
// a tenant.
func TestPasswordLogin_AddressHeldInAnotherTenantAnswersAsAFreeOne(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		hash, err := auth.HashPassword("bcrypt", providerTestPassword)
		require.NoError(t, err)
		h := NewAuthHandler(users, nil, pool, "test-secret-at-least-32-bytes-long!!", 3600, false)
		h.WithRiskBasedMFADisabled(true)

		heldByProvider := "held-sso-" + uuid.NewString()[:8] + "@agency.example"
		_, _, err = users.CreateTenantLocal(ctx, "elsewhere", heldByProvider, hash, nil, true)
		require.NoError(t, err)
		heldByPassword := "held-pw-" + uuid.NewString()[:8] + "@agency.example"
		_, err = users.Create(ctx, heldByPassword, hash, nil, "elsewhere")
		require.NoError(t, err)
		free := "free-" + uuid.NewString()[:8] + "@agency.example"

		doors := map[string]func(http.ResponseWriter, *http.Request){"login": h.Login, "token": h.Token}
		for doorName, door := range doors {
			send := func(email string) (int, string, time.Duration) {
				body := fmt.Sprintf(`{"email":%q,"password":%q,"tenant":"acme"}`, email, providerTestPassword)
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				start := time.Now()
				door(rec, req)
				return rec.Code, rec.Body.String(), time.Since(start)
			}
			const iterations = 10
			times := map[string][]time.Duration{}
			for i := 0; i < iterations; i++ {
				freeCode, freeBody, freeTook := send(free)
				require.Equal(t, http.StatusUnauthorized, freeCode, doorName)
				times["free"] = append(times["free"], freeTook)
				for name, email := range map[string]string{"provider": heldByProvider, "password": heldByPassword} {
					code, body, took := send(email)
					assert.Equal(t, freeCode, code, "%s, held by a %s account", doorName, name)
					assert.Equal(t, freeBody, body, "%s, held by a %s account", doorName, name)
					times[name] = append(times[name], took)
				}
			}
			freeMedian := median(times["free"])
			for _, name := range []string{"provider", "password"} {
				ratio := float64(median(times[name])) / float64(freeMedian)
				assert.True(t, ratio >= 0.5 && ratio <= 2.0, "%s, held by a %s account: timing ratio %.2f, free %v", doorName, name, ratio, freeMedian)
			}
		}
	})
}

func median(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}
