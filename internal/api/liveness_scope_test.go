package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Under sustained load the pool-utilization probe fails, and a liveness
// failure restarts a pod that is only busy. /healthz is wired to the kubelet's
// livenessProbe in deploy/kubernetes.yaml, and a restart does not reduce the
// load that saturated the pool.
func TestLiveness_PoolSaturationDoesNotKillThePod(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(NewDBProbe(func(context.Context) error { return nil }))
	reg.Add(NewPoolUtilizationProbe(func() (int, int) { return 25, 25 }, 0.8))

	rec := httptest.NewRecorder()
	healthzHandler(reg)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz with a saturated pool = %d, want 200", rec.Code)
	}

	// Nor does it take the pod out of service. Load is not a reason to stop
	// answering: the load moves to peers that are just as loaded, and the
	// reference deployment runs one replica, so the first pod over the line is
	// the whole service. It is still reported, because operators should see it.
	report, ok := reg.Readiness(context.Background())
	if !ok {
		t.Error("readiness failed with a saturated pool, want pass")
	}
	if !hasFailedProbe(report, "pool_utilization") {
		t.Error("readiness report does not report pool_utilization as saturated")
	}
}

// The database is the one dependency whose loss means the engine has nothing
// to serve, so it stays the liveness signal.
func TestLiveness_UnreachableDatabaseStillFails(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(NewDBProbe(func(context.Context) error { return errors.New("database unavailable") }))

	rec := httptest.NewRecorder()
	healthzHandler(reg)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/healthz with an unreachable database = %d, want 503", rec.Code)
	}
}

// Every other Required probe answers "should this pod take traffic", not
// "is this process broken".
func TestLiveness_DependencyProbesAreReadinessOnly(t *testing.T) {
	reg := NewProbeRegistry()
	reg.Add(NewDBProbe(func(context.Context) error { return nil }))
	reg.Add(NewRedisProbe(func(context.Context) error { return errors.New("cache unreachable") }, true))

	rec := httptest.NewRecorder()
	healthzHandler(reg)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz with an unreachable cache = %d, want 200", rec.Code)
	}
	if _, ok := reg.Readiness(context.Background()); ok {
		t.Error("readiness passed with an unreachable cache, want failure")
	}
}

func hasFailedProbe(report ProbeReport, name string) bool {
	for _, r := range report.Results {
		if r.Name == name && !r.Passed {
			return true
		}
	}
	return false
}
