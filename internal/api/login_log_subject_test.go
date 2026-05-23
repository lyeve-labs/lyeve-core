//go:build !mutest

package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// alertingRiskAssessor scores every sign-in as suspicious and asks for a
// second factor, so both risk lines of a password login are written.
type alertingRiskAssessor struct{ forceMFARiskAssessor }

func (alertingRiskAssessor) Assess(context.Context, uuid.UUID, security.DeviceFingerprint) (*security.RiskAssessment, error) {
	return &security.RiskAssessment{Level: security.RiskHigh, Score: 80, Reason: "new device", RequireMFA: true, RequireAlert: true}, nil
}

// The logging plugin stores log lines, so the risk lines of a sign-in name the
// account by its id and the digest of its address, never by the address.
func TestAuthIntegration_Login_RiskLinesLogADigestNotTheAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	const address = "risk-log-subject@example.com"
	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"
	seedUser(t, pool, address, "correct-password", []string{"editor"})
	h := NewAuthHandler(db.NewUserStore(pool), nil, pool, secret, 3600, false)
	h.WithDeviceRiskAssessor(alertingRiskAssessor{})

	var out bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login",
		strings.NewReader(makeLoginBody(address, "correct-password")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Login(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}

	logged := out.String()
	for _, message := range []string{"suspicious login detected", "risk escalation wanted MFA but the account has none enrolled"} {
		if !strings.Contains(logged, message) {
			t.Errorf("expected %q in the log: %s", message, logged)
		}
	}
	if strings.Contains(logged, address) {
		t.Errorf("the log names the address: %s", logged)
	}
	if n := strings.Count(logged, `"subject_ref":"`+compliance.SubjectRef(address)+`"`); n != 2 {
		t.Errorf("subject_ref appears %d times, want 2: %s", n, logged)
	}
}
