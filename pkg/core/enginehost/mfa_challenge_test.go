package enginehost

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type fixedCompleter struct{ token string }

func (c fixedCompleter) MFAChallengeAccount(context.Context, string) (core.MFAChallengeAccount, error) {
	return core.MFAChallengeAccount{Email: "a@example.com"}, nil
}

func (c fixedCompleter) CompleteMFAChallenge(context.Context, string) (string, error) {
	return c.token, nil
}

// Before the admin router wires a completion, a challenge cannot complete,
// and the answer is an outage rather than a refused credential. Once wired,
// the host forwards to it.
func TestEngineHost_CompletesChallengesThroughTheWiredCompletion(t *testing.T) {
	host := &engineHost{}
	var c core.MFAChallengeCompleter = host

	_, err := c.CompleteMFAChallenge(context.Background(), "challenge")
	require.Error(t, err)
	assert.NotErrorIs(t, err, core.ErrMFAChallengeInvalid)
	_, err = c.MFAChallengeAccount(context.Background(), "challenge")
	require.Error(t, err)

	host.SetMFAChallengeCompleter(fixedCompleter{token: "session"})
	token, err := c.CompleteMFAChallenge(context.Background(), "challenge")
	require.NoError(t, err)
	assert.Equal(t, "session", token)
	account, err := c.MFAChallengeAccount(context.Background(), "challenge")
	require.NoError(t, err)
	assert.Equal(t, "a@example.com", account.Email)
}
