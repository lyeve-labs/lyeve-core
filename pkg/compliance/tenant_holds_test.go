package compliance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

// fakeTenantHoldChecker answers both questions and records the resource type
// the tenant question was asked about.
type fakeTenantHoldChecker struct {
	fakeHoldChecker
	tenantHolds []compliance.HoldRef
	tenantErr   error
	askedType   string
}

func (f *fakeTenantHoldChecker) TenantHolds(_ context.Context, resourceType string) ([]compliance.HoldRef, error) {
	f.askedType = resourceType
	return f.tenantHolds, f.tenantErr
}

func TestCheckTenantHolds_Answers(t *testing.T) {
	t.Cleanup(compliance.ResetHoldChecker)
	down := errors.New("retention store is down")
	tests := []struct {
		name      string
		checker   compliance.HoldChecker
		proceed   bool
		wantHolds int
		wantErr   error
	}{
		{name: "no checker holds nothing", checker: nil},
		{
			name:    "a checker without the question holds nothing",
			checker: &fakeHoldChecker{holds: []compliance.HoldRef{{ID: "subject-hold"}}},
		},
		{
			name:      "a checker with the question answers it",
			checker:   &fakeTenantHoldChecker{tenantHolds: []compliance.HoldRef{{ID: "h-1", Reason: "litigation"}}},
			wantHolds: 1,
		},
		{
			name:    "a failed check refuses",
			checker: &fakeTenantHoldChecker{tenantErr: down},
			wantErr: compliance.ErrHoldCheckUnavailable,
		},
		{
			name:    "a failed check refuses when erasure would proceed",
			checker: &fakeTenantHoldChecker{tenantErr: down},
			proceed: true,
			wantErr: compliance.ErrHoldCheckUnavailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			compliance.ResetHoldChecker()
			if tc.checker != nil {
				compliance.RegisterHoldChecker(tc.checker)
			}
			if tc.proceed {
				t.Setenv("LYEVE_GDPR_HOLD_CHECK", "proceed")
			}

			holds, err := compliance.CheckTenantHolds(context.Background(), "content_entry")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if len(holds) != tc.wantHolds {
				t.Fatalf("holds = %v, want %d", holds, tc.wantHolds)
			}
			if f, ok := tc.checker.(*fakeTenantHoldChecker); ok && f.askedType != "content_entry" {
				t.Errorf("asked about %q, want the caller's resource type", f.askedType)
			}
			if f, ok := tc.checker.(*fakeHoldChecker); ok && f.called != 0 {
				t.Errorf("subject question asked %d times, want none", f.called)
			}
		})
	}
}
