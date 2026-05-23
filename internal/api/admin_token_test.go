package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
)

// fakeAdminTokens is the store a tenancy plugin supplies, kept in memory. The
// engine holds the policy and this holds the rows, so these tests read the
// policy: which token is honored, on which route, from which address, and
// what is written to the request log.
//
// The owner's email and erasure come from sys_users, which the engine creates,
// so they are read back from the pool the way the real store's join does.
type fakeAdminTokens struct {
	mu   sync.Mutex
	pool db.DB
	rows map[uuid.UUID]*core.AdminToken
	reqs []core.AdminTokenRequest
}

func newFakeAdminTokens(pool db.DB) *fakeAdminTokens {
	return &fakeAdminTokens{pool: pool, rows: map[uuid.UUID]*core.AdminToken{}}
}

// hydrate fills the owner columns and returns a copy, so a caller mutating
// what it got back cannot reach into the store.
func (f *fakeAdminTokens) hydrate(ctx context.Context, t *core.AdminToken) *core.AdminToken {
	out := *t
	out.Grants = append([]string(nil), t.Grants...)
	out.AllowedIPs = append([]string(nil), t.AllowedIPs...)
	row, err := f.pool.QueryRow(ctx, `SELECT COALESCE(email, ''), anonymized FROM sys_users WHERE id = $1`, t.OwnerUserID)
	if err == nil {
		var email string
		var erased bool
		if err := row.Scan(&email, &erased); err == nil {
			out.OwnerEmail = email
			out.OwnerErased = erased
		}
	}
	return &out
}

func (f *fakeAdminTokens) Create(_ context.Context, t *core.AdminToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := *t
	f.rows[t.ID] = &stored
	return nil
}

func (f *fakeAdminTokens) GetByHash(ctx context.Context, hash string) (*core.AdminToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.rows {
		if t.TokenHash == hash {
			return f.hydrate(ctx, t), nil
		}
	}
	return nil, core.ErrNotFound
}

func (f *fakeAdminTokens) Get(ctx context.Context, tenantID string, id uuid.UUID) (*core.AdminToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok || t.TenantID != tenantID {
		return nil, core.ErrNotFound
	}
	return f.hydrate(ctx, t), nil
}

func (f *fakeAdminTokens) List(ctx context.Context, tenantID string, owner *uuid.UUID, limit, offset int) ([]*core.AdminToken, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []*core.AdminToken
	for _, t := range f.rows {
		if t.TenantID != tenantID {
			continue
		}
		if owner != nil && t.OwnerUserID != *owner {
			continue
		}
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID.String() > all[j].ID.String()
	})
	total := len(all)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	page := make([]*core.AdminToken, 0, end-offset)
	for _, t := range all[offset:end] {
		page = append(page, f.hydrate(ctx, t))
	}
	// The newest successor wins, matching the store's ordered read.
	for _, t := range page {
		var newest *core.AdminToken
		for _, cand := range f.rows {
			if cand.TenantID != tenantID || cand.RotatedFrom == nil || *cand.RotatedFrom != t.ID {
				continue
			}
			if newest == nil || cand.CreatedAt.After(newest.CreatedAt) {
				newest = cand
			}
		}
		if newest != nil {
			id := newest.ID
			t.ReplacedBy = &id
		}
	}
	return page, total, nil
}

func (f *fakeAdminTokens) Revoke(_ context.Context, tenantID string, id uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok || t.TenantID != tenantID || t.RevokedAt != nil {
		return core.ErrNotFound
	}
	when := at.UTC()
	t.RevokedAt = &when
	return nil
}

func (f *fakeAdminTokens) Rotate(_ context.Context, tenantID string, oldID uuid.UUID, successor *core.AdminToken, overlapUntil time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	old, ok := f.rows[oldID]
	if !ok || old.TenantID != tenantID || old.RevokedAt != nil {
		return core.ErrNotFound
	}
	if overlapUntil.Before(old.ExpiresAt) {
		old.ExpiresAt = overlapUntil.UTC()
	}
	stored := *successor
	f.rows[successor.ID] = &stored
	return nil
}

func (f *fakeAdminTokens) TouchLastUsed(_ context.Context, tenantID string, id uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok || t.TenantID != tenantID {
		return nil
	}
	at = at.UTC()
	if t.LastUsedAt == nil || t.LastUsedAt.Before(at.Add(-time.Minute)) {
		t.LastUsedAt = &at
	}
	return nil
}

func (f *fakeAdminTokens) LogRequests(_ context.Context, rows []core.AdminTokenRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rows {
		if r.ID == uuid.Nil {
			r.ID = uuid.New()
		}
		if r.CreatedAt.IsZero() {
			r.CreatedAt = time.Now()
		}
		r.CreatedAt = r.CreatedAt.UTC()
		f.reqs = append(f.reqs, r)
	}
	return nil
}

func (f *fakeAdminTokens) ListRequests(_ context.Context, tenantID string, tokenID uuid.UUID, limit, offset int) ([]*core.AdminTokenRequest, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []core.AdminTokenRequest
	for _, r := range f.reqs {
		if r.TokenID == tokenID && r.TenantID == tenantID {
			all = append(all, r)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID.String() > all[j].ID.String()
	})
	total := len(all)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	out := make([]*core.AdminTokenRequest, 0, end-offset)
	for i := offset; i < end; i++ {
		r := all[i]
		out = append(out, &r)
	}
	return out, total, nil
}

func (f *fakeAdminTokens) PruneRequests(_ context.Context, before time.Time, limit int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sort.Slice(f.reqs, func(i, j int) bool { return f.reqs[i].CreatedAt.Before(f.reqs[j].CreatedAt) })
	kept := make([]core.AdminTokenRequest, 0, len(f.reqs))
	var n int64
	for _, r := range f.reqs {
		if r.CreatedAt.Before(before) && n < int64(limit) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	f.reqs = kept
	return n, nil
}

// expire brings a token's expiry into the past, which is what a clock passing
// does and what no route offers.
func (f *fakeAdminTokens) expire(id uuid.UUID, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.rows[id]; ok {
		t.ExpiresAt = at.UTC()
	}
}

const tokenTestPassword = "correct-horse-battery-staple"

// tokenRig is an admin router and a content API router over one real
// database, with a plugin that declares one granted route and one public one.
type tokenRig struct {
	t      *testing.T
	pool   db.DB
	cfg    *config.Config
	admin  http.Handler
	api    http.Handler
	users  *db.UserStore
	tokens *fakeAdminTokens
}

func newTokenRig(t *testing.T, opts ...RouterOption) *tokenRig {
	t.Helper()
	return newTokenRigWith(t, false, nil, opts...)
}

// newConfiguredTokenRig is newTokenRig with the configuration changed by
// configure before the routers are built.
func newConfiguredTokenRig(t *testing.T, configure func(*config.Config), opts ...RouterOption) *tokenRig {
	t.Helper()
	return newTokenRigWith(t, false, configure, opts...)
}

// newStorelessTokenRig builds the routers a deployment with no tenancy plugin
// gets: nothing supplies somewhere to keep admin tokens, so the engine has
// none.
func newStorelessTokenRig(t *testing.T, opts ...RouterOption) *tokenRig {
	t.Helper()
	return newTokenRigWith(t, true, nil, opts...)
}

func newTokenRigWith(t *testing.T, storeless bool, configure func(*config.Config), opts ...RouterOption) *tokenRig {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	pool := testdb.Postgres(t)
	cfg := testConfig()
	if configure != nil {
		configure(cfg)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := core.GetClaims(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]any{"roles": c.Roles, "tenant_id": core.TenantIDFromCtx(r.Context())})
	})
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("handler failed") })
	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{
			// The schema routes are a plugin's, so the rig registers them
			// here and an admin token reaching a schema read exercises a
			// plugin route.
			{Method: http.MethodGet, Pattern: "/api/admin/schemas", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantSchemasRead},
			{Method: http.MethodGet, Pattern: "/api/admin/schemas/{name}", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantSchemasRead},
			{Method: http.MethodGet, Pattern: "/api/admin/schemas/{name}/stats", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantSchemasRead},
			// No grant: a write to the catalog is session only, which is what
			// the refusal cases here are about.
			{Method: http.MethodPost, Pattern: "/api/admin/schemas", Group: plugin.GroupAdmin, Handler: ok},
			{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/items", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantFlowsRead},
			{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/open", Group: plugin.GroupPublic, Handler: ok},
			{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/whoami", Group: plugin.GroupAdmin, Handler: whoami, AdminGrant: core.AdminGrantFlowsRead},
			{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/boom", Group: plugin.GroupAdmin, Handler: boom, AdminGrant: core.AdminGrantFlowsRead},
		},
	}}
	tokens := newFakeAdminTokens(pool)
	base := []RouterOption{WithLifetime(testLifetime(t)), WithPluginRoutes(routes)}
	if !storeless {
		base = append(base, WithAdminTokenStore(tokens))
	}
	all := append(base, opts...)
	admin, err := NewAdminRouter(pool, cfg, all...)
	require.NoError(t, err)
	apiRouter, err := NewAPIRouter(pool, cfg, nil, WithLifetime(testLifetime(t)))
	require.NoError(t, err)
	return &tokenRig{t: t, pool: pool, cfg: cfg, admin: admin, api: apiRouter, users: db.NewUserStore(pool), tokens: tokens}
}

// user creates an account and returns it with a session token for it.
func (rg *tokenRig) user(roles ...string) (*domain.User, string) {
	rg.t.Helper()
	return rg.userIn("default", roles...)
}

// userIn creates an account homed in tenant, with a session in it.
func (rg *tokenRig) userIn(tenant string, roles ...string) (*domain.User, string) {
	rg.t.Helper()
	hash, err := auth.HashPassword("bcrypt", tokenTestPassword)
	require.NoError(rg.t, err)
	u, err := rg.users.Create(context.Background(), "owner-"+uuid.NewString()[:8]+"@example.com", hash, roles, tenant)
	require.NoError(rg.t, err)
	session, err := auth.Sign(rg.cfg.JWTSecret, 3600, u.ID, u.Email, u.Roles, tenant, u.TokenVersion)
	require.NoError(rg.t, err)
	return u, session
}

type tokenCall struct {
	method, path, bearer string
	body                 any
	header               map[string]string
	remote               string
	router               http.Handler
}

func (rg *tokenRig) do(c tokenCall) *httptest.ResponseRecorder {
	rg.t.Helper()
	var body *bytes.Reader
	if c.body != nil {
		b, err := json.Marshal(c.body)
		require.NoError(rg.t, err)
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(c.method, c.path, body)
	req.Header.Set("Content-Type", "application/json")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	if c.remote != "" {
		req.RemoteAddr = c.remote
	}
	rec := httptest.NewRecorder()
	h := c.router
	if h == nil {
		h = rg.admin
	}
	h.ServeHTTP(rec, req)
	return rec
}

// issue creates a token through the API and returns it and its id.
func (rg *tokenRig) issue(session string, grants []string, extra map[string]any) (string, string) {
	rg.t.Helper()
	body := map[string]any{
		"name":       "ci",
		"grants":     grants,
		"expires_at": time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"password":   tokenTestPassword,
	}
	for k, v := range extra {
		body[k] = v
	}
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session, body: body})
	require.Equal(rg.t, http.StatusCreated, rec.Code, rec.Body.String())
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	require.NoError(rg.t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.True(rg.t, strings.HasPrefix(out.Token, core.AdminTokenPrefix), "token %q", out.Token)
	return out.Token, out.ID
}

func (rg *tokenRig) get(path, bearer string) int {
	rg.t.Helper()
	return rg.do(tokenCall{method: http.MethodGet, path: path, bearer: bearer}).Code
}

// requestRows waits for the asynchronous request log to hold n rows for the
// token, and returns them oldest first.
func (rg *tokenRig) requestRows(id string, n int) []*core.AdminTokenRequest {
	rg.t.Helper()
	tokenID := uuid.MustParse(id)
	var rows []*core.AdminTokenRequest
	require.Eventually(rg.t, func() bool {
		got, total, err := rg.tokens.ListRequests(context.Background(), "default", tokenID, 100, 0)
		if err != nil || total < n {
			return false
		}
		rows = got
		return true
	}, 5*time.Second, 20*time.Millisecond, "request log never reached %d rows", n)
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

func TestAdminToken_GrantsDecideTheRoute(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	schemas, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)
	flows, _ := rg.issue(session, []string{core.AdminGrantFlowsRead}, nil)

	assert.Equal(t, http.StatusOK, rg.get("/api/admin/schemas", schemas))
	assert.Equal(t, http.StatusOK, rg.get("/api/admin/test-plugin/items", flows))

	rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/test-plugin/items", bearer: schemas})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "This token is not granted flows:read.")

	// A route that declares no grant is session only, whatever the owner's role.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/admin/users"},
		{http.MethodPost, "/api/admin/schemas"},
		{http.MethodGet, "/api/admin/entitlements"},
	} {
		rec := rg.do(tokenCall{method: c.method, path: c.path, bearer: schemas, body: map[string]any{}})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s", c.method, c.path)
		assert.Contains(t, rec.Body.String(), "This route needs a signed-in session.", "%s %s", c.method, c.path)
	}

	// An unknown token is refused, never read as an anonymous caller.
	assert.Equal(t, http.StatusUnauthorized, rg.get("/api/admin/schemas", core.AdminTokenPrefix+"not-a-token"))
}

func TestAdminToken_RefusedOffTheAdminAPI(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	tok, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)

	for _, c := range []tokenCall{
		{method: http.MethodGet, path: "/api/v1/schemas", bearer: tok, router: rg.api},
		{method: http.MethodGet, path: "/healthz", bearer: tok, router: rg.api},
		{method: http.MethodPost, path: "/api/admin/auth/login", bearer: tok, body: map[string]any{}},
		{method: http.MethodGet, path: "/api/admin/test-plugin/open", bearer: tok},
	} {
		rec := rg.do(c)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s %s", c.method, c.path)
		assert.Contains(t, rec.Body.String(), msgAdminTokenOffAdminAPI, "%s %s", c.method, c.path)
	}
}

func TestAdminToken_StatelessRouterRefusesATokenBearer(t *testing.T) {
	t.Parallel()
	r := NewStatelessRouter(testConfig(), NewStatelessMode(nil), WithLifetime(testLifetime(t)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/schemas", nil)
	req.Header.Set("Authorization", "Bearer "+core.AdminTokenPrefix+"anything")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), msgAdminTokenOffAdminAPI)
}

func TestAdminToken_ExpiredAndRevoked(t *testing.T) {
	rg := newTokenRig(t)
	owner, session := rg.user("admin")

	raw := core.AdminTokenPrefix + "expired-" + uuid.NewString()
	past := time.Now().Add(-time.Hour)
	require.NoError(t, rg.tokens.Create(context.Background(), &core.AdminToken{
		ID: uuid.New(), TenantID: "default", OwnerUserID: owner.ID,
		Name: "old", TokenHash: security.HashKeyPeppered(raw), DisplayPrefix: "expired-",
		Grants: []string{core.AdminGrantSchemasRead}, ExpiresAt: past, CreatedAt: past.Add(-time.Hour),
	}))
	rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/schemas", bearer: raw})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), msgAdminTokenExpired)

	tok, id := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)
	require.Equal(t, http.StatusOK, rg.get("/api/admin/schemas", tok))
	del := rg.do(tokenCall{method: http.MethodDelete, path: "/api/admin/admin-tokens/" + id, bearer: session})
	require.Equal(t, http.StatusNoContent, del.Code, del.Body.String())
	rec = rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/schemas", bearer: tok})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), msgAdminTokenRevoked)
}

func TestAdminToken_RotationOverlap(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	old, oldID := rg.issue(session, []string{core.AdminGrantSchemasRead}, map[string]any{
		"expires_at":  time.Now().Add(80 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"allowed_ips": []string{"192.0.2.0/24"},
	})

	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens/" + oldID + "/rotate", bearer: session, body: map[string]any{
		"expires_at": time.Now().Add(60 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"password":   tokenTestPassword,
	}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var next struct {
		Token       string   `json:"token"`
		RotatedFrom string   `json:"rotated_from"`
		Grants      []string `json:"grants"`
		AllowedIPs  []string `json:"allowed_ips"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &next))
	assert.Equal(t, oldID, next.RotatedFrom)
	assert.Equal(t, []string{core.AdminGrantSchemasRead}, next.Grants)
	assert.Equal(t, []string{"192.0.2.0/24"}, next.AllowedIPs)

	// Inside the overlap both work.
	assert.Equal(t, http.StatusOK, rg.get("/api/admin/schemas", next.Token))
	assert.Equal(t, http.StatusOK, rg.get("/api/admin/schemas", old))
	stored, err := rg.tokens.Get(context.Background(), "default", uuid.MustParse(oldID))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(7*24*time.Hour), stored.ExpiresAt, time.Minute)

	// Past the overlap the old token is expired and the successor is not.
	rg.tokens.expire(uuid.MustParse(oldID), time.Now().Add(-time.Second))
	assert.Equal(t, http.StatusUnauthorized, rg.get("/api/admin/schemas", old))
	assert.Equal(t, http.StatusOK, rg.get("/api/admin/schemas", next.Token))
}

func TestAdminToken_FollowsItsOwner(t *testing.T) {
	rg := newTokenRig(t)
	ctx := context.Background()

	t.Run("demoted", func(t *testing.T) {
		owner, session := rg.user("admin")
		tok, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)
		require.Equal(t, http.StatusOK, rg.get("/api/admin/schemas", tok))
		_, err := rg.users.UpdateRoles(ctx, owner.ID, []string{"editor"})
		require.NoError(t, err)
		// Below admin the token reaches nothing, not even a route any role reads.
		rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/schemas", bearer: tok})
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), msgAdminTokenOwner)
	})
	t.Run("deleted", func(t *testing.T) {
		owner, session := rg.user("admin")
		tok, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)
		require.NoError(t, rg.users.Delete(ctx, owner.ID))
		assert.Equal(t, http.StatusUnauthorized, rg.get("/api/admin/schemas", tok))
	})
	t.Run("owner logs out", func(t *testing.T) {
		_, session := rg.user("admin")
		tok, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)
		out := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/logout", bearer: session, body: map[string]any{}})
		require.Equal(t, http.StatusOK, out.Code, out.Body.String())
		// The session is over and the token is not.
		require.Equal(t, http.StatusUnauthorized, rg.get("/api/admin/auth/me", session))
		assert.Equal(t, http.StatusOK, rg.get("/api/admin/schemas", tok))
	})
	t.Run("owner's password set", func(t *testing.T) {
		owner, session := rg.user("admin")
		tok, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)
		_, err := rg.users.SetPassword(ctx, owner.ID, "$2a$10$anotherplaceholderhashXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, rg.get("/api/admin/schemas", tok))
	})
	t.Run("erased", func(t *testing.T) {
		owner, session := rg.user("admin")
		tok, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)
		// What an erasure leaves on the row, roles kept so only the mark decides.
		_, err := rg.pool.Exec(ctx, `UPDATE sys_users SET anonymized = $1 WHERE id = $2`, true, owner.ID)
		require.NoError(t, err)
		assert.Equal(t, http.StatusUnauthorized, rg.get("/api/admin/schemas", tok))
	})
	t.Run("disabled", func(t *testing.T) {
		owner, session := rg.user("admin")
		tok, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)
		disabled := true
		_, err := rg.users.SetAccountState(ctx, owner.ID, &disabled, nil)
		require.NoError(t, err)
		rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/schemas", bearer: tok})
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), msgAdminTokenOwner)
	})
	t.Run("account expired", func(t *testing.T) {
		owner, session := rg.user("admin")
		tok, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)
		past := time.Now().Add(-time.Minute)
		pp := &past
		_, err := rg.users.SetAccountState(ctx, owner.ID, nil, &pp)
		require.NoError(t, err)
		assert.Equal(t, http.StatusUnauthorized, rg.get("/api/admin/schemas", tok))
	})
}

func TestAdminToken_TenantAndAddress(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("super_admin")
	tok, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)

	rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/schemas", bearer: tok, header: map[string]string{"X-Tenant-ID": "globex"}})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), msgAdminTokenTenant)
	// Naming its own tenant asks for nothing.
	rec = rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/schemas", bearer: tok, header: map[string]string{"X-Tenant-ID": "default"}})
	assert.Equal(t, http.StatusOK, rec.Code)

	held, _ := rg.issue(session, []string{core.AdminGrantSchemasRead}, map[string]any{"allowed_ips": []string{"10.1.2.3", "198.51.100.0/24"}})
	rec = rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/schemas", bearer: held, remote: "192.0.2.10:4000"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), msgAdminTokenAddress)
	rec = rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/schemas", bearer: held, remote: "198.51.100.7:4000"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAdminToken_CannotManageTokens(t *testing.T) {
	keyClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") == "" {
				next.ServeHTTP(w, r)
				return
			}
			claims := &core.AuthClaims{UserID: uuid.NewString(), Roles: []string{"super_admin"}, TenantID: "default", IsAPIKey: true}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}
	rg := newTokenRig(t, WithAPIKeyAuth(keyClaims))
	_, session := rg.user("super_admin")
	tok, id := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)

	calls := []struct{ method, path string }{
		{http.MethodGet, "/api/admin/admin-tokens"},
		{http.MethodGet, "/api/admin/admin-tokens/grants"},
		{http.MethodPost, "/api/admin/admin-tokens"},
		{http.MethodPost, "/api/admin/admin-tokens/" + id + "/rotate"},
		{http.MethodDelete, "/api/admin/admin-tokens/" + id},
		{http.MethodGet, "/api/admin/admin-tokens/" + id + "/requests"},
	}
	for _, c := range calls {
		rec := rg.do(tokenCall{method: c.method, path: c.path, bearer: tok, body: map[string]any{}})
		assert.Equal(t, http.StatusForbidden, rec.Code, "token: %s %s", c.method, c.path)
		rec = rg.do(tokenCall{method: c.method, path: c.path, body: map[string]any{}, header: map[string]string{"X-API-Key": "k"}})
		// An API key with an admin role is refused on the whole admin API.
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "key: %s %s", c.method, c.path)
	}
}

func TestAdminToken_EveryRequestIsLogged(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	tok, id := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)

	calls := []struct {
		path   string
		header map[string]string
		want   int
	}{
		{"/api/admin/schemas", nil, http.StatusOK},
		{"/api/admin/test-plugin/items", nil, http.StatusForbidden},
		{"/api/admin/users", nil, http.StatusForbidden},
		{"/api/admin/schemas", map[string]string{"X-Tenant-ID": "globex"}, http.StatusForbidden},
	}
	for _, c := range calls {
		rec := rg.do(tokenCall{method: http.MethodGet, path: c.path, bearer: tok, header: c.header, remote: "203.0.113.5:1234"})
		require.Equal(t, c.want, rec.Code, c.path)
	}
	rows := rg.requestRows(id, len(calls))
	require.Len(t, rows, len(calls))
	for i, c := range calls {
		assert.Equal(t, http.MethodGet, rows[i].Method)
		assert.Equal(t, c.path, rows[i].RoutePattern, "row %d", i)
		assert.Equal(t, c.want, rows[i].Status, "row %d", i)
		assert.Equal(t, "203.0.113.5", rows[i].ClientIP, "row %d", i)
	}

	// The pattern is recorded, not the path.
	rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/schemas/articles", bearer: tok})
	require.NotEqual(t, http.StatusUnauthorized, rec.Code)
	rows = rg.requestRows(id, len(calls)+1)
	assert.Equal(t, "/api/admin/schemas/{name}", rows[len(rows)-1].RoutePattern)

	// The log is served to the owner, newest first.
	list := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/admin-tokens/" + id + "/requests?limit=2", bearer: session})
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var page struct {
		Data       []core.AdminTokenRequest `json:"data"`
		TotalCount int                      `json:"total_count"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &page))
	assert.Equal(t, len(calls)+1, page.TotalCount)
	require.Len(t, page.Data, 2)
	assert.Equal(t, "/api/admin/schemas/{name}", page.Data[0].RoutePattern)

	// last_used_at was recorded.
	require.Eventually(t, func() bool {
		stored, err := rg.tokens.Get(context.Background(), "default", uuid.MustParse(id))
		return err == nil && stored.LastUsedAt != nil
	}, 5*time.Second, 20*time.Millisecond)
}

func TestAdminToken_ListAndCatalog(t *testing.T) {
	rg := newTokenRig(t)
	_, adminSession := rg.user("admin")
	_, otherSession := rg.user("admin")
	_, superSession := rg.user("super_admin")
	rg.issue(adminSession, []string{core.AdminGrantSchemasRead}, nil)
	rg.issue(otherSession, []string{core.AdminGrantSchemasRead}, nil)

	count := func(session string) int {
		rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/admin-tokens", bearer: session})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "token_hash")
		var page struct {
			TotalCount int `json:"total_count"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
		return page.TotalCount
	}
	assert.Equal(t, 1, count(adminSession), "an admin sees only their own tokens")
	assert.Equal(t, 2, count(superSession), "a super admin sees every token in the tenant")

	rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/admin-tokens/grants", bearer: adminSession})
	require.Equal(t, http.StatusOK, rec.Code)
	var catalog struct {
		Data []adminTokenGrant `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &catalog))
	require.Len(t, catalog.Data, len(core.AdminGrants()))
	byName := map[string]adminTokenGrant{}
	for _, g := range catalog.Data {
		byName[g.Name] = g
	}
	assert.Contains(t, byName[core.AdminGrantSchemasRead].Routes, adminGrantRoute{Method: "GET", Pattern: "/api/admin/schemas/{name}/stats"})
	assert.Equal(t, []adminGrantRoute{
		{Method: "GET", Pattern: "/api/admin/test-plugin/items"},
		{Method: "GET", Pattern: "/api/admin/test-plugin/whoami"},
		{Method: "GET", Pattern: "/api/admin/test-plugin/boom"},
	}, byName[core.AdminGrantFlowsRead].Routes)
	assert.Equal(t, []adminGrantRoute{}, byName[core.AdminGrantAuditRead].Routes)
}

func TestAdminToken_CreationIsValidated(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	create := func(body map[string]any) *httptest.ResponseRecorder {
		full := map[string]any{
			"name":       "ci",
			"grants":     []string{core.AdminGrantSchemasRead},
			"expires_at": time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"password":   tokenTestPassword,
		}
		for k, v := range body {
			full[k] = v
		}
		return rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session, body: full})
	}

	cases := []struct {
		name string
		body map[string]any
		want int
		msg  string
	}{
		{"beyond 90 days", map[string]any{"expires_at": time.Now().Add(91 * 24 * time.Hour).UTC().Format(time.RFC3339)}, http.StatusUnprocessableEntity, "at most 90 days"},
		{"no expiry", map[string]any{"expires_at": nil}, http.StatusUnprocessableEntity, "expires_at is required"},
		{"past expiry", map[string]any{"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}, http.StatusUnprocessableEntity, "in the future"},
		{"invalid CIDR", map[string]any{"allowed_ips": []string{"10.0.0.0/8", "10.0.0.0/33"}}, http.StatusUnprocessableEntity, "10.0.0.0/33"},
		{"unknown grant", map[string]any{"grants": []string{"users:write"}}, http.StatusUnprocessableEntity, "users:write"},
		{"no grants", map[string]any{"grants": []string{}}, http.StatusUnprocessableEntity, "at least one grant"},
		{"wrong password", map[string]any{"password": "not-it"}, http.StatusForbidden, "The password is not valid."},
		{"no password", map[string]any{"password": ""}, http.StatusForbidden, "password is required"},
		{"another tenant", map[string]any{"tenant_id": "globex"}, http.StatusForbidden, "tenant you act in"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := create(tc.body)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.msg)
		})
	}

	// Exactly 90 days is accepted.
	rec := create(map[string]any{"expires_at": time.Now().Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339)})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// An editor cannot own one.
	_, editor := rg.user("editor")
	rec = rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: editor, body: map[string]any{}})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// mfaEnrolledStore reports every account enrolled, with one TOTP secret.
type mfaEnrolledStore struct {
	fakeMFAStore
	enc string
}

func (s *mfaEnrolledStore) IsEnabled(context.Context, uuid.UUID) (bool, error) { return true, nil }
func (s *mfaEnrolledStore) GetEnabled(context.Context, uuid.UUID) (string, []string, error) {
	return s.enc, nil, nil
}

func TestAdminToken_MFAOwnerStepsUpWithACode(t *testing.T) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "LyEve", AccountName: "owner@example.com"})
	require.NoError(t, err)
	enc, err := security.EncryptSecret(key.Secret(), testConfig().JWTSecret)
	require.NoError(t, err)
	rg := newTokenRig(t, WithMFAStore(&mfaEnrolledStore{enc: enc}))
	_, session := rg.user("admin")
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{
			"name":       "ci",
			"grants":     []string{core.AdminGrantSchemasRead},
			"expires_at": time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),
		}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}

	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session, body: body(map[string]any{"password": tokenTestPassword})})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a password does not stand in for the code")
	assert.Contains(t, rec.Body.String(), "mfa_code is required")

	rec = rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session, body: body(map[string]any{"mfa_code": "000000"})})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "The MFA code is not valid.")

	code, err := totp.GenerateCode(key.Secret(), time.Now())
	require.NoError(t, err)
	rec = rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session, body: body(map[string]any{"mfa_code": code})})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// The same code again, still inside its window, is refused.
	rec = rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session, body: body(map[string]any{"mfa_code": code})})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "already used")
}

// A super admin with no home tenant who enrolled MFA while acting in a
// tenant has a secret sealed under that tenant, and confirms with a code
// while acting there.
func TestAdminToken_MFASealedUnderTheActingTenant(t *testing.T) {
	ks, err := encryption.NewKeyStore("admin-token-test-master-key-0123456789")
	require.NoError(t, err)
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "LyEve", AccountName: "root@example.com"})
	require.NoError(t, err)
	enc, err := ks.EncryptPlaintext("acme", []byte(key.Secret()))
	require.NoError(t, err)
	rg := newTokenRig(t, WithKeyStore(ks), WithMFAStore(&mfaEnrolledStore{enc: enc}))
	_, session := rg.userIn("", "super_admin")

	code, err := totp.GenerateCode(key.Secret(), time.Now())
	require.NoError(t, err)
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session,
		header: map[string]string{"X-Tenant-ID": "acme"},
		body: map[string]any{
			"name":       "ci",
			"grants":     []string{core.AdminGrantSchemasRead},
			"expires_at": time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"mfa_code":   code,
		}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out struct {
		TenantID string `json:"tenant_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "acme", out.TenantID)
}

func TestAdminToken_StepUpPasswordLocksOut(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	create := func(password string) *httptest.ResponseRecorder {
		return rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session, body: map[string]any{
			"name":       "ci",
			"grants":     []string{core.AdminGrantSchemasRead},
			"expires_at": time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"password":   password,
		}})
	}
	locked := false
	for i := 0; i < 20; i++ {
		rec := create("wrong-" + strconv.Itoa(i))
		if rec.Code == http.StatusTooManyRequests {
			locked = true
			break
		}
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	}
	require.True(t, locked, "twenty wrong passwords never locked the step-up")
	// Once locked, the right password is refused too.
	assert.Equal(t, http.StatusTooManyRequests, create(tokenTestPassword).Code)
}

func TestAdminRouter_RefusesABadGrantDeclaration(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	cases := []struct {
		name string
		decl plugin.RouteDecl
		want string
	}{
		{"session-only route", plugin.RouteDecl{Method: http.MethodGet, Pattern: "/api/admin/users/{id}", Group: plugin.GroupSuperAdmin, Handler: ok, AdminGrant: core.AdminGrantContentRead}, "session only"},
		{"unknown grant", plugin.RouteDecl{Method: http.MethodGet, Pattern: "/api/admin/things", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: "things:read"}, "not in the catalog"},
		{"super admin route", plugin.RouteDecl{Method: http.MethodGet, Pattern: "/api/admin/things", Group: plugin.GroupSuperAdmin, Handler: ok, AdminGrant: core.AdminGrantContentRead}, "super admin only"},
		{"engine route", plugin.RouteDecl{Method: http.MethodGet, Pattern: "/api/admin/entitlements", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantContentRead}, "served by the engine"},
		{"engine route under another name", plugin.RouteDecl{Method: http.MethodGet, Pattern: "/api/admin/plugins/{plugin}/schema", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantContentRead}, "served by the engine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)),
				WithPluginRoutes([]plugin.PluginRoutes{{Name: "bad-plugin", Routes: []plugin.RouteDecl{tc.decl}}}))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "bad-plugin")
		})
	}
}

func TestAdminRouter_WithoutADatabaseATokenIsRefused(t *testing.T) {
	t.Parallel()
	router, err := NewAdminRouter(nil, testConfig(), WithLifetime(testLifetime(t)))
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/schemas", nil)
	req.Header.Set("Authorization", "Bearer "+core.AdminTokenPrefix+"anything")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/api/admin/admin-tokens/grants", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "the token routes need a database")
}

func TestRouteFinderOf_RefusesARouterThatCannotResolvePatterns(t *testing.T) {
	_, err := routeFinderOf(http.NewServeMux())
	require.Error(t, err)
	_, err = routeFinderOf(chi.NewRouter())
	require.NoError(t, err)
}

// A token never acts as super_admin: its owner's super_admin is read as
// admin of the token's tenant.
func TestAdminToken_NeverCarriesSuperAdmin(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("super_admin")
	tok, _ := rg.issue(session, []string{core.AdminGrantFlowsRead, core.AdminGrantSchemasRead}, nil)

	rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/test-plugin/whoami", bearer: tok, header: map[string]string{"X-Tenant-ID": "default"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var who struct {
		Roles    []string `json:"roles"`
		TenantID string   `json:"tenant_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &who))
	assert.Equal(t, []string{"admin"}, who.Roles)
	assert.Equal(t, "default", who.TenantID)

	// Its own tenant named in the header works. It is still that tenant's admin.
	rec = rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/schemas", bearer: tok, header: map[string]string{"X-Tenant-ID": "default"}})
	assert.Equal(t, http.StatusOK, rec.Code)
}

// The engine table states each route's role gate, and an admin, the most a
// token acts as, passes every one of them.
func TestEngineAdminGrants_MatchTheirRoleGates(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	for _, rd := range engineAdminGrants {
		require.NotEqual(t, core.GroupSuperAdmin, rd.Group, "%s %s", rd.Method, rd.Pattern)
		path := strings.ReplaceAll(rd.Pattern, "{name}", "articles")
		rec := rg.do(tokenCall{method: rd.Method, path: path, bearer: session})
		assert.NotEqual(t, http.StatusForbidden, rec.Code, "%s %s: %s", rd.Method, rd.Pattern, rec.Body.String())
	}
}

// Every route the engine serves under /api/admin is on the list the grant
// registry reads to refuse a plugin grant on an engine route.
func TestEngineOwnedRoutes_CoverTheAdminRouter(t *testing.T) {
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)))
	require.NoError(t, err)
	owned := engineOwnedRoutes(adminPathPrefix)
	err = chi.Walk(router.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, adminPathPrefix+"/") || strings.HasPrefix(route, "/api/admin/debug/pprof/") {
			return nil
		}
		if _, ok := owned[method+" "+normalizeRoutePattern(route)]; !ok {
			t.Errorf("engine route %s %s is missing from engineOwnedRoutes", method, route)
		}
		return nil
	})
	require.NoError(t, err)
}

func TestAdminToken_UnroutedRequestIs404(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	tok, id := rg.issue(session, []string{core.AdminGrantSchemasRead}, nil)
	// No PATCH route exists at this path. The router alone would answer 405.
	rec := rg.do(tokenCall{method: http.MethodPatch, path: "/api/admin/schemas", bearer: tok, body: map[string]any{}})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/no-such-thing", bearer: tok})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rows := rg.requestRows(id, 2)
	assert.Equal(t, http.StatusNotFound, rows[0].Status)
	assert.Equal(t, "", rows[0].RoutePattern)
}

func TestAdminToken_PanickingHandlerIsLoggedAs500(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	tok, id := rg.issue(session, []string{core.AdminGrantFlowsRead}, nil)
	rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/test-plugin/boom", bearer: tok})
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	rows := rg.requestRows(id, 1)
	assert.Equal(t, "/api/admin/test-plugin/boom", rows[0].RoutePattern)
	assert.Equal(t, http.StatusInternalServerError, rows[0].Status)
}

// A full queue loses rows but not the fact of losing them: the writer
// records, per token, how many it could not take.
func TestAdminTokenLog_CountsDroppedRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	pool := testdb.Postgres(t)
	store := newFakeAdminTokens(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	tok := &core.AdminToken{ID: uuid.New(), TenantID: "default", OwnerUserID: uuid.New(), Name: "q",
		TokenHash: uuid.NewString(), DisplayPrefix: "qqqqqqqq", Grants: []string{core.AdminGrantSchemasRead},
		ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	require.NoError(t, store.Create(ctx, tok))

	// A queue of one, with no writer running, so the test decides when it drains.
	l := &adminTokenLog{store: store, items: make(chan adminTokenLogItem, 1), lastTouch: map[uuid.UUID]time.Time{}, dropped: map[uuid.UUID]*adminTokenDropped{}}
	for i := 0; i < 4; i++ {
		l.request(core.AdminTokenRequest{TokenID: tok.ID, TenantID: "default", Method: "GET", RoutePattern: "/api/admin/schemas", Status: 200, CreatedAt: now})
	}
	l.writeBatch(<-l.items)

	rows, total, err := store.ListRequests(ctx, "default", tok.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	patterns := map[string]int{}
	for _, r := range rows {
		patterns[r.RoutePattern] = r.Status
	}
	assert.Equal(t, 200, patterns["/api/admin/schemas"])
	status, ok := patterns[AdminTokenDroppedPrefix+"3"]
	assert.True(t, ok, "no dropped:3 row in %v", patterns)
	assert.Equal(t, 0, status)
}

// A prune works a backlog off batch after batch, not one batch an hour.
func TestAdminTokenLog_PruneWorksOffABacklog(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	pool := testdb.Postgres(t)
	store := newFakeAdminTokens(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	tok := &core.AdminToken{ID: uuid.New(), TenantID: "default", OwnerUserID: uuid.New(), Name: "old",
		TokenHash: uuid.NewString(), DisplayPrefix: "oooooooo", Grants: []string{core.AdminGrantSchemasRead},
		ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	require.NoError(t, store.Create(ctx, tok))
	old := now.Add(-100 * 24 * time.Hour)
	const backlog = 2*adminTokenPruneBatch + 50
	for start := 0; start < backlog; start += adminTokenLogBatch {
		batch := make([]core.AdminTokenRequest, 0, adminTokenLogBatch)
		for i := start; i < min(start+adminTokenLogBatch, backlog); i++ {
			batch = append(batch, core.AdminTokenRequest{TokenID: tok.ID, TenantID: "default", Method: "GET", RoutePattern: "/api/admin/schemas", Status: 200, CreatedAt: old})
		}
		require.NoError(t, store.LogRequests(ctx, batch))
	}
	l := &adminTokenLog{store: store}
	l.prune(ctx, now)
	_, total, err := store.ListRequests(ctx, "default", tok.ID, 1, 0)
	require.NoError(t, err)
	assert.Zero(t, total, "rows past the window survived the prune")
}

// An admin token is a credential, so a build with nowhere to keep one has to
// refuse every token presented rather than read it as no credential at all.
// Reading it as anonymous would hand a route's own auth the job of noticing,
// and a public route would answer the caller as a stranger without ever
// saying the credential was not honored.
//
// The routes that issue tokens are not mounted either, because a create that
// cannot store anything is worse than an absent route.
func TestAdminToken_WithNoStoreEveryTokenIsRefused(t *testing.T) {
	rg := newStorelessTokenRig(t)
	_, session := rg.user("super_admin")

	for _, path := range []string{"/api/admin/schemas", "/api/admin/test-plugin/items"} {
		rec := rg.do(tokenCall{method: http.MethodGet, path: path, bearer: core.AdminTokenPrefix + "anything-at-all"})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, path)
		assert.Contains(t, rec.Body.String(), "This admin token is not valid.", path)
	}

	// The session is untouched: people still administer the instance.
	assert.Equal(t, http.StatusOK, rg.get("/api/admin/schemas", session))

	// Nothing issues, rotates, revokes or audits a token here.
	for _, c := range []tokenCall{
		{method: http.MethodGet, path: "/api/admin/admin-tokens"},
		{method: http.MethodGet, path: "/api/admin/admin-tokens/grants"},
		{method: http.MethodPost, path: "/api/admin/admin-tokens", body: map[string]any{}},
		{method: http.MethodDelete, path: "/api/admin/admin-tokens/" + uuid.NewString()},
		{method: http.MethodGet, path: "/api/admin/admin-tokens/" + uuid.NewString() + "/requests"},
	} {
		c.bearer = session
		rec := rg.do(c)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s", c.method, c.path)
	}
}
