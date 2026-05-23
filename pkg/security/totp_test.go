package security

import (
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	require.NoError(t, err)
	return code
}

func TestMatchTOTPStep_ReportsTheCodesOwnStep(t *testing.T) {
	t.Parallel()
	secret, _, err := GenerateTOTP("user@example.com", "")
	require.NoError(t, err)
	now := time.Unix(1_800_000_015, 0).UTC()
	current := now.Unix() / 30

	for _, offset := range []int64{-1, 0, 1} {
		step, ok := MatchTOTPStep(secret, codeAt(t, secret, time.Unix((current+offset)*30, 0)), now)
		require.True(t, ok, "offset %d", offset)
		assert.Equal(t, current+offset, step, "offset %d", offset)
	}

	_, ok := MatchTOTPStep(secret, codeAt(t, secret, time.Unix((current+2)*30, 0)), now)
	assert.False(t, ok, "two steps ahead is outside the window")
	_, ok = MatchTOTPStep(secret, " "+codeAt(t, secret, now)+" ", now)
	assert.True(t, ok, "surrounding space is ignored")
	_, ok = MatchTOTPStep(secret, "12345", now)
	assert.False(t, ok, "a short code is refused")
}

// A code from the next step is recorded as its own step, not the current one.
// Recording the current step would let it work again once the next step
// arrived.
func TestVerifyTOTPNoReplay_NextStepCodeWorksOnce(t *testing.T) {
	t.Parallel()
	secret, _, err := GenerateTOTP("user@example.com", "")
	require.NoError(t, err)
	next := time.Now().Add(30 * time.Second)
	code := codeAt(t, secret, next)

	ok, step := VerifyTOTPNoReplay(secret, code, 0)
	require.True(t, ok)
	assert.Equal(t, next.Unix()/30, step)

	ok, _ = VerifyTOTPNoReplay(secret, code, step)
	assert.False(t, ok, "the same code against its recorded step is a replay")
}
