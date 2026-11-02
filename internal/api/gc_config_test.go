package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	runtimedebug "runtime/debug"
	"runtime/metrics"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The collector settings belong to the whole process, so these tests do not
// run in parallel, and each puts back what it changes.

// setGC sets the collector percentage and memory limit for one test.
func setGC(t *testing.T, gogc int, limit int64) {
	t.Helper()
	prevGOGC := runtimedebug.SetGCPercent(gogc)
	prevLimit := runtimedebug.SetMemoryLimit(limit)
	t.Cleanup(func() {
		runtimedebug.SetGCPercent(prevGOGC)
		runtimedebug.SetMemoryLimit(prevLimit)
	})
}

func gogcInForce() int64 {
	s := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(s)
	return int64(s[0].Value.Uint64())
}

func gcConfigRequest(t *testing.T, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	router, err := NewAdminRouter(&fakeDB{engine: "postgres", queryRowFactory: newTokenVersionRow}, testConfig(), WithLifetime(testLifetime(t)))
	require.NoError(t, err)
	req := httptest.NewRequest(method, "/api/admin/debug/gc-config", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearerWithRoles(t, "super_admin"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// A negative GOGC turns the collector off, and with no memory limit the heap
// then grows until the kernel kills the process.
func TestAdminRouter_GCConfigRefusesTurningTheCollectorOffWithoutAMemoryLimit(t *testing.T) {
	setGC(t, 100, math.MaxInt64)

	rec := gcConfigRequest(t, http.MethodPost, `{"gogc": -1}`)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, int64(100), gogcInForce(), "the refused setting was applied")
}

func TestAdminRouter_GCConfigTurnsTheCollectorOffUnderAMemoryLimit(t *testing.T) {
	const limit = 256 << 20
	setGC(t, 100, limit)

	rec := gcConfigRequest(t, http.MethodPost, `{"gogc": -1}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got map[string]int64
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, map[string]int64{"previous_gogc": 100, "current_gogc": -1, "memory_limit": limit}, got)
}

// A limit at or above the machine's memory bounds nothing, because the
// kernel's OOM kill comes first, so it does not let the collector go off. A
// percentage past the ceiling is refused the same way, and the ceiling itself
// is allowed.
func TestAdminRouter_GCConfigTreatsALimitAboveTheMachineAsNone(t *testing.T) {
	setGC(t, 100, 1<<62)

	for _, body := range []string{`{"gogc": -1}`, `{"gogc": 2000000000}`} {
		rec := gcConfigRequest(t, http.MethodPost, body)

		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "%s: %s", body, rec.Body.String())
		assert.Equal(t, int64(100), gogcInForce(), "%s: the refused setting was applied", body)
	}
	rec := gcConfigRequest(t, http.MethodPost, `{"gogc": 1000}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// The report reads the settings the runtime holds, which a call since boot
// may have changed, not the environment the process started with.
func TestAdminRouter_GCConfigReportsTheSettingsInForce(t *testing.T) {
	const limit = 1 << 40
	setGC(t, 150, limit)

	rec := gcConfigRequest(t, http.MethodGet, "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, map[string]any{"gogc": float64(150), "memory_limit": float64(limit)}, got)
}

func TestAdminRouter_GCConfigReportsNoMemoryLimitAsZero(t *testing.T) {
	setGC(t, 100, math.MaxInt64)

	rec := gcConfigRequest(t, http.MethodGet, "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, float64(0), got["memory_limit"])
}

// A body that names no percentage, or names a field the endpoint does not
// read, used to decode to a percentage of 0, which runs the collector without
// pause. Both are refused, and the setting in force stays.
func TestAdminRouter_GCConfigRefusesABodyWithoutAPercentage(t *testing.T) {
	setGC(t, 100, math.MaxInt64)

	for _, tt := range []struct {
		body string
		code int
	}{
		{body: `{}`, code: http.StatusUnprocessableEntity},
		{body: `{"gogc": null}`, code: http.StatusUnprocessableEntity},
		{body: `{"memory_limit": 1}`, code: http.StatusBadRequest},
		{body: `{"gogc": 50, "memory_limit": 1}`, code: http.StatusBadRequest},
	} {
		rec := gcConfigRequest(t, http.MethodPost, tt.body)

		assert.Equal(t, tt.code, rec.Code, "%s: %s", tt.body, rec.Body.String())
		assert.Equal(t, int64(100), gogcInForce(), "%s: the refused setting was applied", tt.body)
	}
}
