package api

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
)

// stubMFAStore satisfies security.MFAStore without a real MFA backend, so
// tests that create AuthHandler with a non-nil MFAStore can exercise
// request parsing and token validation paths.
type stubMFAStore struct{}

func (s *stubMFAStore) IsEnabled(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, nil
}

func (s *stubMFAStore) GetEnabled(_ context.Context, _ uuid.UUID) (string, []string, error) {
	return "", nil, errors.New("MFA not configured")
}

func (s *stubMFAStore) UpdateBackupCodes(_ context.Context, _ uuid.UUID, _ []string) error {
	return errors.New("not implemented")
}

func (s *stubMFAStore) HasWebAuthn(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, nil
}

func (s *stubMFAStore) DisableByAdmin(_ context.Context, _, _ uuid.UUID, _ string) error {
	return errors.New("not implemented")
}

func (s *stubMFAStore) GracePeriodHours() int {
	return 0
}

func (s *stubMFAStore) ConsumeTOTPStep(_ context.Context, _ uuid.UUID, _ int64) (bool, error) {
	return true, nil
}

// totpStepLog keeps each user's last accepted TOTP step the way the MFA
// plugin's table does, so a fake store refuses a replayed code. The zero
// value is ready to use.
type totpStepLog struct {
	mu   sync.Mutex
	last map[uuid.UUID]int64
}

func (l *totpStepLog) consume(userID uuid.UUID, step int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = make(map[uuid.UUID]int64)
	}
	if step <= l.last[userID] {
		return false
	}
	l.last[userID] = step
	return true
}

func (s *stubMFAStore) RegenerateBackupCodes(_ context.Context, _ uuid.UUID, _ int) ([]string, error) {
	return nil, errors.New("not implemented")
}
