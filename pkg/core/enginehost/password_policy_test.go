package enginehost

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A plugin that sets a password reads the policy through the scoped host it
// is handed, so the cases go through ScopedHost to the engine host.
func TestValidatePassword_AppliesTheConfiguredPolicy(t *testing.T) {
	defaults := &config.Config{
		PasswordMinLength:         12,
		PasswordRequireComplexity: true,
		PasswordCheckCommon:       true,
		PasswordHashAlgo:          "bcrypt",
	}
	raised := &config.Config{
		PasswordMinLength:         16,
		PasswordRequireComplexity: true,
		PasswordCheckCommon:       true,
		PasswordHashAlgo:          "bcrypt",
	}
	relaxed := &config.Config{
		PasswordMinLength: 8,
		PasswordHashAlgo:  "argon2id",
	}

	cases := []struct {
		name       string
		cfg        *config.Config
		password   string
		wantCode   string
		wantParams map[string]any
	}{
		{"common", defaults, "Password1234", "AUTH_PASSWORD_TOO_COMMON", nil},
		{"short", defaults, "Short-9", "AUTH_PASSWORD_TOO_SHORT", map[string]any{"min": 12}},
		{"no complexity", defaults, "all-lower-case-words", "AUTH_PASSWORD_NEEDS_COMPLEXITY", nil},
		{"over bcrypt", defaults, "Aa1" + strings.Repeat("x", 70), "AUTH_PASSWORD_TOO_LONG", map[string]any{"max": 72}},
		{"accepted", defaults, "Correct-Horse-9", "", nil},
		{"under a raised minimum", raised, "Correct-Horse-9", "AUTH_PASSWORD_TOO_SHORT", map[string]any{"min": 16}},
		{"relaxed accepts common", relaxed, "password1234", "", nil},
		{"argon2id has no byte cap", relaxed, strings.Repeat("x", 100), "", nil},
		{"no configuration applies the default", nil, "Password1234", "AUTH_PASSWORD_TOO_COMMON", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := core.NewScopedHost(&engineHost{cfg: tc.cfg}, "password-reset", core.CapAll)
			err := h.ValidatePassword(tc.password)
			if tc.wantCode == "" {
				require.NoError(t, err)
				return
			}
			var v *core.PasswordViolation
			require.True(t, errors.As(err, &v), "err = %v", err)
			assert.Equal(t, tc.wantCode, v.Code)
			assert.Equal(t, tc.wantParams, v.Params)
		})
	}
}
