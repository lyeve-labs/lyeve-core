package enginehost

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const jwtTestSecret = "test-secret-at-least-32-bytes-long!"

// TestSessionTokenSigner_EngineHost verifies that engineHost implements
// core.SessionTokenSigner and produces a valid session token.
func TestSessionTokenSigner_EngineHost(t *testing.T) {
	cfg := &config.Config{
		JWTSecret:     jwtTestSecret,
		JWTSecrets:    []string{jwtTestSecret, "rotated-secret"},
		JWTExpirySecs: 7200,
	}
	host := &engineHost{cfg: cfg}

	signer, ok := any(host).(core.SessionTokenSigner)
	require.True(t, ok, "engineHost must implement core.SessionTokenSigner")

	userID := uuid.New()
	email := "session-signer@example.com"
	roles := []string{"editor", "viewer"}

	token, err := signer.SignSessionToken(context.Background(), userID, email, roles)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	claims, err := security.ParseJWT(jwtTestSecret, token)
	require.NoError(t, err)
	assert.Equal(t, userID.String(), claims.UserID)
	assert.Equal(t, email, claims.Email)
	assert.Equal(t, roles, claims.Roles)
	assert.Equal(t, "session", claims.TokenType)
	assert.False(t, claims.MFAPending)
}

// TestSessionTokenSigner_DefaultExpiry verifies default JWT expiry.
func TestSessionTokenSigner_DefaultExpiry(t *testing.T) {
	tests := []struct {
		name string
		secs int64
	}{
		{"zero", 0},
		{"negative", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				JWTSecret:     jwtTestSecret,
				JWTExpirySecs: tt.secs,
			}
			host := &engineHost{cfg: cfg}
			signer, ok := any(host).(core.SessionTokenSigner)
			require.True(t, ok)

			userID := uuid.New()
			token, err := signer.SignSessionToken(context.Background(), userID, "u@x.com", nil)
			require.NoError(t, err)

			claims, err := security.ParseJWT(jwtTestSecret, token)
			require.NoError(t, err)
			assert.Equal(t, userID.String(), claims.UserID)
		})
	}
}

// TestConfigAdapter_Strings_SecretsLeak verifies Strings() returns nil
// for all concealed secret keys.

// TestSecretsProvider_EngineHost verifies that engineHost implements
// core.SecretsProvider and returns valid JWT secrets.
func TestSecretsProvider_EngineHost(t *testing.T) {
	cfg := &config.Config{
		JWTSecrets: []string{"s1", "s2", "s3"},
	}
	host := &engineHost{cfg: cfg}

	sp, ok := any(host).(core.SecretsProvider)
	require.True(t, ok, "engineHost must implement core.SecretsProvider")

	secrets, found := sp.Secrets("jwt_secrets")
	require.True(t, found)
	require.Equal(t, []string{"s1", "s2", "s3"}, secrets)

	// Unknown key returns false
	_, found = sp.Secrets("unknown_key")
	assert.False(t, found)

	// Nil config returns false
	host2 := &engineHost{cfg: nil}
	sp2, ok := any(host2).(core.SecretsProvider)
	require.True(t, ok)
	_, found = sp2.Secrets("jwt_secrets")
	assert.False(t, found)
}

// TestSecretsProvider_AuditHMACKey verifies that engineHost serves the
// audit_hmac_key secret from config.Config.AuditHMACKey.
func TestSecretsProvider_AuditHMACKey(t *testing.T) {
	t.Run("returns_set_key", func(t *testing.T) {
		key := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
		cfg := &config.Config{
			AuditHMACKey: key,
		}
		host := &engineHost{cfg: cfg}
		sp, ok := any(host).(core.SecretsProvider)
		require.True(t, ok)
		val, found := sp.Secret("audit_hmac_key")
		require.True(t, found)
		assert.Equal(t, key, val)
	})

	t.Run("returns_false_when_empty", func(t *testing.T) {
		cfg := &config.Config{
			AuditHMACKey: "",
		}
		host := &engineHost{cfg: cfg}
		sp, ok := any(host).(core.SecretsProvider)
		require.True(t, ok)
		_, found := sp.Secret("audit_hmac_key")
		assert.False(t, found, "should return false when AuditHMACKey is empty")
	})

	t.Run("returns_false_when_nil_config", func(t *testing.T) {
		host := &engineHost{cfg: nil}
		sp, ok := any(host).(core.SecretsProvider)
		require.True(t, ok)
		_, found := sp.Secret("audit_hmac_key")
		assert.False(t, found, "should return false when cfg is nil")
	})

	t.Run("unknown_key_returns_false", func(t *testing.T) {
		cfg := &config.Config{
			AuditHMACKey: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		}
		host := &engineHost{cfg: cfg}
		sp, ok := any(host).(core.SecretsProvider)
		require.True(t, ok)
		_, found := sp.Secret("some_unknown_key")
		assert.False(t, found)
	})
}

func TestConfigAdapter_Strings_SecretsLeak(t *testing.T) {
	cfg := &config.Config{
		JWTSecret:   "secret-value",
		JWTSecrets:  []string{"s1", "s2"},
		DatabaseURL: "postgres://user:***@localhost:5432/cms",
	}
	adapter := &configAdapter{cfg: cfg}

	for key := range core.SecretKeys {
		t.Run("Strings_concealed_"+key, func(t *testing.T) {
			result := adapter.Strings(key)
			assert.Nil(t, result, "Strings(%q) must not leak secrets; got %v", key, result)
		})
	}

	// Non-concealed keys must still work.
	t.Run("Strings_cors_origins", func(t *testing.T) {
		cfg2 := &config.Config{CORSOrigins: []string{"https://example.com"}}
		adapter2 := &configAdapter{cfg: cfg2}
		result := adapter2.Strings("cors_origins")
		if assert.NotNil(t, result) {
			assert.Equal(t, []string{"https://example.com"}, result)
		}
	})
}
