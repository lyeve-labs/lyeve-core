package config

import (
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// Config holds all runtime configuration for the CMS engine. Populated from
// environment variables by Load. Field-level comments document the env var,
// default, and any non-obvious contract. See .env.example for the full list.
type Config struct {
	AdminListenAddr string // cmd/lyeve
	APIListenAddr   string // cmd/lyeve

	// Mode is LYEVE_MODE: empty for an engine with a database, or
	// core.EngineModeStateless for one that runs with none.
	Mode string

	// TLSCertFile and TLSKeyFile, set together, make both listeners serve
	// TLS 1.2 or later with that PEM pair, re-read when either file changes so
	// a rotated certificate is picked up without a restart. Unset, both
	// listeners serve plain HTTP for a proxy that terminates TLS in front.
	TLSCertFile      string // TLS_CERT_FILE
	TLSKeyFile       string // TLS_KEY_FILE
	DatabaseURL      string
	DatabaseMaxConns int32

	DatabaseMinConns          int32         // DB_POOL_MIN_CONNS (default 2)
	DatabaseHealthCheckPeriod time.Duration // DB_POOL_HEALTH_CHECK_PERIOD (default 30s)
	MigrationsPath            string        // path to .sql migration files
	JWTSecret                 string
	JWTSecrets                []string // JWT_SECRETS comma-sep. First is signing key. Falls back to [JWTSecret].

	// EncryptionKey is the master key for data-at-rest encryption (DEK/KEK hierarchy).
	// Set via ENCRYPTION_KEY env var. When empty, falls back to JWTSecret with a
	// deprecation warning. The JWT signing key must never double as the
	// encryption master key.
	EncryptionKey string // ENCRYPTION_KEY

	// SetupToken is the operator's credential for first-run setup
	// (LYEVE_SETUP_TOKEN). Empty makes the engine generate one at boot and
	// print it once, which works only while a single replica serves setup.
	SetupToken string

	JWTExpirySecs       int64
	JWTAlg              string // JWT_ALG: "EdDSA" (default when key exists) or "HS256" (legacy HMAC)
	JWTKeyPath          string // JWT_KEY_PATH: where the Ed25519 keypair is stored (default /var/lib/lyeve/jwt_key.json)
	RefreshTokenTTL     int64  // REFRESH_TOKEN_TTL_SECS: lifetime for refresh tokens (default 30 days)
	CORSOrigins         []string
	CORSPreflightMaxAge int      // CORS_MAX_AGE (default 3600 = 1 hour)
	CORSAllowMethods    string   // CORS_ALLOW_METHODS (default "GET, POST, PUT, DELETE, PATCH, OPTIONS")
	CORSAllowHeaders    string   // CORS_ALLOW_HEADERS (default "Content-Type, Authorization, X-API-Key, X-Tenant-ID, X-Correlation-ID")
	CORSExposeHeaders   string   // CORS_EXPOSE_HEADERS: response headers a cross-origin script may read
	CORSAllowedDomains  []string // CORS_ALLOWED_DOMAINS: suffix allowlist for dynamic CORS origins (empty = fail-closed, no dynamic origins allowed)
	SecureCookie        bool     // set true in production (HTTPS)

	// DebugTracer allows an admin to ask for a request trace instead of the
	// normal response, by sending X-Debug: true. Off unless set. The role check
	// applies either way, so this decides whether the header does anything at
	// all rather than who may use it.
	DebugTracer bool // LYEVE_DEBUG_TRACER_ENABLED

	// Middleware settings (all optional, env-configurable)
	RateLimitRPS     float64  // RATE_LIMIT_RPS: sustained req/s per IP (0 = disabled)
	RateLimitBurst   int      // RATE_LIMIT_BURST, burst size (default: 2× RPS)
	MaxBodyBytes     int64    // MAX_BODY_BYTES, hard limit on request body (default: 10 MiB, covers file uploads)
	MaxJSONBodyBytes int64    // MAX_JSON_BODY_BYTES, soft Content-Length guard for JSON API routes (default: 1 MiB)
	IPAllowlist      []string // IP_ALLOWLIST: comma-separated CIDRs, bare addresses normalized (empty = allow all)
	AllowedHosts     []string // ALLOWED_HOSTS: comma-separated hostnames for Host header validation (empty = disabled)

	// TrustedIssuers is a list of external OIDC issuer base URLs whose JWTs are
	// accepted on the API server without sharing JWT_SECRET.
	// The JWKS endpoint is derived as {issuer}/.well-known/jwks.json.
	// Comma-separated via TRUSTED_ISSUERS env var.
	TrustedIssuers []string // TRUSTED_ISSUERS

	// TrustedIssuerPolicies decide what a trusted issuer's token may do: the
	// tenant its users act in and the engine roles its roles map to. A trusted
	// issuer with no policy has its tokens ignored. JSON array via
	// TRUSTED_ISSUER_POLICIES.
	TrustedIssuerPolicies []auth.IssuerPolicy // TRUSTED_ISSUER_POLICIES

	// InstanceID uniquely identifies this process in logs/traces.
	// Auto-derived from hostname+PID when INSTANCE_ID env var is not set.
	InstanceID string // INSTANCE_ID

	// PasswordHashAlgo selects the password hashing algorithm.
	// Supported: "bcrypt" (default), "argon2id"
	PasswordHashAlgo string // PASSWORD_HASH_ALGO

	// PasswordMinLength is the minimum password length (default 12).
	PasswordMinLength int // PASSWORD_MIN_LENGTH

	// PasswordRequireComplexity requires uppercase + lowercase + digit (default true).
	PasswordRequireComplexity bool // PASSWORD_REQUIRE_COMPLEXITY (default true)

	// PasswordCheckCommon checks passwords against a list of common passwords (default true).
	PasswordCheckCommon bool // PASSWORD_CHECK_COMMON (default true)

	// OTLPEndpoint is the OpenTelemetry collector endpoint.
	// When empty, tracing is disabled.
	OTLPEndpoint string // OTEL_EXPORTER_OTLP_ENDPOINT

	// OTLPInsecure disables TLS for OTLP connections (for local collectors).
	OTLPInsecure bool // OTEL_EXPORTER_OTLP_INSECURE (default false)

	// TracingSamplingRate controls the global trace sampling rate as a float
	// between 0.0 and 1.0. Default 1.0 (always sample). When per-tenant rates
	// are configured via TracingTenantSamplingRates, the global rate acts as
	// the fallback for tenants not listed in the per-tenant map.
	TracingSamplingRate float64 // TRACING_SAMPLING_RATE (default 1.0)

	// TracingTenantSamplingRates is a map from tenant slug to sampling rate
	// (0.0-1.0). Loaded from TRACING_TENANT_SAMPLING_RATES env var as a
	// comma-separated list of tenant=rate pairs (e.g. "acme=0.1,corp=0.5").
	// Tenants not listed fall back to TracingSamplingRate.
	TracingTenantSamplingRates map[string]float64 // TRACING_TENANT_SAMPLING_RATES

	// GracefulShutdownTimeout controls how long to drain in-flight requests on shutdown.
	GracefulShutdownTimeout time.Duration // GRACEFUL_SHUTDOWN_SECS (default 60)

	// PreStopDrainDelay is the time to wait after flipping readiness to
	// 503 and before calling Shutdown() on the HTTP servers. This gives
	// the load balancer time to observe the readiness change and deregister
	// the pod before it stops accepting connections.
	PreStopDrainDelay time.Duration // PRESTOP_DRAIN_SECS (default 5)

	// The engine's own content caches, which stay in process memory.
	CacheTTL        time.Duration // CACHE_TTL (default 60s)
	CacheMaxEntries int           // CACHE_MAX_ENTRIES (default 1000)

	// DatabaseDriver selects the SQL dialect the schema engine generates DDL
	// for. Supported: "postgres", "mysql", "mssql". Defaults to the dialect of
	// DatabaseURL rather than a fixed value, so a MySQL or MSSQL deployment
	// that never sets DATABASE_DRIVER still gets DDL for its own dialect.
	DatabaseDriver string // DATABASE_DRIVER (default: derived from DATABASE_URL)

	// Connection pool lifetime tuning (Go 1.15+)
	DatabaseConnMaxLifetime time.Duration // DB_CONN_MAX_LIFETIME (default 1h)
	DatabaseConnMaxIdleTime time.Duration // DB_CONN_MAX_IDLE_TIME (default 5m)

	// Storage settings.
	StorageDriver           string // STORAGE_DRIVER: "local" (default) | "s3"
	StorageLocalPath        string // STORAGE_LOCAL_PATH (default "./uploads")
	StorageBaseURL          string // STORAGE_BASE_URL (e.g. "http://localhost:3001/uploads")
	StorageS3Bucket         string // STORAGE_S3_BUCKET
	StorageS3Region         string // STORAGE_S3_REGION (default "us-east-1")
	StorageS3Endpoint       string // STORAGE_S3_ENDPOINT (optional, for R2/MinIO)
	StorageS3Key            string // STORAGE_S3_KEY
	StorageS3Secret         string // STORAGE_S3_SECRET
	StorageS3CDNBaseURL     string // STORAGE_S3_CDN_BASE_URL (optional, CDN base URL for public access)
	StorageS3UseSSL         bool   // STORAGE_S3_USE_SSL (default true)
	StorageS3ForcePathStyle bool   // STORAGE_S3_FORCE_PATH_STYLE (default false, set true for MinIO)
	StorageS3MultipartMB    int64  // STORAGE_S3_MULTIPART_MB: threshold in MB (default 5)

	// MultiTenant enables X-Tenant-ID header enforcement.
	MultiTenant bool // MULTI_TENANT (default false)

	// License settings. All optional: an empty LicenseKey runs the engine
	// without a license.
	// LicenseKey (LYEVE_LICENSE_KEY) and LicenseCacheDir
	// (LYEVE_LICENSE_CACHE_DIR, default /var/lib/lyeve) are handed to the
	// licensing implementation, which decides what each holds.
	LicenseKey      string
	LicenseCacheDir string
	// LicenseServerURL is handed to the licensing implementation, which
	// decides what it is for. Empty leaves that implementation's own default
	// in force.
	LicenseServerURL string // LYEVE_LICENSE_SERVER_URL

	// Plugins is the explicit set of plugins to activate on this instance.
	// Must be a subset of the features granted by the license.
	// Empty = activate all entitled plugins.
	// Example: LYEVE_PLUGINS=graphql,grpc
	Plugins []string // LYEVE_PLUGINS

	// MfaGracePeriodHours is the MFA enrollment grace period for new users.
	// During this window, MFA is optional rather than mandatory.
	// Set to 0 (default) to disable the grace period.
	MfaGracePeriodHours int // MFA_GRACE_PERIOD_HOURS (default 0)

	// Rate limiter backend selection.
	//   RATE_LIMIT_BACKEND: "memory" (default) | "redis"
	//   RATE_LIMIT_PER_TENANT: key rate limits by tenant_id+IP (default false)
	RateLimitBackend   string // RATE_LIMIT_BACKEND (default "memory")
	RateLimitPerTenant bool   // RATE_LIMIT_PER_TENANT (default false)

	// PublicRateLimits moves individual public-endpoint caps, as
	// METHOD:/pattern=rate:burst entries using the route pattern exactly as
	// registered. Entries merge over the engine's table, so a route not named
	// here keeps its shipped limit. Empty means the shipped table stands.
	// Example: PUBLIC_RATE_LIMITS="GET:/api/v1/flows/p/{flow_id}=10:50"
	PublicRateLimits []string // PUBLIC_RATE_LIMITS (comma-separated)

	// PublicRateLimitGlobal moves the aggregate per-IP cap that applies to
	// every public route, as rate:burst. Empty keeps the shipped 50:100.
	// Deployments where many callers share one source address (a CDN egress
	// range, a reverse proxy on loopback, a load generator) are what this is
	// for. Example: PUBLIC_RATE_LIMIT_GLOBAL="200:400"
	PublicRateLimitGlobal string // PUBLIC_RATE_LIMIT_GLOBAL

	// PoolSizing captures per-tenant pool allocations for external poolers
	// (PgBouncer / ProxySQL) and per-tenant health monitoring. Nil when no
	// external pooler is configured. Loaded from CONNECTION_POOLER + POOL_*
	// env vars by LoadPoolConfig() in pool.go.
	PoolSizing *db.PoolConfig

	// PoolerMode selects the external connection pooler strategy.
	// Valid values: "none" (default), "pgbouncer", "proxysql".
	// Loaded from CONNECTION_POOLER env var.
	PoolerMode db.PoolerMode

	// DatabaseReplicaURL is the DSN for a read-only database replica.
	// When empty, all reads go to the primary. When set, QuerierRO routes
	// read-only queries (dashboard, schema list) to this replica pool.
	DatabaseReplicaURL string // DATABASE_REPLICA_URL

	// DatabaseReplicaMaxConns caps the replica connection pool.
	// Default 30. Only used when DatabaseReplicaURL is set.
	DatabaseReplicaMaxConns int32 // DATABASE_REPLICA_MAX_CONNS (default 30)

	// InstanceRegion identifies the geographic data region this instance
	// serves. When set, the GeoRoute middleware adds the X-CMS-Region
	// response header so upstream proxies can make routing decisions. A
	// plugin reads it as the host's instance_region setting, so a write guard
	// compares a tenant's region against the same value.
	InstanceRegion string // INSTANCE_REGION (default "")

	// TrustedProxies is a list of CIDRs identifying reverse proxies whose
	// X-Forwarded-For headers are trusted. When empty, XFF is never
	// trusted: ClientIPTrusted falls back to RemoteAddr. Loaded from
	// TRUSTED_PROXIES (comma-separated CIDRs).
	// Example: TRUSTED_PROXIES="10.0.0.0/8,172.16.0.0/12"
	TrustedProxies []string // TRUSTED_PROXIES

	// EnforceAPIKeyPepper makes the API-key HMAC pepper mandatory at
	// startup. When true and no pepper is resolved from the secret-custody
	// chain (API_KEY_PEPPER env or KMS/Vault plugin), the server refuses
	// to start. Set this in production to guarantee defense-in-depth for
	// the sys_api_keys table. Default false.
	EnforceAPIKeyPepper bool // ENFORCE_API_KEY_PEPPER

	// Environment identifies the deployment environment.
	// Set via APP_ENV. Expected values: "production", "staging", "development".
	// Defaults to DefaultEnvironment ("production") when unset, so an
	// unconfigured deployment is validated strictly rather than silently
	// skipping every check in ValidateProduction.
	// Use IsProduction() / IsDevelopment() to check.
	Environment string // APP_ENV

	// BaseURL is the canonical public base URL for this CMS instance.
	// Used by plugins that derive external-facing endpoint URLs
	// and prevent host-header injection attacks. Must be set in production.
	// Example: "https://cms.example.com"
	BaseURL string // LYEVE_BASE_URL

	// ConsoleURL is the public URL of the admin console, the address a person
	// opens in a browser. Links the engine and its plugins mail to a person
	// (password reset, magic-link sign-in) point at pages the console serves,
	// so they are built on this and never on a listen address. The trailing
	// slash is dropped. Example: "https://admin.example.com"
	ConsoleURL string // LYEVE_CONSOLE_URL

	// MetricsToken is a static bearer token for Prometheus scrape access.
	// Prometheus sends this as Authorization: Bearer <token>. When set,
	// the /metrics endpoint accepts this token without requiring a JWT.
	// Must be at least 32 characters. Required in production so k8s
	// ServiceMonitors can scrape without expiring JWTs.
	MetricsToken string // METRICS_TOKEN

	// AdminConsoleKey is the shared secret the admin console signs its engine
	// requests with. A signed request carries the browser's address, so
	// per-address controls count the console's users separately instead of as
	// the console itself. Unset, the engine ignores the console headers. At
	// least 32 characters. The console must hold the same value.
	AdminConsoleKey string // ADMIN_CONSOLE_KEY

	// AdminConsoleKeyPrevious is also accepted while a rotation is under way:
	// set the new key as AdminConsoleKey and the old one here, move the
	// console to the new key, then unset this. Requires AdminConsoleKey.
	AdminConsoleKeyPrevious string // ADMIN_CONSOLE_KEY_PREVIOUS

	// AuditHMACKey is the hex-encoded 32-byte HMAC-SHA256 key for audit
	// hash-chain integrity. Must be a 64-character hex string (32 bytes).
	// In production, boot fails when unset. In dev/test, the default zero
	// key is used: acceptable for local development but insecure for real
	// deployments.
	AuditHMACKey string // LYEVE_AUDIT_HMAC_KEY

	// PluginCapsStrictMode causes boot to fail when any plugin is scoped by
	// neither the host capability policy nor RegisterPluginWithCaps. With the
	// default false, such a plugin still starts, but it is granted no
	// capabilities and boot logs a warning. Set to true to refuse to boot
	// until every plugin is scoped.
	PluginCapsStrictMode bool // PLUGIN_CAPS_STRICT_MODE (default false)

	// Backpressure / load-shedding middleware. Default off (conservative).
	// When enabled, the middleware gates every request through three layers:
	// global concurrency cap, per-tenant fair share, and DB pool pressure
	// shedding. Health/ready/metrics endpoints are always bypassed.
	BackpressureEnabled               bool    // BACKPRESSURE_ENABLED (default false)
	BackpressureMaxInflight           int64   // BACKPRESSURE_MAX_INFLIGHT (default 200)
	BackpressureTenantQuotaPct        float64 // BACKPRESSURE_TENANT_QUOTA_PCT: fraction of max inflight per tenant (default 0.4)
	BackpressurePoolPressureThreshold float64 // BACKPRESSURE_POOL_PRESSURE_THRESHOLD: InUse/MaxOpen (default 0.85)

	// Goroutine Engine enables centralized worker pools and async hooks.
	// GOROUTINE_ENGINE_ENABLED applies GOROUTINE_ENGINE_POOL_SIZE to the worker
	// pool, warms plugin caches in parallel, and lets ASYNC_HOOKS_ENABLED turn
	// async hooks on.
	GoroutineEngineEnabled  bool          // GOROUTINE_ENGINE_ENABLED (default false)
	GoroutineEnginePoolSize int           // GOROUTINE_ENGINE_POOL_SIZE (default 100)
	AsyncHooksEnabled       bool          // ASYNC_HOOKS_ENABLED (default false)
	AsyncHookTimeout        time.Duration // ASYNC_HOOK_TIMEOUT (default 5s)

	// MemoryLimitBytes is the container memory limit for GOMEMLIMIT tuning.
	// Auto-detected from cgroup when unset. Falls back to 0 (no soft limit).
	MemoryLimitBytes int64 // MEMORY_LIMIT_BYTES (auto-detected from cgroup)

	// CPUQuota is the container CPU allowance in whole cores, rounded up, for
	// GOMAXPROCS tuning. Auto-detected from cgroup when unset. Falls back to 0,
	// which leaves the Go default of one thread per host core.
	CPUQuota int // CPU_QUOTA (auto-detected from cgroup)

	// DBConcurrency enables parallel warmup/stop for plugins.
	DBWarmupParallelism   int // DB_WARMUP_PARALLELISM (default 4)
	PluginStopParallelism int // PLUGIN_STOP_PARALLELISM (default 8)
}

// Load reads all environment variables, applies defaults, and returns a
// validated Config. Returns an error when a required variable is missing,
// a numeric/duration value is malformed, or ValidateCritical/ValidateProduction
// rejects the result.
