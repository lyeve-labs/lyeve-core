package plugintest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type fixedRevoker int

func (r fixedRevoker) RevokePasskeys(context.Context, uuid.UUID) (int, error) { return int(r), nil }

// A plugin test that never registers a revoker sees the removal fail, so a
// reset under test cannot pass by removing nothing.
func TestNewHost_PasskeyRevokerAnswersAnOutageWhenUnset(t *testing.T) {
	r, ok := NewHost(t, nil).(core.PasskeyRevoker)
	if !ok {
		t.Fatal("the test host offers passkey removal")
	}
	if n, err := r.RevokePasskeys(context.Background(), uuid.New()); !errors.Is(err, core.ErrServiceUnavailable) || n != 0 {
		t.Fatalf("RevokePasskeys = %d, %v: want 0 and an outage", n, err)
	}

	r = NewHost(t, nil, WithPasskeyRevoker(fixedRevoker(4))).(core.PasskeyRevoker)
	if n, err := r.RevokePasskeys(context.Background(), uuid.New()); err != nil || n != 4 {
		t.Fatalf("RevokePasskeys = %d, %v: want the registered revoker's answer", n, err)
	}
}
