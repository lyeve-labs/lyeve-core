package compliance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

// fakeHoldChecker answers a fixed verdict and records that it was consulted.
type fakeHoldChecker struct {
	holds  []compliance.HoldRef
	err    error
	called int
}

func (f *fakeHoldChecker) SubjectHolds(context.Context, string) ([]compliance.HoldRef, error) {
	f.called++
	return f.holds, f.err
}

// countingEraser records every subject it was asked to erase, so a test can
// assert the fan-out did not run rather than only that it reported no rows.
type countingEraser struct {
	calls int
	rows  int64
}

func (c *countingEraser) EraseSubject(context.Context, string) (int64, error) {
	c.calls++
	return c.rows, nil
}

func TestSubjectErasure_HeldSubjectIsNotErased(t *testing.T) {
	t.Cleanup(func() {
		compliance.ResetHoldChecker()
		compliance.ResetSubjectErasers()
	})

	tests := []struct {
		name        string
		checker     *fakeHoldChecker
		proceedEnv  string
		wantRows    int64
		wantHolds   int
		wantErasure bool
		wantErr     error
	}{
		{
			name:        "no checker registered erases, because no hold can exist",
			checker:     nil,
			wantRows:    3,
			wantErasure: true,
		},
		{
			name:        "no holds erases",
			checker:     &fakeHoldChecker{},
			wantRows:    3,
			wantErasure: true,
		},
		{
			name:        "an active hold skips the fan-out and names the hold",
			checker:     &fakeHoldChecker{holds: []compliance.HoldRef{{ID: "h-1", Reason: "litigation"}}},
			wantRows:    0,
			wantHolds:   1,
			wantErasure: false,
		},
		{
			name:        "a checker that cannot answer refuses rather than guess",
			checker:     &fakeHoldChecker{err: errors.New("retention store is down")},
			wantRows:    0,
			wantErasure: false,
			wantErr:     compliance.ErrHoldCheckUnavailable,
		},
		{
			name:        "an install that opted out erases through a failed check",
			checker:     &fakeHoldChecker{err: errors.New("retention store is down")},
			proceedEnv:  "proceed",
			wantRows:    3,
			wantErasure: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			compliance.ResetHoldChecker()
			compliance.ResetSubjectErasers()
			if tc.checker != nil {
				compliance.RegisterHoldChecker(tc.checker)
			}
			if tc.proceedEnv != "" {
				t.Setenv("LYEVE_GDPR_HOLD_CHECK", tc.proceedEnv)
			}

			eraser := &countingEraser{rows: 3}
			compliance.RegisterSubjectEraser(eraser)

			res, err := compliance.RunSubjectErasureWithHolds(context.Background(), "subject@example.com")

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if res.Rows != tc.wantRows {
				t.Errorf("rows = %d, want %d", res.Rows, tc.wantRows)
			}
			if len(res.Holds) != tc.wantHolds {
				t.Errorf("holds = %d, want %d", len(res.Holds), tc.wantHolds)
			}
			// The row count alone cannot tell a skipped fan-out from erasers
			// that matched nothing, and those are the two outcomes this whole
			// feature exists to keep apart.
			if ran := eraser.calls > 0; ran != tc.wantErasure {
				t.Errorf("erasers ran = %v, want %v", ran, tc.wantErasure)
			}
			if tc.checker != nil && tc.checker.called != 1 {
				t.Errorf("checker consulted %d times, want exactly 1", tc.checker.called)
			}
		})
	}
}

// RunSubjectErasure honors holds as RunSubjectErasureWithHolds does. Callers
// use both entry points, so a hold check that reached only the other one would
// miss every caller of this.
func TestRunSubjectErasure_HonorsHolds(t *testing.T) {
	t.Cleanup(func() {
		compliance.ResetHoldChecker()
		compliance.ResetSubjectErasers()
	})
	compliance.ResetSubjectErasers()
	compliance.RegisterHoldChecker(&fakeHoldChecker{holds: []compliance.HoldRef{{ID: "h-2"}}})
	eraser := &countingEraser{rows: 5}
	compliance.RegisterSubjectEraser(eraser)

	rows, err := compliance.RunSubjectErasure(context.Background(), "subject@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rows != 0 {
		t.Errorf("rows = %d, want 0 for a held subject", rows)
	}
	if eraser.calls != 0 {
		t.Errorf("erasers ran %d times on a held subject, want 0", eraser.calls)
	}
}
