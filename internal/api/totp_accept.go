package api

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// totpOutcome is the result of presenting a TOTP code.
type totpOutcome int

const (
	totpInvalid  totpOutcome = iota // no time step near now produces the code
	totpReplayed                    // the code is valid but its step was used already
	totpAccepted                    // the code is valid and its step is now used
)

// acceptTOTP checks a code against the secret and consumes its time step in
// the MFA store, so each code signs in or confirms once. Sign-in, step-up and
// the MFA plugin's own routes record into the same step, so a code accepted
// on one is refused on every other. An error means the store could not say,
// and the caller fails closed.
func acceptTOTP(ctx context.Context, store security.MFAStore, userID uuid.UUID, secret, code string) (totpOutcome, error) {
	ok, step := security.VerifyTOTPNoReplay(secret, code, 0)
	if !ok {
		return totpInvalid, nil
	}
	consumed, err := store.ConsumeTOTPStep(ctx, userID, step)
	if err != nil {
		return totpInvalid, fmt.Errorf("consume totp step: %w", err)
	}
	if !consumed {
		return totpReplayed, nil
	}
	return totpAccepted, nil
}
