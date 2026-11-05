package enginehost

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// countingRevoker removes count passkeys for every account it is asked about.
type countingRevoker struct {
	count int
	asked []uuid.UUID
}

func (r *countingRevoker) RevokePasskeys(_ context.Context, userID uuid.UUID) (int, error) {
	r.asked = append(r.asked, userID)
	return r.count, nil
}

// revokerSource stands in for the activator: what it answers changes as the
// plugin holding the role starts and stops.
type revokerSource struct{ current core.PasskeyRevoker }

func (s *revokerSource) PasskeyRevoker() core.PasskeyRevoker { return s.current }

// The host asks the source on every call, so a holder that starts after the
// caller, or stops, is seen by the next reset.
func TestEngineHost_RevokePasskeysAsksTheRunningHolder(t *testing.T) {
	src := &revokerSource{}
	h := &engineHost{}
	h.WithPasskeyRevokers(src)
	var host core.PasskeyRevoker = h
	user := uuid.New()

	holder := &countingRevoker{count: 3}
	src.current = holder
	n, err := host.RevokePasskeys(context.Background(), user)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, []uuid.UUID{user}, holder.asked)

	src.current = failingRevoker{}
	_, err = host.RevokePasskeys(context.Background(), user)
	assert.Error(t, err, "the holder's failure reaches the caller")
}

type failingRevoker struct{}

func (failingRevoker) RevokePasskeys(context.Context, uuid.UUID) (int, error) {
	return 0, errors.New("passkey store unreachable")
}

// With no running plugin that stores passkeys, no passkey can sign in, so a
// reset removes nothing and goes ahead. The same holds before the runtime
// wires a source.
func TestEngineHost_RevokePasskeysWithNoHolderRemovesNothing(t *testing.T) {
	unwired := &engineHost{}
	n, err := unwired.RevokePasskeys(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Zero(t, n)

	h := &engineHost{}
	h.WithPasskeyRevokers(&revokerSource{})
	n, err = h.RevokePasskeys(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Zero(t, n)
}
