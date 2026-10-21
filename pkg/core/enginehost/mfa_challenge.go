package enginehost

import (
	"context"
	"errors"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// errMFAChallengeUnwired is the answer before the admin router has handed
// the host its challenge completer. It is not a refused credential, so a
// sign-in route answers it as an outage the client may retry.
var errMFAChallengeUnwired = errors.New("mfa challenge completion is not available yet")

type mfaChallengeBox struct{ completer core.MFAChallengeCompleter }

// SetMFAChallengeCompleter wires the engine's own challenge completion, the
// one its code sign-in uses, so a plugin completes a challenge against the
// same single-use record. Nil clears it.
func (h *engineHost) SetMFAChallengeCompleter(c core.MFAChallengeCompleter) {
	h.mfaChallenges.Store(&mfaChallengeBox{completer: c})
}

func (h *engineHost) mfaChallengeCompleter() core.MFAChallengeCompleter {
	if b := h.mfaChallenges.Load(); b != nil {
		return b.completer
	}
	return nil
}

// MFAChallengeAccount implements core.MFAChallengeCompleter.
func (h *engineHost) MFAChallengeAccount(ctx context.Context, challengeToken string) (core.MFAChallengeAccount, error) {
	c := h.mfaChallengeCompleter()
	if c == nil {
		return core.MFAChallengeAccount{}, errMFAChallengeUnwired
	}
	return c.MFAChallengeAccount(ctx, challengeToken)
}

// CompleteMFAChallenge implements core.MFAChallengeCompleter.
func (h *engineHost) CompleteMFAChallenge(ctx context.Context, challengeToken string) (string, error) {
	c := h.mfaChallengeCompleter()
	if c == nil {
		return "", errMFAChallengeUnwired
	}
	return c.CompleteMFAChallenge(ctx, challengeToken)
}
