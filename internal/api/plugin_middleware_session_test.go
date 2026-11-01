package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// pluginMiddlewareSpy records the caller each request carries when it
// reaches the plugin middleware slot.
type pluginMiddlewareSpy struct {
	mu   sync.Mutex
	seen []*core.AuthClaims
}

func (s *pluginMiddlewareSpy) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, core.GetClaims(r.Context()))
		s.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (s *pluginMiddlewareSpy) last(t *testing.T) *core.AuthClaims {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 {
		t.Fatal("the request never reached the plugin middleware")
	}
	c := s.seen[len(s.seen)-1]
	s.seen = nil
	return c
}

// A plugin middleware runs before the route groups that end a session, so a
// plugin judging the caller by role would act for a session that has ended.
// On both routers it sees the caller while the session is live and no caller
// once the account's password is set, and the route still answers 401.
func TestRouters_EndedSessionReachesPluginMiddlewareWithoutACaller(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	pool := testdb.Postgres(t)
	users := db.NewUserStore(pool)
	user, err := users.Create(context.Background(), "ended-"+uuid.NewString()[:8]+"@example.com",
		"$2a$10$placeholderhashforrevocationXXXXXXXXXXXXXXXXXXXXXXXX", []string{"admin"}, "default")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	cfg := testConfig()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	spy := &pluginMiddlewareSpy{}
	admin, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)), WithMiddleware(spy.middleware),
		WithPluginRoutes([]plugin.PluginRoutes{{
			Name:   "test-plugin",
			Routes: []plugin.RouteDecl{{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/items", Group: plugin.GroupAdmin, Handler: ok}},
		}}))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}
	api, err := NewAPIRouter(pool, cfg, nil, WithLifetime(testLifetime(t)), WithMiddleware(spy.middleware),
		WithPluginRoutes([]plugin.PluginRoutes{{
			Name:   "test-plugin",
			Routes: []plugin.RouteDecl{{Method: http.MethodGet, Pattern: "/api/v1/test-plugin/items", Group: plugin.GroupAuth, Handler: ok}},
		}}))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	tok, err := auth.Sign(cfg.JWTSecret, 3600, user.ID, user.Email, user.Roles, "default", user.TokenVersion)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	routes := []struct {
		name string
		h    http.Handler
		path string
	}{
		{"admin", admin, "/api/admin/test-plugin/items"},
		{"api", api, "/api/v1/test-plugin/items"},
	}
	for _, rt := range routes {
		if rec := get(rt.h, rt.path); rec.Code != http.StatusOK {
			t.Fatalf("%s with a live session = %d, want 200: %s", rt.name, rec.Code, rec.Body)
		}
		if c := spy.last(t); c == nil || !c.HasRole(core.RoleAdmin) {
			t.Fatalf("%s: the plugin middleware saw %+v for a live session, want its admin role", rt.name, c)
		}
	}

	if _, err := users.SetPassword(context.Background(), user.ID, "$2a$10$anotherplaceholderhashXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	for _, rt := range routes {
		rec := get(rt.h, rt.path)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "session invalidated") {
			t.Errorf("%s after the password was set = %d %s, want 401 session invalidated", rt.name, rec.Code, rec.Body)
		}
		if c := spy.last(t); c != nil {
			t.Errorf("%s: the plugin middleware saw %+v for an ended session, want no caller", rt.name, c)
		}
	}
}

// Each way a session check can end, as the plugin middleware and the route
// see it: a current session is read once and reaches both, while a stale
// one, an account that is gone and a store that cannot answer reach the
// plugin middleware with no caller and get the route's own answer.
func TestHideEndedSession_PluginMiddlewareSeesOnlyACurrentSession(t *testing.T) {
	const current = 3
	cases := []struct {
		name     string
		version  int
		err      error
		want     int
		wantSeen bool
	}{
		{"current", current, nil, http.StatusOK, true},
		{"stale", current - 1, nil, http.StatusUnauthorized, false},
		{"account gone", current, domain.ErrNotFound, http.StatusUnauthorized, false},
		{"store down", current, errors.New("connection refused"), http.StatusServiceUnavailable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reads int
			get := func(context.Context, string) (int, error) {
				reads++
				return current, tc.err
			}
			hide, restore := hideEndedSession(get)
			spy := &pluginMiddlewareSpy{}
			var routeSaw *auth.Claims
			r := chi.NewRouter()
			r.Use(hide, spy.middleware, restore)
			r.With(requireAuth, tokenVersionCheck(get)).Get("/item", func(w http.ResponseWriter, r *http.Request) {
				routeSaw = claimsFromCtx(r)
				w.WriteHeader(http.StatusOK)
			})

			claims := makeClaims(uuid.New(), "owner@test.com", []string{"admin"})
			claims.TokenVersion = tc.version
			ctx := context.WithValue(context.Background(), auth.ClaimsKey, claims)
			ctx = context.WithValue(ctx, core.ClaimsKey, claims.AuthClaims())
			req := httptest.NewRequest(http.MethodGet, "/item", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if seen := spy.last(t); (seen != nil) != tc.wantSeen {
				t.Fatalf("plugin middleware saw %+v, want a caller: %v", seen, tc.wantSeen)
			}
			if tc.wantSeen {
				if routeSaw != claims {
					t.Fatal("the route did not get the session's claims")
				}
				if reads != 1 {
					t.Fatalf("a current session's version was read %d times, want 1", reads)
				}
			}
		})
	}
}
