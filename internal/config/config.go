// Package config loads the CMS engine's runtime configuration from environment
// variables, validates the resulting values, and exposes them through the
// Config struct. Each field corresponds to an env var documented in .env.example.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Default constants. The list is not exhaustive: most of Load's defaults are
// literals beside the variable they set, so read Load for the rest.

// DefaultEnvironment is the deployment environment assumed when APP_ENV is
// unset.
//
// It defaults to production because everything in ValidateProduction is gated
// on it: default-credential rejection, the rate-limit floor, the audit HMAC
// key, secure cookies, and aborting boot when JWT signing cannot initialize.
// With an empty default, an operator who pulls the image and runs it gets none
// of those checks and no indication they are missing. Local work opts out
// explicitly with APP_ENV=development.
const DefaultEnvironment = "production"

// DefaultLicenseCacheDir is where the licensing implementation caches what it
// verified, unless LYEVE_LICENSE_CACHE_DIR says otherwise.
const DefaultLicenseCacheDir = "/var/lib/lyeve"

const (
	// Sized so several instances share a stock database rather than one
	// instance claiming all of it. PostgreSQL ships max_connections = 100 with
	// 3 held back for superusers, so a default of 100 would let a single
	// engine exhaust the server and a second replica fail to connect at all.
	// Four replicas at 25 still fit, and a pool wider than the database can
	// serve queues in the driver instead of erroring at the server.
	defaultMaxConns          = 25
	defaultJWTExpirySecs     = 900     // 15 minutes
	defaultRefreshTokenTTL   = 2592000 // 30 days
	defaultCORSPreflightAge  = 3600    // 1 hour
	defaultMaxBodyBytes      = 10 * 1024 * 1024
	defaultMaxJSONBodyBytes  = 1 * 1024 * 1024
	defaultDBConnMaxIdleTime = 5 * time.Minute
)

// Load reads configuration from the environment and the YAML tree, parses each
// value into the Config struct, and validates the result. Call once at startup
// before any other package accesses configuration.
//
// The YAML tree is found at LYEVE_CONFIG, or in the conventional locations when
// that is unset. A deployment with no configuration file resolves everything
// from the environment.
func Load() (*Config, error) {
	return LoadFrom("")
}

// minSetupTokenLen is the shortest LYEVE_SETUP_TOKEN accepted: 16 characters
// is 64 bits of entropy when hex, the floor for a credential that guards an
// unauthenticated route.
const minSetupTokenLen = 16

// The listen addresses when nothing sets them. Setup mode binds the same ones,
// so the admin reaches it where it will reach the engine afterwards.
const (
	defaultAdminListenAddr = "0.0.0.0:3001"
	defaultAPIListenAddr   = "0.0.0.0:3002"
)

// LoadFrom is Load against an explicit configuration file or directory. An
// empty path searches the conventional locations. A path that is named but
// missing is an error: an operator who points at a file and gets silence would
// run with settings they believe are applied.
func LoadFrom(path string) (*Config, error) {
	// .env is optional. Warn when present but malformed so operators know
	// their environment configuration is broken.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "WARNING: .env file found but failed to parse: %v\n", err)
	}

	// Loaded after .env so a ${VAR} reference in the tree can name a variable
	// the .env file supplies, and installed before the first lookup below so
	// every read in this function sees the full set of layers.
	files, err := LoadFiles(path)
	if err != nil {
		return nil, err
	}
	SetActiveResolver(NewResolver(files))

	mode, err := engineMode()
	if err != nil {
		return nil, err
	}
	stateless := mode == core.EngineModeStateless
	if stateless {
		if err := checkStatelessMode(); err != nil {
			return nil, err
		}
	} else if err := checkSetupMode(); err != nil {
		return nil, err
	}

	dbURL := lookup("DATABASE_URL")
	if dbURL == "" && !stateless {
		return nil, fmt.Errorf("DATABASE_URL must be set")
	}

	// Warn when SSL is disabled: production deployments must encrypt connections.
	if strings.Contains(dbURL, "sslmode=disable") {
		fmt.Fprintf(os.Stderr, "WARNING: DATABASE_URL contains sslmode=disable - database connections are unencrypted. Ensure sslmode=require or sslmode=verify-full in production.\n")
	}

	// A stateless engine signs no session and stores nothing encrypted, so it
	// needs neither secret. One that is set is still checked for strength.
	jwtSecret := lookup("JWT_SECRET")
	if jwtSecret == "" && !stateless {
		return nil, fmt.Errorf("JWT_SECRET must be set")
	}

	encryptionKey := lookup("ENCRYPTION_KEY")
	if encryptionKey == "" && !stateless {
		encryptionKey = jwtSecret
		// Deprecated fallback: production must set ENCRYPTION_KEY.
		fmt.Fprintf(os.Stderr, "WARNING: ENCRYPTION_KEY not set - falling back to JWT_SECRET for data encryption. This is deprecated and will be removed in a future release. Set ENCRYPTION_KEY to a separate, high-entropy value.\n")
	}

	// A guessable setup token is the takeover it exists to prevent, so a short
	// one is refused rather than accepted with a warning.
	setupToken := lookup("LYEVE_SETUP_TOKEN")
	if setupToken != "" && len(setupToken) < minSetupTokenLen {
		return nil, fmt.Errorf("LYEVE_SETUP_TOKEN must be at least %d characters", minSetupTokenLen)
	}

	maxConns := int64(defaultMaxConns)
	if v := lookup("DATABASE_MAX_CONNECTIONS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("DATABASE_MAX_CONNECTIONS must be a number: %w", err)
		}
		if n <= 0 {
			return nil, fmt.Errorf("DATABASE_MAX_CONNECTIONS must be positive, got %d", n)
		}
		maxConns = n
	}

	expiry := int64(defaultJWTExpirySecs) // 15 minutes: frequent re-checks for role/state changes
	if v := lookup("JWT_EXPIRY_SECS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("JWT_EXPIRY_SECS must be a number: %w", err)
		}
		expiry = n
	}

	refreshTTL := int64(defaultRefreshTokenTTL) // 30 days
	if v := lookup("REFRESH_TOKEN_TTL_SECS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("REFRESH_TOKEN_TTL_SECS must be a number: %w", err)
		}
		refreshTTL = n
	}

	origins := []string{"http://localhost:5173"}
	if v := lookup("CORS_ORIGINS"); v != "" {
		origins = splitTrim(v, ',')
	}

	corsMaxAge := defaultCORSPreflightAge
	if v := lookup("CORS_MAX_AGE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("CORS_MAX_AGE must be a number: %w", err)
		}
		corsMaxAge = n
	}

	// PATCH included by default for CMS content updates.
	corsAllowMethods := envOr("CORS_ALLOW_METHODS", "GET, POST, PUT, DELETE, PATCH, OPTIONS")

	// Custom CMS headers included by default for API key auth and multi-tenancy.
	corsAllowHeaders := envOr("CORS_ALLOW_HEADERS", "Content-Type, Authorization, X-API-Key, X-Tenant-ID, X-Correlation-ID, X-CSRF-Token")
	// A cross-origin script can read only the CORS-safelisted response headers
	// unless the server names the rest. Without this the request id a client
	// needs to quote in a bug report, and the rate-limit values it needs to back
	// off with, are set on the response but unreadable in the browser.
	corsExposeHeaders := envOr("CORS_EXPOSE_HEADERS", "X-Request-Id, X-Correlation-ID, RateLimit-Limit, RateLimit-Remaining, RateLimit-Reset, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, Retry-After")

	// Empty = fail-closed: no dynamic origins allowed.
	corsAllowedDomains := splitTrim(lookup("CORS_ALLOWED_DOMAINS"), ',')

	var rateLimitRPS float64
	if v := lookup("RATE_LIMIT_RPS"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("RATE_LIMIT_RPS must be a number: %w", err)
		}
		rateLimitRPS = n
	}
	rateLimitBurst := int(rateLimitRPS * 2)
	if rateLimitRPS > 0 && rateLimitBurst < 1 {
		rateLimitBurst = 1
	}
	if v := lookup("RATE_LIMIT_BURST"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("RATE_LIMIT_BURST must be a number: %w", err)
		}
		rateLimitBurst = int(n)
	}

	// Max body size (default 10 MiB, which covers file uploads). JSON routes get a
	// tighter per-group ContentLengthLimit of 1 MiB.
	maxBodyBytes := int64(defaultMaxBodyBytes)
	if v := lookup("MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("MAX_BODY_BYTES must be a number: %w", err)
		}
		if n <= 0 {
			return nil, fmt.Errorf("MAX_BODY_BYTES must be positive, got %d", n)
		}
		maxBodyBytes = n
	}

	// Max JSON body size: per-group Content-Length guard for JSON API routes
	// (default 1 MiB). Only the header is checked. The body is NOT wrapped.
	maxJSONBodyBytes := int64(defaultMaxJSONBodyBytes)
	if v := lookup("MAX_JSON_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("MAX_JSON_BODY_BYTES must be a number: %w", err)
		}
		maxJSONBodyBytes = n
	}

	var ipAllowlist []string
	if v := lookup("IP_ALLOWLIST"); v != "" {
		list, err := parseIPAllowlist(v)
		if err != nil {
			return nil, err
		}
		ipAllowlist = list
	}

	var allowedHosts []string
	if v := lookup("ALLOWED_HOSTS"); v != "" {
		allowedHosts = splitTrim(v, ',')
	}

	backpressureEnabled := envBool("BACKPRESSURE_ENABLED", false)
	backpressureMaxInflight := int64(200)
	if v := lookup("BACKPRESSURE_MAX_INFLIGHT"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("BACKPRESSURE_MAX_INFLIGHT must be a number: %w", err)
		}
		if n <= 0 {
			return nil, fmt.Errorf("BACKPRESSURE_MAX_INFLIGHT must be positive, got %d", n)
		}
		backpressureMaxInflight = n
	}
	backpressureTenantQuotaPct := 0.4
	if v := lookup("BACKPRESSURE_TENANT_QUOTA_PCT"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("BACKPRESSURE_TENANT_QUOTA_PCT must be a number: %w", err)
		}
		if n <= 0 || n > 1 {
			return nil, fmt.Errorf("BACKPRESSURE_TENANT_QUOTA_PCT must be in (0, 1], got %f", n)
		}
		backpressureTenantQuotaPct = n
	}
	backpressurePoolPressureThreshold := 0.85
	if v := lookup("BACKPRESSURE_POOL_PRESSURE_THRESHOLD"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("BACKPRESSURE_POOL_PRESSURE_THRESHOLD must be a number: %w", err)
		}
		if n <= 0 || n > 1 {
			return nil, fmt.Errorf("BACKPRESSURE_POOL_PRESSURE_THRESHOLD must be in (0, 1], got %f", n)
		}
		backpressurePoolPressureThreshold = n
	}

	// Goroutine engine enables centralized worker pools and async hooks.
	goroutineEngineEnabled := envBool("GOROUTINE_ENGINE_ENABLED", false)
	goroutineEnginePoolSize := 100
	if v := lookup("GOROUTINE_ENGINE_POOL_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("GOROUTINE_ENGINE_POOL_SIZE must be a number: %w", err)
		}
		if n <= 0 {
			return nil, fmt.Errorf("GOROUTINE_ENGINE_POOL_SIZE must be positive, got %d", n)
		}
		goroutineEnginePoolSize = n
	}
	asyncHooksEnabled := envBool("ASYNC_HOOKS_ENABLED", false)
	asyncHookTimeout := 5 * time.Second
	if v := lookup("ASYNC_HOOK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("ASYNC_HOOK_TIMEOUT must be a duration: %w", err)
		}
		asyncHookTimeout = d
	}

	// Memory limit: try cgroup v2 first, then env var.
	var memoryLimitBytes int64
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		if n, parseErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); parseErr == nil && n > 0 {
			memoryLimitBytes = n
		}
	}
	if memoryLimitBytes == 0 {
		if v := lookup("MEMORY_LIMIT_BYTES"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("MEMORY_LIMIT_BYTES must be a number: %w", err)
			}
			if n < 0 {
				return nil, fmt.Errorf("MEMORY_LIMIT_BYTES must be non-negative, got %d", n)
			}
			memoryLimitBytes = n
		}
	}

	// CPU quota: the same cgroup v2 story as the memory limit above. Go sizes
	// its scheduler, its garbage collector's worker count and the default
	// parallelism of everything built on them from the number of cores it can
	// see, and it sees the host's cores rather than the share this container
	// was given. Under a fractional or single-core quota that means the runtime
	// arranges far more parallel work than the cgroup will let it run, and the
	// kernel absorbs the difference as throttling, which arrives as latency
	// spikes rather than as an error anyone can trace back to here.
	//
	// cpu.max holds "<quota> <period>", or "max <period>" for no limit. The
	// value is rounded up: half a core still needs one thread to run on.
	var cpuQuota int
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		fields := strings.Fields(strings.TrimSpace(string(data)))
		if len(fields) == 2 && fields[0] != "max" {
			quota, qErr := strconv.ParseInt(fields[0], 10, 64)
			period, pErr := strconv.ParseInt(fields[1], 10, 64)
			if qErr == nil && pErr == nil && quota > 0 && period > 0 {
				cpuQuota = int((quota + period - 1) / period)
			}
		}
	}
	if v := lookup("CPU_QUOTA"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("CPU_QUOTA must be a number: %w", err)
		}
		if n < 0 {
			return nil, fmt.Errorf("CPU_QUOTA must be non-negative, got %d", n)
		}
		cpuQuota = n
	}

	dbWarmupParallelism := 4
	if v := lookup("DB_WARMUP_PARALLELISM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("DB_WARMUP_PARALLELISM must be a number: %w", err)
		}
		if n <= 0 {
			return nil, fmt.Errorf("DB_WARMUP_PARALLELISM must be positive, got %d", n)
		}
		dbWarmupParallelism = n
	}
	pluginStopParallelism := 8
	if v := lookup("PLUGIN_STOP_PARALLELISM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("PLUGIN_STOP_PARALLELISM must be a number: %w", err)
		}
		if n <= 0 {
			return nil, fmt.Errorf("PLUGIN_STOP_PARALLELISM must be positive, got %d", n)
		}
		pluginStopParallelism = n
	}

	var trustedIssuers []string
	if v := lookup("TRUSTED_ISSUERS"); v != "" {
		trustedIssuers = splitTrim(v, ',')
	}
	trustedIssuerPolicies, err := parseIssuerPolicies(lookup("TRUSTED_ISSUER_POLICIES"), trustedIssuers)
	if err != nil {
		return nil, err
	}

	dbMinConns := int64(2)
	if v := lookup("DB_POOL_MIN_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("DB_POOL_MIN_CONNS must be a number: %w", err)
		}
		if n < 0 {
			return nil, fmt.Errorf("DB_POOL_MIN_CONNS must be >= 0, got %d", n)
		}
		dbMinConns = n
	}
	dbConnMaxLifetime := 1 * time.Hour
	if v := lookup("DB_CONN_MAX_LIFETIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("DB_CONN_MAX_LIFETIME must be a duration: %w", err)
		}
		dbConnMaxLifetime = d
	}
	dbConnMaxIdleTime := defaultDBConnMaxIdleTime
	if v := lookup("DB_CONN_MAX_IDLE_TIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("DB_CONN_MAX_IDLE_TIME must be a duration: %w", err)
		}
		dbConnMaxIdleTime = d
	}
	dbHealthCheck := 30 * time.Second
	if v := lookup("DB_POOL_HEALTH_CHECK_PERIOD"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("DB_POOL_HEALTH_CHECK_PERIOD must be a duration: %w", err)
		}
		dbHealthCheck = d
	}

	instanceID := lookup("INSTANCE_ID")
	if instanceID == "" {
		hostname, _ := os.Hostname()
		instanceID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}

	mfaGraceHours := 0
	if v := lookup("MFA_GRACE_PERIOD_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MFA_GRACE_PERIOD_HOURS must be a number: %w", err)
		}
		mfaGraceHours = n
	}

	passwordMinLength := 12
	if v := lookup("PASSWORD_MIN_LENGTH"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("PASSWORD_MIN_LENGTH must be a number: %w", err)
		}
		if n < 1 {
			return nil, fmt.Errorf("PASSWORD_MIN_LENGTH must be >= 1")
		}
		passwordMinLength = n
	}
	passwordRequireComplexity := envBool("PASSWORD_REQUIRE_COMPLEXITY", true)
	passwordCheckCommon := envBool("PASSWORD_CHECK_COMMON", true)

	poolCfg, poolerModeStr := LoadPoolConfig()
	poolerMode := db.PoolerMode(poolerModeStr)
	if poolerMode == "" {
		poolerMode = db.PoolerModeNone
	}

	rateLimitBackend := envOr("RATE_LIMIT_BACKEND", "memory")
	rateLimitPerTenant := lookup("RATE_LIMIT_PER_TENANT") == "true"

	// Public endpoint caps. Both are empty by default, and the routers then
	// use the engine's shipped table unchanged.
	publicRateLimits := splitTrim(lookup("PUBLIC_RATE_LIMITS"), ',')
	publicRateLimitGlobal := trimSpace(lookup("PUBLIC_RATE_LIMIT_GLOBAL"))

	graceful := 60 * time.Second
	if v := lookup("GRACEFUL_SHUTDOWN_SECS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("GRACEFUL_SHUTDOWN_SECS must be a number: %w", err)
		}
		if n <= 0 {
			return nil, fmt.Errorf("GRACEFUL_SHUTDOWN_SECS must be positive, got %d", n)
		}
		graceful = time.Duration(n) * time.Second
	}

	preStopDrain := 5 * time.Second
	if v := lookup("PRESTOP_DRAIN_SECS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("PRESTOP_DRAIN_SECS must be a number: %w", err)
		}
		if n < 0 {
			return nil, fmt.Errorf("PRESTOP_DRAIN_SECS must be non-negative, got %d", n)
		}
		preStopDrain = time.Duration(n) * time.Second
	}

	replicaURL := lookup("DATABASE_REPLICA_URL")
	if strings.Contains(replicaURL, "sslmode=disable") {
		fmt.Fprintf(os.Stderr, "WARNING: DATABASE_REPLICA_URL contains sslmode=disable - replica connections are unencrypted. Ensure sslmode=require or sslmode=verify-full in production.\n")
	}
	replicaMaxConns := int64(30)
	if v := lookup("DATABASE_REPLICA_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("DATABASE_REPLICA_MAX_CONNS must be a number: %w", err)
		}
		if n <= 0 {
			return nil, fmt.Errorf("DATABASE_REPLICA_MAX_CONNS must be positive, got %d", n)
		}
		replicaMaxConns = n
	}

	instanceRegion := lookup("INSTANCE_REGION")

	trustedProxies := splitTrim(lookup("TRUSTED_PROXIES"), ',')

	tlsCertFile, tlsKeyFile := lookup("TLS_CERT_FILE"), lookup("TLS_KEY_FILE")
	if (tlsCertFile == "") != (tlsKeyFile == "") {
		return nil, fmt.Errorf("TLS_CERT_FILE and TLS_KEY_FILE must be set together")
	}

	adminConsoleKey := lookup("ADMIN_CONSOLE_KEY")
	if adminConsoleKey != "" && len(adminConsoleKey) < 32 {
		return nil, fmt.Errorf("ADMIN_CONSOLE_KEY must be at least 32 characters, got %d", len(adminConsoleKey))
	}
	adminConsoleKeyPrevious := lookup("ADMIN_CONSOLE_KEY_PREVIOUS")
	if adminConsoleKeyPrevious != "" && len(adminConsoleKeyPrevious) < 32 {
		return nil, fmt.Errorf("ADMIN_CONSOLE_KEY_PREVIOUS must be at least 32 characters, got %d", len(adminConsoleKeyPrevious))
	}
	if adminConsoleKeyPrevious != "" && adminConsoleKey == "" {
		return nil, fmt.Errorf("ADMIN_CONSOLE_KEY_PREVIOUS is set without ADMIN_CONSOLE_KEY")
	}

	enforceAPIKeyPepper := lookup("ENFORCE_API_KEY_PEPPER") == "true"

	cacheTTL, err := parseCacheTTL()
	if err != nil {
		return nil, err
	}
	cacheMaxEntries, err := parseCacheMaxEntries()
	if err != nil {
		return nil, err
	}

	tracingSamplingRate := 1.0
	if v := lookup("TRACING_SAMPLING_RATE"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("TRACING_SAMPLING_RATE must be a float between 0.0 and 1.0: %w", err)
		}
		if n < 0 || n > 1 {
			return nil, fmt.Errorf("TRACING_SAMPLING_RATE must be between 0.0 and 1.0, got %f", n)
		}
		tracingSamplingRate = n
	}
	tracingTenantSamplingRates, err := parseTenantSamplingRates(lookup("TRACING_TENANT_SAMPLING_RATES"))
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		AdminListenAddr: envOr("ADMIN_LISTEN_ADDR", defaultAdminListenAddr),
		APIListenAddr:   envOr("API_LISTEN_ADDR", defaultAPIListenAddr),
		TLSCertFile:     tlsCertFile,
		TLSKeyFile:      tlsKeyFile,
		Mode:            mode,
		DatabaseURL:     dbURL,

		DatabaseMaxConns:          int32(maxConns),
		DatabaseMinConns:          int32(dbMinConns),
		DatabaseConnMaxLifetime:   dbConnMaxLifetime,
		DatabaseConnMaxIdleTime:   dbConnMaxIdleTime,
		DatabaseHealthCheckPeriod: dbHealthCheck,
		MigrationsPath:            envOr("MIGRATIONS_PATH", "./migrations"),
		JWTSecret:                 jwtSecret,
		JWTSecrets:                buildJWTSecrets(jwtSecret),
		EncryptionKey:             encryptionKey,
		SetupToken:                setupToken,
		JWTExpirySecs:             expiry,
		JWTAlg:                    envOr("JWT_ALG", "EdDSA"),
		JWTKeyPath:                envOr("JWT_KEY_PATH", "/var/lib/lyeve/jwt_key.json"),
		RefreshTokenTTL:           refreshTTL,
		CORSOrigins:               origins,
		CORSPreflightMaxAge:       corsMaxAge,
		CORSAllowMethods:          corsAllowMethods,
		CORSAllowHeaders:          corsAllowHeaders,
		CORSExposeHeaders:         corsExposeHeaders,
		CORSAllowedDomains:        corsAllowedDomains,
		SecureCookie:              lookup("SECURE_COOKIE") == "true",
		RateLimitRPS:              rateLimitRPS,
		RateLimitBurst:            rateLimitBurst,
		MaxBodyBytes:              maxBodyBytes,
		MaxJSONBodyBytes:          maxJSONBodyBytes,
		IPAllowlist:               ipAllowlist,
		AllowedHosts:              allowedHosts,
		TrustedIssuers:            trustedIssuers,
		TrustedIssuerPolicies:     trustedIssuerPolicies,
		InstanceID:                instanceID,
		PasswordHashAlgo:          envOr("PASSWORD_HASH_ALGO", "bcrypt"),
		PasswordMinLength:         passwordMinLength,
		PasswordRequireComplexity: passwordRequireComplexity,
		PasswordCheckCommon:       passwordCheckCommon,
		OTLPEndpoint:              lookup("OTEL_EXPORTER_OTLP_ENDPOINT"),
		//lyeve:allow-test-seam OpenTelemetry's own variable, and it selects the transport to a collector rather than relaxing anything the product enforces
		OTLPInsecure:                      lookup("OTEL_EXPORTER_OTLP_INSECURE") == "true",
		TracingSamplingRate:               tracingSamplingRate,
		TracingTenantSamplingRates:        tracingTenantSamplingRates,
		GracefulShutdownTimeout:           graceful,
		PreStopDrainDelay:                 preStopDrain,
		CacheTTL:                          cacheTTL,
		CacheMaxEntries:                   cacheMaxEntries,
		DatabaseDriver:                    envOr("DATABASE_DRIVER", db.EngineFromDSN(dbURL)),
		StorageDriver:                     envOr("STORAGE_DRIVER", "local"),
		StorageLocalPath:                  envOr("STORAGE_LOCAL_PATH", "./uploads"),
		StorageBaseURL:                    lookup("STORAGE_BASE_URL"),
		StorageS3Bucket:                   lookup("STORAGE_S3_BUCKET"),
		StorageS3Region:                   envOr("STORAGE_S3_REGION", "us-east-1"),
		StorageS3Endpoint:                 lookup("STORAGE_S3_ENDPOINT"),
		StorageS3Key:                      lookup("STORAGE_S3_KEY"),
		StorageS3Secret:                   lookup("STORAGE_S3_SECRET"),
		StorageS3CDNBaseURL:               lookup("STORAGE_S3_CDN_BASE_URL"),
		StorageS3UseSSL:                   envBool("STORAGE_S3_USE_SSL", true),
		StorageS3ForcePathStyle:           lookup("STORAGE_S3_FORCE_PATH_STYLE") == "true",
		StorageS3MultipartMB:              parseS3MultipartMB(),
		MultiTenant:                       lookup("MULTI_TENANT") == "true",
		LicenseKey:                        lookup("LYEVE_LICENSE_KEY"),
		LicenseCacheDir:                   envOr("LYEVE_LICENSE_CACHE_DIR", DefaultLicenseCacheDir),
		LicenseServerURL:                  lookup("LYEVE_LICENSE_SERVER_URL"),
		Plugins:                           parsePlugins(),
		MfaGracePeriodHours:               mfaGraceHours,
		PublicRateLimits:                  publicRateLimits,
		PublicRateLimitGlobal:             publicRateLimitGlobal,
		RateLimitBackend:                  rateLimitBackend,
		RateLimitPerTenant:                rateLimitPerTenant,
		PoolSizing:                        poolCfg,
		PoolerMode:                        poolerMode,
		DatabaseReplicaURL:                replicaURL,
		DatabaseReplicaMaxConns:           int32(replicaMaxConns),
		InstanceRegion:                    instanceRegion,
		TrustedProxies:                    trustedProxies,
		EnforceAPIKeyPepper:               enforceAPIKeyPepper,
		Environment:                       envOr("APP_ENV", DefaultEnvironment),
		DebugTracer:                       lookup("LYEVE_DEBUG_TRACER_ENABLED") == "true",
		BaseURL:                           lookup("LYEVE_BASE_URL"),
		ConsoleURL:                        strings.TrimRight(strings.TrimSpace(lookup("LYEVE_CONSOLE_URL")), "/"),
		MetricsToken:                      lookup("METRICS_TOKEN"),
		AdminConsoleKey:                   adminConsoleKey,
		AdminConsoleKeyPrevious:           adminConsoleKeyPrevious,
		AuditHMACKey:                      lookup("LYEVE_AUDIT_HMAC_KEY"),
		PluginCapsStrictMode:              envBool("PLUGIN_CAPS_STRICT_MODE", false),
		BackpressureEnabled:               backpressureEnabled,
		BackpressureMaxInflight:           backpressureMaxInflight,
		BackpressureTenantQuotaPct:        backpressureTenantQuotaPct,
		BackpressurePoolPressureThreshold: backpressurePoolPressureThreshold,
		GoroutineEngineEnabled:            goroutineEngineEnabled,
		GoroutineEnginePoolSize:           goroutineEnginePoolSize,
		AsyncHooksEnabled:                 asyncHooksEnabled,
		AsyncHookTimeout:                  asyncHookTimeout,
		MemoryLimitBytes:                  memoryLimitBytes,
		CPUQuota:                          cpuQuota,
		DBWarmupParallelism:               dbWarmupParallelism,
		PluginStopParallelism:             pluginStopParallelism,
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	if err := cfg.ValidateCritical(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	if cfg.IsProduction() {
		if err := cfg.ValidateProduction(); err != nil {
			return nil, fmt.Errorf("production config validation: %w", err)
		}
		for _, w := range cfg.ProductionWarnings() {
			slog.Warn("production config", "warning", w)
		}
	}

	return cfg, nil
}

// Validate checks that security-sensitive config fields hold supported values.
// Returns an error describing the first invalid field found.
// Validates JWTAlg (EdDSA/HS256), PasswordHashAlgo (bcrypt/argon2id), and
// backpressure field ranges when enabled.
func (c *Config) Validate() error {
	switch c.JWTAlg {
	case "EdDSA", "HS256":
		// supported
	default:
		return fmt.Errorf("unsupported JWT_ALG %q - must be \"EdDSA\" or \"HS256\"", c.JWTAlg)
	}
	switch c.PasswordHashAlgo {
	case "bcrypt", "argon2id":
		// supported
	default:
		return fmt.Errorf("unsupported PASSWORD_HASH_ALGO %q - must be \"bcrypt\" or \"argon2id\"", c.PasswordHashAlgo)
	}
	if c.BackpressureEnabled {
		if c.BackpressureMaxInflight <= 0 {
			return fmt.Errorf("BACKPRESSURE_MAX_INFLIGHT must be positive, got %d", c.BackpressureMaxInflight)
		}
		if c.BackpressureTenantQuotaPct <= 0 || c.BackpressureTenantQuotaPct > 1 {
			return fmt.Errorf("BACKPRESSURE_TENANT_QUOTA_PCT must be in (0, 1], got %f", c.BackpressureTenantQuotaPct)
		}
		if c.BackpressurePoolPressureThreshold <= 0 || c.BackpressurePoolPressureThreshold > 1 {
			return fmt.Errorf("BACKPRESSURE_POOL_PRESSURE_THRESHOLD must be in (0, 1], got %f", c.BackpressurePoolPressureThreshold)
		}
	}
	return nil
}

func parseCacheTTL() (time.Duration, error) {
	v := lookup("CACHE_TTL")
	if v == "" {
		return 60 * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("CACHE_TTL must be a valid duration: %w", err)
	}
	return d, nil
}

func parseCacheMaxEntries() (int, error) {
	v := lookup("CACHE_MAX_ENTRIES")
	if v == "" {
		return 1000, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("CACHE_MAX_ENTRIES must be a number: %w", err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("CACHE_MAX_ENTRIES must be positive, got %d", n)
	}
	return n, nil
}

func envOr(key, fallback string) string {
	if v := lookup(key); v != "" {
		return v
	}
	return fallback
}

// envBool reads an environment variable as a boolean via strconv.ParseBool.
// Returns fallback when the variable is empty, unset, or unparseable.
func envBool(key string, fallback bool) bool {
	if v := lookup(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fallback
		}
		return b
	}
	return fallback
}

// parseIPAllowlist reads IP_ALLOWLIST as CIDR ranges, taking a bare address
// as the one-host range it names. Every entry is checked here so a typo stops
// the boot with the entry named, rather than one mistyped range turning the
// whole list off.
func parseIPAllowlist(v string) ([]string, error) {
	var out []string
	for _, entry := range splitTrim(v, ',') {
		cidr, err := ParseIPEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("IP_ALLOWLIST entry %q is not an IP address or CIDR range", entry)
		}
		out = append(out, cidr)
	}
	return out, nil
}

// ParseIPEntry reads one address allow list entry, a CIDR range or a bare
// address, and returns the range it names: a bare address is its one-host
// range. It is the parser IP_ALLOWLIST uses, shared so every allow list the
// engine accepts reads an entry the same way.
func ParseIPEntry(entry string) (string, error) {
	cidr := entry
	if !strings.Contains(entry, "/") {
		ip := net.ParseIP(entry)
		if ip == nil {
			return "", fmt.Errorf("%q is not an IP address or CIDR range", entry)
		}
		if ip.To4() != nil {
			cidr = entry + "/32"
		} else {
			cidr = entry + "/128"
		}
	}
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		return "", fmt.Errorf("%q is not an IP address or CIDR range", entry)
	}
	return cidr, nil
}

func splitTrim(s string, sep rune) []string {
	var out []string
	for _, part := range splitRune(s, sep) {
		if t := trimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func splitRune(s string, sep rune) []string {
	var parts []string
	start := 0
	for i, r := range s {
		if r == sep {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func parsePlugins() []string {
	if v := lookup("LYEVE_PLUGINS"); v != "" {
		return splitTrim(v, ',')
	}
	return nil
}

// buildJWTSecrets returns the active JWT secrets for validation.
// JWT_SECRETS (comma-separated) enables zero-downtime rotation: new first,
// old second. Falls back to [primarySecret] when JWT_SECRETS is unset.
func buildJWTSecrets(primarySecret string) []string {
	if v := lookup("JWT_SECRETS"); v != "" {
		parts := splitTrim(v, ',')
		if len(parts) > 0 {
			return parts
		}
	}
	return []string{primarySecret}
}

func parseS3MultipartMB() int64 {
	if v := lookup("STORAGE_S3_MULTIPART_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 5 // default 5 MB threshold
}

// parseTenantSamplingRates parses "tenant=rate" pairs (e.g. "acme=0.1,corp=0.5")
// into a map. Returns nil when input is empty. Rates are clamped to [0.0, 1.0].
func parseTenantSamplingRates(raw string) (map[string]float64, error) {
	if raw == "" {
		return nil, nil
	}
	out := make(map[string]float64)
	for _, pair := range splitTrim(raw, ',') {
		parts := splitRune(pair, '=')
		if len(parts) != 2 {
			return nil, fmt.Errorf("TRACING_TENANT_SAMPLING_RATES: invalid pair %q - expected tenant=rate", pair)
		}
		slug := trimSpace(parts[0])
		if slug == "" {
			return nil, fmt.Errorf("TRACING_TENANT_SAMPLING_RATES: empty tenant slug in pair %q", pair)
		}
		rate, err := strconv.ParseFloat(trimSpace(parts[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("TRACING_TENANT_SAMPLING_RATES: invalid rate for tenant %q: %w", slug, err)
		}
		if rate < 0 {
			rate = 0
		}
		if rate > 1 {
			rate = 1
		}
		out[slug] = rate
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ConsoleKeys returns the keys a console signature may verify under, the
// current one first. Empty when no console key is configured.
func (c *Config) ConsoleKeys() [][]byte {
	var keys [][]byte
	for _, k := range []string{c.AdminConsoleKey, c.AdminConsoleKeyPrevious} {
		if k != "" {
			keys = append(keys, []byte(k))
		}
	}
	return keys
}

// parseIssuerPolicies reads TRUSTED_ISSUER_POLICIES. Every policy must name an
// issuer listed in TRUSTED_ISSUERS, once, and pass IssuerPolicy.Validate.
func parseIssuerPolicies(raw string, trustedIssuers []string) ([]auth.IssuerPolicy, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var policies []auth.IssuerPolicy
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&policies); err != nil {
		return nil, fmt.Errorf("TRUSTED_ISSUER_POLICIES must be a JSON array of policies: %w", err)
	}
	listed := map[string]bool{}
	for _, iss := range trustedIssuers {
		listed[iss] = true
	}
	seen := map[string]bool{}
	for _, p := range policies {
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("TRUSTED_ISSUER_POLICIES: %w", err)
		}
		if !listed[p.Issuer] {
			return nil, fmt.Errorf("TRUSTED_ISSUER_POLICIES: issuer %s is not in TRUSTED_ISSUERS", p.Issuer)
		}
		if seen[p.Issuer] {
			return nil, fmt.Errorf("TRUSTED_ISSUER_POLICIES: issuer %s has two policies", p.Issuer)
		}
		seen[p.Issuer] = true
	}
	return policies, nil
}
