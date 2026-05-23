package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

const keyPrefix = "ly_"
const keyByteLen = 32

// GenerateKey creates a new API key with prefix and returns the raw key and
// its stored hash. The raw key is shown once at creation time. Only the hash
// is persisted. The hash is peppered (HMAC-SHA256) when a pepper is installed
// via SetAPIKeyPepper, and plain SHA-256 otherwise: see HashKeyPeppered.
func GenerateKey() (raw, hash string, err error) {
	b := make([]byte, keyByteLen)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	raw = keyPrefix + hex.EncodeToString(b)
	hash = HashKeyPeppered(raw)
	return raw, hash, nil
}

// HashKey returns the SHA-256 hex digest of raw, used for storing and
// looking up API keys.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// backupCodeBytes is the entropy in one code. Eight bytes, all of them.
//
// All eight bytes are encoded, because a code is stored as an unsalted
// SHA-256. Anyone who reads the column can search a short code offline, and
// the lockout that bounds the login path does not apply to that search.
//
// Hex rather than base64url because a person types these from paper.
const backupCodeBytes = 8

// GenerateBackupCodes generates n random one-time backup codes.
// Each code is 16 hex characters, which is the full 64 bits.
func GenerateBackupCodes(n int) ([]string, error) {
	codes := make([]string, n)
	buf := make([]byte, backupCodeBytes)
	for i := range codes {
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		codes[i] = hex.EncodeToString(buf)
	}
	return codes, nil
}
