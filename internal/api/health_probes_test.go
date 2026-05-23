package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test helpers

// makeCheck builds a probe whose failure text reaches the report.
//
// A probe withholds its error by default, because the probe endpoints take no
// credential. These tests are about pass, fail and required-ness rather than
// about the redaction, so they declare the text public and go on asserting
// what they are about. The redaction has its own tests.
func makeCheck(name string, err error, required bool) ProbeCheck {
	return ProbeCheck{
		Name:        name,
		Check:       func(ctx context.Context) error { return err },
		Required:    required,
		PublicError: true,
	}
}

func decodeProbeReport(t *testing.T, rr *httptest.ResponseRecorder) ProbeReport {
	t.Helper()
	var report ProbeReport
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&report))
	return report
}

// ProbeRegistry unit tests

func TestProbeRegistry_Liveness_DBPasses(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(NewDBProbe(func(ctx context.Context) error { return nil }))

	report, err := reg.Liveness(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ok", report.Status)

	// Should have exactly the one Required check (DB)
	require.Len(t, report.Results, 1)
	assert.Equal(t, "database", report.Results[0].Name)
	assert.True(t, report.Results[0].Passed)
	assert.Empty(t, report.Results[0].Error)
	assert.NotEmpty(t, report.Results[0].Took)
}

func TestProbeRegistry_Liveness_DBFails(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(NewDBProbe(func(ctx context.Context) error { return errors.New("connection refused") }))

	report, err := reg.Liveness(context.Background())
	// Liveness returns the report regardless of the error
	require.NoError(t, err)
	assert.Equal(t, "ok", report.Status) // raw report.Status stays "ok" - caller decides status

	require.Len(t, report.Results, 1)
	assert.Equal(t, "database", report.Results[0].Name)
	assert.False(t, report.Results[0].Passed)
	assert.Equal(t, "connection refused", report.Results[0].Error)
}

func TestProbeRegistry_Liveness_EmptyRegistry(t *testing.T) {
	reg := NewProbeRegistry()
	report, err := reg.Liveness(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ok", report.Status)
	assert.Empty(t, report.Results)
}

func TestProbeRegistry_Readiness_AllPass(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("db", nil, true))
	reg.Add(makeCheck("redis", nil, true))
	reg.Add(makeCheck("goroutines", nil, false))

	report, allOK := reg.Readiness(context.Background())
	assert.True(t, allOK)
	assert.Equal(t, "ok", report.Status)
	require.Len(t, report.Results, 3)
	for _, r := range report.Results {
		assert.True(t, r.Passed)
	}
}

func TestProbeRegistry_Readiness_RequiredFails(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("db", nil, true))
	reg.Add(makeCheck("redis", errors.New("down"), true))
	reg.Add(makeCheck("goroutines", nil, false))

	report, allOK := reg.Readiness(context.Background())
	assert.False(t, allOK)
	assert.Equal(t, "degraded", report.Status)
	require.Len(t, report.Results, 3)

	// db passed, redis failed, goroutines warned-but-not-required -> still passed
	assert.True(t, report.Results[0].Passed)  // db
	assert.False(t, report.Results[1].Passed) // redis
	assert.True(t, report.Results[2].Passed)  // goroutines
	assert.Equal(t, "redis", report.Results[1].Name)
	assert.Equal(t, "down", report.Results[1].Error)
}

func TestProbeRegistry_Readiness_AdvisoryOnlyFails(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("db", nil, true))
	reg.Add(makeCheck("goroutines", errors.New("high"), false))

	// All Required checks pass -> allOK true even though advisory warns.
	report, allOK := reg.Readiness(context.Background())
	assert.True(t, allOK)
	assert.Equal(t, "ok", report.Status)
	require.Len(t, report.Results, 2)
	assert.True(t, report.Results[0].Passed)  // db
	assert.False(t, report.Results[1].Passed) // goroutines (advisory, ignored)
}

func TestProbeRegistry_Startup_CachesAfterFirstPass(t *testing.T) {
	reg := NewProbeRegistry()
	callCount := 0
	reg.Add(ProbeCheck{
		Name:     "startup-check",
		Check:    func(ctx context.Context) error { callCount++; return nil },
		Required: true,
	})

	// First call: runs the check.
	report, allOK := reg.Startup(context.Background())
	assert.True(t, allOK)
	assert.Equal(t, "ok", report.Status)
	assert.Equal(t, 1, callCount)

	// Second call: cached, should NOT re-run.
	report2, allOK2 := reg.Startup(context.Background())
	assert.True(t, allOK2)
	assert.Equal(t, "ok", report2.Status)
	assert.Equal(t, "cached", report2.Results[0].Took)
	assert.Equal(t, 1, callCount) // unchanged

	// StartTime should be set.
	assert.False(t, reg.StartTime().IsZero())
}

func TestProbeRegistry_Startup_DoesNotCacheOnFailure(t *testing.T) {
	reg := NewProbeRegistry()
	callCount := 0
	reg.Add(ProbeCheck{
		Name: "startup-check",
		Check: func(ctx context.Context) error {
			callCount++
			return errors.New("not ready")
		},
		Required: true,
	})

	// First call: fails.
	_, allOK := reg.Startup(context.Background())
	assert.False(t, allOK)
	assert.Equal(t, 1, callCount)
	assert.True(t, reg.StartTime().IsZero())

	// Second call: should re-run (not cached).
	_, allOK2 := reg.Startup(context.Background())
	assert.False(t, allOK2)
	assert.Equal(t, 2, callCount) // re-ran
	assert.True(t, reg.StartTime().IsZero())
}

func TestProbeRegistry_Startup_CachedAfterPass_ThenPassesOut(t *testing.T) {
	reg := NewProbeRegistry()
	callCount := 0
	reg.Add(ProbeCheck{
		Name: "startup-check",
		Check: func(ctx context.Context) error {
			callCount++
			if callCount <= 2 {
				return errors.New("not ready yet")
			}
			return nil
		},
		Required: true,
	})

	// Fail twice.
	for i := 0; i < 2; i++ {
		_, allOK := reg.Startup(context.Background())
		assert.False(t, allOK)
	}
	assert.True(t, reg.StartTime().IsZero())

	// Third time passes.
	_, allOK := reg.Startup(context.Background())
	assert.True(t, allOK)
	assert.Equal(t, 3, callCount)
	assert.False(t, reg.StartTime().IsZero())

	// Fourth time cached.
	_, allOK = reg.Startup(context.Background())
	assert.True(t, allOK)
	assert.Equal(t, 3, callCount) // no additional call
}

// Built-in check constructors

func TestNewDBProbe(t *testing.T) {
	probe := NewDBProbe(func(ctx context.Context) error { return nil })
	assert.Equal(t, "database", probe.Name)
	assert.True(t, probe.Required)

	err := probe.Check(context.Background())
	assert.NoError(t, err)
}

func TestNewRedisProbe(t *testing.T) {
	probe := NewRedisProbe(func(ctx context.Context) error { return nil }, false)
	assert.Equal(t, "redis", probe.Name)
	assert.False(t, probe.Required, "the cache is advisory unless the operator opts in")

	err := probe.Check(context.Background())
	assert.NoError(t, err)

	strict := NewRedisProbe(func(ctx context.Context) error { return nil }, true)
	assert.True(t, strict.Required, "HEALTH_REDIS_PROBE_REQUIRED=true must fail closed")
}

func TestNewGoroutineProbe(t *testing.T) {
	probe := NewGoroutineProbe(10000)
	assert.Equal(t, "goroutines", probe.Name)
	assert.False(t, probe.Required) // advisory

	// Should pass since we have far fewer than 10000 goroutines.
	err := probe.Check(context.Background())
	assert.NoError(t, err)
}

func TestNewGoroutineProbe_ExceedsThreshold(t *testing.T) {
	// Set threshold to 1: we should always have more goroutines than that.
	probe := NewGoroutineProbe(1)
	err := probe.Check(context.Background())
	require.Error(t, err)
	assert.IsType(t, &goroutineCountError{}, err)
}

func TestNewDiskProbe_CurrentDir(t *testing.T) {
	// Check disk space on /tmp: should be fine.
	probe := NewDiskProbe("/tmp", 1) // min 1 byte: always passes unless disk is truly full
	err := probe.Check(context.Background())
	assert.NoError(t, err)
}

func TestNewDiskProbe_ImpossibleThreshold(t *testing.T) {
	// Check with a threshold that no real disk will satisfy.
	probe := NewDiskProbe("/", 1<<60) // 1 EiB: no consumer disk has this much free
	err := probe.Check(context.Background())
	require.Error(t, err)
	assert.IsType(t, &diskSpaceError{}, err)
}

// HTTP handler tests

func TestHealthzHandler_AllPass(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(NewDBProbe(func(ctx context.Context) error { return nil }))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	healthzHandler(reg).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	report := decodeProbeReport(t, rr)
	assert.Equal(t, "ok", report.Status)
	require.Len(t, report.Results, 1)
	assert.True(t, report.Results[0].Passed)
}

func TestHealthzHandler_DBFails(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(NewDBProbe(func(ctx context.Context) error { return errors.New("timeout") }))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	healthzHandler(reg).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	report := decodeProbeReport(t, rr)
	require.Len(t, report.Results, 1)
	assert.False(t, report.Results[0].Passed)
	assert.Equal(t, "timeout", report.Results[0].Error)
}

// readyzHandler runs the startup probe itself when it has not passed, so a
// registry whose probes all pass needs no help. A case that wants the
// readiness path with a FAILING dependency does: without a prior pass, readyz
// reports "starting" rather than "degraded".
func started(t *testing.T, reg *ProbeRegistry) *ProbeRegistry {
	t.Helper()
	if _, ok := reg.Startup(context.Background()); !ok {
		t.Fatal("startup probe did not pass")
	}
	return reg
}

func TestReadyzHandler_AllPass(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("db", nil, true))
	reg.Add(makeCheck("redis", nil, true))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	readyzHandler(reg).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	report := decodeProbeReport(t, rr)
	assert.Equal(t, "ok", report.Status)
	assert.Len(t, report.Results, 2)
}

func TestReadyzHandler_RequiredFails(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("db", nil, true))
	// The dependency goes down after the engine came up, which is the case
	// this covers: a startup that never passes is the gate's business, not
	// the readiness probes'.
	started(t, reg)
	reg.Add(makeCheck("redis", errors.New("unresponsive"), true))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	readyzHandler(reg).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	report := decodeProbeReport(t, rr)
	assert.Equal(t, "degraded", report.Status)
}

func TestStartupHandler_FirstPassThenCached(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("db", nil, true))

	// First call: runs the check, should pass.
	req1 := httptest.NewRequest(http.MethodGet, "/startup", nil)
	rr1 := httptest.NewRecorder()
	startupHandler(reg).ServeHTTP(rr1, req1)

	assert.Equal(t, http.StatusOK, rr1.Code)
	report1 := decodeProbeReport(t, rr1)
	assert.Equal(t, "ok", report1.Status)
	assert.NotEqual(t, "cached", report1.Results[0].Took)

	// Second call: cached.
	req2 := httptest.NewRequest(http.MethodGet, "/startup", nil)
	rr2 := httptest.NewRecorder()
	startupHandler(reg).ServeHTTP(rr2, req2)

	assert.Equal(t, http.StatusOK, rr2.Code)
	report2 := decodeProbeReport(t, rr2)
	assert.Equal(t, "cached", report2.Results[0].Took)
}

func TestStartupHandler_FailsUntilPass(t *testing.T) {
	calls := 0
	reg := NewProbeRegistry()
	reg.Add(ProbeCheck{
		Name: "slow-start",
		Check: func(ctx context.Context) error {
			calls++
			if calls < 3 {
				return errors.New("starting up")
			}
			return nil
		},
		Required: true,
	})

	// Fail twice.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/startup", nil)
		rr := httptest.NewRecorder()
		startupHandler(reg).ServeHTTP(rr, req)
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	}

	// Pass.
	req := httptest.NewRequest(http.MethodGet, "/startup", nil)
	rr := httptest.NewRecorder()
	startupHandler(reg).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 3, calls)

	// Cached.
	req = httptest.NewRequest(http.MethodGet, "/startup", nil)
	rr = httptest.NewRecorder()
	startupHandler(reg).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 3, calls) // no extra call
}

func TestHealthzEmptyRegistry(t *testing.T) {
	reg := NewProbeRegistry()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	healthzHandler(reg).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	report := decodeProbeReport(t, rr)
	assert.Equal(t, "ok", report.Status)
	assert.Empty(t, report.Results)
}

func TestReadyzEmptyRegistry(t *testing.T) {
	reg := NewProbeRegistry()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	readyzHandler(reg).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	report := decodeProbeReport(t, rr)
	assert.Equal(t, "ok", report.Status)
	assert.Empty(t, report.Results)
}

func TestStartupEmptyRegistry(t *testing.T) {
	reg := NewProbeRegistry()
	req := httptest.NewRequest(http.MethodGet, "/startup", nil)
	rr := httptest.NewRecorder()
	startupHandler(reg).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	report := decodeProbeReport(t, rr)
	assert.Equal(t, "ok", report.Status)
}

// Draining flag tests

func TestReadyzDrainingReturns503(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("db", nil, true))
	reg.Add(makeCheck("redis", nil, true))

	// Before draining: 200 OK.
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	readyzHandler(reg).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)

	reg.SetDraining()

	// After draining: 503.
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr = httptest.NewRecorder()
	readyzHandler(reg).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)

	report := decodeProbeReport(t, rr)
	assert.Equal(t, "draining", report.Status)
	require.Len(t, report.Results, 1)
	assert.Equal(t, "draining", report.Results[0].Name)
	assert.False(t, report.Results[0].Passed)
}

func TestHealthzStillPassesWhileDraining(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(NewDBProbe(func(ctx context.Context) error { return nil }))

	// Set draining.
	reg.SetDraining()

	// Healthz should still return 200: liveness is unaffected.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	healthzHandler(reg).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestStartupStillWorksWhileDraining(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("db", nil, true))

	// Set draining.
	reg.SetDraining()

	// Startup should still run checks: draining doesn't affect it.
	req := httptest.NewRequest(http.MethodGet, "/startup", nil)
	rr := httptest.NewRecorder()
	startupHandler(reg).ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestSetDrainingIsIdempotent(t *testing.T) {
	reg := NewProbeRegistry()

	// Double SetDraining should not panic.
	reg.SetDraining()
	reg.SetDraining()

	assert.True(t, reg.IsDraining())
}

func TestIsDrainingFalseByDefault(t *testing.T) {
	reg := NewProbeRegistry()
	assert.False(t, reg.IsDraining())
}

// WithHealthProbes RouterOption

func TestWithHealthProbes_SetsProbeRegistry(t *testing.T) {
	reg := NewProbeRegistry()
	var opts routerOptions
	opt := WithHealthProbes(reg)
	opt(&opts)

	assert.Same(t, reg, opts.healthProbes)
}

func TestWithHealthProbes_NilDoesNotPanic(t *testing.T) {
	var opts routerOptions
	opt := WithHealthProbes(nil)
	opt(&opts)

	assert.Nil(t, opts.healthProbes)
}

// ProbeReport JSON serialization

func TestProbeReport_JSON_Roundtrip(t *testing.T) {
	report := ProbeReport{
		Status:  "ok",
		Checked: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		Results: []ProbeResult{
			{Name: "db", Passed: true, Took: "2ms"},
			{Name: "redis", Passed: true, Took: "1ms"},
		},
	}

	var decoded ProbeReport
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	assert.Equal(t, report.Status, decoded.Status)
	assert.Equal(t, report.Checked, decoded.Checked)
	assert.Len(t, decoded.Results, 2)
	assert.Equal(t, "db", decoded.Results[0].Name)
	assert.True(t, decoded.Results[0].Passed)
}

// TestProbeReport_JSON_PoolErrorSanitized verifies that the Error() field in
// probe reports does not leak internal pool metrics to unauthenticated clients.
func TestProbeReport_JSON_PoolErrorSanitized(t *testing.T) {
	reg := NewProbeRegistry()
	// Pool at 85% - exceeds 80% threshold.
	reg.Add(NewPoolUtilizationProbe(func() (int, int) {
		return 85, 100
	}, 0.8))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	readyzHandler(reg).ServeHTTP(rr, req)

	// Saturation is advisory, so the pod stays in service and reports it.
	assert.Equal(t, http.StatusOK, rr.Code)

	var report ProbeReport
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&report))

	require.Len(t, report.Results, 1)
	assert.Equal(t, "pool_utilization", report.Results[0].Name)
	assert.False(t, report.Results[0].Passed)
	assert.Equal(t, "pool utilization exceeds threshold", report.Results[0].Error)

	// The error must NOT contain internal pool metrics.
	assert.NotContains(t, report.Results[0].Error, "85")
	assert.NotContains(t, report.Results[0].Error, "100")
	assert.NotContains(t, report.Results[0].Error, "80.0")
}

func TestProbeReport_JSON_WithErrors(t *testing.T) {
	report := ProbeReport{
		Status:  "degraded",
		Checked: "2025-06-01T12:00:00Z",
		Results: []ProbeResult{
			{Name: "db", Passed: true, Took: "1ms"},
			{Name: "redis", Passed: false, Error: "connection refused", Took: "2s"},
		},
	}

	var decoded ProbeReport
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	assert.Equal(t, "degraded", decoded.Status)
	assert.Equal(t, "connection refused", decoded.Results[1].Error)
}

// Context cancellation: probe timeout behavior

func TestLiveness_RespectsContextCancellation(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(ProbeCheck{
		Name: "slow-db",
		Check: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				return nil
			}
		},
		Required: true,
		Liveness: true,
		// The subject here is that cancellation reaches the caller, so the
		// reason has to be in the report to assert on.
		PublicError: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	report, _ := reg.Liveness(ctx)
	require.Len(t, report.Results, 1)
	assert.False(t, report.Results[0].Passed)
	assert.Contains(t, report.Results[0].Error, "deadline")
}

func TestReadiness_RespectsContextCancellation(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(ProbeCheck{
		Name: "slow-check",
		Check: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				return nil
			}
		},
		Required: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, allOK := reg.Readiness(ctx)
	assert.False(t, allOK)
}

// Pool utilization probe

func TestNewPoolUtilizationProbe_PassesUnderThreshold(t *testing.T) {
	probe := NewPoolUtilizationProbe(func() (int, int) {
		return 50, 100 // 50% utilization
	}, 0.8)
	assert.Equal(t, "pool_utilization", probe.Name)
	assert.False(t, probe.Required, "saturation is advisory: see NewPoolUtilizationProbe")

	err := probe.Check(context.Background())
	assert.NoError(t, err)
}

func TestNewPoolUtilizationProbe_FailsOverThreshold(t *testing.T) {
	probe := NewPoolUtilizationProbe(func() (int, int) {
		return 85, 100 // 85% utilization
	}, 0.8)

	err := probe.Check(context.Background())
	require.Error(t, err)
	assert.IsType(t, &poolUtilizationError{}, err)
	assert.Equal(t, "pool utilization exceeds threshold", err.Error())

	var pErr *poolUtilizationError
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, 85, pErr.inUse)
	assert.Equal(t, 100, pErr.maxOpen)
	assert.InDelta(t, 0.85, pErr.utilization, 0.001)
	assert.InDelta(t, 0.8, pErr.threshold, 0.001)
}

func TestNewPoolUtilizationProbe_ExactThreshold(t *testing.T) {
	// Exactly at threshold should pass (not exceed).
	probe := NewPoolUtilizationProbe(func() (int, int) {
		return 80, 100 // exactly 80%
	}, 0.8)

	err := probe.Check(context.Background())
	assert.NoError(t, err)
}

func TestNewPoolUtilizationProbe_ZeroMaxOpen(t *testing.T) {
	// Pool not configured: should skip check.
	probe := NewPoolUtilizationProbe(func() (int, int) {
		return 0, 0
	}, 0.8)

	err := probe.Check(context.Background())
	assert.NoError(t, err)
}

func TestNewPoolUtilizationProbe_PoolExhaustion(t *testing.T) {
	// Simulate pool exhaustion: all connections in use.
	probe := NewPoolUtilizationProbe(func() (int, int) {
		return 100, 100 // 100% utilization
	}, 0.8)

	err := probe.Check(context.Background())
	require.Error(t, err)
	assert.Equal(t, "pool utilization exceeds threshold", err.Error())

	var pErr *poolUtilizationError
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, 100, pErr.inUse)
	assert.Equal(t, 100, pErr.maxOpen)
}

func TestNewPoolUtilizationProbe_NearExhaustion(t *testing.T) {
	// Simulate near-exhaustion: 95/100 connections in use.
	probe := NewPoolUtilizationProbe(func() (int, int) {
		return 95, 100
	}, 0.8)

	err := probe.Check(context.Background())
	require.Error(t, err)

	var pErr *poolUtilizationError
	require.ErrorAs(t, err, &pErr)
	assert.InDelta(t, 0.95, pErr.utilization, 0.001)
}

func TestPoolUtilizationProbe_InReadiness_ReportedButAdvisory(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("db", nil, true))
	// Pool at 90% - exceeds 80% threshold.
	reg.Add(NewPoolUtilizationProbe(func() (int, int) {
		return 90, 100
	}, 0.8))

	report, allOK := reg.Readiness(context.Background())
	assert.True(t, allOK, "a busy pool must not unready the pod")
	assert.Equal(t, "ok", report.Status)

	// The pool_utilization check should be in the results.
	var found bool
	for _, r := range report.Results {
		if r.Name == "pool_utilization" {
			found = true
			assert.False(t, r.Passed)
			assert.Equal(t, "pool utilization exceeds threshold", r.Error)
		}
	}
	assert.True(t, found, "pool_utilization check not found in readiness report")
}

func TestPoolUtilizationProbe_InReadiness_Passes(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("db", nil, true))
	// Pool at 50% - under 80% threshold.
	reg.Add(NewPoolUtilizationProbe(func() (int, int) {
		return 50, 100
	}, 0.8))

	report, allOK := reg.Readiness(context.Background())
	assert.True(t, allOK)
	assert.Equal(t, "ok", report.Status)
}

func TestProbeRegistry_AwaitStartup_OpensGateWithoutAnExternalProbe(t *testing.T) {
	reg := NewProbeRegistry()
	var calls atomic.Int32
	reg.Add(ProbeCheck{
		Name: "plugins",
		Check: func(ctx context.Context) error {
			if calls.Add(1) < 3 {
				return errors.New("still starting")
			}
			return nil
		},
		Required: true,
	})
	gated := ReadinessGate(reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	gated.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/admin/setup", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.True(t, reg.AwaitStartup(ctx, time.Millisecond))
	assert.EqualValues(t, 3, calls.Load())

	rr = httptest.NewRecorder()
	gated.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/admin/setup", nil))
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestProbeRegistry_AwaitStartup_StopsWhenContextEnds(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(makeCheck("database", errors.New("connection refused"), true))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	assert.False(t, reg.AwaitStartup(ctx, time.Millisecond))
	assert.True(t, reg.StartTime().IsZero())
}
