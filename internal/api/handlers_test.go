package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/internal/metrics"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// Test DB fake: satisfies db.DB for handler tests

type fakeDB struct {
	pingErr   error
	stats     sql.DBStats
	engine    string
	queryRows *sql.Rows
	queryRow  *sql.Row
	// queryRowFactory, when set, supplies a fresh *sql.Row per QueryRow call
	// (a single *sql.Row can only be scanned once).
	queryRowFactory func() *sql.Row
	beginTx         *sql.Tx
	beginErr        error
	connVal         *sql.Conn
	connErr         error
}

// QueryRow answers a row or an error, never neither. Callers check the error
// and then Scan, so handing back a nil row with a nil error panics inside
// database/sql on whichever path the test did not mean to reach. A fake with
// nothing configured for the query is a fake that cannot serve it, and a
// closed pool says exactly that.
func (f *fakeDB) QueryRow(ctx context.Context, q string, args ...any) (*sql.Row, error) {
	if f.queryRowFactory != nil {
		return f.queryRowFactory(), nil
	}
	if f.queryRow == nil {
		return getClosedDB().QueryRowContext(ctx, q, args...), nil
	}
	return f.queryRow, nil
}
func (f *fakeDB) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if f.queryRows == nil {
		return nil, sql.ErrConnDone
	}
	return f.queryRows, nil
}
func (f *fakeDB) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return nil, nil
}
func (f *fakeDB) Begin(ctx context.Context) (*sql.Tx, error) {
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return f.beginTx, nil
}
func (f *fakeDB) Conn(ctx context.Context) (*sql.Conn, error) {
	if f.connErr != nil {
		return nil, f.connErr
	}
	return f.connVal, nil
}
func (f *fakeDB) Ping(ctx context.Context) error                            { return f.pingErr }
func (f *fakeDB) Close() error                                              { return nil }
func (f *fakeDB) Stats() sql.DBStats                                        { return f.stats }
func (f *fakeDB) Engine() string                                            { return f.engine }
func (f *fakeDB) SQLDB() *sql.DB                                            { return nil }
func (f *fakeDB) QuerierRO(ctx context.Context) (db.ReadOnlyQuerier, error) { return f, nil }

var _ db.DB = (*fakeDB)(nil)

// Helpers

// makeJWT creates a valid HS256-signed JWT for testing.
func makeJWT(secret string, userID uuid.UUID, email string, roles []string, expirySecs int64) string {
	tok, err := auth.Sign(secret, expirySecs, userID, email, roles, "", 1)
	if err != nil {
		panic("makeJWT: " + err.Error())
	}
	return tok
}

func makeClaims(userID uuid.UUID, email string, roles []string) *auth.Claims {
	return &auth.Claims{
		UserID: userID.String(),
		Email:  email,
		Roles:  roles,
	}
}

// chiCtx wraps a request with chi URL params for handler tests.
func chiCtx(r *http.Request, params map[string]string) *http.Request {
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
	return r
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body: %v (raw=%q)", err, rr.Body.String())
	}
	return m
}

// Auth Middleware Tests

func TestBearerToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		auth string
		want string
	}{
		{"valid bearer", "Bearer my.secret.token", "my.secret.token"},
		{"no prefix", "Basic dXNlcjpwYXNz", ""},
		{"empty", "", ""},
		{"bearer lowercase", "bearer mytoken", ""},
		{"no space", "Bearermytoken", ""},
		{"extra whitespace", "Bearer    token", "   token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.auth != "" {
				r.Header.Set("Authorization", tt.auth)
			}
			got := bearerToken(r)
			if got != tt.want {
				t.Errorf("bearerToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestJwtAuth_ValidTokenInjectsClaims verifies a valid Bearer token is parsed
// and claims are injected into the request context.
func TestJwtAuth_ValidTokenInjectsClaims(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!"
	uid := uuid.New()
	tok := makeJWT(secret, uid, "admin@test.com", []string{"admin"}, 3600)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()

	var capturedClaims *auth.Claims
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedClaims = claimsFromCtx(r)
		w.WriteHeader(http.StatusOK)
	})

	jwtAuth([]string{secret}, false)(next).ServeHTTP(rr, r)

	if capturedClaims == nil {
		t.Fatal("expected claims in context, got nil")
	}
	if capturedClaims.UserID != uid.String() {
		t.Errorf("UserID = %q, want %q", capturedClaims.UserID, uid.String())
	}
	if capturedClaims.Email != "admin@test.com" {
		t.Errorf("Email = %q, want admin@test.com", capturedClaims.Email)
	}
}

// TestJwtAuth_InvalidTokenPassesThrough verifies that an invalid token does NOT
// inject claims: the request passes through to the next handler without auth.
func TestJwtAuth_InvalidTokenPassesThrough(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!"
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer garbage.token.here")
	rr := httptest.NewRecorder()

	var capturedClaims *auth.Claims
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedClaims = claimsFromCtx(r)
		w.WriteHeader(http.StatusOK)
	})

	jwtAuth([]string{secret}, false)(next).ServeHTTP(rr, r)

	if capturedClaims != nil {
		t.Errorf("expected nil claims for invalid token, got %+v", capturedClaims)
	}
}

// TestJwtAuth_ExpiredTokenPassesThrough verifies expired tokens don't inject claims.
func TestJwtAuth_ExpiredTokenPassesThrough(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!"
	uid := uuid.New()
	// Negative expiry = already expired
	tok := makeJWT(secret, uid, "expired@test.com", []string{"admin"}, -60)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()

	var capturedClaims *auth.Claims
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedClaims = claimsFromCtx(r)
		w.WriteHeader(http.StatusOK)
	})

	jwtAuth([]string{secret}, false)(next).ServeHTTP(rr, r)

	if capturedClaims != nil {
		t.Errorf("expected nil claims for expired token, got %+v", capturedClaims)
	}
}

// TestJwtAuth_CookieFallback verifies the middleware reads the token from the
// sys_session cookie when no Bearer header is present.
func TestJwtAuth_CookieFallback(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!"
	uid := uuid.New()
	tok := makeJWT(secret, uid, "cookie@test.com", []string{"editor"}, 3600)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: security.SessionCookieNameInsecure, Value: tok})
	rr := httptest.NewRecorder()

	var capturedClaims *auth.Claims
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedClaims = claimsFromCtx(r)
		w.WriteHeader(http.StatusOK)
	})

	jwtAuth([]string{secret}, false)(next).ServeHTTP(rr, r)

	if capturedClaims == nil {
		t.Fatal("expected claims from cookie, got nil")
	}
}

// TestJwtAuth_NoTokenPassesThrough verifies no token means no claims, no 401.
func TestJwtAuth_NoTokenPassesThrough(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!"
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	var capturedClaims *auth.Claims
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedClaims = claimsFromCtx(r)
		w.WriteHeader(http.StatusOK)
	})

	jwtAuth([]string{secret}, false)(next).ServeHTTP(rr, r)

	if capturedClaims != nil {
		t.Errorf("expected nil claims with no token, got %+v", capturedClaims)
	}
}

// TestJwtAuth_KeyRotation verifies ParseMulti tries all secrets in order,
// so a token signed with an old key still validates.
func TestJwtAuth_KeyRotation(t *testing.T) {
	t.Parallel()

	newSecret := "new-secret-at-least-32-bytes-long!!"
	oldSecret := "old-secret--at-least-32-bytes-long-"
	uid := uuid.New()

	// Sign with the old secret
	tok := makeJWT(oldSecret, uid, "rotate@test.com", []string{"admin"}, 3600)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()

	var capturedClaims *auth.Claims
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedClaims = claimsFromCtx(r)
		w.WriteHeader(http.StatusOK)
	})

	// New secret first, old second
	jwtAuth([]string{newSecret, oldSecret}, false)(next).ServeHTTP(rr, r)

	if capturedClaims == nil {
		t.Fatal("expected claims with key rotation, got nil")
	}
}

// requireAuth middleware

func TestRequireAuth_NoClaims_Returns401(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler should not be called")
	})
	requireAuth(next).ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	body := decodeBody(t, rr)
	if body["error"] != "authentication required" {
		t.Errorf("error = %q", body["error"])
	}
}

func TestRequireAuth_ValidClaims_Proceeds(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	claims := makeClaims(uid, "user@test.com", []string{"editor"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	rr := httptest.NewRecorder()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	requireAuth(next).ServeHTTP(rr, r)

	if !called {
		t.Fatal("next handler was not called")
	}
}

func TestRequireAuth_MFAPending_Returns401(t *testing.T) {
	t.Parallel()

	claims := &auth.Claims{
		UserID:     uuid.New().String(),
		Email:      "mfa@test.com",
		Roles:      []string{"admin"},
		MFAPending: true,
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	rr := httptest.NewRecorder()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler should not be called when MFA is pending")
	})
	requireAuth(next).ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	body := decodeBody(t, rr)
	if body["error"] != "mfa verification required" {
		t.Errorf("error = %q", body["error"])
	}
}

// requireRole middleware

func TestRequireRole_NoClaims_Returns401(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not be called")
	})
	requireRole("admin")(next).ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestRequireRole_HasRole_Proceeds(t *testing.T) {
	t.Parallel()

	claims := makeClaims(uuid.New(), "admin@test.com", []string{"editor", "admin"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	rr := httptest.NewRecorder()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	requireRole("admin")(next).ServeHTTP(rr, r)

	if !called {
		t.Fatal("next handler was not called")
	}
}

func TestRequireRole_MissingRole_Returns403(t *testing.T) {
	t.Parallel()

	claims := makeClaims(uuid.New(), "editor@test.com", []string{"editor"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	rr := httptest.NewRecorder()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not be called")
	})
	requireRole("admin")(next).ServeHTTP(rr, r)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

// CORS middleware

// corsMiddlewareCompat wraps the CORSConfig-based corsMiddleware for tests
// written against an (origins, allowCredentials) signature.
func corsMiddlewareCompat(origins []string, allowCredentials bool) func(http.Handler) http.Handler {
	return corsMiddleware(CORSConfig{
		Origins:          origins,
		AllowCredentials: allowCredentials,
		PreflightMaxAge:  3600,
		AllowMethods:     "GET, POST, PUT, DELETE, PATCH, OPTIONS",
		AllowHeaders:     "Content-Type, Authorization, X-API-Key, X-Tenant-ID, X-Correlation-ID",
	})
}

func TestCORSMiddleware_MatchingOrigin_SetsHeaders(t *testing.T) {
	t.Parallel()

	origins := []string{"https://app.example.com", "https://admin.example.com"}
	mw := corsMiddlewareCompat(origins, true)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://app.example.com")
	rr := httptest.NewRecorder()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw(next).ServeHTTP(rr, r)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO = %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("ACAC = %q, want true", got)
	}
}

func TestCORSMiddleware_NonMatchingOrigin_NoACAC(t *testing.T) {
	t.Parallel()

	mw := corsMiddlewareCompat([]string{"https://app.example.com"}, false)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Origin", "https://evil.com")
	rr := httptest.NewRecorder()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw(next).ServeHTTP(rr, r)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO should be empty for non-matching origin, got %q", got)
	}
}

func TestCORSMiddleware_OPTIONS_Returns204(t *testing.T) {
	t.Parallel()

	mw := corsMiddlewareCompat([]string{"https://app.example.com"}, false)
	r := httptest.NewRequest(http.MethodOptions, "/", nil)
	r.Header.Set("Origin", "https://app.example.com")
	rr := httptest.NewRecorder()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next should not be called for OPTIONS")
	})
	mw(next).ServeHTTP(rr, r)

	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

// Health handlers

func TestHealthHandler_DBUp_Returns200(t *testing.T) {
	t.Parallel()
	db := &fakeDB{engine: "postgres"}
	h := healthHandlerFn(db)

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	h(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := decodeBody(t, rr)
	if body["status"] != "ok" {
		t.Errorf("status field = %q", body["status"])
	}
}

func TestHealthHandler_DBDown_Returns503(t *testing.T) {
	t.Parallel()
	db := &fakeDB{engine: "postgres", pingErr: context.DeadlineExceeded}
	h := healthHandlerFn(db)

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	h(rr, r)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyHandler_DBUp_Returns200(t *testing.T) {
	t.Parallel()
	db := &fakeDB{engine: "postgres"}
	h := readyHandlerFn(db, func() int { return 5 })

	r := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()
	h(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := decodeBody(t, rr)
	if body["status"] != "ready" {
		t.Errorf("status = %q", body["status"])
	}
	if body["schema_count"] != float64(5) {
		t.Errorf("schema_count = %v, want 5", body["schema_count"])
	}
}

func TestMetricsHandler_ReturnsPrometheus(t *testing.T) {
	t.Parallel()

	// Init the Prometheus registry (lazy-init in Handler() does this anyway).
	metrics.Init()
	// Record a request so the metric vector has at least one observation.
	// Prometheus text format hides vectors with zero observations.
	r := httptest.NewRequest(http.MethodGet, "/api/admin/schemas", nil)
	metrics.RecordRequest(r, 200, 0.001)

	h := metrics.Handler()

	mr := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, mr)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "lyeve_requests_total") {
		t.Error("missing lyeve_requests_total metric")
	}
	// DB pool metrics come from the collector, which may not be registered in unit tests.
	// Verify at least that the handler doesn't panic and returns valid Prometheus text.
	_ = body
}

// Entitlements handler

func TestEntitlementsHandler_UnlicensedPlanName(t *testing.T) {
	t.Parallel()

	h := entitlementsHandler(unlicensedEntitlements{}, false)
	r := httptest.NewRequest(http.MethodGet, "/api/admin/entitlements", nil)
	rr := httptest.NewRecorder()
	h(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rr.Header().Get("Cache-Control"))
	}
	body := decodeBody(t, rr)
	if body["plan"] != "free" {
		t.Errorf("plan = %q, want free", body["plan"])
	}
}

// Plugin status handler: additional edge cases

func TestPluginsStatusHandler_ProviderReturnsEmpty(t *testing.T) {
	t.Parallel()

	prov := stubPluginStatusProvider{
		report: plugin.PluginStatusReport{
			Compiled: []string{},
			Entitled: []string{},
			Plugins:  []plugin.PluginStatus{},
		},
	}
	h := pluginsStatusHandler(prov)

	r := httptest.NewRequest(http.MethodGet, "/api/admin/plugins/status", nil)
	rr := httptest.NewRecorder()
	h(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d", rr.Code)
	}
}

// Middleware chain integration: full router with test DB

func TestRespondAndRespondErr(t *testing.T) {
	t.Parallel()

	t.Run("respond", func(t *testing.T) {
		rr := httptest.NewRecorder()
		respond(rr, http.StatusCreated, map[string]string{"id": "123"})

		if rr.Code != http.StatusCreated {
			t.Errorf("status = %d", rr.Code)
		}
		if rr.Header().Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", rr.Header().Get("Content-Type"))
		}
		body := decodeBody(t, rr)
		if body["id"] != "123" {
			t.Errorf("id = %q", body["id"])
		}
	})

	t.Run("respond_nil_body", func(t *testing.T) {
		rr := httptest.NewRecorder()
		respond(rr, http.StatusNoContent, nil)

		if rr.Code != http.StatusNoContent {
			t.Errorf("status = %d", rr.Code)
		}
		if rr.Body.Len() != 0 {
			t.Errorf("body should be empty for nil body, got %q", rr.Body.String())
		}
	})

	t.Run("ErrorReq", func(t *testing.T) {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/test", nil)
		r = r.WithContext(logging.WithRequestID(r.Context(), "req-test-123"))
		httpx.ErrorReq(rr, r, http.StatusBadRequest, "bad input")

		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d", rr.Code)
		}
		body := decodeBody(t, rr)
		if body["error"] != "bad input" {
			t.Errorf("error = %q", body["error"])
		}
		if body["request_id"] != "req-test-123" {
			t.Errorf("request_id = %q, want %q", body["request_id"], "req-test-123")
		}
		if body["code"] != "bad_request" {
			t.Errorf("code = %q, want %q", body["code"], "bad_request")
		}
	})
}

// structuredLogger middleware: smoke test

func TestStructuredLogger_WrapsRequest(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	structuredLogger(next).ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d", rr.Code)
	}
}

// applyRouterOptions defaults

func TestApplyRouterOptions_Defaults(t *testing.T) {
	t.Parallel()

	o := applyRouterOptions(nil)
	if o.entitlements == nil {
		t.Fatal("entitlements must be non-nil (fail-closed)")
	}
	if o.pluginStatus != nil {
		t.Error("pluginStatus should default to nil")
	}

	// Verify with options
	prov := stubPluginStatusProvider{report: plugin.PluginStatusReport{}}
	o2 := applyRouterOptions([]RouterOption{WithPluginStatus(prov)})
	if o2.pluginStatus == nil {
		t.Fatal("pluginStatus should be set by option")
	}
}

func TestApplyRouterOptions_WithMiddleware(t *testing.T) {
	t.Parallel()
	o := applyRouterOptions([]RouterOption{
		WithMiddleware(func(next http.Handler) http.Handler {
			return next
		}),
	})
	if len(o.extra) != 1 {
		t.Errorf("extra middleware count = %d, want 1", len(o.extra))
	}
}

// unlicensedEntitlements

func TestUnlicensedEntitlements_Snapshot(t *testing.T) {
	t.Parallel()

	f := unlicensedEntitlements{}
	snap := f.Snapshot()

	if snap.Plan != "free" {
		t.Errorf("plan = %q, want free", snap.Plan)
	}
	if snap.State != "free" {
		t.Errorf("state = %q", snap.State)
	}
	if len(snap.Features) != 0 {
		t.Errorf("features should be empty with no license, got %v", snap.Features)
	}
}

// logout handler

func TestLogout_ClearsCookie(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/logout", nil)
	rr := httptest.NewRecorder()
	h.Logout(rr, r)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Check Set-Cookie clears the session
	cookies := rr.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == security.SessionCookieNameInsecure {
			found = true
			if c.Value != "" {
				t.Errorf("cookie value should be empty, got %q", c.Value)
			}
			if c.MaxAge != -1 {
				t.Errorf("cookie MaxAge = %d, want -1", c.MaxAge)
			}
		}
	}
	if !found {
		t.Error("Set-Cookie header not found for sys_session")
	}
}

// Me handler

func TestMe_NoClaims_Returns401(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	r := httptest.NewRequest(http.MethodGet, "/api/admin/auth/me", nil)
	rr := httptest.NewRecorder()
	h.Me(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestMe_WithClaims_ReturnsUser(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	claims := makeClaims(uid, "me@test.com", []string{"admin", "editor"})

	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	r := httptest.NewRequest(http.MethodGet, "/api/admin/auth/me", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	rr := httptest.NewRecorder()
	h.Me(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := decodeBody(t, rr)
	if body["id"] != uid.String() {
		t.Errorf("id = %q, want %q", body["id"], uid.String())
	}
	if body["email"] != "me@test.com" {
		t.Errorf("email = %q", body["email"])
	}
}

// claimsFromCtx helper

func TestClaimsFromCtx_Nil(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	c := claimsFromCtx(r)
	if c != nil {
		t.Errorf("expected nil, got %+v", c)
	}
}

func TestClaimsFromCtx_WithClaims(t *testing.T) {
	t.Parallel()
	claims := makeClaims(uuid.New(), "c@t.com", []string{"editor"})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
	c := claimsFromCtx(r)
	if c == nil {
		t.Fatal("expected non-nil claims")
	}
	if c.Email != "c@t.com" {
		t.Errorf("email = %q", c.Email)
	}
}

// ETag helpers (used by content handler)

func TestSetETag_NotModified(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	// Exercises the etag path: etagFor hashes the response body.
	dummy := map[string]any{"id": "test"}
	etag := etagFor(dummy)
	if etag == "" {
		t.Error("etag should not be empty")
	}

	// Set If-None-Match to trigger 304
	r.Header.Set("If-None-Match", etag)
	rr := httptest.NewRecorder()
	modified := setETagAndCheckNotModified(rr, r, etag)
	if !modified {
		t.Error("expected not-modified=true (304) when If-None-Match matches")
	}
	if rr.Code != http.StatusNotModified {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusNotModified)
	}
}

func TestSetETag_NoMatch_Proceeds(t *testing.T) {
	t.Parallel()

	dummy := map[string]any{"id": "test"}
	etag := etagFor(dummy)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("If-None-Match", `"different-etag"`)
	rr := httptest.NewRecorder()
	modified := setETagAndCheckNotModified(rr, r, etag)
	if modified {
		t.Error("expected modified=false when If-None-Match differs")
	}
	if rr.Header().Get("ETag") != etag {
		t.Errorf("ETag header = %q, want %q", rr.Header().Get("ETag"), etag)
	}
}

// query helpers

func TestQueryInt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		query   string
		def     int
		want    int
		wantErr bool
	}{
		{"valid", "limit=50", 25, 50, false},
		{"default", "", 25, 25, false},
		{"invalid", "limit=abc", 25, 0, true},
		{"negative", "limit=-5", 25, -5, false},
		{"zero", "limit=0", 25, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil)
			got, err := reqparse.QueryInt(r, "limit", tt.def)
			if tt.wantErr {
				if err == nil {
					t.Errorf("QueryInt(limit) expected error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Errorf("QueryInt(limit) unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("QueryInt(limit) = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseFilters(t *testing.T) {
	t.Parallel()

	t.Run("single", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[author_id]=abc-123", nil)
		f, _ := parseFilters(r)
		if f["author_id"] != "abc-123" {
			t.Errorf("filters[author_id] = %q", f["author_id"])
		}
	})
	t.Run("multiple", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[status]=draft&filters[type]=post", nil)
		f, _ := parseFilters(r)
		if len(f) != 2 {
			t.Errorf("filter count = %d, want 2", len(f))
		}
		if f["status"] != "draft" || f["type"] != "post" {
			t.Errorf("filters = %v", f)
		}
	})
	t.Run("no_filters", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?limit=10", nil)
		f, _ := parseFilters(r)
		if len(f) != 0 {
			t.Errorf("should be no filters from missing filters prefix, got %d", len(f))
		}
	})

	// SQL injection: filter keys with embedded = or SQL operators
	// These MUST be captured verbatim so the ContentStore allowlist can
	// reject them. r.URL.Query() drops keys containing = because it splits on
	// every equals sign, so parseFilters reads the raw query.
	//
	// When = appears UNENCODED inside brackets, Go's HTTP parser treats it
	// as the KV separator, producing a key like "filters[1" with no closing
	// "]". parseFilters detects this and returns an error so callers
	// return HTTP 400 instead of silently ignoring the injection attempt.
	// Properly URL-encoded payloads (e.g. %3D for =) are still extracted
	// verbatim and rejected downstream by the allowlist.

	t.Run("unencoded_=_in_brackets_returns_error", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[1=1--]=x", nil)
		f, err := parseFilters(r)
		if err == nil {
			t.Errorf("expected error for unencoded = in filter key, got nil; f=%v", f)
		}
	})
	t.Run("URL_encoded_=_in_brackets_is_extracted", func(t *testing.T) {
		// Properly encoded: filters%5B1%3D1--%5D -> key=1=1--
		r := httptest.NewRequest(http.MethodGet, "/?filters%5B1%3D1--%5D=x", nil)
		f, err := parseFilters(r)
		if err != nil {
			t.Fatalf("unexpected error for encoded key: %v", err)
		}
		if v, ok := f["1=1--"]; !ok {
			t.Errorf("filters[1=1--] was silently dropped - SQL injection bypass")
		} else if v != "x" {
			t.Errorf("filters[1=1--] = %q, want %q", v, "x")
		}
	})
	t.Run("SQL_injection_key_OR_1=1 is captured", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[OR%201%3D1]=x", nil)
		f, _ := parseFilters(r)
		if v, ok := f["OR 1=1"]; !ok {
			t.Errorf("filters[OR 1=1] was silently dropped - SQL injection bypass")
		} else if v != "x" {
			t.Errorf("filters[OR 1=1] = %q, want %q", v, "x")
		}
	})
	t.Run("SQL_injection_key_semicolon is captured", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[%3B%20DROP%20TABLE%20users%3B--]=x", nil)
		f, _ := parseFilters(r)
		if v, ok := f["; DROP TABLE users;--"]; !ok {
			t.Errorf("filters[; DROP TABLE users;--] was silently dropped - SQL injection bypass")
		} else if v != "x" {
			t.Errorf("filters[; DROP TABLE users;--] = %q, want %q", v, "x")
		}
	})
	t.Run("SQL_injection_key_UNION_SELECT is captured", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[UNION%20SELECT]=x", nil)
		f, _ := parseFilters(r)
		if v, ok := f["UNION SELECT"]; !ok {
			t.Errorf("filters[UNION SELECT] was silently dropped - SQL injection bypass")
		} else if v != "x" {
			t.Errorf("filters[UNION SELECT] = %q, want %q", v, "x")
		}
	})

	t.Run("empty_bracket_key_skipped", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[]=x", nil)
		f, _ := parseFilters(r)
		if len(f) != 0 {
			t.Errorf("filters[] should be skipped, got %d entries", len(f))
		}
	})
	t.Run("URL_encoded_value", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[title]=hello%20world", nil)
		f, _ := parseFilters(r)
		if f["title"] != "hello world" {
			t.Errorf("filters[title] = %q, want %q", f["title"], "hello world")
		}
	})
	t.Run("first_value_wins_on_duplicate", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/?filters[title]=first&filters[title]=second", nil)
		f, _ := parseFilters(r)
		if f["title"] != "first" {
			t.Errorf("filters[title] = %q, want %q", f["title"], "first")
		}
	})
	t.Run("empty_query", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		f, _ := parseFilters(r)
		if len(f) != 0 {
			t.Errorf("empty query should yield empty map, got %d entries", len(f))
		}
	})
}

// EntitlementProvider interface compliance

// Ensure unlicensedEntitlements satisfies the interface.
var _ EntitlementProvider = unlicensedEntitlements{}

// Ensure stubEntitlements satisfies the interface.
var _ EntitlementProvider = stubEntitlements{}

// Content handler: structural validation via unexported helpers

func TestContentHandler_PermissionChecks(t *testing.T) {
	t.Parallel()

	t.Run("no_rule_engine_refuses_an_ordinary_role", func(t *testing.T) {
		// With no rule engine the kernel's own checker answers, so an install
		// without one is closed, not open.
		h := &ContentHandler{store: nil, schemas: nil, hooks: hooks.NewRegistry(), perms: nil}
		rr := httptest.NewRecorder()
		if h.checkPermission(rr, withRoles("editor"), "posts", "read") {
			t.Error("editor allowed with no rule engine")
		}
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rr.Code)
		}
	})
	t.Run("no_claims_is_refused", func(t *testing.T) {
		// requireAuth runs first on every content route, so a request with no
		// caller means the handler was mounted without it, and the gate
		// refuses it.
		h := &ContentHandler{store: nil, schemas: nil, hooks: hooks.NewRegistry(), perms: nil}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		if h.checkPermission(rr, r, "posts", "delete") {
			t.Error("allowed a request with no caller")
		}
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rr.Code)
		}
	})
}

func TestContentHandler_MaskFields(t *testing.T) {
	t.Parallel()

	h := &ContentHandler{store: nil, schemas: nil, hooks: hooks.NewRegistry(), perms: nil}
	data := map[string]any{"title": "hello", "secret": "shh", "public": true}

	rr := httptest.NewRecorder()
	mask, ok := h.maskFor(rr, withRoles("editor"), "posts")
	if !ok {
		t.Fatalf("mask refused: %d", rr.Code)
	}
	if got := mask(data); len(got) != 3 {
		t.Errorf("expected no masking without a rule engine, got %d fields", len(got))
	}
}

// Publish/Unpublish: ContentHandler methods

func TestContentHandler_Publish_IDValidation(t *testing.T) {
	t.Parallel()

	h := &ContentHandler{store: nil, schemas: nil, hooks: hooks.NewRegistry(), perms: nil}

	r := httptest.NewRequest(http.MethodPut, "/content/posts/bad-id/publish", nil)
	r = contentReq(r, map[string]string{"schema": "posts", "id": "not-a-uuid"})
	rr := httptest.NewRecorder()
	h.Publish(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for invalid id", rr.Code, http.StatusBadRequest)
	}
}

func TestContentHandler_Unpublish_IDValidation(t *testing.T) {
	t.Parallel()

	h := &ContentHandler{store: nil, schemas: nil, hooks: hooks.NewRegistry(), perms: nil}

	r := httptest.NewRequest(http.MethodPut, "/content/posts/bad-id/unpublish", nil)
	r = contentReq(r, map[string]string{"schema": "posts", "id": "not-a-uuid"})
	rr := httptest.NewRecorder()
	h.Unpublish(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for invalid id", rr.Code, http.StatusBadRequest)
	}
}

// Content handler: Create / Update validation

func TestContentHandler_Create_BadJSON(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	r := httptest.NewRequest(http.MethodPost, "/content/posts", strings.NewReader("not json"))
	r = contentReq(r, map[string]string{"schema": "posts"})
	rr := httptest.NewRecorder()
	h.Create(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestContentHandler_Update_BadID(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	body := `{"data":{"title":"updated"}}`
	r := httptest.NewRequest(http.MethodPut, "/content/posts/bad-id", strings.NewReader(body))
	r = contentReq(r, map[string]string{"schema": "posts", "id": "not-a-uuid"})
	rr := httptest.NewRecorder()
	h.Update(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestContentHandler_Delete_BadID(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	r := httptest.NewRequest(http.MethodDelete, "/content/posts/bad-id", nil)
	r = contentReq(r, map[string]string{"schema": "posts", "id": "not-a-uuid"})
	rr := httptest.NewRecorder()
	h.Delete(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestContentHandler_Get_BadID(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	r := httptest.NewRequest(http.MethodGet, "/content/posts/bad-id", nil)
	r = contentReq(r, map[string]string{"schema": "posts", "id": "not-a-uuid"})
	rr := httptest.NewRecorder()
	h.Get(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestContentHandler_BulkCreate_Empty(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	body := `{"items":[]}`
	r := httptest.NewRequest(http.MethodPost, "/content/posts/bulk", strings.NewReader(body))
	r = contentReq(r, map[string]string{"schema": "posts"})
	rr := httptest.NewRecorder()
	h.BulkCreate(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestContentHandler_BulkCreate_TooMany(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	// Build 501 items
	var items []string
	for i := 0; i < 501; i++ {
		items = append(items, `{"title":"x"}`)
	}
	body := `{"items":[` + strings.Join(items, ",") + `]}`
	r := httptest.NewRequest(http.MethodPost, "/content/posts/bulk", strings.NewReader(body))
	r = contentReq(r, map[string]string{"schema": "posts"})
	rr := httptest.NewRecorder()
	h.BulkCreate(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestContentHandler_BulkCreate_UnderLimit_ReachesStore(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	// No store -> will panic on store.BulkInsert. We expect the structural
	// validation to pass (500 items is fine, 1 item is fine, the body is valid
	// JSON) and the handler to proceed to the store call, which will be nil
	// and panic. This proves the validation logic works. Store integration
	// needs db tests.
	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	body := `{"items":[{"title":"hello"}]}`
	r := httptest.NewRequest(http.MethodPost, "/content/posts/bulk", strings.NewReader(body))
	r = contentReq(r, map[string]string{"schema": "posts"})
	rr := httptest.NewRecorder()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic from nil store.BulkInsert - validates JSON parsing + hook pipeline passed")
			}
		}()
		h.BulkCreate(rr, r)
	}()
}

// SetRelations / ListRelations: ID validation

func TestContentHandler_SetRelations_BadID(t *testing.T) {
	t.Parallel()

	h := &ContentHandler{store: nil, schemas: nil, hooks: hooks.NewRegistry(), perms: nil}

	body := `{"ids":["` + uuid.New().String() + `"]}`
	r := httptest.NewRequest(http.MethodPut, "/content/posts/bad-id/relations/tags", strings.NewReader(body))
	r = contentReq(r, map[string]string{"schema": "posts", "id": "not-a-uuid", "field": "tags"})
	rr := httptest.NewRecorder()
	h.SetRelations(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestContentHandler_ListRelations_BadID(t *testing.T) {
	t.Parallel()

	h := &ContentHandler{store: nil, schemas: nil, hooks: hooks.NewRegistry(), perms: nil}

	r := httptest.NewRequest(http.MethodGet, "/content/posts/bad-id/relations/tags", nil)
	r = contentReq(r, map[string]string{"schema": "posts", "id": "not-a-uuid", "field": "tags"})
	rr := httptest.NewRecorder()
	h.ListRelations(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

// BeforeRequest hook abort

func TestContentHandler_BeforeRequest_Aborts(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	reg.Register("*", hooks.BeforeRequest, func(ctx context.Context, e hooks.Event) error {
		return errors.New("access denied")
	})

	h := &ContentHandler{store: nil, schemas: nil, hooks: reg, perms: nil}

	r := httptest.NewRequest(http.MethodGet, "/content/posts", nil)
	r = chiCtx(r, map[string]string{"schema": "posts"})
	rr := httptest.NewRecorder()
	h.List(rr, r)

	// BeforeRequest hook returning error should produce 403
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d when BeforeRequest hook returns error", rr.Code, http.StatusForbidden)
	}
}

// NewAdminRouter / NewAPIRouter smoke: compile-time guard

// These functions are tested implicitly by the package compiling.
// Full integration tests (with real DB) live in lyeve-core/cmd/lyeve/ or via TestMain.

// mountPluginRoutes: smoke test

func TestMountPluginRoutes_Empty(t *testing.T) {
	t.Parallel()
	r := chi.NewRouter()
	// This should not panic
	mountPluginRoutes(r, nil, "/api/admin", false, 1<<20, nil, nil, nil)
	mountPluginRoutes(r, []plugin.PluginRoutes{}, "/api/v1", false, 1<<20, nil, nil, nil)
}

// uuid validation helper: content handler uses uuid.Parse, test it

func TestUUIDParse_Guard(t *testing.T) {
	t.Parallel()
	// Verify uuid.Parse rejects non-uuid strings (used across all handlers)
	tests := []struct {
		in    string
		valid bool
	}{
		{uuid.New().String(), true},
		{"not-a-uuid", false},
		{"", false},
		{"00000000-0000-0000-0000-00000000000", false}, // too short
	}
	for _, tt := range tests {
		_, err := uuid.Parse(tt.in)
		if tt.valid && err != nil {
			t.Errorf("uuid.Parse(%q) unexpected error: %v", tt.in, err)
		}
		if !tt.valid && err == nil {
			t.Errorf("uuid.Parse(%q) should have returned error", tt.in)
		}
	}
}

// Respond content-type validation

func TestRespond_BodyEncoding(t *testing.T) {
	t.Parallel()

	t.Run("slice_body", func(t *testing.T) {
		rr := httptest.NewRecorder()
		respond(rr, http.StatusOK, []string{"a", "b"})
		if rr.Header().Get("Content-Type") != "application/json" {
			t.Error("Content-Type should be application/json")
		}
		body := rr.Body.String()
		if !strings.Contains(body, `"a"`) || !strings.Contains(body, `"b"`) {
			t.Errorf("slice body = %q", body)
		}
	})
}

// testLifetime bounds the background work a router starts, which is the
// schema cache's poller. Without it the poller outlives the test that built
// the router: against a stub pool it fails every second and logs through
// whatever the global logger is by then.
func testLifetime(tb testing.TB) context.Context {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tb.Cleanup(cancel)
	return ctx
}
