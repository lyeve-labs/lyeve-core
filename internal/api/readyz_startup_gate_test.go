package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ReadinessGate refuses every non-probe request until the startup probe has
// passed once. A readyz that answers 200 before then tells a load balancer to
// send traffic the node will only answer with 503.
func TestReadyz_IsNotReadyWhileStartupFails(t *testing.T) {
	t.Parallel()

	failing := true
	reg := NewProbeRegistry()
	reg.Add(ProbeCheck{Name: "dependency", Required: true, Check: func(context.Context) error {
		if failing {
			return errors.New("not up yet")
		}
		return nil
	}})
	h := readyzHandler(reg)

	t.Run("while startup cannot pass", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 while the gate is shut", rr.Code)
		}
		var report ProbeReport
		if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
			t.Fatalf("body is not a probe report: %v", err)
		}
		if report.Status != "starting" {
			t.Errorf("status = %q, want starting", report.Status)
		}
		if rr.Header().Get("Retry-After") == "" {
			t.Error("a caller told to wait needs to know for how long")
		}
	})

	// Nothing runs the startup probe at boot: only the /startup handler does -
	// so readyz has to run it rather than wait for a flag someone else sets.
	t.Run("once the dependency comes up, without anyone polling /startup", func(t *testing.T) {
		failing = false
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
		}
		if reg.StartTime().IsZero() {
			t.Error("readyz must record the startup pass, which is what opens the gate")
		}
	})
}
