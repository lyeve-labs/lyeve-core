package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The operator's refusal of memory profile exports holds on the engine's own
// pprof routes, not only on the profiler plugin's. The other profiles stay
// served, and with the refusal off the memory profiles are served again.
func TestAdminRouter_PprofMemoryProfilesHonorTheExportRefusal(t *testing.T) {
	router, err := NewAdminRouter(&fakeDB{engine: "postgres", queryRowFactory: newTokenVersionRow}, testConfig(), WithLifetime(testLifetime(t)))
	require.NoError(t, err)
	session := bearerWithRoles(t, "super_admin")
	get := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+session)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	t.Setenv("PROFILER_NO_HEAP_EXPORT", "true")
	for _, path := range []string{"/api/admin/debug/pprof/heap", "/api/admin/debug/pprof/allocs", "/api/admin/debug/pprof/heap?debug=1"} {
		assert.Equal(t, http.StatusForbidden, get(path), "%s served a memory profile the operator refused", path)
	}
	for _, path := range []string{"/api/admin/debug/pprof/goroutine", "/api/admin/debug/pprof/threadcreate", "/api/admin/debug/pprof/"} {
		assert.Equal(t, http.StatusOK, get(path), "%s is not a memory profile", path)
	}

	t.Setenv("PROFILER_NO_HEAP_EXPORT", "false")
	assert.Equal(t, http.StatusOK, get("/api/admin/debug/pprof/heap"))
	assert.Equal(t, http.StatusOK, get("/api/admin/debug/pprof/allocs"))
}
