package security

import "context"

// KeyStore provides data-at-rest encryption for plugin secrets using the
// DEK/KEK key hierarchy. Plugins receive a KeyStore from the host to
// encrypt and decrypt sensitive configuration values before persisting them.
type KeyStore interface {
	// Encrypt encrypts plaintext bytes under a per-tenant DEK derived from
	// the master KEK. Returns a hex-encoded ciphertext string.
	Encrypt(ctx context.Context, tenantID string, plaintext []byte) (string, error)
	// Decrypt decrypts a hex-encoded ciphertext string back to plaintext.
	Decrypt(ctx context.Context, tenantID string, ciphertext string) ([]byte, error)
}
