//go:build !mutest

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMemberships stands in for the reader a tenancy plugin supplies. The
// engine stores no memberships, so this is a non-database boundary and gets a
// hand-written fake rather than a table.
//
// Lookups fold case on the tenant, which is what MySQL and MSSQL do with the
// stored value. A fake that matched exactly would pass the spelling test on
// its own strictness rather than on the engine's guard.
type fakeMemberships struct {
	rows map[uuid.UUID]map[string]*core.Membership
}

func newFakeMemberships() *fakeMemberships {
	return &fakeMemberships{rows: map[uuid.UUID]map[string]*core.Membership{}}
}

func (f *fakeMemberships) Grant(_ context.Context, userID uuid.UUID, tenantID string, roles []string) (*core.Membership, error) {
	byTenant, ok := f.rows[userID]
	if !ok {
		byTenant = map[string]*core.Membership{}
		f.rows[userID] = byTenant
	}
	now := time.Now().UTC()
	m := &core.Membership{UserID: userID, TenantID: tenantID, Roles: roles, CreatedAt: now, UpdatedAt: now}
	byTenant[strings.ToLower(tenantID)] = m
	return m, nil
}

func (f *fakeMemberships) Revoke(_ context.Context, userID uuid.UUID, tenantID string) error {
	byTenant, ok := f.rows[userID]
	if !ok {
		return core.ErrNotFound
	}
	key := strings.ToLower(tenantID)
	if _, ok := byTenant[key]; !ok {
		return core.ErrNotFound
	}
	delete(byTenant, key)
	return nil
}

func (f *fakeMemberships) Get(_ context.Context, userID uuid.UUID, tenantID string) (*core.Membership, error) {
	m, ok := f.rows[userID][strings.ToLower(tenantID)]
	if !ok {
		return nil, core.ErrNotFound
	}
	return m, nil
}

func (f *fakeMemberships) ListForUser(_ context.Context, userID uuid.UUID) ([]*core.Membership, error) {
	byTenant := f.rows[userID]
	keys := make([]string, 0, len(byTenant))
	for k := range byTenant {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*core.Membership, 0, len(keys))
	for _, k := range keys {
		out = append(out, byTenant[k])
	}
	return out, nil
}

// membershipLoginFixture seeds one account with a home tenant and returns the
// pieces every test below needs.
type membershipLoginFixture struct {
	pool     db.DB
	users    *db.UserStore
	members  *fakeMemberships
	handler  *AuthHandler
	secret   string
	email    string
	password string
	userID   uuid.UUID
}

func newMembershipLoginFixture(t *testing.T, homeTenant string, roles []string) *membershipLoginFixture {
	t.Helper()
	return newMembershipLoginFixtureOn(t, testdb.Postgres(t), homeTenant, roles)
}

func newMembershipLoginFixtureOn(t *testing.T, pool db.DB, homeTenant string, roles []string) *membershipLoginFixture {
	t.Helper()

	secret := "test-secret-at-least-32-bytes-long!!"
	password := "correct-horse-battery-staple"

	hash, err := auth.HashPassword("bcrypt", password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	users := db.NewUserStore(pool)
	email := fmt.Sprintf("member-%s@example.com", uuid.NewString()[:8])
	user, err := users.Create(context.Background(), email, hash, roles, homeTenant)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	members := newFakeMemberships()
	h := NewAuthHandler(users, nil, pool, secret, 3600, false)
	h.WithMemberships(members)
	h.WithRiskBasedMFADisabled(true)

	return &membershipLoginFixture{
		pool: pool, users: users, members: members, handler: h,
		secret: secret, email: email, password: password, userID: user.ID,
	}
}

// login posts a login request naming tenant (empty for the home tenant) and
// returns the recorder.
func (f *membershipLoginFixture) login(t *testing.T, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]string{"email": f.email, "password": f.password}
	if tenant != "" {
		payload["tenant"] = tenant
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.handler.Login(rr, req)
	return rr
}

// membershipEntry mirrors one row of GET /api/admin/auth/memberships.
type membershipEntry struct {
	TenantID string   `json:"tenant_id"`
	Roles    []string `json:"roles"`
	Home     bool     `json:"home"`
}

// memberships calls the caller's own membership listing with the session the
// recorder holds.
func (f *membershipLoginFixture) memberships(t *testing.T, rr *httptest.ResponseRecorder) []membershipEntry {
	t.Helper()
	claims := f.claimsFrom(t, rr)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/memberships", nil)
	req = req.WithContext(context.WithValue(req.Context(), core.ClaimsKey, &core.AuthClaims{
		UserID: claims.UserID, TenantID: claims.TenantID, Roles: claims.Roles,
	}))
	rec := httptest.NewRecorder()
	f.handler.Memberships(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("memberships: status=%d body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Memberships []membershipEntry `json:"memberships"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode memberships: %v; body=%q", err, rec.Body.String())
	}
	return body.Memberships
}

func (f *membershipLoginFixture) claimsFrom(t *testing.T, rr *httptest.ResponseRecorder) *auth.Claims {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login response: %v; body=%q", err, rr.Body.String())
	}
	tok, _ := resp["token"].(string)
	if tok == "" {
		t.Fatalf("login returned no token; body=%q", rr.Body.String())
	}
	claims, err := auth.Parse(f.secret, tok)
	if err != nil {
		t.Fatalf("parse issued token: %v", err)
	}
	return claims
}

// TestLoginMembership_HomeTenantNeedsNoGrant is the compatibility guard.
// Identity-provider plugins create the user row with no membership, so the
// home tenant must need no grant, or every account they create would be
// locked out.
func TestLoginMembership_HomeTenantNeedsNoGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "brand_a", []string{"editor"})

	rr := f.login(t, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("home login rejected: status=%d body=%q", rr.Code, rr.Body.String())
	}
	claims := f.claimsFrom(t, rr)
	if claims.TenantID != "brand_a" {
		t.Errorf("tenant claim = %q, want brand_a", claims.TenantID)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "editor" {
		t.Errorf("roles = %v, want [editor]", claims.Roles)
	}
}

// TestLoginMembership_GrantedTenantCarriesItsOwnRoles proves the point of the
// feature: the same credentials produce a session in a second tenant, with the
// roles held there rather than the roles held at home.
func TestLoginMembership_GrantedTenantCarriesItsOwnRoles(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "brand_a", []string{"editor"})
	if _, err := f.members.Grant(context.Background(), f.userID, "brand_b", []string{"admin"}); err != nil {
		t.Fatalf("grant brand_b: %v", err)
	}

	claims := f.claimsFrom(t, f.login(t, "brand_b"))
	if claims.TenantID != "brand_b" {
		t.Errorf("tenant claim = %q, want brand_b", claims.TenantID)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "admin" {
		t.Errorf("roles = %v, want [admin] - the roles held in brand_b", claims.Roles)
	}

	// The home session is unchanged by the grant.
	homeClaims := f.claimsFrom(t, f.login(t, ""))
	if homeClaims.TenantID != "brand_a" {
		t.Errorf("home tenant claim = %q, want brand_a", homeClaims.TenantID)
	}
	if len(homeClaims.Roles) != 1 || homeClaims.Roles[0] != "editor" {
		t.Errorf("home roles = %v, want [editor]", homeClaims.Roles)
	}
}

// TestLoginMembership_UngrantedTenantIsRefusedAsBadCredentials proves the
// refusal is indistinguishable from a wrong password. Answering differently
// would turn login into a probe for which tenants an address belongs to, which
// is the enumeration property the rest of the handler protects.
func TestLoginMembership_UngrantedTenantIsRefusedAsBadCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "brand_a", []string{"editor"})

	denied := f.login(t, "brand_b")
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("ungranted tenant status = %d, want 401; body=%q", denied.Code, denied.Body.String())
	}

	// Compare against a genuine wrong password on the same account.
	wrong := map[string]string{"email": f.email, "password": "not-the-password"}
	raw, err := json.Marshal(wrong)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.handler.Login(rr, req)

	if rr.Code != denied.Code {
		t.Errorf("status differs from a wrong password: %d vs %d", denied.Code, rr.Code)
	}
	if rr.Body.String() != denied.Body.String() {
		t.Errorf("body differs from a wrong password:\n  ungranted: %s\n  wrong pw:  %s",
			denied.Body.String(), rr.Body.String())
	}
}

// TestLoginMembership_RevokeEndsAccessToThatTenant proves the grant is the
// live authority, not a one-off decision recorded at grant time.
func TestLoginMembership_RevokeEndsAccessToThatTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "brand_a", []string{"editor"})
	ctx := context.Background()
	if _, err := f.members.Grant(ctx, f.userID, "brand_b", []string{"editor"}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if rr := f.login(t, "brand_b"); rr.Code != http.StatusOK {
		t.Fatalf("granted login rejected: %d", rr.Code)
	}
	if err := f.members.Revoke(ctx, f.userID, "brand_b"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rr := f.login(t, "brand_b"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("revoked tenant still logs in: status=%d body=%q", rr.Code, rr.Body.String())
	}
	if rr := f.login(t, ""); rr.Code != http.StatusOK {
		t.Fatalf("revoking a grant broke the home tenant: status=%d", rr.Code)
	}
}

// TestLoginMembership_GrantOverridesHomeRoles proves a membership row naming
// the home tenant is honored, so an administrator can change someone's roles
// in their own tenant through the same surface as every other tenant.
func TestLoginMembership_GrantOverridesHomeRoles(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "brand_a", []string{"editor"})
	if _, err := f.members.Grant(context.Background(), f.userID, "brand_a", []string{"admin"}); err != nil {
		t.Fatalf("grant home: %v", err)
	}
	claims := f.claimsFrom(t, f.login(t, ""))
	if len(claims.Roles) != 1 || claims.Roles[0] != "admin" {
		t.Errorf("home roles = %v, want [admin] from the membership row", claims.Roles)
	}
}

// TestLoginMembership_SuperAdminCrossesWithoutAGrant records a deliberate
// exception. super_admin already crosses tenants by X-Tenant-ID through the
// tenancy middleware, so requiring a grant at login would close a door that is
// open one layer down, and only for this path.
func TestLoginMembership_SuperAdminCrossesWithoutAGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "", []string{"super_admin"})

	claims := f.claimsFrom(t, f.login(t, "brand_b"))
	if claims.TenantID != "brand_b" {
		t.Errorf("tenant claim = %q, want brand_b", claims.TenantID)
	}
}

// TestLoginMembership_TokenEndpointResolvesTheSameWay guards against the two
// credential endpoints drifting apart. A tenant honored by one and ignored by
// the other is a bypass on whichever one is laxer.
func TestLoginMembership_TokenEndpointResolvesTheSameWay(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "brand_a", []string{"editor"})
	if _, err := f.members.Grant(context.Background(), f.userID, "brand_b", []string{"admin"}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	post := func(tenant string) *httptest.ResponseRecorder {
		payload := map[string]string{"email": f.email, "password": f.password}
		if tenant != "" {
			payload["tenant"] = tenant
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/token", strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		f.handler.Token(rr, req)
		return rr
	}

	rr := post("brand_b")
	if rr.Code != http.StatusOK {
		t.Fatalf("token endpoint rejected a granted tenant: %d %s", rr.Code, rr.Body.String())
	}
	claims := f.claimsFrom(t, rr)
	if claims.TenantID != "brand_b" || len(claims.Roles) != 1 || claims.Roles[0] != "admin" {
		t.Errorf("token claims = tenant %q roles %v, want brand_b [admin]", claims.TenantID, claims.Roles)
	}

	if rr := post("brand_c"); rr.Code != http.StatusUnauthorized {
		t.Errorf("token endpoint allowed an ungranted tenant: %d", rr.Code)
	}
}

// TestLoginMembership_RefreshKeepsTheActingTenant proves a rotated session
// stays in the tenant it was logged into.
//
// The refresh path has no access token to read, so the family record carries
// the tenant. Signing the user row's tenant would move anyone who switched
// tenants back home at the first rotation, with the home roles attached.
func TestLoginMembership_RefreshKeepsTheActingTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "brand_a", []string{"editor"})
	f.handler.WithRefreshTokenStore(auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test"), time.Hour)

	ctx := context.Background()
	if _, err := f.members.Grant(ctx, f.userID, "brand_b", []string{"admin"}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	rr := f.login(t, "brand_b")
	if rr.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rr.Code, rr.Body.String())
	}
	var loginResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	refreshToken, _ := loginResp["refresh_token"].(string)
	if refreshToken == "" {
		t.Fatalf("login issued no refresh token; body=%q", rr.Body.String())
	}

	refresh := func(token string) *httptest.ResponseRecorder {
		raw, err := json.Marshal(map[string]string{"refresh_token": token})
		if err != nil {
			t.Fatalf("marshal refresh: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/refresh", strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.handler.Refresh(rec, req)
		return rec
	}

	rec := refresh(refreshToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	claims := f.claimsFrom(t, rec)
	if claims.TenantID != "brand_b" {
		t.Errorf("refresh moved the session to %q, want brand_b", claims.TenantID)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "admin" {
		t.Errorf("refresh roles = %v, want [admin]", claims.Roles)
	}

	// Revoking the grant ends the session at the next rotation rather than at
	// token expiry, because refresh re-resolves instead of replaying.
	var refreshed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &refreshed); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	next, _ := refreshed["refresh_token"].(string)
	if next == "" {
		t.Fatalf("rotation returned no refresh token")
	}
	if err := f.members.Revoke(ctx, f.userID, "brand_b"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rec := refresh(next); rec.Code != http.StatusUnauthorized {
		t.Errorf("refresh after revoke = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
}

// MySQL and MSSQL match the membership row case insensitively. A login naming
// another spelling of a granted tenant must not be signed into a token under
// that spelling, which would make one tenant addressable under many names.
func TestLoginMembership_OtherSpellingOfATenantIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	dialects := map[string]func(*testing.T) db.DB{
		"postgres": testdb.Postgres,
		"mysql":    testdb.MySQL,
		"mssql":    testdb.MSSQL,
	}
	for name, open := range dialects {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("%s not selected", name)
			}
			f := newMembershipLoginFixtureOn(t, open(t), "brand_a", []string{"editor"})
			if _, err := f.members.Grant(context.Background(), f.userID, "brand_b", []string{"admin"}); err != nil {
				t.Fatalf("grant brand_b: %v", err)
			}

			for _, spelling := range []string{"BRAND_B", "Brand_b"} {
				rr := f.login(t, spelling)
				if rr.Code != http.StatusUnauthorized {
					t.Fatalf("login as %q: status=%d body=%q, want 401", spelling, rr.Code, rr.Body.String())
				}
			}
			if claims := f.claimsFrom(t, f.login(t, "brand_b")); claims.TenantID != "brand_b" {
				t.Errorf("tenant claim = %q, want brand_b", claims.TenantID)
			}
		})
	}
}

// A build with no membership reader is the single-tenant install, and it has
// to be safe in both directions. Nothing may be granted that the account row
// does not already grant, and nobody may lose the tenant they belong to.
//
// So the home tenant signs in with the roles on its own row, and a sign-in
// naming any other tenant is refused with exactly what a wrong password is
// refused with. Answering differently would turn sign-in into a probe for
// which tenants an address belongs to.
func TestLoginMembership_WithNoReaderHomeWorksAndNothingElseDoes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "brand_a", []string{"editor"})
	f.handler.WithMemberships(nil)

	rr := f.login(t, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	claims := f.claimsFrom(t, rr)
	assert.Equal(t, "brand_a", claims.TenantID)
	assert.Equal(t, []string{"editor"}, claims.Roles, "the roles on the account row, nothing added")

	rr = f.login(t, "brand_a")
	assert.Equal(t, http.StatusOK, rr.Code, "naming the home tenant is still the home tenant")

	for _, tenant := range []string{"brand_b", "brand_c"} {
		rr := f.login(t, tenant)
		assert.Equal(t, http.StatusUnauthorized, rr.Code, "%s was admitted with nothing granting it", tenant)
	}

	// The caller's own listing answers the home tenant rather than failing.
	body := f.memberships(t, f.login(t, ""))
	require.Len(t, body, 1)
	assert.Equal(t, "brand_a", body[0].TenantID)
	assert.True(t, body[0].Home)
}

// A super admin still crosses tenants with no reader wired. It crosses by
// header one layer down, so refusing here would close a door that is open
// anyway and only on the sign-in path.
func TestLoginMembership_SuperAdminStillCrossesWithNoReader(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "brand_a", []string{"super_admin"})
	f.handler.WithMemberships(nil)

	rr := f.login(t, "brand_b")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "brand_b", f.claimsFrom(t, rr).TenantID)
}
