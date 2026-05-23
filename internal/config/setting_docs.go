package config

// SettingDoc says what one setting does and what applies when nothing sets it.
type SettingDoc struct {
	Description string
	Default     string
}

// settingDocs describes the engine's own settings, keyed by the environment
// name every layer resolves to. The configuration endpoint returns the entry
// with each setting so an operator reads what a key does where they change
// it. A key with no entry, such as one a plugin reads, is reported without one.
var settingDocs = map[string]SettingDoc{
	"ADMIN_LISTEN_ADDR":        {Description: "Bind address for the Admin API.", Default: "Default port 3001"},
	"API_LISTEN_ADDR":          {Description: "Bind address for the Content API.", Default: "Default port 3002"},
	"APP_ENV":                  {Description: "development or production. Production applies a stricter validation pass that refuses to boot on a weak configuration, and warns on every boot about a plaintext database connection.", Default: "production"},
	"CORS_ALLOW_HEADERS":       {Description: "Request headers a browser may send. Replaces the default list rather than adding to it, so keep Authorization and X-Tenant-ID if you set it.", Default: "Content-Type, Authorization, X-API-Key, X-Tenant-ID, X-Correlation-ID, X-CSRF-Token"},
	"CORS_ALLOW_METHODS":       {Description: "Methods the preflight advertises.", Default: "GET, POST, PUT, DELETE, PATCH, OPTIONS"},
	"CORS_EXPOSE_HEADERS":      {Description: "Response headers a cross-origin script may read. The default names the request id and the rate-limit headers; without them a browser client can neither quote a request id in a bug report nor read the values it needs to back off with.", Default: "request id, correlation id, both rate-limit header families, Retry-After"},
	"CORS_MAX_AGE":             {Description: "How long a browser may cache the preflight, in seconds.", Default: "3600"},
	"CORS_ORIGINS":             {Description: "Allowed origins for cross-origin requests, comma-separated.", Default: "http://localhost:5173"},
	"DATABASE_MAX_CONNECTIONS": {Description: "Max open connections in the pool.", Default: "25"},
	"DATABASE_URL":             {Description: "Connection string for PostgreSQL, MySQL, or SQL Server. The engine detects the dialect from the URL scheme. In production a PostgreSQL URL whose sslmode is missing, disable, allow or prefer boots but logs a warning on every start.", Default: "Required"},
	"ENCRYPTION_KEY":           {Description: "Source key material for deriving the AES-256-GCM data-at-rest key via PBKDF2-HMAC-SHA256 (600,000 iterations, random 16-byte salt per operation). Must differ from JWT_SECRET and be at least 32 characters.", Default: "Required"},
	"LYEVE_OVERRIDABLE":        {Description: "Comma-separated key names the admin configuration page may take over from the environment. A listed key keeps its variable's value until something is stored against it, and behaves exactly as a file key tagged !overridable. Read once at boot, and it can never name itself or an operator-only key.", Default: "Empty, so every variable is fixed"},
	"JWT_EXPIRY_SECS":          {Description: "Access token lifetime, in seconds. Production rejects any value above 3600.", Default: "900 (15 minutes)"},
	"JWT_KEY_PATH":             {Description: "Filesystem path where the generated Ed25519 keypair is persisted across restarts. Mount a volume here, or every restart mints a new keypair and invalidates every issued token.", Default: "/var/lib/lyeve/jwt_key.json"},
	"JWT_SECRET":               {Description: "HS256 signing secret, used only as a legacy fallback. EdDSA (Ed25519) is the primary signing algorithm; a keypair is generated on first boot if one doesn't exist. Required regardless: the engine refuses to boot without it, before any algorithm decision is made.", Default: "Required"},
	"LYEVE_AUDIT_HMAC_KEY":     {Description: "Key for the audit log's hash chain. Exactly 64 hex characters.", Default: "Required in production"},
	"LYEVE_CONSOLE_URL":        {Description: "Public URL of the admin console, the address a person opens it at, such as https://admin.example.com. Password reset and magic-link sign-in mail links to pages on it, and device sign-in answers its approval page on it. It must be an absolute http or https URL with no query, and https in production. Unset in production, both plugins refuse to start and device sign-in refuses every request rather than send a person to a page that is not there.", Default: "http://localhost:5173 outside production, required in production for those plugins and device sign-in"},
	"LYEVE_GDPR_HOLD_CHECK":    {Description: "What an erasure request does when a legal-hold check is registered but cannot answer. With refuse it returns 503 and erases nothing. With proceed it erases anyway. An install with no legal-hold check registered has no holds to honor and is unaffected either way.", Default: "refuse"},
	"LYEVE_LICENSE_CACHE_DIR":  {Description: "A directory passed to the linked licensing implementation for what it keeps between restarts.", Default: "Default /var/lib/lyeve"},
	"LYEVE_LICENSE_KEY":        {Description: "The license, passed to the linked licensing implementation. Plugins that need no license run with or without it.", Default: "Optional"},
	"LYEVE_LICENSE_SERVER_URL": {Description: "An address passed to the linked licensing implementation. https only.", Default: "Unset, the licensing implementation decides."},
	"LYEVE_MODE":               {Description: "stateless boots the engine with no database: API keys come from the api_keys section of the configuration file, each declared by the SHA-256 of a key with roles or scopes, one tenant is served, nothing is metered or kept across a restart, and only plugins that declare they run without a database start. It refuses DATABASE_URL, DATABASE_REPLICA_URL, MULTI_TENANT and LYEVE_SETUP_MODE. Unset, the engine needs a database.", Default: "unset"},
	"LYEVE_PLUGINS":            {Description: "Comma-separated allowlist of plugin names to request at boot. Active plugins are the intersection of compiled, entitled, and requested. Empty means request every entitled plugin.", Default: "Default empty (all entitled)"},
	"LYEVE_SETUP_MODE":         {Description: "true lets the engine boot without DATABASE_URL, JWT_SECRET or ENCRYPTION_KEY into setup mode: only /healthz, /readyz (not ready) and the setup routes are served, no plugin starts, and the admin shows what to set with freshly generated secrets shown once. Unset, a missing setting fails the boot.", Default: "unset"},
	"LYEVE_SETUP_TOKEN":        {Description: "The credential POST /api/admin/setup demands before it creates the first super admin. Unset, the engine prints a one-time token to its log at startup while no account exists. Set it when more than one replica can answer setup, since each replica prints its own. Stops working once the first super admin exists. At least 16 characters.", Default: "unset (a logged one-time token)"},
	"MAX_BODY_BYTES":           {Description: "Ceiling on any request body, in bytes. Sized for uploads.", Default: "10485760 (10 MiB)"},
	"MAX_JSON_BODY_BYTES":      {Description: "Tighter ceiling for JSON API route groups. Only Content-Length is checked; the body itself is not wrapped, so a chunked request without the header passes this guard and meets MAX_BODY_BYTES instead.", Default: "1048576 (1 MiB)"},
	"METRICS_TOKEN":            {Description: "Bearer token for GET /api/admin/metrics on the admin listener. Unset does not close the endpoint: the middleware falls back to a plain super_admin role check, so any super-admin session reaches it. Set it to authenticate a scraper without giving it an admin account.", Default: "unset"},
	"PASSWORD_HASH_ALGO":       {Description: "Password hashing algorithm: bcrypt or argon2id. An unrecognized value is a boot error.", Default: "bcrypt"},
	"PUBLIC_RATE_LIMITS":       {Description: "Per-route caps for public endpoints, comma-separated METHOD:/pattern=rate:burst entries using the route pattern exactly as registered. Entries merge over the engine's shipped table, so a route you do not name keeps its own limit. A malformed entry stops the engine at boot, naming the entry, rather than leaving a cap you believe you moved where it was. On sign-in, the rate-limit plugin's own protection is stricter than this table and is changed on its page, not here.", Default: "shipped table"},
	"PUBLIC_RATE_LIMIT_GLOBAL": {Description: "Aggregate per-IP cap across every public route, as rate:burst. Raise it where many callers share one source address: a CDN egress range, a reverse proxy on loopback, a load generator.", Default: "50:100"},
	"RATE_LIMIT_RPS":           {Description: "Per-IP request ceiling for the global limiter. Unset means the limiter is off, which production rejects.", Default: "unset"},
	"SECURE_COOKIE":            {Description: "Whether cookies are marked Secure (requires HTTPS).", Default: "false"},
}

// DescribeSetting returns what a setting does, and false for a key the engine
// does not describe.
func DescribeSetting(key string) (SettingDoc, bool) {
	d, ok := settingDocs[normalizeKey(key)]
	return d, ok
}
