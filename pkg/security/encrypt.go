package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/pbkdf2"
)

const (
	// saltSize is the PBKDF2 salt length in bytes (128 bits).
	saltSize = 16

	// pbkdf2Iter is the PBKDF2 iteration count (OWASP 2023 minimum).
	pbkdf2Iter = 600_000

	// gcmNonceSize is the AES-GCM nonce size (12 bytes per NIST SP 800-38D).
	gcmNonceSize = 12
)

// DeriveEncKey derives a 32-byte AES-256 encryption key from a passphrase
// using a single SHA-256 hash with no salt and no PBKDF2 strengthening.
//
// Deprecated: Kept only to decrypt ciphertexts in the unsalted SHA-256
// format. Encryption uses PBKDF2-HMAC-SHA256 with a 16-byte random salt and
// 600,000 iterations via derivePBKDF2Key.
func DeriveEncKey(passphrase string) []byte {
	h := sha256.Sum256([]byte(passphrase))
	return h[:]
}

// derivePBKDF2Key derives a 32-byte key using PBKDF2-HMAC-SHA256 with
// the given salt and 600,000 iterations (OWASP 2023 minimum).
func derivePBKDF2Key(passphrase string, salt []byte) []byte {
	return pbkdf2.Key([]byte(passphrase), salt, pbkdf2Iter, 32, sha256.New)
}

// EncryptSecret encrypts plaintext with AES-256-GCM, deriving the key from
// passphrase via PBKDF2-HMAC-SHA256 with a random 16-byte salt and 600,000
// iterations. Returns a hex-encoded string of (salt || nonce || ciphertext).
//
// Format:
//
//	salt (16) || nonce (12) || ciphertext || GCM auth tag (16)
//
// DecryptSecret also reads the unsalted SHA-256 format, which is 32 hex chars
// shorter because it has no salt prefix:
//
//	nonce (12) || ciphertext || GCM auth tag (16)
func EncryptSecret(plaintext, passphrase string) (string, error) {
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("encrypt: generate salt: %w", err)
	}
	key := derivePBKDF2Key(passphrase, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("encrypt: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("encrypt: create gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("encrypt: generate nonce: %w", err)
	}
	// Build: salt || nonce || ciphertext+tag
	buf := make([]byte, 0, saltSize+len(nonce)+len(plaintext)+gcm.Overhead())
	buf = append(buf, salt...)
	buf = append(buf, nonce...)
	buf = gcm.Seal(buf, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(buf), nil
}

// DecryptSecret decrypts a hex-encoded value produced by EncryptSecret.
// It reads both the PBKDF2 format (salt || nonce || ciphertext+tag) and the
// unsalted SHA-256 format (nonce || ciphertext+tag).
//
// Detection: when the decoded ciphertext is at least saltSize+gcmNonceSize
// bytes long, the PBKDF2 format is tried first. If GCM authentication fails,
// the SHA-256 derivation is tried as a fallback.
func DecryptSecret(encHex, passphrase string) (string, error) {
	raw, err := hex.DecodeString(encHex)
	if err != nil {
		return "", fmt.Errorf("decrypt: decode hex: %w", err)
	}

	// Try the PBKDF2 format first: salt(16) || nonce(12) || ciphertext+tag
	if len(raw) >= saltSize+gcmNonceSize {
		salt := raw[:saltSize]
		key := derivePBKDF2Key(passphrase, salt)
		plaintext, decErr := decryptGCM(key, raw[saltSize:])
		if decErr == nil {
			return string(plaintext), nil
		}
		// PBKDF2 failed: fall through to the unsalted SHA-256 format
	}

	// Unsalted format: nonce(12) || ciphertext+tag, SHA-256 key derivation
	key := DeriveEncKey(passphrase)
	plaintext, err := decryptGCM(key, raw)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// DecryptSecretStrict decrypts a hex-encoded value produced by EncryptSecret,
// but rejects the unsalted SHA-256 fallback: the ciphertext must be in the
// PBKDF2 format (salt || nonce || ciphertext+tag). Use this for callers that
// must never accept the deprecated unsalted format.
func DecryptSecretStrict(encHex, passphrase string) (string, error) {
	raw, err := hex.DecodeString(encHex)
	if err != nil {
		return "", fmt.Errorf("decrypt: decode hex: %w", err)
	}
	if len(raw) < saltSize+gcmNonceSize {
		return "", fmt.Errorf("decrypt: ciphertext too short for PBKDF2 format")
	}
	salt := raw[:saltSize]
	key := derivePBKDF2Key(passphrase, salt)
	plaintext, err := decryptGCM(key, raw[saltSize:])
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// ReEncryptSecret decrypts oldHex with oldPassphrase and re-encrypts the
// plaintext with newPassphrase in the PBKDF2 format. Use this to move a
// ciphertext in the unsalted format to the stronger key derivation.
func ReEncryptSecret(oldHex, oldPassphrase, newPassphrase string) (string, error) {
	plaintext, err := DecryptSecret(oldHex, oldPassphrase)
	if err != nil {
		return "", fmt.Errorf("re-encrypt: decrypt old: %w", err)
	}
	enc, err := EncryptSecret(plaintext, newPassphrase)
	if err != nil {
		return "", fmt.Errorf("re-encrypt: encrypt new: %w", err)
	}
	return enc, nil
}

// decryptGCM decrypts (nonce || ciphertext+tag) with the given AES-256 key.
func decryptGCM(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("decrypt: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("decrypt: create gcm: %w", err)
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, fmt.Errorf("decrypt: ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:ns], data[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

// EncryptBytes encrypts plaintext with AES-256-GCM using PBKDF2-HMAC-SHA256
// key derivation (random 16-byte salt, 600K iterations). Returns
// (salt || nonce || ciphertext+tag) as raw bytes. Use this when the caller
// needs binary output (e.g., writing to files or streams). For database
// storage, prefer EncryptSecret which returns a hex-encoded string.
func EncryptBytes(plaintext []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("encrypt: generate salt: %w", err)
	}
	key := derivePBKDF2Key(passphrase, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encrypt: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encrypt: create gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("encrypt: generate nonce: %w", err)
	}
	buf := make([]byte, 0, saltSize+len(nonce)+len(plaintext)+gcm.Overhead())
	buf = append(buf, salt...)
	buf = append(buf, nonce...)
	buf = gcm.Seal(buf, nonce, plaintext, nil)
	return buf, nil
}

// DecryptBytes decrypts data produced by EncryptBytes. Supports both the
// PBKDF2 format (salt || nonce || ciphertext+tag) and the unsalted SHA-256
// format (nonce || ciphertext+tag).
func DecryptBytes(ciphertext []byte, passphrase string) ([]byte, error) {
	// Try the PBKDF2 format first: salt(16) || nonce(12) || ciphertext+tag
	if len(ciphertext) >= saltSize+gcmNonceSize {
		salt := ciphertext[:saltSize]
		key := derivePBKDF2Key(passphrase, salt)
		plaintext, decErr := decryptGCM(key, ciphertext[saltSize:])
		if decErr == nil {
			return plaintext, nil
		}
	}
	// Unsalted format
	key := DeriveEncKey(passphrase)
	return decryptGCM(key, ciphertext)
}
