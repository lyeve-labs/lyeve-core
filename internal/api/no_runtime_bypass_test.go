package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Nothing reachable at runtime may weaken a control. The binary carries one set
// of thresholds and one set of probe semantics. A request cannot select another.

func TestLockoutPolicy_IsConstant(t *testing.T) {
	t.Parallel()

	maxFailedAttempts, lockoutWindow := lockoutPolicy()

	if maxFailedAttempts != 5 {
		t.Errorf("MaxFailedAttempts = %d; must stay at 5", maxFailedAttempts)
	}
	if lockoutWindow != 15*time.Minute {
		t.Errorf("LockoutWindow = %s; must stay at 15m", lockoutWindow)
	}
}

func TestHealthProbes_IgnoreInjectionQuery(t *testing.T) {
	t.Parallel()

	// Query parameters a fault injector would read. A healthy pool must answer
	// 200 to every one of them.
	queries := []string{
		"?force_db_fail=true",
		"?debug=1",
		"?fail=database",
		"?test=1",
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			t.Parallel()

			for name, h := range map[string]http.HandlerFunc{
				"health": healthHandlerFn(&fakeDB{engine: "postgres"}),
				"ready":  readyHandlerFn(&fakeDB{engine: "postgres"}, func() int { return 5 }),
			} {
				rr := httptest.NewRecorder()
				h(rr, httptest.NewRequest(http.MethodGet, "/"+name+q, nil))

				if rr.Code != http.StatusOK {
					t.Errorf("%s%s = %d, want %d: a query parameter changed probe behavior",
						name, q, rr.Code, http.StatusOK)
				}
			}
		})
	}
}
