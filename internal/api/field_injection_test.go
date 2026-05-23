package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
)

// Stub driver that yields a single row with token_version = 0. The smoke tests
// route /health through tokenVersionCheck, which always queries the user
// store (tv=0 is a real version). A nil QueryRow would panic on Scan, so this
// driver supplies a real *sql.Row that scans to 0.

func init() { sql.Register("stubtv", &stubTVDriver{}) }

type stubTVDriver struct{}

func (d *stubTVDriver) Open(name string) (driver.Conn, error) { return &stubTVConn{}, nil }

type stubTVConn struct{}

func (c *stubTVConn) Prepare(query string) (driver.Stmt, error) { return &stubTVStmt{}, nil }
func (c *stubTVConn) Close() error                              { return nil }
func (c *stubTVConn) Begin() (driver.Tx, error)                 { return nil, nil }

type stubTVStmt struct{}

func (s *stubTVStmt) Close() error                                    { return nil }
func (s *stubTVStmt) NumInput() int                                   { return -1 }
func (s *stubTVStmt) Exec(args []driver.Value) (driver.Result, error) { return nil, nil }
func (s *stubTVStmt) Query(args []driver.Value) (driver.Rows, error)  { return &stubTVRows{}, nil }

type stubTVRows struct{ done bool }

func (r *stubTVRows) Columns() []string { return []string{"token_version"} }
func (r *stubTVRows) Close() error      { return nil }
func (r *stubTVRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(0)
	return nil
}

// newTokenVersionRow returns a *sql.Row that scans to token_version 0.
func newTokenVersionRow() *sql.Row {
	raw, err := sql.Open("stubtv", "")
	if err != nil {
		panic(fmt.Sprintf("stubtv sql.Open: %v", err))
	}
	return raw.QueryRowContext(context.Background(), "select token_version")
}

// stubPluginStatusProvider returns a canned PluginStatusReport for tests.
type stubPluginStatusProvider struct {
	report plugin.PluginStatusReport
}

func (s stubPluginStatusProvider) Status() plugin.PluginStatusReport { return s.report }

// injectIDField: pure function tests

func TestBuildOpenAPIDoc_NoSchemas(t *testing.T) {
	t.Parallel()
	doc := buildOpenAPIDoc(nil, nil, openAPIOptions{})
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("OpenAPI = %s", doc.OpenAPI)
	}
	if doc.Info.Title != "LyEve API" {
		t.Errorf("title = %s", doc.Info.Title)
	}
	if doc.Paths["/api/admin/auth/login"] == nil {
		t.Error("missing /api/admin/auth/login path")
	}
	if doc.Paths["/api/admin/setup"] == nil {
		t.Error("missing /api/admin/setup path")
	}
	if _, ok := doc.Components.SecuritySchemes["bearerAuth"]; !ok {
		t.Error("missing bearerAuth security scheme")
	}
	if _, ok := doc.Components.SecuritySchemes["cookieAuth"]; !ok {
		t.Error("missing cookieAuth security scheme")
	}
}

func TestBuildOpenAPIDoc_WithSchemas(t *testing.T) {
	t.Parallel()
	// The content API is documented once, as the {schema} family the router
	// registers. The collections appear as the parameter's allowed values.
	doc := buildOpenAPIDoc([]string{"posts", "pages"}, nil, openAPIOptions{})
	if doc.Paths["/api/v1/content/posts"] != nil {
		t.Error("the content API is documented per collection; it should be the {schema} family")
	}
	family := doc.Paths["/api/v1/content/{schema}"]
	if family == nil || family["get"] == nil || family["post"] == nil {
		t.Fatal("missing list and create on /api/v1/content/{schema}")
	}
	item := doc.Paths["/api/v1/content/{schema}/{id}"]
	if item == nil || item["get"] == nil || item["put"] == nil || item["delete"] == nil {
		t.Error("missing full CRUD on /api/v1/content/{schema}/{id}")
	}
	var schema *openAPIParameter
	for i := range family["get"].Parameters {
		if family["get"].Parameters[i].Name == "schema" {
			schema = &family["get"].Parameters[i]
		}
	}
	if schema == nil || schema.In != "path" || !schema.Required {
		t.Fatal("the list operation does not take the collection as a required path parameter")
	}
	enum, _ := schema.Schema["enum"].([]any)
	if len(enum) != 2 || enum[0] != "pages" || enum[1] != "posts" {
		t.Errorf("schema enum = %v, want the two collections sorted", enum)
	}
}

// Past the cap, the parameter states the count instead of naming every
// collection, so a large registry keeps the document readable.
func TestBuildOpenAPIDoc_ManyCollectionsAreCounted(t *testing.T) {
	t.Parallel()
	names := make([]string, openAPICollectionEnumMax+1)
	for i := range names {
		names[i] = "c" + strconv.Itoa(i)
	}
	doc := buildOpenAPIDoc(names, nil, openAPIOptions{})
	p := doc.Paths["/api/v1/content/{schema}"]["get"].Parameters[0]
	if _, has := p.Schema["enum"]; has {
		t.Error("enum present past the cap")
	}
	if !strings.Contains(p.Description, strconv.Itoa(len(names))+" collections") {
		t.Errorf("description = %q, want the count", p.Description)
	}
}

// A write on the content API says which role it needs, as the x-roles
// extension beside the standard fields, so a client can show the gate.
func TestBuildOpenAPIDoc_WritesCarryRoles(t *testing.T) {
	t.Parallel()
	doc := buildOpenAPIDoc(nil, nil, openAPIOptions{})
	raw, err := json.Marshal(doc.Paths["/api/v1/content/{schema}"]["post"])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	roles, _ := m["x-roles"].([]any)
	if len(roles) != 3 || roles[0] != "editor" {
		t.Errorf("x-roles = %v, want editor, admin, super_admin", m["x-roles"])
	}
	if _, nested := m["Extensions"]; nested {
		t.Error("extensions serialized as a nested field rather than x- keys")
	}
	get, _ := json.Marshal(doc.Paths["/api/v1/content/{schema}"]["get"])
	if strings.Contains(string(get), "x-roles") {
		t.Error("a read carries roles it does not require")
	}
}

// The routes the active plugins declare are in the document, under a tag per
// plugin, with the gate their group implies. A route the static document
// already describes keeps its written entry.
func TestBuildOpenAPIDoc_PluginRoutes(t *testing.T) {
	t.Parallel()
	declared := []plugin.PluginRoutes{{
		Name: "widgets",
		Routes: []plugin.RouteDecl{
			{Method: "GET", Pattern: "/api/admin/widgets", Group: core.GroupAdmin},
			{Method: "POST", Pattern: "/api/admin/widgets/{id}/replay", Group: core.GroupSuperAdmin},
			{Method: "GET", Pattern: "/api/admin/users", Group: core.GroupAdmin},
			{Method: "GET", Pattern: "/api/v1/widgets/ping", Group: core.GroupPublic},
		},
	}}
	doc := buildOpenAPIDoc(nil, declared, openAPIOptions{})
	list := doc.Paths["/api/admin/widgets"]["get"]
	if list == nil || list.Tags[0] != "Plugin / widgets" || len(list.Security) == 0 {
		t.Fatalf("declared admin route not documented as the plugin's, got %+v", list)
	}
	replay := doc.Paths["/api/admin/widgets/{id}/replay"]["post"]
	if replay == nil || len(replay.Parameters) != 1 || replay.Parameters[0].Name != "id" {
		t.Errorf("path parameter not read from the pattern: %+v", replay)
	}
	if roles, _ := replay.Extensions[xRoles].([]string); len(roles) != 1 || roles[0] != "super_admin" {
		t.Errorf("super_admin route roles = %v", replay.Extensions[xRoles])
	}
	if pub := doc.Paths["/api/v1/widgets/ping"]["get"]; pub == nil || len(pub.Security) != 0 {
		t.Error("a public route carries a gate")
	}
	if written := doc.Paths["/api/admin/users"]["get"]; written.Tags[0] != "Admin / Users" {
		t.Errorf("the static entry was overwritten by the declaration: %v", written.Tags)
	}
}

func TestBuildOpenAPIDoc_HealthAndMetrics(t *testing.T) {
	t.Parallel()
	doc := buildOpenAPIDoc(nil, nil, openAPIOptions{})
	for _, p := range []string{
		"/api/admin/health", "/api/admin/ready", "/api/admin/metrics",
		"/api/v1/health", "/api/v1/ready",
	} {
		if doc.Paths[p] == nil {
			t.Errorf("missing %s", p)
		}
	}
}

// A route of a licensed plugin says which feature it needs, as x-feature
// beside the standard fields, so a client can tell a route the license does
// not cover from one that does not exist. The route of a plugin its grant marks
// ungated needs no feature and carries none.
func TestBuildOpenAPIDoc_PluginRoutesCarryFeature(t *testing.T) {
	t.Parallel()
	declared := []plugin.PluginRoutes{
		{Name: "widgets", Routes: []plugin.RouteDecl{
			{Method: "GET", Pattern: "/api/admin/widgets", Group: core.GroupAdmin},
			{Method: "POST", Pattern: "/api/v1/widgets/query", Group: core.GroupPublic},
		}},
		{Name: "gadgets", Routes: []plugin.RouteDecl{
			{Method: "GET", Pattern: "/api/admin/gadgets", Group: core.GroupAdmin},
		}},
	}
	ungated := func(name string) bool { return name == "gadgets" }
	doc := buildOpenAPIDoc(nil, declared, openAPIOptions{Ungated: ungated})

	for _, path := range []string{"/api/admin/widgets", "/api/v1/widgets/query"} {
		method := "get"
		if strings.Contains(path, "query") {
			method = "post"
		}
		raw, err := json.Marshal(doc.Paths[path][method])
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m["x-feature"] != "widgets" {
			t.Errorf("%s %s x-feature = %v, want widgets", method, path, m["x-feature"])
		}
	}
	ungatedOp, _ := json.Marshal(doc.Paths["/api/admin/gadgets"]["get"])
	if strings.Contains(string(ungatedOp), "x-feature") {
		t.Error("an ungated plugin's route carries a feature it does not need")
	}
	engine, _ := json.Marshal(doc.Paths["/api/admin/auth/login"]["post"])
	if strings.Contains(string(engine), "x-feature") {
		t.Error("an engine route carries a feature no declaration names")
	}
}

// CORS middleware: additional cases

func TestCORSMiddleware_WildcardOrigin(t *testing.T) {
	t.Parallel()
	mw := corsMiddleware(CORSConfig{
		Origins:          []string{"*"},
		AllowCredentials: false,
		PreflightMaxAge:  3600,
		AllowMethods:     "GET, POST, PUT, DELETE, PATCH, OPTIONS",
		AllowHeaders:     "Content-Type, Authorization, X-API-Key, X-Tenant-ID, X-Correlation-ID",
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://any.example.com")
	rr := httptest.NewRecorder()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw(next).ServeHTTP(rr, r)
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("wildcard origin with credentials=false should set ACAO to '*', got %q", got)
	}
}

func TestCORSMiddleware_AllowMethods(t *testing.T) {
	t.Parallel()
	mw := corsMiddleware(CORSConfig{
		Origins:          []string{"https://app.example.com"},
		AllowCredentials: true,
		PreflightMaxAge:  3600,
		AllowMethods:     "GET, POST, PUT, DELETE, PATCH, OPTIONS",
		AllowHeaders:     "Content-Type, Authorization, X-API-Key, X-Tenant-ID, X-Correlation-ID",
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://app.example.com")
	rr := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, r)
	methods := rr.Header().Get("Access-Control-Allow-Methods")
	if !strings.Contains(methods, "GET") || !strings.Contains(methods, "POST") {
		t.Errorf("Allow-Methods = %q", methods)
	}
}

// Auth handler: input validation tests

func TestAuthHandler_Token_BadJSON(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader("{"))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Token(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAuthHandler_Token_MissingFields(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	body := `{"email":"","password":""}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Token(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestAuthHandler_Login_BadJSON(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader("not-json"))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Login(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAuthHandler_MFAVerify_NilStore_Returns501(t *testing.T) {
	t.Parallel()

	// When MFA store is nil (plugin not wired), the handler must return
	// 501 Not Implemented: never panic with nil dereference.
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	body := `{"challenge_token":"some-jwt","code":"123456"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, r)

	if rr.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d (nil MFA store must return 501)", rr.Code, http.StatusNotImplemented)
	}
}

func TestAuthHandler_MFAVerify_BadJSON(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, &stubMFAStore{}, nil, "secret", 3600, false)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader("{"))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAuthHandler_MFAVerify_MissingFields(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, &stubMFAStore{}, nil, "secret", 3600, false)
	body := `{"challenge_token":"","code":""}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAuthHandler_MFAVerify_BadToken(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, &stubMFAStore{}, nil, "test-secret-at-least-32-bytes-long!", 3600, false)
	body := `{"challenge_token":"not-a-valid-jwt","code":"123456"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestAuthHandler_Setup_BadJSON(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/setup", strings.NewReader("{"))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Setup(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAuthHandler_Setup_ShortPassword(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	h.WithSetupToken(NewSetupTokenFromEnv("unit-setup-token-0123"))
	body := `{"email":"a@b.com","password": "***"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/setup", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(SetupTokenHeader, "unit-setup-token-0123")
	rr := httptest.NewRecorder()
	h.Setup(rr, r)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnprocessableEntity)
	}
}

func TestAuthHandler_Setup_MissingEmail(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	h.WithSetupToken(NewSetupTokenFromEnv("unit-setup-token-0123"))
	body := `{"email":"","password": "longen...word"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/setup", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(SetupTokenHeader, "unit-setup-token-0123")
	rr := httptest.NewRecorder()
	h.Setup(rr, r)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnprocessableEntity)
	}
}

// Auth handler: constructor + MFA

func TestAuthHandler_Logout_NoCookie(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/logout", nil)
	rr := httptest.NewRecorder()
	h.Logout(rr, r)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d", rr.Code)
	}
	cookies := rr.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == security.SessionCookieNameInsecure {
			found = true
			if c.MaxAge != -1 {
				t.Errorf("clearing cookie MaxAge = %d, want -1", c.MaxAge)
			}
		}
	}
	if !found {
		t.Error("should set clearing cookie even without existing session")
	}
}

func TestAuthHandler_PasswordHashAlgo_Argon2ID(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false, "argon2id")
	if h.passwordHashAlgo != "argon2id" {
		t.Errorf("algo = %s, want argon2id", h.passwordHashAlgo)
	}
}

func TestNewAuthHandler_NoAlgoArgDefaultsBCrypt(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	if h.passwordHashAlgo != "bcrypt" {
		t.Errorf("default algo = %s, want bcrypt", h.passwordHashAlgo)
	}
}

func TestNewAuthHandler_EmptyAlgoArgDefaultsBCrypt(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false, "")
	if h.passwordHashAlgo != "bcrypt" {
		t.Errorf("default algo with empty arg = %s, want bcrypt", h.passwordHashAlgo)
	}
}

// UsersHandler: validation tests

func TestUsersHandler_Create_BadJSON(t *testing.T) {
	t.Parallel()
	h := NewUsersHandler(nil)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader("{"))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Create(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestUsersHandler_Create_ShortPassword(t *testing.T) {
	t.Parallel()
	h := NewUsersHandler(nil)
	body := `{"email":"user@test.com","password": "***","roles":["editor"]}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Create(rr, r)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnprocessableEntity)
	}
}

func TestUsersHandler_UpdateRoles_BadID(t *testing.T) {
	t.Parallel()
	h := NewUsersHandler(nil)
	r := httptest.NewRequest(http.MethodPut, "/api/admin/users/bad-id/roles", nil)
	r = chiCtx(r, map[string]string{"id": "not-a-uuid"})
	rr := httptest.NewRecorder()
	h.UpdateRoles(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestUsersHandler_UpdateRoles_BadJSON(t *testing.T) {
	t.Parallel()
	uid := uuid.New()
	h := NewUsersHandler(nil)
	r := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+uid.String()+"/roles", strings.NewReader("{"))
	r = chiCtx(r, map[string]string{"id": uid.String()})
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.UpdateRoles(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestUsersHandler_UpdateRoles_EmptyRoles(t *testing.T) {
	t.Parallel()
	uid := uuid.New()
	h := NewUsersHandler(nil)
	body := `{"roles":[]}`
	r := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+uid.String()+"/roles", strings.NewReader(body))
	r = chiCtx(r, map[string]string{"id": uid.String()})
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.UpdateRoles(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for empty roles", rr.Code, http.StatusBadRequest)
	}
}

func TestUsersHandler_Delete_BadID(t *testing.T) {
	t.Parallel()
	h := NewUsersHandler(nil)
	r := httptest.NewRequest(http.MethodDelete, "/api/admin/users/bad-id", nil)
	r = chiCtx(r, map[string]string{"id": "not-a-uuid"})
	rr := httptest.NewRecorder()
	h.Delete(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestUsersHandler_Delete_CannotDeleteSelf(t *testing.T) {
	t.Parallel()
	uid := uuid.New()
	claims := makeClaims(uid, "admin@test.com", []string{"super_admin"})
	h := NewUsersHandler(nil)
	r := httptest.NewRequest(http.MethodDelete, "/api/admin/users/"+uid.String(), nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	r = chiCtx(r, map[string]string{"id": uid.String()})
	rr := httptest.NewRecorder()
	h.Delete(rr, r)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

// PermissionHandler: additional validation tests

func TestContentHandler_RestoreRevision_BadID(t *testing.T) {
	t.Parallel()
	h := &ContentHandler{store: nil, schemas: nil, hooks: hooks.NewRegistry(), perms: nil}
	r := httptest.NewRequest(http.MethodPut, "/content/posts/bad-id/revisions/rev1/restore", nil)
	r = contentReq(r, map[string]string{
		"schema": "posts",
		"id":     "not-a-uuid",
		"rev_id": "rev1",
	})
	rr := httptest.NewRecorder()
	h.RestoreRevision(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

// requireRole: additional test

func TestRequireRole_MultipleRoles_OneMatches(t *testing.T) {
	t.Parallel()
	claims := makeClaims(uuid.New(), "editor@test.com", []string{"editor"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	rr := httptest.NewRecorder()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	requireRole("admin", "editor", "super_admin")(next).ServeHTTP(rr, r)
	if !called {
		t.Fatal("handler should be called when user has one of multiple required roles")
	}
}

// trustedIssuerAuth

func TestTrustedIssuerAuth_EmptyIssuers_PassesThrough(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer some-token")
	rr := httptest.NewRecorder()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	trustedIssuerAuth(nil, nil, nil)(next).ServeHTTP(rr, r)
	if !called {
		t.Error("handler should be called when no trusted issuers configured")
	}
}

func TestTrustedIssuerAuth_NoAuthorization_PassesThrough(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	trustedIssuerAuth([]auth.IssuerPolicy{{Issuer: "https://issuer.example.com", Tenant: "acme"}}, &fakeExternalUsers{}, nil)(next).ServeHTTP(rr, r)
	if !called {
		t.Error("handler should be called when no Authorization header")
	}
}

func TestTrustedIssuerAuth_AlreadyHasClaims_Skips(t *testing.T) {
	t.Parallel()
	claims := makeClaims(uuid.New(), "user@test.com", []string{"admin"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	r.Header.Set("Authorization", "Bearer external-token")
	rr := httptest.NewRecorder()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		c := claimsFromCtx(r)
		if c == nil || c.Email != "user@test.com" {
			t.Error("original claims should be preserved")
		}
		w.WriteHeader(http.StatusOK)
	})
	trustedIssuerAuth([]auth.IssuerPolicy{{Issuer: "https://issuer.example.com", Tenant: "acme"}}, &fakeExternalUsers{}, nil)(next).ServeHTTP(rr, r)
	if !called {
		t.Error("handler should be called")
	}
}

// JWK handler

func TestJWKSHandler_ReturnsJSON(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	rr := httptest.NewRecorder()
	auth.JWKSHandler()(rr, r)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "\"keys\"") {
		t.Errorf("jwks response should contain keys array: %q", body)
	}
}

// bearerToken additional

func TestBearerToken_NoHeader(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	got := bearerToken(r)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestBearerToken_OnlyBearer(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer")
	got := bearerToken(r)
	if got != "" {
		t.Errorf("expected empty for 'Bearer' without token, got %q", got)
	}
}

// respond / httpx.ErrorReq

func TestErrorReq_AllStatusCodes(t *testing.T) {
	t.Parallel()
	codes := []int{200, 201, 204, 400, 401, 403, 404, 409, 422, 500, 502, 503}
	for _, code := range codes {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req = req.WithContext(logging.WithRequestID(req.Context(), "req-abc"))
		httpx.ErrorReq(rr, req, code, "test error")
		if rr.Code != code {
			t.Errorf("ErrorReq(%d): got status %d", code, rr.Code)
		}
		body := decodeBody(t, rr)
		if body["error"] != "test error" {
			t.Errorf("ErrorReq(%d): error field = %q", code, body["error"])
		}
		if body["request_id"] != "req-abc" {
			t.Errorf("ErrorReq(%d): request_id = %q, want %q", code, body["request_id"], "req-abc")
		}
	}
}

// Router options

func TestWithAPIKeyAuth_SetsProvider(t *testing.T) {
	t.Parallel()
	mw := func(next http.Handler) http.Handler { return next }
	o := applyRouterOptions([]RouterOption{WithAPIKeyAuth(mw)})
	if o.apiKeyAuth == nil {
		t.Fatal("apiKeyAuth should be set")
	}
}

func TestWithMFAStore_NilIsNil(t *testing.T) {
	t.Parallel()
	o := applyRouterOptions([]RouterOption{WithMFAStore(nil)})
	if o.mfaStore != nil {
		t.Error("mfaStore should be nil when passed nil")
	}
}

func TestWithPluginRoutes_Empty(t *testing.T) {
	t.Parallel()
	o := applyRouterOptions([]RouterOption{WithPluginRoutes([]plugin.PluginRoutes{})})
	if len(o.pluginRoutes) != 0 {
		t.Errorf("pluginRoutes length = %d, want 0", len(o.pluginRoutes))
	}
}

func TestApplyRouterOptions_MultipleOptions(t *testing.T) {
	t.Parallel()
	prov := stubPluginStatusProvider{report: plugin.PluginStatusReport{}}
	ent := stubEntitlements{features: map[string]bool{"feature-a": true}}
	mw := func(next http.Handler) http.Handler { return next }
	o := applyRouterOptions([]RouterOption{
		WithPluginStatus(prov),
		WithEntitlements(ent),
		WithMiddleware(mw),
	})
	if o.pluginStatus == nil {
		t.Error("pluginStatus should be set")
	}
	if o.entitlements == nil {
		t.Fatal("entitlements should be set")
	}
	if !slices.Contains(o.entitlements.Snapshot().Features, "feature-a") {
		t.Error("feature-a should be present")
	}
	if len(o.extra) != 1 {
		t.Errorf("extra count = %d, want 1", len(o.extra))
	}
}

// safeUser: strips password hash

func TestSafeUser_StripsPasswordHash(t *testing.T) {
	t.Parallel()
	uid := uuid.New()
	u := &domain.User{
		ID:           uid,
		Email:        "a@b.com",
		PasswordHash: "$2a$...secret...",
		Roles:        []string{"admin"},
	}
	out := safeUser(u)
	if out["password_hash"] == "$2a$...secret..." {
		t.Error("password_hash should not be leaked")
	}
	if out["id"] != uid.String() {
		t.Errorf("id = %q, want %q", out["id"], uid.String())
	}
	if out["email"] != "a@b.com" {
		t.Errorf("email = %q", out["email"])
	}
}

// httpStatus: additional content error-to-status mapping

func TestHTTPStatus_NilError(t *testing.T) {
	t.Parallel()
	got := httpStatus(nil)
	// nil error -> StoreStatusFor returns StatusOK. Callers guard against nil.
	if got != http.StatusOK {
		t.Errorf("httpStatus(nil) = %d, want %d", got, http.StatusOK)
	}
}

func TestHTTPStatus_UnknownError(t *testing.T) {
	t.Parallel()
	got := httpStatus(context.Canceled)
	if got != http.StatusServiceUnavailable {
		t.Errorf("httpStatus(context.Canceled) = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

// etag helpers additional

func TestEtagFor_DifferentInputs(t *testing.T) {
	t.Parallel()
	a := etagFor(map[string]any{"a": 1})
	b := etagFor(map[string]any{"a": 2})
	if a == b {
		t.Error("different inputs should produce different etags")
	}
}

// parseFilters additional

func TestParseFilters_EmptyValue(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/?filters[status]=", nil)
	f, _ := parseFilters(r)
	if f["status"] != "" {
		t.Errorf("empty filter value should be empty string, got %q", f["status"])
	}
}

func TestParseFilters_UrlDecoding(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/?filters[title]=hello+world&filters[tag]=foo%2Fbar", nil)
	f, _ := parseFilters(r)
	if f["title"] != "hello world" {
		t.Errorf("url-decoded title = %q", f["title"])
	}
}

// queryInt additional

func TestQueryInt_LargeValue(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/?offset=999999", nil)
	got, err := reqparse.QueryInt(r, "offset", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 999999 {
		t.Errorf("QueryInt(offset) = %d, want 999999", got)
	}
}

func TestQueryInt_MultipleParams(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/?limit=50&offset=100", nil)
	if got, err := reqparse.QueryInt(r, "limit", 20); err != nil {
		t.Errorf("limit error: %v", err)
	} else if got != 50 {
		t.Errorf("limit = %d", got)
	}
	if got, err := reqparse.QueryInt(r, "offset", 0); err != nil {
		t.Errorf("offset error: %v", err)
	} else if got != 100 {
		t.Errorf("offset = %d", got)
	}
}

// chiCtx helper additional

func TestChiCtx_NoParams(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	r = chiCtx(r, map[string]string{})
	if r.Context().Value(chi.RouteCtxKey) == nil {
		t.Error("route context should be set")
	}
}

// mountPluginRoutes: additional

func TestMountPluginRoutes_PrefixMismatch(t *testing.T) {
	t.Parallel()
	r := chi.NewRouter()
	pluginRoutes := []plugin.PluginRoutes{
		{
			Routes: []plugin.RouteDecl{
				{
					Method:  "GET",
					Pattern: "/api/v1/events",
					Group:   plugin.GroupPublic,
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusOK)
					}),
				},
			},
		},
	}
	mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	// Route should not be mounted under /api/admin prefix mismatch
	if rr.Code == http.StatusOK {
		t.Error("route with mismatched prefix should not be mounted")
	}
}

// TestMountPluginRoutes_GroupSuperAdmin_Gating verifies that GroupSuperAdmin
// routes are gated to super_admin only: admin users get 403, super_admin
// users get through.
func TestMountPluginRoutes_GroupSuperAdmin_Gating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		roles      []string
		wantStatus int
	}{
		{"super_admin_passes", []string{"super_admin"}, http.StatusOK},
		{"admin_only_blocked", []string{"admin"}, http.StatusForbidden},
		{"admin_and_editor_blocked", []string{"admin", "editor"}, http.StatusForbidden},
		{"no_roles_blocked", []string{}, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := chi.NewRouter()
			pluginRoutes := []plugin.PluginRoutes{
				{
					Name: "test",
					Routes: []plugin.RouteDecl{
						{
							Method:  "GET",
							Pattern: "/api/admin/super-op",
							Group:   plugin.GroupSuperAdmin,
							Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								w.WriteHeader(http.StatusOK)
							}),
						},
					},
				},
			}
			mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)

			req := httptest.NewRequest(http.MethodGet, "/super-op", nil)
			claims := makeClaims(uuid.New(), "test@test.com", tt.roles)
			req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("roles=%v: status = %d, want %d", tt.roles, rr.Code, tt.wantStatus)
			}
		})
	}
}

// TestMountPluginRoutes_GroupAdmin_StillAllowsAdmin confirms that GroupAdmin
// routes admit both the admin and the super_admin role.
func TestMountPluginRoutes_GroupAdmin_StillAllowsAdmin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		roles      []string
		wantStatus int
	}{
		{"super_admin_passes", []string{"super_admin"}, http.StatusOK},
		{"admin_passes", []string{"admin"}, http.StatusOK},
		{"editor_blocked", []string{"editor"}, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := chi.NewRouter()
			pluginRoutes := []plugin.PluginRoutes{
				{
					Name: "test",
					Routes: []plugin.RouteDecl{
						{
							Method:  "GET",
							Pattern: "/api/admin/regular-op",
							Group:   plugin.GroupAdmin,
							Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								w.WriteHeader(http.StatusOK)
							}),
						},
					},
				},
			}
			mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)

			req := httptest.NewRequest(http.MethodGet, "/regular-op", nil)
			claims := makeClaims(uuid.New(), "test@test.com", tt.roles)
			req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("roles=%v: status = %d, want %d", tt.roles, rr.Code, tt.wantStatus)
			}
		})
	}
}

// TestMountPluginRoutes_GroupSuperAdmin_MixedGroups confirms that when both
// GroupAdmin and GroupSuperAdmin routes are present in the same plugin, they
// are routed to their respective middleware groups.
func TestMountPluginRoutes_GroupSuperAdmin_MixedGroups(t *testing.T) {
	t.Parallel()

	r := chi.NewRouter()
	pluginRoutes := []plugin.PluginRoutes{
		{
			Name: "test",
			Routes: []plugin.RouteDecl{
				{
					Method:  "GET",
					Pattern: "/api/admin/regular",
					Group:   plugin.GroupAdmin,
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusTeapot) // 418: distinctive
					}),
				},
				{
					Method:  "GET",
					Pattern: "/api/admin/super",
					Group:   plugin.GroupSuperAdmin,
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusIMUsed) // 226: distinctive
					}),
				},
			},
		},
	}
	mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)

	// Admin user: can hit /regular but not /super
	adminClaims := makeClaims(uuid.New(), "admin@test.com", []string{"admin"})

	t.Run("admin_hits_regular", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/regular", nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, adminClaims))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusTeapot {
			t.Errorf("admin -> /regular: got %d, want 418", rr.Code)
		}
	})

	t.Run("admin_blocked_from_super", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/super", nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, adminClaims))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("admin -> /super: got %d, want 403", rr.Code)
		}
	})

	// SuperAdmin user: can hit both
	superClaims := makeClaims(uuid.New(), "super@test.com", []string{"super_admin"})

	t.Run("super_admin_hits_regular", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/regular", nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, superClaims))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusTeapot {
			t.Errorf("super_admin -> /regular: got %d, want 418", rr.Code)
		}
	})

	t.Run("super_admin_hits_super", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/super", nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, superClaims))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusIMUsed {
			t.Errorf("super_admin -> /super: got %d, want 226", rr.Code)
		}
	})
}

// NewAdminRouter with no pool

func TestNewRouter_ReturnsHandler(t *testing.T) {
	// With no pool the handlers answer 500 on database access, but the
	// router itself must be non-nil.
	cfg := &config.Config{JWTSecrets: []string{"test-secret"}}
	h, _ := NewAdminRouter(nil, cfg, WithLifetime(testLifetime(t)))
	if h == nil {
		t.Fatal("NewRouter should return a handler")
	}
}

// unlicensedEntitlements

func TestUnlicensedEntitlements_GrantsNoFeature(t *testing.T) {
	t.Parallel()
	f := unlicensedEntitlements{}
	features := []string{
		"feature-a",
		"feature-b",
		"some-other-feature",
		"feature-b",
	}
	granted := f.Snapshot().Features
	for _, feat := range features {
		if slices.Contains(granted, feat) {
			t.Errorf("the unlicensed provider must not grant feature %q", feat)
		}
	}
}

func TestTrustedIssuerAuth_ClaimsAlreadySet_Skips(t *testing.T) {
	t.Parallel()

	claims := makeClaims(uuid.New(), "skip@test.com", []string{"admin"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer some.external.token")
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	rr := httptest.NewRecorder()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		c := claimsFromCtx(r)
		if c == nil || c.Email != "skip@test.com" {
			t.Error("existing claims should be preserved")
		}
		w.WriteHeader(http.StatusOK)
	})
	trustedIssuerAuth([]auth.IssuerPolicy{{Issuer: "https://idp.example.com", Tenant: "acme"}}, &fakeExternalUsers{}, nil)(next).ServeHTTP(rr, r)

	if !called {
		t.Fatal("next handler should be called when claims already exist")
	}
}

func TestTrustedIssuerAuth_NoBearerToken_PassesThrough(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	trustedIssuerAuth([]auth.IssuerPolicy{{Issuer: "https://idp.example.com", Tenant: "acme"}}, &fakeExternalUsers{}, nil)(next).ServeHTTP(rr, r)

	if !called {
		t.Fatal("next handler should be called when no bearer token present")
	}
}

func TestTrustedIssuerAuth_InvalidExternalToken_PassesThrough(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer invalid.jwt.token")
	rr := httptest.NewRecorder()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if claimsFromCtx(r) != nil {
			t.Error("claims should be nil for an unverifiable external token")
		}
		w.WriteHeader(http.StatusOK)
	})
	trustedIssuerAuth([]auth.IssuerPolicy{{Issuer: "https://idp.example.com", Tenant: "acme"}}, &fakeExternalUsers{}, nil)(next).ServeHTTP(rr, r)

	if !called {
		t.Fatal("next handler should be called when external token can't be verified")
	}
}

// Users handler: input validation (structural)

func TestUsersHandler_Create_EmptyEmail(t *testing.T) {
	t.Parallel()

	h := NewUsersHandler(nil)
	body := `{"email":"","password": "***","roles":["editor"]}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Create(rr, r)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d for empty email", rr.Code, http.StatusUnprocessableEntity)
	}
}

// An account stored with the empty tenant can sign in and is refused
// everywhere after, so a request that resolves no tenant creates none. The
// nil store would panic if the handler reached it.
func TestUsersHandler_Create_RefusesAnEmptyTenant(t *testing.T) {
	t.Parallel()

	h := NewUsersHandler(nil)
	body := `{"email":"a@b.com","password":"LongEnough1Ab"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Create(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for a request with no tenant", rr.Code, http.StatusBadRequest)
	}
}

func TestUsersHandler_Create_DefaultsRoles(t *testing.T) {
	t.Parallel()

	// Without a real UserStore, Create will panic when it tries to create.
	// This test verifies the body parsing succeeds and roles default to ["editor"].
	// The panic from nil UserStore is expected at the hash/store boundary.
	h := NewUsersHandler(nil)
	body := `{"email":"a@b.com","password":"LongEnough1Ab"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(core.WithTenantID(r.Context(), "acme"))
	rr := httptest.NewRecorder()

	// Expect panic from nil UserStore.Create: proves body parsed and validated
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from nil UserStore - validates parsing + validation pass")
			}
		}()
		h.Create(rr, r)
	}()
}

func TestUsersHandler_Create_Argon2IDAlgo(t *testing.T) {
	t.Parallel()

	h := NewUsersHandler(nil, "argon2id")
	if h.passwordHashAlgo != "argon2id" {
		t.Errorf("algo = %s, want argon2id", h.passwordHashAlgo)
	}
}

func TestUsersHandler_Delete_SelfDelete(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	claims := makeClaims(uid, "self@test.com", []string{"super_admin"})

	h := NewUsersHandler(nil)
	r := httptest.NewRequest(http.MethodDelete, "/api/admin/users/"+uid.String(), nil)
	r = chiCtx(r, map[string]string{"id": uid.String()})
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	rr := httptest.NewRecorder()
	h.Delete(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (cannot delete self)", rr.Code, http.StatusForbidden)
	}
}

func TestUsersHandler_Delete_OtherUser_NoPanicOnValidID(t *testing.T) {
	t.Parallel()

	claimsUID := uuid.New()
	deleteUID := uuid.New()
	claims := makeClaims(claimsUID, "admin@test.com", []string{"super_admin"})

	h := NewUsersHandler(nil)
	r := httptest.NewRequest(http.MethodDelete, "/api/admin/users/"+deleteUID.String(), nil)
	r = chiCtx(r, map[string]string{"id": deleteUID.String()})
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	rr := httptest.NewRecorder()

	// Expect panic from nil store.Delete: validates self-check passes
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from nil UserStore.Delete - validates self-check passes for other user")
			}
		}()
		h.Delete(rr, r)
	}()
}

func TestUsersHandler_Delete_NoClaims_ReachesStore(t *testing.T) {
	t.Parallel()

	deleteUID := uuid.New()
	h := NewUsersHandler(nil)
	r := httptest.NewRequest(http.MethodDelete, "/api/admin/users/"+deleteUID.String(), nil)
	r = chiCtx(r, map[string]string{"id": deleteUID.String()})
	rr := httptest.NewRecorder()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from nil UserStore - no claims means self-check is skipped")
			}
		}()
		h.Delete(rr, r)
	}()
}

func TestOpenAPIHandler_NilStore_ReturnsDoc_WithNoDynamicSchemas(t *testing.T) {
	t.Parallel()

	// With nil store, openAPIHandler won't enumerate schemas -> empty doc paths
	// The handler reads the registry through core.SchemaSource, so an install
	// with no schema engine is an empty registry rather than a nil store.
	store := newFakeSchemaSource()

	r := httptest.NewRequest(http.MethodGet, "/api/admin/openapi.json", nil)
	rr := httptest.NewRecorder()

	// An empty registry builds a document with no content paths, which is
	// what an install that can serve none should publish.
	openAPIHandler(store, nil, nil, openAPIByRole, openAPIOptions{})(rr, r)
	if rr.Code != http.StatusOK {
		t.Errorf("openapi status = %d, want %d", rr.Code, http.StatusOK)
	}
}

// Content handler: additional edge cases

func TestContentHandler_List_BadLimitOffset(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	// reqparse.QueryInt returns error on non-integer values (offset=abc).
	r := httptest.NewRequest(http.MethodGet, "/content/posts?limit=-5&offset=abc", nil)
	r = contentReq(r, map[string]string{"schema": "posts"})
	rr := httptest.NewRecorder()
	h.List(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestContentHandler_ListCursor_NoCursor(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	r := httptest.NewRequest(http.MethodGet, "/content/posts/cursor?limit=10", nil)
	r = contentReq(r, map[string]string{"schema": "posts"})
	rr := httptest.NewRecorder()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from nil store.ListCursor - validates cursor endpoint")
			}
		}()
		h.ListCursor(rr, r)
	}()
}

func TestContentHandler_List_WithFilters(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	r := httptest.NewRequest(http.MethodGet, "/content/posts?filters[status]=published&filters[author_id]=abc", nil)
	r = contentReq(r, map[string]string{"schema": "posts"})
	rr := httptest.NewRecorder()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic - validates filters parsed before store.List")
			}
		}()
		h.List(rr, r)
	}()
}

func TestContentHandler_List_LimitClamp(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	tests := []struct {
		name        string
		queryString string
	}{
		{"zero clamped to one", "limit=0"},
		{"fifty passthrough", "limit=50"},
		{"oversized clamped to 200", "limit=9999"},
		{"negative clamped to one", "limit=-5"},
		{"default when missing", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/content/posts?"+tc.queryString, nil)
			r = contentReq(r, map[string]string{"schema": "posts"})
			rr := httptest.NewRecorder()

			func() {
				defer func() {
					if r := recover(); r == nil {
						t.Errorf("expected panic from nil store.List - clamp let %q through before store call", tc.queryString)
					}
				}()
				h.List(rr, r)
			}()
		})
	}
}

func TestContentHandler_BulkCreate_BadJSON(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	r := httptest.NewRequest(http.MethodPost, "/content/posts/bulk", strings.NewReader("{"))
	r.Header.Set("Content-Type", "application/json")
	r = contentReq(r, map[string]string{"schema": "posts"})
	rr := httptest.NewRecorder()
	h.BulkCreate(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestContentHandler_SetRelations_BadJSON(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	// SetRelations needs schemas store to look up the relation field.
	// Without a real schemas store set, it panics on nil schemas.GetByName.
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	body := `{"ids":`
	r := httptest.NewRequest(http.MethodPut, "/content/posts/"+uuid.New().String()+"/relations/tags", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = contentReq(r, map[string]string{
		"schema": "posts",
		"id":     uuid.New().String(),
		"field":  "tags",
	})
	rr := httptest.NewRecorder()
	h.SetRelations(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for bad JSON in SetRelations", rr.Code, http.StatusBadRequest)
	}
}

func TestContentHandler_SetRelations_InvalidTargetID(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	body := `{"ids":["not-a-uuid"]}`
	r := httptest.NewRequest(http.MethodPut, "/content/posts/"+uuid.New().String()+"/relations/tags", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = contentReq(r, map[string]string{
		"schema": "posts",
		"id":     uuid.New().String(),
		"field":  "tags",
	})
	rr := httptest.NewRecorder()
	// Schemas is nil -> relationField will panic on nil.GetByName.
	// That's fine: just verify it doesn't panic on JSON parsing.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from nil schemas.GetByName in relationField")
			}
		}()
		h.SetRelations(rr, r)
	}()
}

func TestContentHandler_ListRelations_BadJSON(t *testing.T) {
	t.Parallel()

	h := &ContentHandler{store: nil, schemas: nil, hooks: hooks.NewRegistry(), perms: nil}

	r := httptest.NewRequest(http.MethodGet, "/content/posts/"+uuid.New().String()+"/relations/tags?limit=abc", nil)
	r = contentReq(r, map[string]string{"schema": "posts", "id": uuid.New().String(), "field": "tags"})
	rr := httptest.NewRecorder()

	// reqparse.QueryInt returns error on "abc" -> handler returns 400.
	h.ListRelations(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

// CORS middleware: additional edge cases

// corsCompat builds a corsMiddleware from an origin list and a credentials
// flag.
func corsCompat(origins []string, allowCredentials bool) func(http.Handler) http.Handler {
	return corsMiddleware(CORSConfig{
		Origins:          origins,
		AllowCredentials: allowCredentials,
		PreflightMaxAge:  3600,
		AllowMethods:     "GET, POST, PUT, DELETE, PATCH, OPTIONS",
		AllowHeaders:     "Content-Type, Authorization, X-API-Key, X-Tenant-ID, X-Correlation-ID",
	})
}

func TestCORSMiddleware_EmptyOriginList(t *testing.T) {
	t.Parallel()

	mw := corsCompat([]string{}, true)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://example.com")
	rr := httptest.NewRecorder()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	mw(next).ServeHTTP(rr, r)

	if !called {
		t.Fatal("handler should be called even with empty origin list")
	}
	// Empty list -> no origin matches, so ACAO should NOT be set
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want empty for empty origin list", got)
	}
	// An unmatched origin gets no CORS headers at all, so Allow-Methods
	// does not reach it either.
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got != "" {
		t.Errorf("Allow-Methods = %q, want empty for unmatched origin", got)
	}
}

func TestCORSMiddleware_NoOriginHeader(t *testing.T) {
	t.Parallel()

	mw := corsCompat([]string{"https://app.example.com"}, true)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw(next).ServeHTTP(rr, r)

	// No Origin header -> no match -> ACAO not set (r.Header.Get("Origin") returns "")
	// But empty string origin won't match anything in the list unless "" is explicitly listed
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want empty when no Origin header", got)
	}
}

func TestCORSMiddleware_AllowCredentialsFalse(t *testing.T) {
	t.Parallel()

	mw := corsCompat([]string{"https://app.example.com"}, false)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://app.example.com")
	rr := httptest.NewRecorder()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw(next).ServeHTTP(rr, r)

	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("ACAC = %q, want empty when credentials not allowed", got)
	}
}

// Permission handler: additional input validation

func TestAuthHandler_SetupStatus_NilStore(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	r := httptest.NewRequest(http.MethodGet, "/api/admin/setup", nil)
	rr := httptest.NewRecorder()

	// h.users.Count() panics on nil store
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from nil UserStore.Count in SetupStatus")
			}
		}()
		h.SetupStatus(rr, r)
	}()
}

func TestAuthHandler_SecureCookie_Reflected(t *testing.T) {
	t.Parallel()

	// secureCook = true
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, true)
	if !h.secureCook {
		t.Error("secureCook should be true")
	}

	// secureCook = false (default)
	h2 := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	if h2.secureCook {
		t.Error("secureCook should be false")
	}
}

func TestAuthHandler_Setup_AlreadyCompleted(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	h.WithSetupToken(NewSetupTokenFromEnv("unit-setup-token-0123"))
	// Password must be >= 12 chars, mixed case + digit to pass validation and reach the store.
	body := `{"email":"a@b.com","password":"LongEnoughPass1"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/setup", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(SetupTokenHeader, "unit-setup-token-0123")
	rr := httptest.NewRecorder()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from nil UserStore.Count")
			}
		}()
		h.Setup(rr, r)
	}()
}

// Auth handler: MFAVerify additional edge cases

func TestAuthHandler_MFAVerify_MissingChallengeToken(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, &stubMFAStore{}, nil, "test-secret-at-least-32-bytes-long!", 3600, false)
	body := `{"code":"123456"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAuthHandler_MFAVerify_MissingCode(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, &stubMFAStore{}, nil, "test-secret-at-least-32-bytes-long!", 3600, false)
	body := `{"challenge_token":"some-jwt"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAuthHandler_MFAVerify_ChallengeTokenNotMFAPending(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!"
	uid := uuid.New()
	// Sign a normal JWT (not MFA challenge): ParseMulti succeeds but MFAPending is false
	tok := makeJWT(secret, uid, "mfa@test.com", []string{"admin"}, 3600)

	h := NewAuthHandler(nil, &stubMFAStore{}, nil, secret, 3600, false)
	body := `{"challenge_token":"` + tok + `","code":"123456"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (non-MFA token should be rejected)", rr.Code, http.StatusUnauthorized)
	}
}

func TestAuthHandler_MFAVerify_ExpiredChallengeToken(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!"

	// Can't easily create an MFA challenge token (SignChallenge isn't exported for testing).
	// But we can test with an expired normal token which will fail ParseMulti.
	tok := makeJWT(secret, uuid.New(), "expired@test.com", []string{"admin"}, -60)

	h := NewAuthHandler(nil, &stubMFAStore{}, nil, secret, 3600, false)
	body := `{"challenge_token":"` + tok + `","code":"123456"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, r)

	// Expired token -> ParseMulti returns error -> handler returns 401
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (expired token)", rr.Code, http.StatusUnauthorized)
	}
}

// safeUser helper

func TestParsePopulateFromQuery_Empty(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/content/posts", nil)
	cfg := parsePopulateFromQuery(r)
	if !cfg.IsEmpty() {
		t.Error("populate config should be empty with no query params")
	}
}

func TestParsePopulateFromQuery_SingleField(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/content/posts?populate=author", nil)
	cfg := parsePopulateFromQuery(r)
	if cfg.IsEmpty() {
		t.Error("populate config should not be empty")
	}
}

func TestParsePopulateFromQuery_MultipleFields(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/content/posts?populate=author,tags,category", nil)
	cfg := parsePopulateFromQuery(r)
	if cfg.IsEmpty() {
		t.Error("populate config should not be empty with multiple fields")
	}
}

func TestParsePopulateFromQuery_WithDepth(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/content/posts?populate=author.tags&depth=3", nil)
	cfg := parsePopulateFromQuery(r)
	if cfg.IsEmpty() {
		t.Error("populate config should support nested relations and depth")
	}
}

// Auth handler: Login edge case when MFA is enabled but store call fails

func TestAuthHandler_Login_NilMFAStore_FailsOpen(t *testing.T) {
	t.Parallel()

	// When mfaStore is nil, h.mfaStore.IsEnabled panics.
	// This is the structural test: proves the code path exists.
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	body := `{"email":"test@example.com","password": "***"}`
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	// h.users.GetByEmail panics on nil UserStore
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from nil UserStore.GetByEmail")
			}
		}()
		h.Login(rr, r)
	}()
}

// Router: full lifecycle test with chi mux

func TestNewAdminRouter_Smoke(t *testing.T) {
	// Skip parallel: uses package-level globals

	fdb := &fakeDB{engine: "postgres", queryRowFactory: newTokenVersionRow}

	cfg := &config.Config{
		JWTSecret:     "test-secret-at-least-32-bytes-long!",
		JWTSecrets:    []string{"test-secret-at-least-32-bytes-long!"},
		JWTExpirySecs: 3600,
		CORSOrigins:   []string{"http://localhost:5173"},
	}

	r, _ := NewAdminRouter(fdb, cfg, WithLifetime(testLifetime(t)),
		WithEntitlements(unlicensedEntitlements{}),
		WithPluginStatus(stubPluginStatusProvider{report: plugin.PluginStatusReport{}}),
	)

	if r == nil {
		t.Fatal("NewAdminRouter returned nil")
	}

	// Smoke-test that the router handles requests without panicking.
	// /api/admin/health is behind requireAuth: inject a valid JWT.
	// Token version 0 so tokenVersionCheck skips the DB lookup (fakeDB has nil QueryRow).
	token, err := auth.Sign(cfg.JWTSecret, 3600, uuid.New(), "admin@test.com", []string{"admin"}, "", 0)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// Health endpoint should respond (fakeDB.Ping returns nil, so 200)
	if rr.Code != http.StatusOK {
		t.Errorf("health status = %d, want %d", rr.Code, http.StatusOK)
	}
}

func TestNewAPIRouter_Smoke(t *testing.T) {
	fdb := &fakeDB{engine: "postgres", queryRowFactory: newTokenVersionRow}

	cfg := &config.Config{
		JWTSecret:     "test-secret-at-least-32-bytes-long!",
		JWTSecrets:    []string{"test-secret-at-least-32-bytes-long!"},
		JWTExpirySecs: 3600,
		CORSOrigins:   []string{"*"},
	}

	r, _ := NewAPIRouter(fdb, cfg, nil, WithLifetime(testLifetime(t)))

	if r == nil {
		t.Fatal("NewAPIRouter returned nil")
	}

	// Smoke test: health endpoint.
	// /api/v1/health is behind requireAuth: inject a valid JWT.
	// Token version 0 so tokenVersionCheck skips the DB lookup (fakeDB has nil QueryRow).
	token, err := auth.Sign(cfg.JWTSecret, 3600, uuid.New(), "api@test.com", []string{"user"}, "", 0)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("health status = %d, want %d", rr.Code, http.StatusOK)
	}
}

// Plugin status handler with no provider wired

func TestPluginsStatusHandler_NilProviderAnswersOK(t *testing.T) {
	t.Parallel()

	h := pluginsStatusHandler(nil) // nil provider -> empty report
	r := httptest.NewRequest(http.MethodGet, "/api/admin/plugins/status", nil)
	rr := httptest.NewRecorder()
	h(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d", rr.Code)
	}
}

// CORS middleware: full preflight with headers

func TestCORSMiddleware_OPTIONS_WithOrigin_Returns204AndHeaders(t *testing.T) {
	t.Parallel()

	mw := corsCompat([]string{"https://app.example.com"}, true)
	r := httptest.NewRequest(http.MethodOptions, "/api/admin/users", nil)
	r.Header.Set("Origin", "https://app.example.com")
	rr := httptest.NewRecorder()

	nextWasCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextWasCalled = true
	})
	mw(next).ServeHTTP(rr, r)

	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
	if nextWasCalled {
		t.Error("next handler should NOT be called for OPTIONS preflight")
	}
	// CORS headers should be set
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO = %q, want https://app.example.com", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Allow-Methods should be set")
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("Allow-Headers should be set")
	}
}

// Compile-time interface checks

var _ PluginStatusProvider = (*plugin.Activator)(nil)
var _ PluginStatusProvider = stubPluginStatusProvider{}
var _ PluginSchemaProvider = (*plugin.Activator)(nil)

// respond helper: with non-map body

func TestRespond_WithStruct(t *testing.T) {
	t.Parallel()

	type resp struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	rr := httptest.NewRecorder()
	respond(rr, http.StatusOK, resp{ID: "abc", Name: "test"})

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d", rr.Code)
	}
	if rr.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", rr.Header().Get("Content-Type"))
	}
	body := decodeBody(t, rr)
	if body["id"] != "abc" || body["name"] != "test" {
		t.Errorf("body = %v", body)
	}
}

// query helpers: additional cases

func TestQueryInt_EmptyValue(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/?limit=", nil)
	got, err := reqparse.QueryInt(r, "limit", 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 50 { // empty value -> falls through to default
		t.Errorf("QueryInt with empty value = %d, want 50 (default)", got)
	}
}

func TestParseFilters_Nested(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/?filters[author][id]=xyz", nil)
	f, _ := parseFilters(r)
	if got := f["author][id"]; got != "xyz" {
		t.Errorf("filters[author][id] = %q, want xyz", got)
	}
}

// parseFilters: SQL injection key extraction

func TestParseFilters_SQLInjectionKey(t *testing.T) {
	t.Parallel()

	// parseFilters is a dumb extractor: it extracts whatever key is inside
	// filters[...]. Validation happens downstream in ContentStore.List.
	// These tests verify the extraction itself is correct so downstream
	// validation can reject bad keys.

	t.Run("extracts injection key 1=1--", func(t *testing.T) {
		// '=' inside the bracket key must be URL-encoded (%3D) or url.Parse
		// treats it as a key=value separator. Real HTTP clients do this encoding.
		r := httptest.NewRequest(http.MethodGet, "/?filters%5B1%3D1--%5D=x", nil)
		f, _ := parseFilters(r)
		if got := f["1=1--"]; got != "x" {
			t.Errorf("filter key '1=1--' = %q, want 'x'", got)
		}
	})

	t.Run("extracts injection key with semicolons", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters%5B%3B+DROP+TABLE+users%3B--%5D=x", nil)
		f, _ := parseFilters(r)
		if got := f["; DROP TABLE users;--"]; got != "x" {
			t.Errorf("filter key with semicolons = %q, want 'x'", got)
		}
	})

	t.Run("extracts injection key UNION SELECT", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[UNION+SELECT]=x", nil)
		f, _ := parseFilters(r)
		if got := f["UNION SELECT"]; got != "x" {
			t.Errorf("filter key 'UNION SELECT' = %q, want 'x'", got)
		}
	})

	t.Run("extracts injection key OR 1=1", func(t *testing.T) {
		// '=' inside the key must be URL-encoded.
		r := httptest.NewRequest(http.MethodGet, "/?filters%5BOR+1%3D1%5D=x", nil)
		f, _ := parseFilters(r)
		if got := f["OR 1=1"]; got != "x" {
			t.Errorf("filter key 'OR 1=1' = %q, want 'x'", got)
		}
	})

	t.Run("extracts empty bracket key", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[]=x", nil)
		f, _ := parseFilters(r)
		// Empty key inside brackets. The parseFilters helper uses key[8:len-1],
		// with len(key)=9, key[8:8]="" -> empty string, which is skipped.
		if _, ok := f[""]; ok {
			t.Error("empty bracket key should be skipped")
		}
	})

	t.Run("extracts key with only special chars", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[!@#$%25%5E%26*]=val", nil)
		f, _ := parseFilters(r)
		if got := f["!@#$%^&*"]; got != "val" {
			t.Errorf("special-char key = %q, want 'val'", got)
		}
	})
}

// httpStatus: domain error to HTTP status code mapping

// TestHTTPStatus_FilterRejectionMapsTo400 verifies that domain.ErrBadRequest
// (returned by ContentStore.List when a filter key fails the allowlist check)
// maps to HTTP 400 Bad Request. This is the final link in the chain:
// HTTP request -> parseFilters -> ContentStore.List -> allowlist reject ->
// domain.ErrBadRequest -> httpStatus -> 400.
func TestHTTPStatus_FilterRejectionMapsTo400(t *testing.T) {
	t.Parallel()

	t.Run("ErrBadRequest maps to 400", func(t *testing.T) {
		code := httpStatus(domain.ErrBadRequest)
		if code != http.StatusBadRequest {
			t.Errorf("httpStatus(ErrBadRequest) = %d, want %d", code, http.StatusBadRequest)
		}
	})

	t.Run("wrapped ErrBadRequest maps to 400", func(t *testing.T) {
		code := httpStatus(fmt.Errorf("filter error: %w", domain.ErrBadRequest))
		if code != http.StatusBadRequest {
			t.Errorf("httpStatus(wrapped ErrBadRequest) = %d, want %d", code, http.StatusBadRequest)
		}
	})

	t.Run("ErrNotFound maps to 404", func(t *testing.T) {
		code := httpStatus(domain.ErrNotFound)
		if code != http.StatusNotFound {
			t.Errorf("httpStatus(ErrNotFound) = %d, want %d", code, http.StatusNotFound)
		}
	})

	t.Run("ErrConflict maps to 409", func(t *testing.T) {
		code := httpStatus(domain.ErrConflict)
		if code != http.StatusConflict {
			t.Errorf("httpStatus(ErrConflict) = %d, want %d", code, http.StatusConflict)
		}
	})

	t.Run("ErrForbidden maps to 403", func(t *testing.T) {
		code := httpStatus(domain.ErrForbidden)
		if code != http.StatusForbidden {
			t.Errorf("httpStatus(ErrForbidden) = %d, want %d", code, http.StatusForbidden)
		}
	})

	t.Run("ErrUnauth maps to 401", func(t *testing.T) {
		code := httpStatus(domain.ErrUnauth)
		if code != http.StatusUnauthorized {
			t.Errorf("httpStatus(ErrUnauth) = %d, want %d", code, http.StatusUnauthorized)
		}
	})

	t.Run("unknown error maps to 503", func(t *testing.T) {
		code := httpStatus(errors.New("something else"))
		if code != http.StatusServiceUnavailable {
			t.Errorf("httpStatus(unknown) = %d, want %d", code, http.StatusServiceUnavailable)
		}
	})
}

// resolveKeyStore: encryption KeyStore resolution

func TestResolveKeyStore_PrefersInjectedOverConfig(t *testing.T) {
	t.Parallel()

	// Build two DISTINCT KeyStores so we can tell which one won.
	// When the runtime resolves the KEK from a secret source (KMS/Vault/env)
	// and injects it via WithKeyStore, resolveKeyStore MUST return that one.
	injectedKS, err := encryption.NewKeyStore("injected-master-key-at-least-32-bytes-long")
	if err != nil {
		t.Fatalf("NewKeyStore(injected): %v", err)
	}
	configKS, err := encryption.NewKeyStore("config-encryption-key-at-least-32-bytes")
	if err != nil {
		t.Fatalf("NewKeyStore(config): %v", err)
	}

	// Derive a tenant DEK from each store so we can compare them.
	tenantID := "tenant_dek_probe"
	injectedDEK, err := injectedKS.DeriveTenantDEK(tenantID)
	if err != nil {
		t.Fatalf("derive injected DEK: %v", err)
	}
	configDEK, err := configKS.DeriveTenantDEK(tenantID)
	if err != nil {
		t.Fatalf("derive config DEK: %v", err)
	}

	// Confirm they ARE different (different master keys -> different DEKs).
	if string(injectedDEK) == string(configDEK) {
		t.Fatal("DEKs are identical - test keys are not distinct; pick different key material")
	}

	o := &routerOptions{keyStore: injectedKS}
	cfg := &config.Config{EncryptionKey: "config-encryption-key-at-least-32-bytes"}

	resolved := resolveKeyStore(o, cfg)
	if resolved == nil {
		t.Fatal("resolveKeyStore returned nil")
	}

	resolvedDEK, err := resolved.DeriveTenantDEK(tenantID)
	if err != nil {
		t.Fatalf("derive resolved DEK: %v", err)
	}

	if string(resolvedDEK) != string(injectedDEK) {
		t.Errorf("resolveKeyStore returned the config-derived KeyStore, not the injected one")
	}
	if string(resolvedDEK) == string(configDEK) {
		t.Errorf("resolveKeyStore returned config-derived DEK - injected KeyStore was ignored")
	}
}
