package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// weakSecretMarks are the substrings that make a secret not one.
//
// The test is a substring match, not an exact one, because a placeholder such
// as "change-me-to-a-32-plus-character-jwt-secret" is long enough to clear the
// length check and equals no entry. A substring test catches the placeholder
// wherever it is copied from: a manifest, an example environment file, a
// documentation page, a support message.
var weakSecretMarks = []string{
	"change-me",
	"change-this",
	"changeme",
	"replace-me",
	"replaceme",
	"your-secret",
	"dev-secret",
	"insecure",
	"example",
	"placeholder",
}

// weakSecretWords are values that are weak on their own rather than as part
// of a longer string, so they stay an exact match: "default" as a substring
// would refuse a perfectly good random secret that happens to contain it.
var weakSecretWords = map[string]bool{
	"secret":   true,
	"password": true,
	"default":  true,
}

// isWeakSecret reports whether a configured secret is a placeholder or a
// common weak value.
func isWeakSecret(v string) bool {
	if weakSecretWords[strings.ToLower(v)] {
		return true
	}
	lower := strings.ToLower(v)
	for _, mark := range weakSecretMarks {
		if strings.Contains(lower, mark) {
			return true
		}
	}
	return false
}

// IsProduction returns true when the Environment field is set to "production"
// (case-insensitive).
func (c *Config) IsProduction() bool {
	return strings.ToLower(c.Environment) == "production"
}

// ValidateCritical performs environment-agnostic safety checks that should
// fail in any environment: weak secrets, driver/provider validation, etc.
func (c *Config) ValidateCritical() error {
	var errs []string

	// Weak JWT secret
	if isWeakSecret(c.JWTSecret) {
		errs = append(errs, fmt.Sprintf("JWT_SECRET %q is too weak - must be a strong, unique key", c.JWTSecret))
	}
	if len(c.JWTSecret) > 0 && len(c.JWTSecret) < 16 {
		errs = append(errs, fmt.Sprintf("JWT_SECRET too short (%d chars) - must be at least 16 characters", len(c.JWTSecret)))
	}

	// Weak encryption key
	// Empty EncryptionKey falls back to JWTSecret: not a hard error here.
	if c.EncryptionKey != "" {
		if isWeakSecret(c.EncryptionKey) {
			errs = append(errs, fmt.Sprintf("ENCRYPTION_KEY %q is too weak - must be a strong, unique key", c.EncryptionKey))
		}
		if len(c.EncryptionKey) < 32 {
			errs = append(errs, fmt.Sprintf("ENCRYPTION_KEY too short (%d chars) - must be at least 32 chars", len(c.EncryptionKey)))
		}
	}
	if c.EncryptionKey != "" && c.EncryptionKey == c.JWTSecret {
		errs = append(errs, "ENCRYPTION_KEY must not equal JWT_SECRET - key hierarchy requires separate values")
	}

	// CORS wildcards: fail-closed in all environments
	for _, origin := range c.CORSOrigins {
		if origin == "*" {
			errs = append(errs, "CORS_ORIGINS contains wildcard \"*\" - this is a CSRF vector. Use explicit origins")
		}
	}

	// Password hash algorithm
	switch c.PasswordHashAlgo {
	case "", "bcrypt", "argon2id":
		// supported (empty is acceptable: Load() defaults to bcrypt)
	default:
		errs = append(errs, fmt.Sprintf("PASSWORD_HASH_ALGO %q is invalid - must be one of: bcrypt, argon2id", c.PasswordHashAlgo))
	}

	// Database driver
	switch c.DatabaseDriver {
	case "":
		// empty is defaulted to "postgres" by Load()
	case "postgres", "mysql", "mssql":
		// supported
	default:
		errs = append(errs, fmt.Sprintf("DATABASE_DRIVER %q is invalid - must be one of: postgres, mysql, mssql", c.DatabaseDriver))
	}

	// Storage driver
	if c.StorageDriver != "" && c.StorageDriver != "local" && c.StorageDriver != "s3" {
		errs = append(errs, fmt.Sprintf("STORAGE_DRIVER %q is invalid - must be one of: local, s3", c.StorageDriver))
	}

	if msg := consoleURLProblem(c.ConsoleURL); msg != "" {
		errs = append(errs, msg)
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("config validation errors:\n\t%s", strings.Join(errs, "\n\t"))
}

// validateStatelessProduction is the production check for an engine with no
// database. It signs no session, keeps no audit chain and connects to no
// database, so the checks on those settings have nothing to protect. What it
// still serves is an HTTP API open to the network, authenticated by keys that
// do not expire on their own.
func (c *Config) validateStatelessProduction() error {
	var prodErrs []string
	for _, origin := range c.CORSOrigins {
		if origin == "*" {
			prodErrs = append(prodErrs, "CORS_ORIGINS contains wildcard \"*\" - this is a CSRF vector. Use explicit origins in production")
		}
	}
	// No cookie is set here, but the same setting turns on HSTS and the
	// redirect to HTTPS, which keep a long-lived API key off plain HTTP.
	if !c.SecureCookie {
		prodErrs = append(prodErrs, "SECURE_COOKIE must be true in production - it turns on HSTS and the HTTPS redirect that keep API keys off plain HTTP")
	}
	if c.RateLimitRPS <= 0 {
		prodErrs = append(prodErrs, "RATE_LIMIT_RPS is 0 or unset - the global per-IP rate limiter is disabled. Set RATE_LIMIT_RPS to a reasonable value (e.g. 100) for production")
	}
	if len(prodErrs) == 0 {
		return nil
	}
	return fmt.Errorf("production config validation errors:\n\t%s", strings.Join(prodErrs, "\n\t"))
}

// ProductionWarnings returns the production settings that are allowed but
// weaken the install, one message each. A plaintext database connection is
// one: an engine and its database on one private network may run without TLS,
// so the choice belongs to the operator, and it is reported on every boot
// rather than refused.
func (c *Config) ProductionWarnings() []string {
	if !c.IsProduction() {
		return nil
	}
	var warns []string
	// A stateless engine has no accounts to send a person to the console for.
	if !c.Stateless() && c.ConsoleURL == "" {
		warns = append(warns, "LYEVE_CONSOLE_URL is unset, so nothing can send a person to the console: password reset and magic-link sign-in refuse to start, and device sign-in refuses every request, until it names the console's public URL")
	}
	if !c.Stateless() && (c.DatabaseDriver == "" || c.DatabaseDriver == "postgres") {
		switch mode := sslMode(c.DatabaseURL); mode {
		case "require", "verify-ca", "verify-full":
		case "":
			warns = append(warns, "DATABASE_URL sets no sslmode, so the driver may connect without TLS - set sslmode=require or sslmode=verify-full unless the database is on a private network")
		default:
			warns = append(warns, "DATABASE_URL sets sslmode="+mode+", so the connection can run without TLS - set sslmode=require or sslmode=verify-full unless the database is on a private network")
		}
	}
	return warns
}

// sslMode returns the sslmode a PostgreSQL URL or keyword DSN names, lower
// case, or "" when it names none.
func sslMode(dsn string) string {
	lower := strings.ToLower(dsn)
	i := strings.Index(lower, "sslmode=")
	if i < 0 {
		return ""
	}
	v := lower[i+len("sslmode="):]
	if j := strings.IndexAny(v, "& "); j >= 0 {
		v = v[:j]
	}
	return v
}

// ValidateProduction performs production-specific safety checks that should
// only be enforced in a production environment. In non-production environments
// this is a no-op. It also includes all ValidateCritical checks.
func (c *Config) ValidateProduction() error {
	if !c.IsProduction() {
		return nil
	}
	if c.Stateless() {
		return c.validateStatelessProduction()
	}

	var prodErrs []string

	// JWT secret: must be set
	if c.JWTSecret == "" && len(c.JWTSecrets) == 0 {
		prodErrs = append(prodErrs, "JWT_SECRET must be set in production - missing or empty signing key")
	}

	// Encryption key: warn if empty (falls back to JWT)
	if c.EncryptionKey == "" {
		prodErrs = append(prodErrs, "ENCRYPTION_KEY is empty - falls back to JWT_SECRET. Set a separate, high-entropy ENCRYPTION_KEY in production")
	}
	if c.EncryptionKey != "" && c.EncryptionKey == c.JWTSecret {
		prodErrs = append(prodErrs, "ENCRYPTION_KEY must not equal JWT_SECRET - key hierarchy requires separate values in production")
	}

	// Secure cookie
	if !c.SecureCookie {
		prodErrs = append(prodErrs, "SECURE_COOKIE must be true in production - session cookies must set Secure flag")
	}

	// CORS wildcard in production (extra severity)
	for _, origin := range c.CORSOrigins {
		if origin == "*" {
			prodErrs = append(prodErrs, "CORS_ORIGINS contains wildcard \"*\" - this is a CSRF vector. Use explicit origins in production")
		}
	}

	// JWT expiry: cap
	if c.JWTExpirySecs > 3600 { // max 1 hour
		prodErrs = append(prodErrs, "JWT_EXPIRY_SECS is unreasonably long - maximum 3600 seconds for production JWTs")
	}

	// Default DB credentials
	for _, dsn := range []string{c.DatabaseURL, c.DatabaseReplicaURL} {
		if hasDefaultDBCredentials(dsn) {
			prodErrs = append(prodErrs, fmt.Sprintf("%s uses default/dev credentials (postgres/root/sa) - production deployments must use non-default credentials",
				dsn))
		}
	}

	// JWT key file permissions (only when the key file exists)
	if c.JWTKeyPath != "" {
		if info, err := os.Stat(c.JWTKeyPath); err == nil {
			perm := info.Mode().Perm()
			if perm&0o077 != 0 {
				prodErrs = append(prodErrs, fmt.Sprintf("JWT_KEY_PATH %s has permissions %o (world-readable) - must be 0600 or stricter", c.JWTKeyPath, perm))
			}
		}
		// If the file doesn't exist that's fine: InitJWTSigning creates it
	}

	// Rate limiter: must be enabled in production
	if c.RateLimitRPS <= 0 {
		prodErrs = append(prodErrs, "RATE_LIMIT_RPS is 0 or unset - the global per-IP rate limiter is disabled. Set RATE_LIMIT_RPS to a reasonable value (e.g. 100) for production")
	}

	// A link mailed over plain http hands its token to every hop on the way.
	if c.ConsoleURL != "" && !strings.HasPrefix(strings.ToLower(c.ConsoleURL), "https://") {
		prodErrs = append(prodErrs, fmt.Sprintf("LYEVE_CONSOLE_URL %q must use https in production, because the links mailed to it carry a sign-in token", c.ConsoleURL))
	}

	// Audit HMAC key: must be set in production
	if c.AuditHMACKey == "" {
		prodErrs = append(prodErrs, "LYEVE_AUDIT_HMAC_KEY is empty - audit hash-chain HMAC key must be set in production. Generate with: openssl rand -hex 32")
	} else if len(c.AuditHMACKey) != 64 {
		prodErrs = append(prodErrs, fmt.Sprintf("LYEVE_AUDIT_HMAC_KEY has length %d - must be exactly 64 hex characters (32 bytes). Generate with: openssl rand -hex 32", len(c.AuditHMACKey)))
	}

	if len(prodErrs) == 0 {
		return nil
	}
	return fmt.Errorf("production config validation errors:\n\t%s", strings.Join(prodErrs, "\n\t"))
}

// hasDefaultDBCredentials reports whether a DSN authenticates as a well-known
// default superuser: postgres, root, sa, or admin.
//
// The username is extracted by hand rather than with net/url because the
// canonical MySQL DSN embeds a host in parentheses, and
// "mysql://root:pw@tcp(db:3306)/app" makes url.Parse fail on the port inside
// tcp(...), so a net/url-based check would miss the one DSN shape MySQL users
// actually write.
func hasDefaultDBCredentials(dsn string) bool {
	if dsn == "" {
		return false
	}

	scheme, rest := "", dsn
	if i := strings.Index(dsn, "://"); i >= 0 {
		scheme, rest = strings.ToLower(dsn[:i]), dsn[i+3:]
	}
	// Userinfo, when present, sits before the last @ of the authority. Cut the
	// path and query first so an @ in a database name cannot be mistaken for
	// the credential separator.
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		rest = rest[:i]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return false
	}
	username := strings.ToLower(rest[:at])
	if i := strings.Index(username, ":"); i >= 0 {
		username = username[:i]
	}
	if username == "" {
		return false
	}

	// A named engine is held to its own default superuser: "root" is the MySQL
	// default but a legitimate role name on Postgres.
	switch scheme {
	case "postgres", "postgresql":
		return username == "postgres"
	case "mysql":
		return username == "root"
	case "sqlserver", "mssql":
		return username == "sa"
	case "":
		// No scheme to narrow by, so any well-known default counts.
		switch username {
		case "postgres", "root", "sa", "admin":
			return true
		}
	}
	return false
}

// consoleURLProblem describes what is wrong with a LYEVE_CONSOLE_URL value, or
// returns "" when it is unset or usable. Links are built by appending a path to
// it, so it has to be an absolute http or https URL naming a host, with nothing
// a path could not follow: no query, no fragment and no credentials.
func consoleURLProblem(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil, u.Scheme != "http" && u.Scheme != "https":
		return fmt.Sprintf("LYEVE_CONSOLE_URL %q must be an absolute URL starting with https:// or http://", raw)
	case u.Host == "" || u.Hostname() == "":
		return fmt.Sprintf("LYEVE_CONSOLE_URL %q names no host", raw)
	case u.User != nil, u.RawQuery != "", u.ForceQuery, u.Fragment != "":
		return fmt.Sprintf("LYEVE_CONSOLE_URL %q must hold only a scheme, a host and an optional path", raw)
	}
	return ""
}
