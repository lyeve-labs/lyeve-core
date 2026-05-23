// Package secrets defines the secret-custody seam for LyEve CMS.
//
// Cryptographic algorithms (AES-256-GCM, PBKDF2, HKDF, HMAC) are public by
// design: their strength is the secrecy of the key, not the code
// (Kerckhoffs's principle). What actually protects data at rest and the
// integrity of API-key hashes is *where the secret material lives*. This
// package abstracts that custody behind the Source interface so the raw
// secrets (the HMAC pepper, the master KEK, the JWT signing secret) can move
// out of the process environment and into a KMS/Vault backend (usually
// supplied by a plugin) without any change to the consuming code.
//
// The default EnvSource reads the process environment. Production
// deployments layer a KMS/Vault-backed Source in front of it via Chain.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
)

const (
	// APIKeyPepper keys the HMAC used by security.HashKeyPeppered so a leaked
	// sys_api_keys table cannot be used to validate or forge keys offline.
	APIKeyPepper = "api_key_pepper"

	// EncryptionKEK is the master Key Encryption Key for data-at-rest
	// encryption (the DEK/KEK hierarchy in pkg/security/encryption).
	EncryptionKEK = "encryption_kek"

	// JWTSigning is the primary symmetric JWT signing secret (HS256 fallback
	// path. EdDSA uses an on-disk keypair instead).
	JWTSigning = "jwt_signing"
)

// ErrNotFound is returned by a Source when it holds no value for a name.
// Callers chain sources and treat ErrNotFound as "try the next backend".
// Any other error is a real backend failure and must not be swallowed.
var ErrNotFound = errors.New("secrets: not found")

// Source supplies secret material from a custody backend. Implementations must
// be safe for concurrent use and must never return a nil error together with
// empty bytes (return ErrNotFound instead).
type Source interface {
	// Secret returns the bytes for the given logical name, or ErrNotFound
	// when this backend has no value for it.
	Secret(ctx context.Context, name string) ([]byte, error)
}

// DefaultEnvMapping is the built-in logical-name -> environment-variable
// translation used by a zero-value EnvSource.
var DefaultEnvMapping = map[string]string{
	APIKeyPepper:  "API_KEY_PEPPER",
	EncryptionKEK: "ENCRYPTION_KEY",
	JWTSigning:    "JWT_SECRET",
}

// EnvSource resolves logical names to environment variables. A missing or
// empty variable yields ErrNotFound so callers can fall through to another
// Source. The zero value is usable and uses DefaultEnvMapping.
type EnvSource struct {
	// Mapping overrides the default logical-name -> env-var translation.
	// A nil map selects DefaultEnvMapping.
	Mapping map[string]string
}

// Secret implements Source by reading the mapped environment variable.
func (e EnvSource) Secret(_ context.Context, name string) ([]byte, error) {
	mapping := e.Mapping
	if mapping == nil {
		mapping = DefaultEnvMapping
	}
	envVar, ok := mapping[name]
	if !ok {
		return nil, fmt.Errorf("secrets: no env mapping for %q: %w", name, ErrNotFound)
	}
	v := os.Getenv(envVar)
	if v == "" {
		return nil, fmt.Errorf("secrets: env %s unset: %w", envVar, ErrNotFound)
	}
	return []byte(v), nil
}

// StaticSource serves secrets from an in-memory map. Intended for tests and
// for injecting a known-good bootstrap value: not for production custody,
// where the point is to keep raw secrets out of process memory for as long as
// possible.
type StaticSource map[string][]byte

// Secret implements Source. A defensive copy is returned so callers cannot
// mutate the backing map.
func (s StaticSource) Secret(_ context.Context, name string) ([]byte, error) {
	if v, ok := s[name]; ok && len(v) > 0 {
		return append([]byte(nil), v...), nil
	}
	return nil, fmt.Errorf("secrets: %q: %w", name, ErrNotFound)
}

// Chain queries each Source in order and returns the first hit. A name absent
// from every source yields ErrNotFound. A non-ErrNotFound error from any
// source is returned immediately (a real backend failure must not be masked
// by silently falling through to a weaker source). Nil entries are skipped,
// so Chain{kmsSource, EnvSource{}} is valid even when kmsSource is nil.
type Chain []Source

// Secret implements Source by trying each member in order.
func (c Chain) Secret(ctx context.Context, name string) ([]byte, error) {
	for _, s := range c {
		if s == nil {
			continue
		}
		v, err := s.Secret(ctx, name)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("secrets: %q not found in any source: %w", name, ErrNotFound)
}
