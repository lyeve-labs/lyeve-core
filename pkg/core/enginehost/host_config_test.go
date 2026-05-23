package enginehost

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestConfigAdapter_String_ConcealedKeys(t *testing.T) {
	cfg := &config.Config{
		JWTSecret:   "super-secret-key",
		JWTSecrets:  []string{"key1", "key2"},
		DatabaseURL: "postgres://user:pass@localhost:5432/cms",
		CORSOrigins: []string{"https://example.com"},
	}
	adapter := &configAdapter{cfg: cfg}

	// Redacted values: should return empty string
	for name, key := range map[string]string{
		"database_url":         "database_url",
		"jwt_secret":           "jwt_secret",
		"jwt_secrets":          "jwt_secrets",
		"encryption_key":       "encryption_key",
		"redis_url":            "redis_url",
		"storage_s3_key":       "storage_s3_key",
		"storage_s3_secret":    "storage_s3_secret",
		"search_es_api_key":    "search_es_api_key",
		"search_es_password":   "search_es_password",
		"search_meili_api_key": "search_meili_api_key",
		"smtp_pass":            "smtp_pass",
		"smtp_user":            "smtp_user",
		"license_key":          "license_key",
		"llm_api_key":          "llm_api_key",
		"rate_limit_redis_url": "rate_limit_redis_url",
	} {
		t.Run("String_concealed_"+name, func(t *testing.T) {
			assert.Equal(t, "", adapter.String(key), "String(%q) must not leak secrets", key)
		})
	}

	// Non-concealed value: should return real value
	t.Run("String_cors_origins", func(t *testing.T) {
		assert.Equal(t, "", adapter.String("cors_origins"), "cors_origins has no String representation")
	})
}

func TestConfigAdapter_Strings_ConcealedKeys(t *testing.T) {
	cfg := &config.Config{
		JWTSecret:   "super-secret-key",
		JWTSecrets:  []string{"key1", "key2"},
		DatabaseURL: "postgres://user:pass@localhost:5432/cms",
		CORSOrigins: []string{"https://example.com"},
		IPAllowlist: []string{"10.0.0.0/8"},
	}
	adapter := &configAdapter{cfg: cfg}

	// Concealed keys: must return nil for Strings too
	for name, key := range map[string]string{
		"jwt_secrets":          "jwt_secrets",
		"jwt_secret":           "jwt_secret",
		"database_url":         "database_url",
		"encryption_key":       "encryption_key",
		"redis_url":            "redis_url",
		"storage_s3_key":       "storage_s3_key",
		"storage_s3_secret":    "storage_s3_secret",
		"search_es_api_key":    "search_es_api_key",
		"search_es_password":   "search_es_password",
		"search_meili_api_key": "search_meili_api_key",
		"smtp_pass":            "smtp_pass",
		"smtp_user":            "smtp_user",
		"license_key":          "license_key",
		"llm_api_key":          "llm_api_key",
		"rate_limit_redis_url": "rate_limit_redis_url",
	} {
		t.Run("Strings_concealed_"+name, func(t *testing.T) {
			result := adapter.Strings(key)
			assert.Nil(t, result, "Strings(%q) must not leak secrets; got %v", key, result)
		})
	}

	// Non-concealed keys: must return real values
	t.Run("Strings_cors_origins", func(t *testing.T) {
		result := adapter.Strings("cors_origins")
		if assert.NotNil(t, result) {
			assert.Equal(t, []string{"https://example.com"}, result)
		}
	})

	t.Run("Strings_ip_allowlist", func(t *testing.T) {
		result := adapter.Strings("ip_allowlist")
		if assert.NotNil(t, result) {
			assert.Equal(t, []string{"10.0.0.0/8"}, result)
		}
	})

	// Unknown key (not in switch, not concealed): should return nil since String returns ""
	t.Run("Strings_unknown", func(t *testing.T) {
		result := adapter.Strings("nonexistent_key")
		assert.Nil(t, result)
	})
}

// TestSecretKeys_Complete ensures every key in core.SecretKeys
// is guarded by String() and Strings(), and the test confirms both
// redactions fire for every key in the canonical set.
func TestSecretKeys_Complete(t *testing.T) {
	keys := make([]string, 0, len(core.SecretKeys))
	for k := range core.SecretKeys {
		keys = append(keys, k)
	}

	cfg := &config.Config{
		JWTSecret:  "secret",
		JWTSecrets: []string{"s1", "s2"},
	}
	adapter := &configAdapter{cfg: cfg}

	for _, key := range keys {
		// Every concealed key must be guarded by both String() and Strings()
		assert.Equal(t, "", adapter.String(key), "String(%q) must return empty for concealed keys", key)
		assert.Nil(t, adapter.Strings(key), "Strings(%q) must return nil for concealed keys", key)
	}
}

// A plugin reads the instance's region as the engine booted with it, the value
// the engine's own region routing answers from, whatever a later read of the
// setting would say.
func TestConfigAdapter_String_InstanceRegionIsTheOneTheEngineBootedWith(t *testing.T) {
	t.Setenv("INSTANCE_REGION", "us-east-1")

	assert.Equal(t, "eu-west-1", (&configAdapter{cfg: &config.Config{InstanceRegion: "eu-west-1"}}).String("instance_region"))
	assert.Equal(t, "", (&configAdapter{cfg: &config.Config{}}).String("instance_region"),
		"an engine that booted with no region names none")
}
