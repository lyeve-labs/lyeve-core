package auth

import (
	"fmt"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// TOTPConfig holds parameters for TOTP key generation.
type TOTPConfig struct {
	Issuer string // shown against the entry in the user's authenticator app
	Period uint   // seconds per step (default 30)
	Digits otp.Digits
}

var defaultTOTPConfig = TOTPConfig{
	Issuer: security.DefaultTOTPIssuer,
	Period: 30,
	Digits: otp.DigitsSix,
}

// GenerateTOTP creates a new TOTP secret for accountName (typically user email).
// Returns the raw base32 secret and a provisioning URI suitable for QR codes.
func GenerateTOTP(accountName string, cfg *TOTPConfig) (secret string, provisioningURI string, err error) {
	if cfg == nil {
		cfg = &defaultTOTPConfig
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      cfg.Issuer,
		AccountName: accountName,
		Period:      cfg.Period,
		Digits:      cfg.Digits,
	})
	if err != nil {
		return "", "", fmt.Errorf("totp: generate key: %w", err)
	}
	return key.Secret(), key.URL(), nil
}

// VerifyTOTP checks a 6-digit TOTP code against the given base32 secret.
// Uses a clock skew tolerance of ±1 step (default ±30 s).
func VerifyTOTP(secret, code string) bool {
	valid, err := totp.ValidateCustom(code, secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	return err == nil && valid
}
