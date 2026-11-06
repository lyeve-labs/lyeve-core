package config

import (
	"strings"
	"testing"
)

// A long random secret that happens to contain a refused word is still a
// secret, so the refusal names the variable and never prints the value. Every
// JWT_SECRETS entry signs or verifies tokens, so each one is held to the
// JWT_SECRET rules.
func TestValidateCritical_SecretRefusalsNameTheVariableOnly(t *testing.T) {
	const strong = "a-very-long-jwt-secret-key-that-is-secure"
	for _, tc := range []struct {
		name, variable, value string
		set                   func(c *Config, v string)
	}{
		{"weak JWT_SECRET", "JWT_SECRET", "9f3a1c-example-7e5b8d2046a1c4e7f0b3d68952",
			func(c *Config, v string) { c.JWTSecret = v; c.JWTSecrets = []string{v} }},
		{"short JWT_SECRET", "JWT_SECRET", "9f3a1c7e5b8d",
			func(c *Config, v string) { c.JWTSecret = v; c.JWTSecrets = []string{v} }},
		{"weak ENCRYPTION_KEY", "ENCRYPTION_KEY", "4be1d0-placeholder-a9c3e7f15b2d8064c1a7",
			func(c *Config, v string) { c.EncryptionKey = v }},
		{"weak JWT_SECRETS entry", "JWT_SECRETS", "77c0e2-changeme-b4f19a3d5e8c2071f6a9b3",
			func(c *Config, v string) { c.JWTSecrets = []string{strong, v} }},
		{"short JWT_SECRETS entry", "JWT_SECRETS", "77c0e2b4f19a",
			func(c *Config, v string) { c.JWTSecrets = []string{strong, v} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{
				JWTSecret:     strong,
				JWTSecrets:    []string{strong},
				EncryptionKey: "a-completely-different-encryption-key-32chars",
			}
			tc.set(c, tc.value)
			err := c.ValidateCritical()
			if err == nil {
				t.Fatalf("%s booted", tc.variable)
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Errorf("the refusal must name %s, got: %v", tc.variable, err)
			}
			if strings.Contains(err.Error(), tc.value) {
				t.Errorf("the refusal printed the secret: %v", err)
			}
		})
	}
}
