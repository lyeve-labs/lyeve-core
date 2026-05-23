// Package encryption provides the DEK/KEK key hierarchy for data-at-rest
// encryption in LyEve CMS.
//
// Architecture:
//
//	KEK (Key Encryption Key) - master key from ENCRYPTION_KEY env var
//	DEK (Data Encryption Key) - per-tenant key derived via HKDF-SHA256
//
// Each tenant gets a unique DEK derived from the master KEK + tenant ID.
// Data is encrypted with AES-256-GCM using a random 12-byte nonce.
// The encrypted format is: nonce (12 bytes) || ciphertext + GCM auth tag (16 bytes).
package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

const (
	// AESSize is the AES-256 key size in bytes.
	AESSize = 32

	// NonceSize is the AES-GCM nonce size (12 bytes per NIST SP 800-38D).
	NonceSize = 12

	// HKDFInfo is the HKDF info string for tenant DEK derivation.
	//
	// Do not rename this to match any rebrand. It is an input to the key
	// derivation, not a label: changing the string derives different DEKs and
	// every value already encrypted at rest becomes undecryptable, with no way
	// back. The -v1 suffix is the versioning hook for a deliberate rotation,
	// which needs a decrypt-with-old, re-encrypt-with-new pass first.
	HKDFInfo = "lyeve-cms-dek-v1"

	// pbkdf2Iter is the PBKDF2 iteration count for master KEK derivation
	// (OWASP 2023 minimum: 600,000 for HMAC-SHA256).
	pbkdf2Iter = 600_000

	// kekSalt is the fixed domain-separation salt for deriving the master
	// KEK from the ENCRYPTION_KEY environment variable via PBKDF2.
	//
	// Same warning as HKDFInfo: this salt is an input to the derivation. A
	// rename silently changes the master KEK and orphans every ciphertext.
	kekSalt = "lyeve-cms-kek-v1"

	// minMasterKeyLen is the minimum length of the ENCRYPTION_KEY value.
	minMasterKeyLen = 32
)

// KeyStore holds the master Key Encryption Key (KEK) loaded from
// ENCRYPTION_KEY at startup.
type KeyStore struct {
	kek []byte // master key encryption key (32 bytes)
}

// NewKeyStore creates a KeyStore from the master encryption key. The KEK is
// derived via PBKDF2-HMAC-SHA256 with 600,000 iterations and a fixed
// domain-separation salt. masterKey must be at least 32 characters.
func NewKeyStore(masterKey string) (*KeyStore, error) {
	if len(masterKey) < minMasterKeyLen {
		return nil, fmt.Errorf("encryption: master key too short (%d chars, minimum %d)", len(masterKey), minMasterKeyLen)
	}
	return &KeyStore{kek: deriveKEK(masterKey)}, nil
}

// maxCachedKEKs bounds the derivation cache. A process holds one master key,
// or two across a rotation. Anything past that is derived every time.
const maxCachedKEKs = 4

var (
	kekCacheMu sync.Mutex
	kekCache   = map[string][]byte{}
)

// deriveKEK runs the PBKDF2 derivation once per master key per process. The
// salt is fixed, so every call with one key yields the same KEK, and the
// engine and each plugin that encrypts build their own KeyStore from the same
// ENCRYPTION_KEY: at 600,000 iterations that costs a noticeable share of boot
// per caller. The cache holds nothing the process did not already hold, since
// the master key stays in its configuration and every KeyStore keeps the KEK.
// Callers only read the returned slice.
func deriveKEK(masterKey string) []byte {
	kekCacheMu.Lock()
	defer kekCacheMu.Unlock()
	if kek, ok := kekCache[masterKey]; ok {
		return kek
	}
	kek := pbkdf2.Key([]byte(masterKey), []byte(kekSalt), pbkdf2Iter, AESSize, sha256.New)
	if len(kekCache) < maxCachedKEKs {
		kekCache[masterKey] = kek
	}
	return kek
}

// NewKeyStoreLegacy creates a KeyStore using the single-SHA-256 derivation
// with no PBKDF2 strengthening.
//
// Deprecated: Use NewKeyStore. New deployments and key rotations should use
// PBKDF2-based KEK derivation.
func NewKeyStoreLegacy(masterKey string) (*KeyStore, error) {
	if masterKey == "" {
		return nil, fmt.Errorf("encryption: master key must not be empty")
	}
	h := sha256.Sum256([]byte(masterKey))
	return &KeyStore{kek: h[:]}, nil
}

// DeriveTenantDEK derives a per-tenant Data Encryption Key from the master
// KEK using HKDF-SHA256. Different tenants get cryptographically independent
// keys: compromising one tenant's DEK does not reveal any other tenant's.
//
// The HKDF info parameter is "lyeve-cms-dek-v1" ensuring domain separation.
func (ks *KeyStore) DeriveTenantDEK(tenantID string) ([]byte, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("encryption: tenant ID must not be empty")
	}
	if ks.kek == nil {
		return nil, fmt.Errorf("encryption: KeyStore not initialized")
	}
	reader := hkdf.New(sha256.New, ks.kek, []byte(tenantID), []byte(HKDFInfo))
	dek := make([]byte, AESSize)
	if _, err := io.ReadFull(reader, dek); err != nil {
		return nil, fmt.Errorf("encryption: derive DEK: %w", err)
	}
	return dek, nil
}

// EncryptValue encrypts plaintext with a random 12-byte nonce using the
// provided tenant DEK via AES-256-GCM. Returns hex-encoded ciphertext.
//
// The output format is: nonce (12 bytes) || ciphertext+tag, the same layout
// as the unsalted format security.DecryptSecret reads. Only the key
// derivation differs (the layout is tested in encryption_test.go).
func EncryptValue(tenantDEK, plaintext []byte) (string, error) {
	if len(tenantDEK) != AESSize {
		return "", fmt.Errorf("encryption: invalid DEK length: got %d, want %d", len(tenantDEK), AESSize)
	}
	block, err := aes.NewCipher(tenantDEK)
	if err != nil {
		return "", fmt.Errorf("encryption: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("encryption: create gcm: %w", err)
	}
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("encryption: generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return hex.EncodeToString(ciphertext), nil
}

// DecryptValue decrypts a hex-encoded value produced by EncryptValue using the
// provided tenant DEK. The layout is nonce || ciphertext+tag.
func DecryptValue(tenantDEK []byte, encHex string) ([]byte, error) {
	if len(tenantDEK) != AESSize {
		return nil, fmt.Errorf("encryption: invalid DEK length: got %d, want %d", len(tenantDEK), AESSize)
	}
	enc, err := hex.DecodeString(encHex)
	if err != nil {
		return nil, fmt.Errorf("encryption: decode hex: %w", err)
	}
	block, err := aes.NewCipher(tenantDEK)
	if err != nil {
		return nil, fmt.Errorf("encryption: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encryption: create gcm: %w", err)
	}
	ns := gcm.NonceSize()
	if len(enc) < ns {
		return nil, fmt.Errorf("encryption: ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, enc[:ns], enc[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("encryption: %w", err)
	}
	return plaintext, nil
}

// EncryptPlaintext derives a tenant DEK and encrypts in one call.
func (ks *KeyStore) EncryptPlaintext(tenantID string, plaintext []byte) (string, error) {
	dek, err := ks.DeriveTenantDEK(tenantID)
	if err != nil {
		return "", err
	}
	return EncryptValue(dek, plaintext)
}

// DecryptPlaintext derives a tenant DEK and decrypts in one call.
func (ks *KeyStore) DecryptPlaintext(tenantID, encHex string) ([]byte, error) {
	dek, err := ks.DeriveTenantDEK(tenantID)
	if err != nil {
		return nil, err
	}
	return DecryptValue(dek, encHex)
}
