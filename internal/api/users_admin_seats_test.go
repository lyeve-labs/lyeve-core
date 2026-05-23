//go:build !mutest

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// seatEntitlements is a licensing implementation that states one ceiling on
// admin seats, where 0 is the unlimited ceiling.
type seatEntitlements struct {
	unlicensedEntitlements
	limit int
	state string
}

func (e seatEntitlements) Snapshot() licensing.Snapshot {
	snap := e.unlicensedEntitlements.Snapshot()
	snap.State = e.state
	snap.Caps = map[string]int{core.CapAdminSeats: e.limit}
	return snap
}

// seatRouter is an admin router whose first super admin was claimed through
// setup, and a bearer token for that account.
type seatRouter struct {
	router http.Handler
	store  *db.UserStore
	token  string
}

func newSeatRouter(t *testing.T, ent seatEntitlements) *seatRouter {
	t.Helper()
	if testing.Short() {
		t.Skip("needs a database container")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}
	pool := testdb.Postgres(t)
	cfg := setupRouterConfig()
	router, err := NewAdminRouter(pool, cfg, WithEntitlements(ent), WithLifetime(testLifetime(t)))
	require.NoError(t, err)

	rr := postSetup(t, router, "owner@seats.test", routerSetupToken)
	require.Equal(t, http.StatusCreated, rr.Code, "the setup admin is never refused: %s", rr.Body.String())

	store := db.NewUserStore(pool)
	owner, err := store.GetByEmail(context.Background(), "owner@seats.test")
	require.NoError(t, err)
	tok, err := auth.Sign(cfg.JWTSecret, 3600, owner.ID, owner.Email, owner.Roles, owner.TenantID, owner.TokenVersion)
	require.NoError(t, err)
	return &seatRouter{router: router, store: store, token: tok}
}

func (s *seatRouter) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
	rr := httptest.NewRecorder()
	s.router.ServeHTTP(rr, req)
	return rr
}

func (s *seatRouter) createUser(t *testing.T, email string, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	rolesJSON, err := json.Marshal(roles)
	require.NoError(t, err)
	return s.do(t, http.MethodPost, "/api/admin/users",
		fmt.Sprintf(`{"email":%q,"password":"CorrectHorseBatteryStaple1!","roles":%s}`, email, rolesJSON))
}

func (s *seatRouter) setRoles(t *testing.T, id string, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	rolesJSON, err := json.Marshal(roles)
	require.NoError(t, err)
	return s.do(t, http.MethodPut, "/api/admin/users/"+id+"/roles", fmt.Sprintf(`{"roles":%s}`, rolesJSON))
}

func requireSeatRefusal(t *testing.T, rr *httptest.ResponseRecorder, limit, current int) {
	t.Helper()
	require.Equal(t, http.StatusPaymentRequired, rr.Code, rr.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "cap_exceeded", body["error"])
	assert.Equal(t, core.CapAdminSeats, body["cap"])
	assert.EqualValues(t, limit, body["limit"])
	assert.EqualValues(t, current, body["current"])
}

func userID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var u map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &u))
	id, _ := u["id"].(string)
	require.NotEmpty(t, id)
	return id
}

func TestAdminRouter_AdminSeats_RefusesTheSeatPastTheCeiling(t *testing.T) {
	s := newSeatRouter(t, seatEntitlements{limit: 4, state: "active"})

	second := userID(t, s.createUser(t, "second@seats.test", "admin"))
	userID(t, s.createUser(t, "third@seats.test", "super_admin"))
	userID(t, s.createUser(t, "fourth@seats.test", "admin"))
	requireSeatRefusal(t, s.createUser(t, "fifth@seats.test", "admin"), 4, 4)
	_, err := s.store.GetByEmail(context.Background(), "fifth@seats.test")
	require.Error(t, err, "a refused seat must not create the account")

	editor := userID(t, s.createUser(t, "editor@seats.test", "editor"))
	userID(t, s.createUser(t, "viewer@seats.test", "viewer"))

	requireSeatRefusal(t, s.setRoles(t, editor, "editor", "admin"), 4, 4)
	require.Equal(t, http.StatusOK, s.setRoles(t, editor, "viewer").Code, "a role that grants no seat is never refused")
	require.Equal(t, http.StatusOK, s.setRoles(t, second, "super_admin").Code, "an account holding a seat keeps it")

	require.Equal(t, http.StatusOK, s.setRoles(t, second, "editor").Code)
	require.Equal(t, http.StatusOK, s.setRoles(t, editor, "admin").Code, "a demotion frees the seat for the next admin")
}

func TestAdminRouter_AdminSeats_UnlimitedCeilingAdmitsEverySeat(t *testing.T) {
	s := newSeatRouter(t, seatEntitlements{limit: 0, state: "active"})
	for i := range 4 {
		userID(t, s.createUser(t, fmt.Sprintf("admin%d@seats.test", i), "admin"))
	}
}

// A role change for an account that does not exist answers 404, even at the
// ceiling, where the guard alone would count the id as a new seat.
func TestAdminRouter_AdminSeats_UnknownUserIsNotFound(t *testing.T) {
	s := newSeatRouter(t, seatEntitlements{limit: 1, state: "active"})
	rr := s.setRoles(t, "00000000-0000-4000-8000-000000000001", "admin")
	require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
}

// An install that holds more seats than the ceiling, because the ceiling came
// after its accounts or went down, keeps every one of them.
func TestAdminRouter_AdminSeats_OverTheCeilingKeepsEveryAccount(t *testing.T) {
	s := newSeatRouter(t, seatEntitlements{limit: 4, state: "expired"})
	ctx := context.Background()
	var ids []string
	for i := range 4 {
		u, err := s.store.Create(ctx, fmt.Sprintf("held%d@seats.test", i), "x", []string{"admin"}, "")
		require.NoError(t, err)
		ids = append(ids, u.ID.String())
	}

	requireSeatRefusal(t, s.createUser(t, "sixth@seats.test", "admin"), 4, 5)

	list := s.do(t, http.MethodGet, "/api/admin/users", "")
	require.Equal(t, http.StatusOK, list.Code)
	var users []map[string]any
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &users))
	assert.Len(t, users, 5, "every seat the install held is still there")

	for _, id := range ids {
		require.Equal(t, http.StatusOK, s.setRoles(t, id, "admin", "editor").Code, "a held seat may still be edited")
	}
}

// The setup admin is the first account on an empty install, so the count it
// would meet is zero. Even a ceiling of one admits it, and the ceiling then
// applies to the next.
func TestAdminRouter_AdminSeats_SetupAdminIsNeverRefused(t *testing.T) {
	s := newSeatRouter(t, seatEntitlements{limit: 1, state: "active"})
	requireSeatRefusal(t, s.createUser(t, "second@seats.test", "admin"), 1, 1)
}

// An issuer whose policy grants an admin role makes its user an admin seat,
// so a full install answers that user's first request with the cap refusal,
// and keeps serving the issuer's other users and its admins already seated.
func TestTrustedIssuerAuth_AdminSeats_FullInstallRefusesANewAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a database container")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}
	ctx := context.Background()
	store := db.NewUserStore(testdb.Postgres(t))
	for i := range 4 {
		_, err := store.Create(ctx, fmt.Sprintf("local%d@seats.test", i), "x", []string{"admin"}, "")
		require.NoError(t, err)
	}
	limit := 4
	guard := db.NewAdminSeatGuard(store, func() int { return limit }, nil)

	iss := newTestIssuer(t)
	policy := auth.IssuerPolicy{
		Issuer: iss.srv.URL, Tenant: "agency", RolesClaim: "groups",
		RoleMap: map[string]string{"cms-editors": "editor", "cms-admins": "admin"},
	}
	serve := func(sub string, groups ...string) *httptest.ResponseRecorder {
		h := trustedIssuerAuth([]auth.IssuerPolicy{policy}, store, guard)(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Header.Set("Authorization", "Bearer "+iss.sign(t, map[string]any{"sub": sub, "email": sub + "@agency.example", "groups": groups}))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	requireSeatRefusal(t, serve("new-admin", "cms-admins"), 4, 4)
	_, err := store.GetByID(ctx, auth.ExternalUserID(iss.srv.URL, "new-admin"))
	require.Error(t, err, "a refused seat must not create the account")

	require.Equal(t, http.StatusOK, serve("an-editor", "cms-editors").Code, "a user the policy makes no admin is not a seat")
	requireSeatRefusal(t, serve("an-editor", "cms-admins"), 4, 4)

	limit = 7
	require.Equal(t, http.StatusOK, serve("new-admin", "cms-admins").Code)
	limit = 4
	require.Equal(t, http.StatusOK, serve("new-admin", "cms-admins").Code, "a seated admin keeps signing in at the ceiling")
}
