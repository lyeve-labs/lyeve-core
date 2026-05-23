package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

type auditRow struct {
	key, method, path string
	status            int
}

type auditSink struct {
	mu   sync.Mutex
	rows []auditRow
}

func (s *auditSink) log(key, method, path string, status int, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, auditRow{key, method, path, status})
}

func (s *auditSink) take() []auditRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.rows
	s.rows = nil
	return out
}

// Every API key request on the admin port is logged once, whether the engine
// or a plugin serves it and whether it is refused.
func TestAdminRouter_EveryKeyRequestIsAudited(t *testing.T) {
	t.Parallel()

	keyID := uuid.NewString()
	withKey := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") == "" {
				next.ServeHTTP(w, r)
				return
			}
			claims := &core.AuthClaims{UserID: keyID, Roles: []string{"viewer"}, Scopes: []string{"*:*"}, IsAPIKey: true}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}
	sink := &auditSink{}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)),
		WithAPIKeyAuth(withKey),
		WithAPIKeyAudit(apimw.APIKeyAudit(sink.log, nil)),
		WithPluginRoutes([]plugin.PluginRoutes{{
			Name:   "test-plugin",
			Routes: []plugin.RouteDecl{{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/items", Group: plugin.GroupAdmin, Handler: ok}},
		}}))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/admin/health", http.StatusOK},
		{http.MethodPost, "/api/admin/users", http.StatusForbidden},
		{http.MethodGet, "/api/admin/test-plugin/items", http.StatusForbidden},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-API-Key", "k")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s %s: status = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
		rows := sink.take()
		if len(rows) != 1 {
			t.Fatalf("%s %s: %d audit rows, want exactly 1", tc.method, tc.path, len(rows))
		}
		if got := rows[0]; got.key != keyID || got.method != tc.method || got.path != tc.path || got.status != tc.want {
			t.Fatalf("%s %s: audit row = %+v", tc.method, tc.path, got)
		}
	}

	// A request with no key is not an API key request and is not logged here.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	router.ServeHTTP(httptest.NewRecorder(), req)
	if rows := sink.take(); len(rows) != 0 {
		t.Fatalf("request without a key logged %d rows", len(rows))
	}
}
