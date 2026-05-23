package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Links are built by appending a path to LYEVE_CONSOLE_URL, so a value that
// would make that path land somewhere else refuses the boot in any
// environment.
func TestConsoleURLProblem(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"https://admin.example.com", ""},
		{"http://localhost:5173", ""},
		{"https://example.com/console", ""},
		{"https://[::1]:8443", ""},
		{"admin.example.com", "must be an absolute URL"},
		{"0.0.0.0:3111", "must be an absolute URL"},
		{"ftp://admin.example.com", "must be an absolute URL"},
		{"https://", "names no host"},
		{"https://:8443", "names no host"},
		{"https://user:pw@admin.example.com", "only a scheme"},
		{"https://admin.example.com?next=/", "only a scheme"},
		{"https://admin.example.com/#frag", "only a scheme"},
		{"https://admin.example.com/%zz", "must be an absolute URL"},
	} {
		got := consoleURLProblem(tc.raw)
		if tc.want == "" {
			assert.Empty(t, got, tc.raw)
			continue
		}
		assert.Contains(t, got, tc.want, tc.raw)
	}
}

func TestLoad_ConsoleURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost/db?sslmode=disable")
	t.Setenv("JWT_SECRET", "a-very-long-secret-key-that-is-secure")
	t.Setenv("ENCRYPTION_KEY", "a-separate-encryption-key-of-32-chars-or-more")
	t.Setenv("APP_ENV", "development")

	t.Run("trailing slash is dropped", func(t *testing.T) {
		t.Setenv("LYEVE_CONSOLE_URL", " https://admin.example.com/ ")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, "https://admin.example.com", cfg.ConsoleURL)
	})

	t.Run("a listen address refuses the boot", func(t *testing.T) {
		t.Setenv("LYEVE_CONSOLE_URL", "0.0.0.0:3111")
		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "LYEVE_CONSOLE_URL")
	})
}

// In production a link carries its token over the console URL's scheme, so
// plain http refuses the boot, and an unset value boots with a warning because
// only the plugins that mail links need it, and they refuse to start without it.
func TestValidateProduction_ConsoleURL(t *testing.T) {
	base := func(consoleURL string) *Config {
		return &Config{
			Environment:   "production",
			DatabaseURL:   "postgres://host/db?sslmode=require",
			JWTSecret:     "a-very-long-secret-key-that-is-secure",
			EncryptionKey: "separate-encryption-key-value",
			SecureCookie:  true,
			CORSOrigins:   []string{"https://app.example.com"},
			AuditHMACKey:  "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			RateLimitRPS:  100.0,
			ConsoleURL:    consoleURL,
		}
	}

	require.NoError(t, base("https://admin.example.com").ValidateProduction())
	assert.Empty(t, base("https://admin.example.com").ProductionWarnings())

	err := base("http://admin.example.com").ValidateProduction()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LYEVE_CONSOLE_URL")

	unset := base("")
	require.NoError(t, unset.ValidateProduction())
	warns := unset.ProductionWarnings()
	require.Len(t, warns, 1)
	assert.True(t, strings.HasPrefix(warns[0], "LYEVE_CONSOLE_URL is unset"), warns[0])

	dev := base("")
	dev.Environment = "development"
	assert.Empty(t, dev.ProductionWarnings())
}
