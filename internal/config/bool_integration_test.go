package config

import (
	"os"
	"testing"
)

// TestPasswordBoolEnvVars verifies that PASSWORD_REQUIRE_COMPLEXITY,
// PASSWORD_CHECK_COMMON, and STORAGE_S3_USE_SSL all respect strconv.ParseBool
// semantics: including "0", "FALSE", "f" as falsy values.
func TestPasswordBoolEnvVars(t *testing.T) {
	tests := []struct {
		name        string
		complexity  string
		checkCommon string
		s3UseSSL    string
		wantComplex bool
		wantCommon  bool
		wantS3SSL   bool
	}{
		// Default (unset): all three default to true
		{name: "unset defaults to true", wantComplex: true, wantCommon: true, wantS3SSL: true},

		// Explicit true
		{name: "explicit true",
			complexity: "true", checkCommon: "true", s3UseSSL: "true",
			wantComplex: true, wantCommon: true, wantS3SSL: true},
		{name: "explicit 1",
			complexity: "1", checkCommon: "1", s3UseSSL: "1",
			wantComplex: true, wantCommon: true, wantS3SSL: true},

		// Explicit false must read as false.
		{name: "explicit false",
			complexity: "false", checkCommon: "false", s3UseSSL: "false",
			wantComplex: false, wantCommon: false, wantS3SSL: false},
		{name: "explicit 0",
			complexity: "0", checkCommon: "0", s3UseSSL: "0",
			wantComplex: false, wantCommon: false, wantS3SSL: false},
		{name: "explicit FALSE",
			complexity: "FALSE", checkCommon: "FALSE", s3UseSSL: "FALSE",
			wantComplex: false, wantCommon: false, wantS3SSL: false},
		{name: "explicit f",
			complexity: "f", checkCommon: "f", s3UseSSL: "f",
			wantComplex: false, wantCommon: false, wantS3SSL: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up the minimum env for Load() to succeed.
			os.Setenv("DATABASE_URL", "postgres://localhost/test")
			os.Setenv("JWT_SECRET", "test-secret-16+chars!")
			os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
			t.Cleanup(func() {
				os.Unsetenv("DATABASE_URL")
				os.Unsetenv("JWT_SECRET")
				os.Unsetenv("ENCRYPTION_KEY")
			})

			setOrUnset(t, "PASSWORD_REQUIRE_COMPLEXITY", tt.complexity)
			setOrUnset(t, "PASSWORD_CHECK_COMMON", tt.checkCommon)
			setOrUnset(t, "STORAGE_S3_USE_SSL", tt.s3UseSSL)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() failed: %v", err)
			}

			if cfg.PasswordRequireComplexity != tt.wantComplex {
				t.Errorf("PasswordRequireComplexity = %v (env %q), want %v",
					cfg.PasswordRequireComplexity, tt.complexity, tt.wantComplex)
			}
			if cfg.PasswordCheckCommon != tt.wantCommon {
				t.Errorf("PasswordCheckCommon = %v (env %q), want %v",
					cfg.PasswordCheckCommon, tt.checkCommon, tt.wantCommon)
			}
			if cfg.StorageS3UseSSL != tt.wantS3SSL {
				t.Errorf("StorageS3UseSSL = %v (env %q), want %v",
					cfg.StorageS3UseSSL, tt.s3UseSSL, tt.wantS3SSL)
			}
		})
	}
}

func setOrUnset(t *testing.T, key, val string) {
	t.Helper()
	if val == "" {
		os.Unsetenv(key)
	} else {
		os.Setenv(key, val)
	}
	t.Cleanup(func() { os.Unsetenv(key) })
}
