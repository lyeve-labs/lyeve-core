package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// API-key hashing: pepper (keyed HMAC) layer.
//
// API keys are never stored in plaintext: only a hash is persisted, and the
// raw key is shown once at creation (see GenerateKey/HashKey). The tokens carry
// 256 bits of entropy, so brute-forcing a stolen hash is already infeasible.
// The pepper adds defense-in-depth: when configured, hashes are
// HMAC-SHA256(pepper, raw) instead of plain SHA-256, so an attacker who
// exfiltrates the sys_api_keys table alone still cannot validate or forge keys
// offline. They would also need the pepper, which lives in a separate custody
// backend (see pkg/security/secrets) and is never written to the database.
//
// When no pepper is installed, HashKeyPeppered is byte-for-byte identical to
// HashKey, so a deployment's stored hashes stay valid until it opts in.

var (
	pepperMu     sync.RWMutex
	apiKeyPepper []byte // server-side HMAC key, nil/empty = unpeppered mode
)

// SetAPIKeyPepper installs the server-side pepper used to key the HMAC in
// HashKeyPeppered. Call it once during startup: after resolving the value
// from a secrets.Source and before the server accepts traffic. Passing nil or
// empty bytes clears the pepper (HashKeyPeppered then falls back to HashKey).
//
// A copy is retained, so the caller may zero its own buffer afterwards. Safe
// for concurrent use, though it is expected to be set exactly once at boot.
func SetAPIKeyPepper(pepper []byte) {
	pepperMu.Lock()
	defer pepperMu.Unlock()
	if len(pepper) == 0 {
		apiKeyPepper = nil
		return
	}
	apiKeyPepper = append([]byte(nil), pepper...)
}

// APIKeyPepperConfigured reports whether a non-empty pepper is installed. The
// API-key auth path uses this to decide whether a plain-SHA-256 fallback
// lookup is worth attempting for a key stored without the pepper.
func APIKeyPepperConfigured() bool {
	pepperMu.RLock()
	defer pepperMu.RUnlock()
	return len(apiKeyPepper) > 0
}

// HashKeyPeppered returns the storage/lookup hash for a raw API key. When a
// pepper is configured it returns hex(HMAC-SHA256(pepper, raw)). Otherwise it
// is identical to HashKey (hex(SHA-256(raw))). The output is always 64 hex
// chars, so it is a drop-in replacement for HashKey at the storage layer and
// needs no schema change.
func HashKeyPeppered(raw string) string {
	pepperMu.RLock()
	pepper := apiKeyPepper // SetAPIKeyPepper replaces (never mutates) the slice,
	pepperMu.RUnlock()     // so this captured reference stays valid after unlock.

	if len(pepper) == 0 {
		return HashKey(raw)
	}
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}
