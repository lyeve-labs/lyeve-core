package httpx

import (
	"context"
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// keyStoreHost is an optional interface that Host implementations may satisfy
// to expose a per-tenant DEK encryption KeyStore. When present, encryption
// uses the DEK/KEK key hierarchy. When absent, encryption keyed by the JWT
// secret is used as a fallback.
type keyStoreHost interface {
	KeyStore() security.KeyStore
}

// EncryptPluginSecret encrypts a plaintext secret using the strongest
// available encryption path:
//
//  1. KeyStore (per-tenant DEK): when the host implements keyStoreHost
//     and its KeyStore is non-nil
//  2. JWT secret fallback: when no KeyStore is available but the host
//     exposes jwt_secrets via SecretsProvider
//  3. Plaintext passthrough: when neither path is configured
//
// Returns a hex-encoded ciphertext string suitable for database storage.
// An empty plaintext returns an empty string without error.
func EncryptPluginSecret(host core.Host, tenantID string, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	// Path 1: per-tenant DEK encryption via KeyStore
	if ksh, ok := any(host).(keyStoreHost); ok {
		if ks := ksh.KeyStore(); ks != nil {
			enc, err := ks.Encrypt(context.Background(), tenantID, []byte(plaintext))
			if err != nil {
				return "", fmt.Errorf("httpx: encrypt plugin secret via keystore: %w", err)
			}
			return enc, nil
		}
	}

	// Path 2: encryption keyed by the JWT secret
	if key := encKey(host); key != "" {
		return security.EncryptSecret(plaintext, key)
	}

	// Path 3: no encryption configured, store as plaintext
	return plaintext, nil
}

// DecryptPluginSecret decrypts a hex-encoded ciphertext produced by
// EncryptPluginSecret. It tries the KeyStore path first. On failure it falls
// back to decryption keyed by the JWT secret, which reads a ciphertext that
// EncryptPluginSecret sealed on a host with no KeyStore.
//
// Returns the original plaintext. An empty ciphertext returns an empty string
// without error. A decryption failure returns an error (callers should treat
// this as data corruption or plaintext that was never encrypted).
func DecryptPluginSecret(host core.Host, tenantID string, ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}

	// Path 1: try per-tenant DEK decryption via KeyStore first
	if ksh, ok := any(host).(keyStoreHost); ok {
		if ks := ksh.KeyStore(); ks != nil {
			dec, err := ks.Decrypt(context.Background(), tenantID, ciphertext)
			if err == nil {
				return string(dec), nil
			}
			// KeyStore failed: fall through to JWT secret fallback
		}
	}

	// Path 2: decryption keyed by the JWT secret
	if key := encKey(host); key != "" {
		return security.DecryptSecret(ciphertext, key)
	}

	// Path 3: no encryption configured, stored as plaintext
	return ciphertext, nil
}

// encKey returns the first configured JWT secret for use as an encryption
// passphrase. Uses SecretsProvider to avoid Config().Strings() which hides
// secret keys (ConcealedSecretKeys pattern).
func encKey(host core.Host) string {
	if sp, ok := any(host).(core.SecretsProvider); ok {
		if s, _ := sp.Secrets("jwt_secrets"); len(s) > 0 {
			return s[0]
		}
	}
	return ""
}
