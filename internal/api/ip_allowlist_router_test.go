package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A list the config loader would refuse can still reach the router through a
// config built by hand. The router fails closed on it and admits nobody,
// because skipping the list would admit every address.
func TestIPAllowlist_MalformedListFailsClosed(t *testing.T) {
	cfg := testConfig()
	cfg.IPAllowlist = []string{"10.0.0.0/8", "192.168.1.0/33"}
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}
	for _, remote := range []string{"10.1.2.3:4000", "203.0.113.9:4000"} {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/setup", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("from %s: status = %d, want 503", remote, rec.Code)
		}
	}
}
