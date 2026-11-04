package config

import (
	"strings"
	"testing"
)

// APP_ENV decides whether the production checks run, so a value that names
// neither environment is refused. Read as "not production", a typo or a
// "staging" skipped every check while the operator believed they ran.
func TestLoad_RefusesAnUnknownAppEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:pw@localhost/db?sslmode=require")
	t.Setenv("JWT_SECRET", "load-test-jwt-secret-well-over-32-chars")
	t.Setenv("ENCRYPTION_KEY", "load-test-encryption-key-distinct-value")
	for _, env := range []string{"staging", "prod", "test", " production", "dev"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("APP_ENV", env)
			_, err := Load()
			if err == nil {
				t.Fatalf("APP_ENV=%q booted", env)
			}
			if !strings.Contains(err.Error(), "APP_ENV") {
				t.Errorf("the refusal must name APP_ENV, got: %v", err)
			}
		})
	}
	for _, env := range []string{"development", "Development"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("APP_ENV", env)
			if _, err := Load(); err != nil {
				t.Fatalf("APP_ENV=%q must boot: %v", env, err)
			}
		})
	}
}
