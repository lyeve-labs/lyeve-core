package enginehost

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A plugin that sets a password hashes it with the hasher the engine runs, and
// learns that hasher's byte limit before it spends anything on the password.
func TestEngineHost_HashesPasswordsWithTheConfiguredHasher(t *testing.T) {
	long := strings.Repeat("a", 100)
	cases := map[string]struct {
		host     *engineHost
		maxBytes int
		prefix   string
	}{
		"argon2id": {&engineHost{cfg: &config.Config{PasswordHashAlgo: "argon2id"}}, 0, "$argon2id$"},
		"bcrypt":   {&engineHost{cfg: &config.Config{PasswordHashAlgo: "bcrypt"}}, 72, "$2a$"},
		"default":  {&engineHost{}, 72, "$2a$"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var hasher core.PasswordHasher = core.NewScopedHost(c.host, "password-reset", 0)
			maxBytes, err := hasher.PasswordMaxBytes()
			require.NoError(t, err)
			assert.Equal(t, c.maxBytes, maxBytes)

			password := "a-password-of-twenty-six"
			if c.maxBytes == 0 {
				password = long
			}
			hash, err := hasher.HashPassword(password)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(hash, c.prefix), "hash %q", hash)
			require.NoError(t, auth.VerifyPassword("", hash, password))
		})
	}
}
