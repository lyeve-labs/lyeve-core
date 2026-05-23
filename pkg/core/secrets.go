package core

import "context"

// KeyStore encrypts and decrypts plugin secrets at rest, per tenant. The
// implementation lives in pkg/security/encryption, and a plugin depends on
// this interface rather than on it.
type KeyStore interface {
	Encrypt(ctx context.Context, tenantID string, plaintext []byte) (string, error)
	Decrypt(ctx context.Context, tenantID string, ciphertext string) ([]byte, error)
}

// SecretsProvider is an optional interface that engine Host implementations
// implement to expose secret material to plugins holding CapConfigSecret
// without leaking it through the public Config interface, which redacts it.
// Plugins type-assert the host to this interface to access encryption keys,
// JWT secrets, database URLs, and other credential-level configuration.
//
// The engineHost implementation maps key names to config.Config fields
// directly. The configAdapter redaction guard ensures the same keys return ""
// from Config().String() and nil from Config().Strings().
type SecretsProvider interface {
	// Secret returns the value for a named secret. The second return value
	// is false when the key is unknown or the value is empty.
	Secret(key string) (val string, found bool)

	// Secrets returns a slice value for a named secret (e.g. jwt_secrets
	// rotation list). The second return value is false when the key is
	// unknown or the slice is empty.
	Secrets(key string) (vals []string, found bool)
}

// Secret reads a credential-level config value. It prefers the host's
// SecretsProvider and falls back to Config, which is what test hosts
// implement.
//
// Reach for this rather than Config().String() for anything in SecretKeys:
// configAdapter redacts those to "", so a plain Config read compiles, runs,
// and silently reports the secret as unset.
func Secret(host Host, key string) string {
	if host == nil {
		return ""
	}
	if sp, ok := host.(SecretsProvider); ok {
		if v, found := sp.Secret(key); found {
			return v
		}
	}
	if cfg := host.Config(); cfg != nil {
		return cfg.String(key)
	}
	return ""
}

// SecretList reads a multi-valued credential, such as the jwt_secrets
// rotation list. It follows the same precedence as Secret.
func SecretList(host Host, key string) []string {
	if host == nil {
		return nil
	}
	if sp, ok := host.(SecretsProvider); ok {
		if v, found := sp.Secrets(key); found && len(v) > 0 {
			return v
		}
	}
	if cfg := host.Config(); cfg != nil {
		return cfg.Strings(key)
	}
	return nil
}
