package security

import (
	"crypto/subtle"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// DefaultTOTPIssuer labels an enrollment when the caller names no issuer. It
// is the product name, and this string is what an authenticator app files the
// account under for the rest of its life. Changing it relabels new enrollments
// only. An enrollment already made keeps the label its app recorded, and
// neither the secret nor any code depends on this value.
const DefaultTOTPIssuer = "LyEve"

// GenerateTOTP creates a new TOTP secret for the given account name and issuer.
// Returns the base32 secret and a provisioning URI suitable for QR code display.
//
// The issuer is what an authenticator app files the entry under, so it is the
// product name a user sees for the rest of the account's life. Callers that
// know the deployment's own branding should pass it. DefaultTOTPIssuer is the
// fallback for those that do not.
func GenerateTOTP(accountName, issuer string) (secret, uri string, err error) {
	if issuer == "" {
		issuer = DefaultTOTPIssuer
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: accountName,
		Period:      30,
		Digits:      otp.DigitsSix,
	})
	if err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

// totpPeriod is the length of one TOTP time step in seconds.
const totpPeriod = 30

// VerifyTOTPNoReplay validates a TOTP code and refuses one whose time step is
// not later than lastTimestep. It returns (true, step) for an accepted code,
// where step is the time step the code belongs to, and (false, 0) otherwise.
// lastTimestep of 0 means no code has been accepted yet.
//
// The step recorded is the code's own step, not the current one. A code is
// valid for one step either side of now, so recording the current step would
// let a code from the next step be used again once that step arrives.
func VerifyTOTPNoReplay(secret, code string, lastTimestep int64) (bool, int64) {
	step, ok := MatchTOTPStep(secret, code, time.Now().UTC())
	if !ok || step <= lastTimestep {
		return false, 0
	}
	return true, step
}

// MatchTOTPStep reports the time step whose code equals code, checking the
// step holding now and one step either side. When two steps produce the same
// code it reports the later, so a replay check that records it errs toward
// refusing.
func MatchTOTPStep(secret, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != int(otp.DigitsSix) {
		return 0, false
	}
	current := now.Unix() / totpPeriod
	var matched int64
	found := false
	for step := current - 1; step <= current+1; step++ {
		want, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriod, 0).UTC(), totp.ValidateOpts{
			Period:    totpPeriod,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			matched, found = step, true
		}
	}
	return matched, found
}
