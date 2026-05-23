package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateProduction_NonProduction_NoOp(t *testing.T) {
	cfg := &Config{
		Environment:   "development",
		DatabaseURL:   "postgres://localhost/db?sslmode=disable",
		JWTSecret:     "short",
		EncryptionKey: "short", // same as JWTSecret
		SecureCookie:  false,
		CORSOrigins:   []string{"*"},
	}
	if err := cfg.ValidateProduction(); err != nil {
		t.Errorf("development mode should not validate, got: %v", err)
	}
}

func TestValidateProduction_AllClean(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://host/db?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		AuditHMACKey:  "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		RateLimitRPS:  100.0,
	}
	if err := cfg.ValidateProduction(); err != nil {
		t.Errorf("clean config should pass, got: %v", err)
	}
}

// A plaintext database connection boots with a warning rather than refusing
// the boot, because an engine and its database on one private network may run
// without TLS.
func TestValidateProduction_PlaintextDatabaseBootsWithAWarning(t *testing.T) {
	base := func(url string) *Config {
		return &Config{
			Environment:   "production",
			DatabaseURL:   url,
			JWTSecret:     "a-very-long-secret-key-that-is-secure",
			EncryptionKey: "separate-encryption-key-value",
			SecureCookie:  true,
			CORSOrigins:   []string{"https://app.example.com"},
			ConsoleURL:    "https://admin.example.com",
		}
	}
	for _, tc := range []struct {
		url  string
		warn string
	}{
		{"postgres://host/db?sslmode=disable", "sslmode=disable"},
		{"postgres://host/db?sslmode=prefer&connect_timeout=5", "sslmode=prefer"},
		{"postgres://host/db", "sets no sslmode"},
		{"host=db user=app sslmode=allow", "sslmode=allow"},
		{"postgres://host/db?sslmode=require", ""},
		{"postgres://host/db?SSLMODE=verify-full", ""},
	} {
		cfg := base(tc.url)
		if err := cfg.ValidateProduction(); err != nil && contains(err.Error(), "sslmode") {
			t.Errorf("%s: sslmode must not refuse the boot, got: %v", tc.url, err)
		}
		warns := cfg.ProductionWarnings()
		if tc.warn == "" {
			if len(warns) != 0 {
				t.Errorf("%s: expected no warning, got %v", tc.url, warns)
			}
			continue
		}
		if len(warns) != 1 || !contains(warns[0], tc.warn) {
			t.Errorf("%s: expected one warning naming %q, got %v", tc.url, tc.warn, warns)
		}
	}

	dev := base("postgres://host/db?sslmode=disable")
	dev.Environment = "development"
	if w := dev.ProductionWarnings(); len(w) != 0 {
		t.Errorf("development must not warn, got %v", w)
	}
	mysql := base("app:pw@tcp(db:3306)/app")
	mysql.DatabaseDriver = "mysql"
	if w := mysql.ProductionWarnings(); len(w) != 0 {
		t.Errorf("a MySQL DSN has no sslmode to read, got %v", w)
	}
}

func TestValidateProduction_EncryptionKeyFallback(t *testing.T) {
	secret := "a-very-long-secret-key-that-is-secure"
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://host/db?sslmode=require",
		JWTSecret:     secret,
		EncryptionKey: secret, // same as JWTSecret = fallback
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
	}
	err := cfg.ValidateProduction()
	if err == nil {
		t.Fatal("expected error for encryption key fallback")
	}
	if got := err.Error(); !contains(got, "ENCRYPTION_KEY") {
		t.Errorf("error should mention ENCRYPTION_KEY, got: %s", got)
	}
}

func TestValidateProduction_WeakJWTSecret(t *testing.T) {
	tests := []struct {
		name   string
		secret string
	}{
		{"change-me", "change-me"},
		{"changeme", "changeme"},
		{"secret", "secret"},
		{"password", "password"},
		{"short", "abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Environment:   "production",
				DatabaseURL:   "postgres://host/db?sslmode=require",
				JWTSecret:     tt.secret,
				EncryptionKey: "separate-encryption-key-value",
				SecureCookie:  true,
				CORSOrigins:   []string{"https://app.example.com"},
			}
			// Weak JWT secrets are checked by ValidateCritical (runs in all environments).
			err := cfg.ValidateCritical()
			if err == nil {
				t.Fatalf("expected error for weak secret %q", tt.secret)
			}
		})
	}
}

func TestValidateProduction_CORSWildcard(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://host/db?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value",
		SecureCookie:  true,
		CORSOrigins:   []string{"*"},
	}
	// CORS wildcard is checked by ValidateCritical (runs in all environments).
	err := cfg.ValidateCritical()
	if err == nil {
		t.Fatal("expected error for CORS wildcard")
	}
	if got := err.Error(); !contains(got, "wildcard") {
		t.Errorf("error should mention wildcard, got: %s", got)
	}
}

func TestValidateProduction_SecureCookieDisabled(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://host/db?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value",
		SecureCookie:  false,
		CORSOrigins:   []string{"https://app.example.com"},
	}
	err := cfg.ValidateProduction()
	if err == nil {
		t.Fatal("expected error for SecureCookie disabled")
	}
	if got := err.Error(); !contains(got, "SECURE_COOKIE") {
		t.Errorf("error should mention SECURE_COOKIE, got: %s", got)
	}
}

func TestValidateProduction_MultipleErrors(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://host/db?sslmode=disable",
		JWTSecret:     "short",
		EncryptionKey: "short",
		SecureCookie:  false,
		CORSOrigins:   []string{"*"},
	}
	// Critical checks (JWT_SECRET, CORS) + production checks (ENCRYPTION_KEY, SECURE_COOKIE).
	// sslmode=disable is a warning, not an error.
	critErr := cfg.ValidateCritical()
	prodErr := cfg.ValidateProduction()
	if critErr == nil && prodErr == nil {
		t.Fatal("expected multiple errors")
	}
	// Check that critical errors contain JWT_SECRET and wildcard
	if critErr != nil {
		critMsg := critErr.Error()
		for _, want := range []string{"JWT_SECRET", "wildcard"} {
			if !contains(critMsg, want) {
				t.Errorf("critical error should contain %q, got: %s", want, critMsg)
			}
		}
	}
	// Check that production errors contain ENCRYPTION_KEY and SECURE_COOKIE
	if prodErr != nil {
		prodMsg := prodErr.Error()
		if contains(prodMsg, "sslmode") {
			t.Errorf("sslmode must be a warning, not a production error: %s", prodMsg)
		}
		for _, want := range []string{"ENCRYPTION_KEY", "SECURE_COOKIE"} {
			if !contains(prodMsg, want) {
				t.Errorf("production error should contain %q, got: %s", want, prodMsg)
			}
		}
	}
}

func TestIsProduction(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"production", true},
		{"staging", false},
		{"development", false},
		{"", false},
	}
	for _, tt := range tests {
		cfg := &Config{Environment: tt.env}
		if got := cfg.IsProduction(); got != tt.want {
			t.Errorf("IsProduction(%q) = %v, want %v", tt.env, got, tt.want)
		}
	}
}

// ENCRYPTION_KEY entropy validation

func TestValidateCritical_WeakEncryptionKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"change-me", "change-me"},
		{"secret", "secret"},
		{"password", "password"},
		{"default", "default"},
		{"dev-secret", "dev-secret"},
		{"insecure", "insecure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				JWTSecret:     "a-very-long-jwt-secret-key-that-is-secure",
				EncryptionKey: tt.key,
				CORSOrigins:   []string{"https://app.example.com"},
			}
			err := cfg.ValidateCritical()
			if err == nil {
				t.Fatalf("expected error for weak ENCRYPTION_KEY %q", tt.key)
			}
			if got := err.Error(); !contains(got, "ENCRYPTION_KEY") {
				t.Errorf("error should mention ENCRYPTION_KEY, got: %s", got)
			}
		})
	}
}

func TestValidateCritical_EncryptionKeyTooShort(t *testing.T) {
	cfg := &Config{
		JWTSecret:     "a-very-long-jwt-secret-key-that-is-secure",
		EncryptionKey: "too-short-key", // < 32 chars
		CORSOrigins:   []string{"https://app.example.com"},
	}
	err := cfg.ValidateCritical()
	if err == nil {
		t.Fatal("expected error for short ENCRYPTION_KEY")
	}
	if got := err.Error(); !contains(got, "32 chars") {
		t.Errorf("error should mention 32 chars, got: %s", got)
	}
}

func TestValidateCritical_EncryptionKeyEqualsJWTSecret(t *testing.T) {
	secret := "a-very-long-jwt-secret-key-that-is-secure"
	cfg := &Config{
		JWTSecret:     secret,
		EncryptionKey: secret, // same as JWT_SECRET
		CORSOrigins:   []string{"https://app.example.com"},
	}
	err := cfg.ValidateCritical()
	if err == nil {
		t.Fatal("expected error when ENCRYPTION_KEY equals JWT_SECRET")
	}
	if got := err.Error(); !contains(got, "must not equal JWT_SECRET") {
		t.Errorf("error should mention key separation, got: %s", got)
	}
}

func TestValidateCritical_EncryptionKeyValid(t *testing.T) {
	cfg := &Config{
		JWTSecret:     "a-very-long-jwt-secret-key-that-is-secure",
		EncryptionKey: "a-completely-different-encryption-key-32chars",
		CORSOrigins:   []string{"https://app.example.com"},
	}
	if err := cfg.ValidateCritical(); err != nil {
		t.Errorf("valid ENCRYPTION_KEY should pass, got: %v", err)
	}
}

func TestValidateCritical_EncryptionKeyEmpty_DefaultsToJWT(t *testing.T) {
	// When ENCRYPTION_KEY is empty (not explicitly set), Load() falls back
	// to JWT_SECRET. In that case EncryptionKey == JWTSecret, which should
	// be caught by the key separation check.
	secret := "a-very-long-jwt-secret-key-that-is-secure"
	cfg := &Config{
		JWTSecret:     secret,
		EncryptionKey: secret, // simulates the Load() fallback
		CORSOrigins:   []string{"https://app.example.com"},
	}
	err := cfg.ValidateCritical()
	if err == nil {
		t.Fatal("expected error when EncryptionKey falls back to JWTSecret")
	}
}

// TestValidateProduction_EncryptionKeyFallback_Blocked confirms that the
// ENCRYPTION_KEY -> JWT_SECRET fallback cannot reach production. The check
// exists in both ValidateCritical (all envs) and ValidateProduction (prod).
func TestValidateProduction_EncryptionKeyFallback_Blocked(t *testing.T) {
	secret := "a-very-long-jwt-secret-key-that-is-secure"
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://host/db?sslmode=require",
		JWTSecret:     secret,
		EncryptionKey: secret, // fallback to JWT_SECRET
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
	}
	// ValidateCritical should catch it (all environments).
	if err := cfg.ValidateCritical(); err == nil {
		t.Fatal("ValidateCritical should reject ENCRYPTION_KEY == JWT_SECRET")
	}
	// ValidateProduction should also catch it (redundant but defensive).
	if err := cfg.ValidateProduction(); err == nil {
		t.Fatal("ValidateProduction should reject ENCRYPTION_KEY == JWT_SECRET")
	}
}

// TestValidateProduction_NoJWTSecrets confirms that a production instance
// without any JWT signing secret fails validation: it must not boot.
func TestValidateProduction_NoJWTSecrets(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://host/db?sslmode=require",
		JWTSecret:     "",
		JWTSecrets:    nil,
		EncryptionKey: "separate-encryption-key-value",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
	}
	err := cfg.ValidateProduction()
	if err == nil {
		t.Fatal("expected error for missing JWT_SECRET in production")
	}
	if got := err.Error(); !contains(got, "JWT_SECRET") {
		t.Errorf("error should mention JWT_SECRET, got: %s", got)
	}
}

// Production fail-closed: default DB credentials

func TestValidateProduction_DefaultDBCredentials_Postgres(t *testing.T) {
	tests := []struct {
		name   string
		dbURL  string
		expect bool // true = should fail
	}{
		{"postgres:postgres", "postgres://postgres:***@localhost/db?sslmode=require", true},
		{"postgres no pass", "postgres://postgres@localhost/db?sslmode=require", true},
		{"root pass", "root:***@localhost/db?sslmode=require", true},
		{"root no pass", "mysql://root@localhost/db", true},
		{"admin pass", "admin:***@localhost/db?sslmode=require", true},
		{"sa no pass (mssql)", "sqlserver://sa@localhost/db", true},
		{"valid creds", "postgres://myapp:strongpass123@db.prod.internal/mydb?sslmode=require", false},
		{"valid root with strong pass", "postgres://root:very-long-random-password-here@localhost/db?sslmode=require", false},
		// The canonical MySQL DSN puts the host in parentheses, which makes
		// url.Parse fail on the port inside tcp(...), so a net/url-based
		// check would miss root here.
		{"mysql tcp form", "mysql://root:pw@tcp(db:3306)/app", true},
		{"mysql tcp form, non-default user", "mysql://appuser:pw@tcp(db:3306)/app", false},
		{"mysql tcp form, no scheme", "root:pw@tcp(db:3306)/app", true},
		{"postgresql scheme spelled out", "postgresql://postgres:pw@localhost/db?sslmode=require", true},
		{"mssql scheme alias", "mssql://sa:pw@localhost:1433?database=db", true},
		{"password containing an at sign", "postgres://appuser:p@ssw0rd@localhost/db?sslmode=require", false},
		{"default user with an at sign in the password", "postgres://postgres:p@ss@localhost/db?sslmode=require", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Environment:   "production",
				DatabaseURL:   tt.dbURL,
				JWTSecret:     "a-very-long-secret-key-that-is-secure",
				EncryptionKey: "separate-encryption-key-value",
				SecureCookie:  true,
				CORSOrigins:   []string{"https://app.example.com"},
			}
			err := cfg.ValidateProduction()
			if tt.expect && err == nil {
				t.Fatalf("expected error for default DB credentials in %q", tt.dbURL)
			}
			if tt.expect && err != nil {
				if got := err.Error(); !contains(got, "default/dev credentials") {
					t.Errorf("error should mention default credentials, got: %s", got)
				}
			}
			if !tt.expect && err != nil {
				// Filter out non-credential errors
				if contains(err.Error(), "default/dev credentials") {
					t.Errorf("should not flag valid credentials as default, got: %v", err)
				}
			}
		})
	}
}

// Production fail-closed: JWT key file permissions

func TestValidateProduction_JWTKeyFileWorldReadable(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "jwt_key.json")
	if err := os.WriteFile(keyPath, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:strongpass@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		JWTKeyPath:    keyPath,
	}
	err := cfg.ValidateProduction()
	if err == nil {
		t.Fatal("expected error for world-readable JWT key file")
	}
	if got := err.Error(); !contains(got, "world-readable") {
		t.Errorf("error should mention world-readable, got: %s", got)
	}
}

func TestValidateProduction_JWTKeyFileSecure(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "jwt_key.json")
	if err := os.WriteFile(keyPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:strongpass@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		JWTKeyPath:    keyPath,
	}
	// Should NOT flag the secure key file (other checks may still fail,
	// so only check that world-readable is not in the error).
	err := cfg.ValidateProduction()
	if err != nil && contains(err.Error(), "world-readable") {
		t.Errorf("secure key file should not trigger world-readable error, got: %v", err)
	}
}

func TestValidateProduction_JWTKeyFileMissing(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:strongpass@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		JWTKeyPath:    "/nonexistent/path/jwt_key.json",
	}
	// Missing file is fine: InitJWTSigning will create it.
	err := cfg.ValidateProduction()
	if err != nil && contains(err.Error(), "world-readable") {
		t.Errorf("missing key file should not trigger world-readable error, got: %v", err)
	}
}

// Production fail-closed: CORS wildcard + credentials

func TestValidateProduction_CORSWildcardWithCredentials(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:strongpass@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value",
		SecureCookie:  true,
		CORSOrigins:   []string{"*"},
	}
	err := cfg.ValidateProduction()
	if err == nil {
		t.Fatal("expected error for CORS wildcard in production")
	}
	if got := err.Error(); !contains(got, "CSRF") {
		t.Errorf("error should mention CSRF vector, got: %s", got)
	}
}

// Production fail-closed: JWT expiry cap

func TestValidateProduction_JWTExpiryHigh(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:***@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		JWTExpirySecs: 7200, // 2 hours: too long
	}
	err := cfg.ValidateProduction()
	if err == nil {
		t.Fatal("expected error for high JWT_EXPIRY_SECS in production")
	}
	if got := err.Error(); !contains(got, "JWT_EXPIRY_SECS") {
		t.Errorf("error should mention JWT_EXPIRY_SECS, got: %s", got)
	}
}

func TestValidateProduction_JWTExpiryAcceptable(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:***@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		JWTExpirySecs: 900, // 15 minutes: default, acceptable
	}
	err := cfg.ValidateProduction()
	if err != nil && contains(err.Error(), "JWT_EXPIRY_SECS") {
		t.Errorf("default 15m expiry should not trigger JWT_EXPIRY_SECS error, got: %v", err)
	}
}

// hasDefaultDBCredentials unit tests

func TestHasDefaultDBCredentials(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"postgres://postgres:postgres@localhost/db", true},
		{"postgres://postgres@localhost/db", true},
		{"postgres://myapp:strongpass@localhost/db", false},
		{"mysql://root:root@tcp(localhost)/db", true},
		{"mysql://root@tcp(localhost)/db", true},
		{"sqlserver://sa@localhost/db", true},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := hasDefaultDBCredentials(tt.url); got != tt.want {
				t.Errorf("hasDefaultDBCredentials(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

// Critical validation: PASSWORD_HASH_ALGO allowlist

func TestValidateCritical_PasswordHashAlgo_Invalid(t *testing.T) {
	cfg := &Config{
		JWTSecret:        "a-very-long-jwt-secret-key-that-is-secure",
		CORSOrigins:      []string{"https://app.example.com"},
		PasswordHashAlgo: "argon2d", // typo: not in allowlist
	}
	err := cfg.ValidateCritical()
	if err == nil {
		t.Fatal("expected error for unsupported PASSWORD_HASH_ALGO")
	}
	got := err.Error()
	if !contains(got, "PASSWORD_HASH_ALGO") {
		t.Errorf("error should mention PASSWORD_HASH_ALGO, got: %s", got)
	}
	if !contains(got, "must be one of: bcrypt, argon2id") {
		t.Errorf("error should list supported values, got: %s", got)
	}
}

func TestValidateCritical_PasswordHashAlgo_Bcrypt(t *testing.T) {
	cfg := &Config{
		JWTSecret:        "a-very-long-jwt-secret-key-that-is-secure",
		CORSOrigins:      []string{"https://app.example.com"},
		PasswordHashAlgo: "bcrypt",
	}
	err := cfg.ValidateCritical()
	if err != nil {
		t.Errorf("bcrypt should pass validation, got: %v", err)
	}
}

func TestValidateCritical_PasswordHashAlgo_Argon2ID(t *testing.T) {
	cfg := &Config{
		JWTSecret:        "a-very-long-jwt-secret-key-that-is-secure",
		CORSOrigins:      []string{"https://app.example.com"},
		PasswordHashAlgo: "argon2id",
	}
	err := cfg.ValidateCritical()
	if err != nil {
		t.Errorf("argon2id should pass validation, got: %v", err)
	}
}

func TestValidateCritical_PasswordHashAlgo_Empty(t *testing.T) {
	// Empty PASSWORD_HASH_ALGO is acceptable: Load() always defaults
	// to "bcrypt" via envOr. ValidateCritical should pass when empty.
	cfg := &Config{
		JWTSecret:        "a-very-long-jwt-secret-key-that-is-secure",
		CORSOrigins:      []string{"https://app.example.com"},
		PasswordHashAlgo: "",
	}
	err := cfg.ValidateCritical()
	if err != nil {
		t.Errorf("empty PASSWORD_HASH_ALGO should pass (Load() defaults to bcrypt), got: %v", err)
	}
}

// Critical fail-closed: driver/provider validation

func TestValidateCritical_DatabaseDriver_Invalid(t *testing.T) {
	cfg := &Config{
		JWTSecret:      "a-very-long-jwt-secret-key-that-is-secure",
		CORSOrigins:    []string{"https://app.example.com"},
		DatabaseDriver: "sqlite",
	}
	err := cfg.ValidateCritical()
	if err == nil {
		t.Fatal("expected error for unsupported DATABASE_DRIVER")
	}
	got := err.Error()
	if !contains(got, "DATABASE_DRIVER") {
		t.Errorf("error should mention DATABASE_DRIVER, got: %s", got)
	}
	if !contains(got, "must be one of: postgres, mysql, mssql") {
		t.Errorf("error should list supported values, got: %s", got)
	}
}

func TestValidateCritical_DatabaseDriver_Valid(t *testing.T) {
	for _, driver := range []string{"postgres", "mysql", "mssql"} {
		t.Run(driver, func(t *testing.T) {
			cfg := &Config{
				JWTSecret:      "a-very-long-jwt-secret-key-that-is-secure",
				CORSOrigins:    []string{"https://app.example.com"},
				DatabaseDriver: driver,
			}
			if err := cfg.ValidateCritical(); err != nil {
				t.Errorf("%s should pass validation, got: %v", driver, err)
			}
		})
	}
}

func TestValidateCritical_DatabaseDriver_Empty(t *testing.T) {
	// Empty is acceptable: Load() always defaults to "postgres" via envOr.
	cfg := &Config{
		JWTSecret:      "a-very-long-jwt-secret-key-that-is-secure",
		CORSOrigins:    []string{"https://app.example.com"},
		DatabaseDriver: "",
	}
	if err := cfg.ValidateCritical(); err != nil {
		t.Errorf("empty DATABASE_DRIVER should pass (Load() defaults to postgres), got: %v", err)
	}
}

func TestValidateCritical_StorageDriver_Invalid(t *testing.T) {
	cfg := &Config{
		JWTSecret:     "a-very-long-jwt-secret-key-that-is-secure",
		CORSOrigins:   []string{"https://app.example.com"},
		StorageDriver: "r2",
	}
	err := cfg.ValidateCritical()
	if err == nil {
		t.Fatal("expected error for unsupported STORAGE_DRIVER")
	}
	got := err.Error()
	if !contains(got, "STORAGE_DRIVER") {
		t.Errorf("error should mention STORAGE_DRIVER, got: %s", got)
	}
	if !contains(got, "must be one of: local, s3") {
		t.Errorf("error should list supported values, got: %s", got)
	}
}

func TestValidateCritical_StorageDriver_Valid(t *testing.T) {
	for _, driver := range []string{"local", "s3"} {
		t.Run(driver, func(t *testing.T) {
			cfg := &Config{
				JWTSecret:     "a-very-long-jwt-secret-key-that-is-secure",
				CORSOrigins:   []string{"https://app.example.com"},
				StorageDriver: driver,
			}
			if err := cfg.ValidateCritical(); err != nil {
				t.Errorf("%s should pass validation, got: %v", driver, err)
			}
		})
	}
}

func TestValidateCritical_StorageDriver_Empty(t *testing.T) {
	cfg := &Config{
		JWTSecret:     "a-very-long-jwt-secret-key-that-is-secure",
		CORSOrigins:   []string{"https://app.example.com"},
		StorageDriver: "",
	}
	if err := cfg.ValidateCritical(); err != nil {
		t.Errorf("empty STORAGE_DRIVER should pass (Load() defaults to local), got: %v", err)
	}
}

func TestValidateCritical_MultipleDriverErrors(t *testing.T) {
	// Both invalid: should produce two separate error lines.
	cfg := &Config{
		JWTSecret:      "a-very-long-jwt-secret-key-that-is-secure",
		CORSOrigins:    []string{"https://app.example.com"},
		DatabaseDriver: "nope",
		StorageDriver:  "nope",
	}
	err := cfg.ValidateCritical()
	if err == nil {
		t.Fatal("expected error for both invalid drivers")
	}
	got := err.Error()
	if !contains(got, "DATABASE_DRIVER") {
		t.Error("error should mention DATABASE_DRIVER")
	}
	if !contains(got, "STORAGE_DRIVER") {
		t.Error("error should mention STORAGE_DRIVER")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// envBool helper tests

func TestEnvBool_Unset(t *testing.T) {
	os.Unsetenv("TEST_ENVBOOL_UNSET")
	if got := envBool("TEST_ENVBOOL_UNSET", true); !got {
		t.Error("envBool with default=true should return true when unset")
	}
	if got := envBool("TEST_ENVBOOL_UNSET", false); got {
		t.Error("envBool with default=false should return false when unset")
	}
}

func TestEnvBool_TrueValues(t *testing.T) {
	tests := []string{"1", "t", "T", "TRUE", "true", "True"}
	for _, v := range tests {
		os.Setenv("TEST_ENVBOOL_TRUE", v)
		if got := envBool("TEST_ENVBOOL_TRUE", false); !got {
			t.Errorf("envBool(=%q, default=false) = false; want true", v)
		}
	}
	os.Unsetenv("TEST_ENVBOOL_TRUE")
}

func TestEnvBool_FalseValues(t *testing.T) {
	tests := []string{"0", "f", "F", "FALSE", "false", "False"}
	for _, v := range tests {
		os.Setenv("TEST_ENVBOOL_FALSE", v)
		if got := envBool("TEST_ENVBOOL_FALSE", true); got {
			t.Errorf("envBool(=%q, default=true) = true; want false", v)
		}
	}
	os.Unsetenv("TEST_ENVBOOL_FALSE")
}

func TestEnvBool_InvalidUsesDefault(t *testing.T) {
	tests := []struct {
		val string
		def bool
	}{
		{"yes", false},
		{"no", true},
		{"on", false},
		{"off", true},
		{"2", true},
		{"garbage", false},
		{"", false}, // empty string treated as unset
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			if tt.val == "" {
				os.Unsetenv("TEST_ENVBOOL_INVALID")
			} else {
				os.Setenv("TEST_ENVBOOL_INVALID", tt.val)
				defer os.Unsetenv("TEST_ENVBOOL_INVALID")
			}
			if got := envBool("TEST_ENVBOOL_INVALID", tt.def); got != tt.def {
				t.Errorf("envBool(=%q, default=%v) = %v; want %v", tt.val, tt.def, got, tt.def)
			}
		})
	}
}

// Audit HMAC key

func TestValidateProduction_AuditHMACKey_Unset(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:***@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value-sk32",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		RateLimitRPS:  100,
		AuditHMACKey:  "",
	}
	err := cfg.ValidateProduction()
	if err == nil {
		t.Fatal("expected error for empty AuditHMACKey in production")
	}
	if got := err.Error(); !contains(got, "LYEVE_AUDIT_HMAC_KEY") {
		t.Errorf("error should mention LYEVE_AUDIT_HMAC_KEY, got: %s", got)
	}
}

func TestValidateProduction_AuditHMACKey_TooShort(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:***@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value-sk32",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		AuditHMACKey:  "aabb", // 4 chars, not 64
	}
	err := cfg.ValidateProduction()
	if err == nil {
		t.Fatal("expected error for too-short AuditHMACKey in production")
	}
	if got := err.Error(); !contains(got, "length 4") {
		t.Errorf("error should mention length, got: %s", got)
	}
}

func TestValidateProduction_AuditHMACKey_Valid(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:***@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value-sk32",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		AuditHMACKey:  "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", // 64 chars
	}
	err := cfg.ValidateProduction()
	if err != nil && contains(err.Error(), "LYEVE_AUDIT_HMAC_KEY") {
		t.Errorf("valid AuditHMACKey should not trigger HMAC key errors, got: %v", err)
	}
}

// Production fail-closed: rate limiter disabled

func TestValidateProduction_RateLimitRPS_Unset(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:***@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value-sk32",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		AuditHMACKey:  "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		RateLimitRPS:  0, // unset / disabled
	}
	err := cfg.ValidateProduction()
	if err == nil {
		t.Fatal("expected error for disabled RATE_LIMIT_RPS in production")
	}
	if got := err.Error(); !contains(got, "RATE_LIMIT_RPS") {
		t.Errorf("error should mention RATE_LIMIT_RPS, got: %s", got)
	}
}

func TestValidateProduction_RateLimitRPS_Set(t *testing.T) {
	cfg := &Config{
		Environment:   "production",
		DatabaseURL:   "postgres://myapp:***@db.internal/mydb?sslmode=require",
		JWTSecret:     "a-very-long-secret-key-that-is-secure",
		EncryptionKey: "separate-encryption-key-value-sk32",
		SecureCookie:  true,
		CORSOrigins:   []string{"https://app.example.com"},
		AuditHMACKey:  "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		RateLimitRPS:  100.0,
	}
	err := cfg.ValidateProduction()
	if err != nil && contains(err.Error(), "RATE_LIMIT_RPS") {
		t.Errorf("RATE_LIMIT_RPS=100 should not trigger rate limiter warning, got: %v", err)
	}
}
func TestValidateNonProduction_AuditHMACKey_EmptyAllowed(t *testing.T) {
	// In non-production, zero key is acceptable: ValidateProduction is a no-op.
	cfg := &Config{
		Environment:  "development",
		DatabaseURL:  "postgres://localhost/db?sslmode=disable",
		JWTSecret:    "dev-secret-that-is-long-enough",
		AuditHMACKey: "",
	}
	err := cfg.ValidateProduction()
	if err != nil {
		t.Fatalf("ValidateProduction should be no-op in development: %v", err)
	}
}

// An unset APP_ENV must land in production, not in a mode that skips every
// check in ValidateProduction: default-credential rejection, the rate-limit
// floor, the audit HMAC key requirement, and aborting boot when JWT signing
// cannot initialize.
func TestLoad_UnsetAppEnvDefaultsToProduction(t *testing.T) {
	t.Setenv("APP_ENV", "")
	os.Unsetenv("APP_ENV")
	t.Setenv("DATABASE_URL", "postgres://user:pw@localhost/db?sslmode=require")
	t.Setenv("JWT_SECRET", "load-test-jwt-secret-well-over-32-chars")
	t.Setenv("ENCRYPTION_KEY", "load-test-encryption-key-distinct-value")

	_, err := Load()
	if err == nil {
		t.Fatal("Load with APP_ENV unset succeeded: production validation did not run")
	}
	if !strings.Contains(err.Error(), "production config validation") {
		t.Fatalf("expected production validation to reject this config, got: %v", err)
	}
}

// The default is a constant so callers and docs cannot drift from it.
func TestDefaultEnvironment_IsProduction(t *testing.T) {
	if DefaultEnvironment != "production" {
		t.Fatalf("DefaultEnvironment = %q, want production", DefaultEnvironment)
	}
	cfg := &Config{Environment: DefaultEnvironment}
	if !cfg.IsProduction() {
		t.Fatal("DefaultEnvironment must satisfy IsProduction")
	}
}
