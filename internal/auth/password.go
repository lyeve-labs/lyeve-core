package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"

	"crypto/rand"
)

// HashPassword hashes password using the given algorithm.
// Supported values: "bcrypt" (default), "argon2id".
func HashPassword(algo, password string) (string, error) {
	switch algo {
	case "argon2id":
		return hashArgon2id(password)
	default: // "bcrypt" or anything else
		h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return "", fmt.Errorf("bcrypt hash: %w", err)
		}
		return string(h), nil
	}
}

// OAuthOnlyMarker is the sentinel stored in the password hash field for
// accounts created via OAuth2/OIDC that must never authenticate with a password.
const OAuthOnlyMarker = "!OAUTH_ONLY!"

// ExternalIssuerMarker is the sentinel stored in the password hash field of
// the local account a trusted issuer's user acts as. The issuer decides who
// the user is and, through its policy, what they hold, so the account never
// signs in any other way: no password, no reset, no magic link. A separate
// marker from OAuthOnlyMarker so plugins can refuse these accounts without
// changing what an OAuth-created account may do.
const ExternalIssuerMarker = "!EXTERNAL_ISSUER!"

// DummyBcryptHash is a fixed bcrypt hash at the cost real bcrypt hashes use.
// A login for an email with no account compares the password against it, so
// the handler runs bcrypt.CompareHashAndPassword whether the email exists or
// not, and response time does not reveal which emails are registered. The
// result is always discarded, so the plaintext behind the hash does not
// matter.
const DummyBcryptHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// VerifyPassword checks that password matches the stored hash.
// The algorithm is auto-detected from the hash prefix:
//   - "$argon2id$..." -> argon2id
//   - "$2a$..." / "$2b$..." -> bcrypt
//
// The special sentinel OAuthOnlyMarker is rejected immediately: these accounts
// are OAuth-only and password authentication is disabled.
func VerifyPassword(_, hash, password string) error {
	if hash == OAuthOnlyMarker {
		return errors.New("account is oauth-only: no password authentication")
	}
	if hash == ExternalIssuerMarker {
		return errors.New("account belongs to a trusted issuer: no password authentication")
	}
	if strings.HasPrefix(hash, "$argon2id$") {
		return verifyArgon2id(hash, password)
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// Argon2id helpers

// argonTime and related constants are deliberately conservative argon2id
// parameters suitable for a CMS.
const (
	argonTime    uint32 = 1
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// hashArgon2id produces a PHC-format string: $argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>
func hashArgon2id(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("argon2id salt: %w", err)
	}
	h := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt),
		enc.EncodeToString(h),
	), nil
}

// verifyArgon2id parses a PHC hash string and verifies password.
func verifyArgon2id(encoded, password string) error {
	parts := strings.Split(encoded, "$")
	// $argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>
	//  0        1       2       3                4      5
	if len(parts) != 6 {
		return errors.New("invalid argon2id hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return fmt.Errorf("argon2id version: %w", err)
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return fmt.Errorf("argon2id params: %w", err)
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return fmt.Errorf("argon2id salt decode: %w", err)
	}
	wantHash, err := enc.DecodeString(parts[5])
	if err != nil {
		return fmt.Errorf("argon2id hash decode: %w", err)
	}
	gotHash := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(wantHash)))
	if subtle.ConstantTimeCompare(gotHash, wantHash) != 1 {
		return errors.New("invalid password")
	}
	return nil
}

// Password Policy

// PasswordPolicy configures password validation rules.
type PasswordPolicy struct {
	MinLength         int  // minimum characters (default 12)
	RequireUpperLower bool // require at least one uppercase and one lowercase letter
	RequireDigit      bool // require at least one digit
	CheckCommon       bool // check against embedded common password list
}

// DefaultPasswordPolicy returns a secure default policy:
// min 12 chars, mixed case + digit required, common password check enabled.
func DefaultPasswordPolicy() PasswordPolicy {
	return PasswordPolicy{
		MinLength:         12,
		RequireUpperLower: true,
		RequireDigit:      true,
		CheckCommon:       true,
	}
}

// ErrPasswordTooShort and related errors are sentinel errors returned by
// ValidatePassword. Callers use errors.Is to match.
var (
	ErrPasswordTooShort     = errors.New("password too short")
	ErrPasswordNoComplexity = errors.New("password must include uppercase, lowercase, and digit")
	ErrPasswordTooCommon    = errors.New("password is too common")
)

// ValidatePassword checks password against the given policy.
// Returns nil if the password satisfies all rules. Otherwise returns one of
// ErrPasswordTooShort, ErrPasswordNoComplexity, or ErrPasswordTooCommon.
func ValidatePassword(policy PasswordPolicy, password string) error {
	if len(password) < policy.MinLength {
		return fmt.Errorf("%w: need %d got %d", ErrPasswordTooShort, policy.MinLength, len(password))
	}

	if policy.RequireUpperLower || policy.RequireDigit {
		var hasUpper, hasLower, hasDigit bool
		for _, r := range password {
			switch {
			case unicode.IsUpper(r):
				hasUpper = true
			case unicode.IsLower(r):
				hasLower = true
			case unicode.IsDigit(r):
				hasDigit = true
			}
		}
		needUpper := policy.RequireUpperLower && !hasUpper
		needLower := policy.RequireUpperLower && !hasLower
		needDigit := policy.RequireDigit && !hasDigit
		if needUpper || needLower || needDigit {
			return ErrPasswordNoComplexity
		}
	}

	if policy.CheckCommon && isCommonPassword(strings.ToLower(password)) {
		return ErrPasswordTooCommon
	}

	return nil
}
