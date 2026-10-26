package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

type failingHoldChecker struct{}

func (failingHoldChecker) SubjectHolds(context.Context, string) ([]compliance.HoldRef, error) {
	return nil, errors.New("retention store is down")
}

// A checker that cannot answer refuses the erasure unless the install set
// LYEVE_GDPR_HOLD_CHECK=proceed. The analytics self-service route asks the
// same function, so the two routes answer alike under each setting.
func TestDSARHandler_Erase_FailedHoldCheckFollowsTheSetting(t *testing.T) {
	t.Cleanup(func() {
		compliance.ResetHoldChecker()
		compliance.ResetSubjectErasers()
	})
	for _, tc := range []struct {
		setting    string
		wantStatus int
		wantErased bool
	}{
		{"", http.StatusServiceUnavailable, false},
		{"refuse", http.StatusServiceUnavailable, false},
		{"proceed", http.StatusOK, true},
	} {
		t.Run("setting="+tc.setting, func(t *testing.T) {
			compliance.ResetHoldChecker()
			compliance.ResetSubjectErasers()
			compliance.RegisterHoldChecker(failingHoldChecker{})
			eraser := &recordingEraser{}
			compliance.RegisterSubjectEraser(eraser)
			t.Setenv("LYEVE_GDPR_HOLD_CHECK", tc.setting)

			req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/erase",
				strings.NewReader(`{"identifier":"subject@example.com"}`))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			NewDSARHandler(&mockAuditWriter{}, nil, nil).Erase(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if erased := len(eraser.seen) > 0; erased != tc.wantErased {
				t.Fatalf("erasers ran = %v, want %v", erased, tc.wantErased)
			}
		})
	}
}
