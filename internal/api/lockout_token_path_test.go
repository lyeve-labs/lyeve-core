package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// countingLockout records what each login path does to the counter, which is
// the whole subject here: a path may read it and never write it, and nothing
// about the responses shows that.
type countingLockout struct {
	failCount int
	recorded  []uuid.UUID
}

func (c *countingLockout) ReserveAttempt(_ context.Context, userID uuid.UUID, _ int) (Reservation, bool, error) {
	c.recorded = append(c.recorded, userID)
	return Reservation{Count: c.failCount + len(c.recorded)}, true, nil
}

func (c *countingLockout) ReleaseAttempt(context.Context, uuid.UUID, Reservation) error {
	return nil
}

// Both login paths must feed the lockout they check.
//
// A missing write has no symptom. The endpoint would answer correctly for
// every legitimate caller and refuse nobody, exactly as it would with a
// working lockout that no account had tripped. Hence a test that asserts on
// the counter rather than on a status.
func TestLockout_BothLoginPathsRecordAFailure(t *testing.T) {
	t.Parallel()

	const password = "correct-horse-battery-staple"
	hash, err := auth.HashPassword("bcrypt", password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	uid := uuid.New()

	for _, tc := range []struct {
		name string
		path string
		call func(*AuthHandler, http.ResponseWriter, *http.Request)
	}{
		{"admin login", "/api/admin/auth/login", func(h *AuthHandler, w http.ResponseWriter, r *http.Request) { h.Login(w, r) }},
		{"api token", "/api/v1/auth/token", func(h *AuthHandler, w http.ResponseWriter, r *http.Request) { h.Token(w, r) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			users := &fakeUserStore{
				byEmail: map[string]*domain.User{
					"user@test.com": {ID: uid, Email: "user@test.com", PasswordHash: hash},
				},
			}
			lockout := &countingLockout{}
			h := NewAuthHandler(users, nil, nil, "secret", 3600, false)
			h.WithLockoutChecker(lockout)

			body := makeLoginBody("user@test.com", "not-the-password")
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			tc.call(h, rr, req)

			if len(lockout.recorded) != 1 {
				t.Fatalf("%s recorded %d failures, want 1: a path that checks the lockout must also feed it, or the control is absent while reporting as present",
					tc.path, len(lockout.recorded))
			}
			if lockout.recorded[0] != uid {
				t.Errorf("recorded against %s, want %s", lockout.recorded[0], uid)
			}
		})
	}
}
