package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The leak profile answers beside the goroutine profile, to a super admin
// only. The index cannot serve it under the admin prefix, because it looks
// for the profile name after /debug/pprof/ at the start of the path.
func TestAdminRouter_PprofServesTheGoroutineLeakProfile(t *testing.T) {
	router, err := NewAdminRouter(&fakeDB{engine: "postgres", queryRowFactory: newTokenVersionRow}, testConfig(), WithLifetime(testLifetime(t)))
	require.NoError(t, err)
	get := func(role string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/debug/pprof/goroutineleak?debug=1", nil)
		req.Header.Set("Authorization", "Bearer "+bearerWithRoles(t, role))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := get("super_admin")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "goroutineleak profile: total")

	assert.Equal(t, http.StatusForbidden, get("admin").Code, "an admin read the leak profile")
}
