package api

import (
	"context"
	"errors"
	"testing"
)

// The probe endpoints take no credential, so whatever a probe returns is
// served to whoever asks. A probe that passed a driver error straight through
// would answer an anonymous request with the host and port of a backend.
func TestPublicProbeError_WithholdsWhatTheProbeDidNotDeclare(t *testing.T) {
	c := ProbeCheck{Name: "redis"}
	got := publicProbeError(c, errors.New("dial tcp 10.0.3.14:6379: connect: connection refused"))

	if got == "dial tcp 10.0.3.14:6379: connect: connection refused" {
		t.Fatal("the backend address reached an unauthenticated response")
	}
	if got != probeUnavailable {
		t.Fatalf("got %q, want %q", got, probeUnavailable)
	}
}

// A probe whose failure text is a fixed string it chose keeps it, so an
// operator still reads something useful on the two probes that were written
// for this.
func TestPublicProbeError_KeepsWhatTheProbeDeclared(t *testing.T) {
	c := ProbeCheck{Name: "database", PublicError: true}
	if got := publicProbeError(c, errors.New("database unavailable")); got != "database unavailable" {
		t.Fatalf("got %q, want the probe's own text", got)
	}
}

// The default is withholding. A probe added later without thinking about this
// gets the safe answer rather than the leak.
func TestProbeCheck_DefaultsToWithholding(t *testing.T) {
	var c ProbeCheck
	if c.PublicError {
		t.Fatal("PublicError must default to false")
	}
}

// The registry has to reach the helper, not only its own test. Both Readiness
// and Liveness fill the same field, so both must withhold.
func TestReadinessAndLiveness_BothWithhold(t *testing.T) {
	leak := errors.New("dial tcp 10.0.3.14:6379: connect: connection refused")

	reg := NewProbeRegistry()
	reg.Add(ProbeCheck{
		Name:     "redis",
		Check:    func(context.Context) error { return leak },
		Required: false,
	})
	reg.Add(ProbeCheck{
		Name:     "beyond-repair",
		Check:    func(context.Context) error { return leak },
		Liveness: true,
	})

	ready, _ := reg.Readiness(context.Background())
	for _, pr := range ready.Results {
		if pr.Error != probeUnavailable {
			t.Errorf("readiness leaked %q for %s", pr.Error, pr.Name)
		}
	}

	live, err := reg.Liveness(context.Background())
	if err != nil {
		t.Fatalf("liveness: %v", err)
	}
	for _, pr := range live.Results {
		if pr.Error != probeUnavailable {
			t.Errorf("liveness leaked %q for %s", pr.Error, pr.Name)
		}
	}
}
