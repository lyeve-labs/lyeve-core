package auth

import (
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// EncryptSecret encrypts plaintext with AES-256-GCM. Thin wrapper around
// security.EncryptSecret with auth-package error context.
//
// Deprecated: uses raw SHA-256 key derivation with no HKDF expansion. Use
// core/encryption.EncryptValue with a KeyStore and per-tenant DEK
// instead. Kept only for backwards compatibility with existing data.
func EncryptSecret(plaintext, passphrase string) (string, error) {
	enc, err := security.EncryptSecret(plaintext, passphrase)
	if err != nil {
		return "", fmt.Errorf("auth.EncryptSecret: %w", err)
	}
	return enc, nil
}

// DecryptSecret decrypts a hex-encoded value produced by EncryptSecret.
// Thin wrapper around security.DecryptSecret with auth-package error context.
//
// Deprecated: use core/encryption.DecryptValue with a per-tenant DEK
// derived via KeyStore.DeriveTenantDEK instead.
func DecryptSecret(encHex, passphrase string) (string, error) {
	dec, err := security.DecryptSecret(encHex, passphrase)
	if err != nil {
		return "", fmt.Errorf("auth.DecryptSecret: %w", err)
	}
	return dec, nil
}
