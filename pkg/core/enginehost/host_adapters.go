package enginehost

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// pgxTx wraps a raw *sql.Tx as a core.Querier.
//
// engine names the dialect so statements issued inside the transaction get the
// same placeholder rewriting the pool applies outside one. Without it the $1
// and $2 a plugin writes reach MySQL and MSSQL verbatim, and the only symptom
// is the driver's own complaint ("Unknown column '$1' in 'field list'") from
// whichever statement happened to be in a transaction.
//
// An empty engine means no rewriting, which is what AdminQuerier wants: it
// talks to the raw pool in the engine's own dialect.
//
// sysDB names the engine's own database so sys_* references inside the
// transaction are qualified the way the pool qualifies the ones outside it. A
// transaction begun on a tenant-scoped connection inherits that connection's
// database, so without it every statement in the transaction that touches a
// catalog table looks for it in the tenant's database. Empty disables the
// qualification, which is what Postgres and AdminQuerier both want.
type pgxTx struct {
	tx     *sql.Tx
	engine string
	sysDB  string
}

func (t *pgxTx) rewrite(q string, args []any) (string, []any) {
	if t.engine == "" {
		return q, args
	}
	q = db.QualifySysTables(q, t.engine, t.sysDB)
	return db.RewritePlaceholders(q, t.engine, args)
}

func (t *pgxTx) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	sql, args = t.rewrite(sql, args)
	return t.tx.QueryRowContext(ctx, sql, args...), nil
}

func (t *pgxTx) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	sql, args = t.rewrite(sql, args)
	rows, err := t.tx.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows: rows}, nil
}

func (t *pgxTx) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	sql, args = t.rewrite(sql, args)
	tag, err := t.tx.ExecContext(ctx, sql, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := tag.RowsAffected()
	return core.CommandTag{RowsAffected: n}, nil
}

// Begin creates a savepoint-backed nested transaction. *sql.Tx has no native
// nested transaction support, so explicit savepoints simulate the behavior.
func (t *pgxTx) Begin(ctx context.Context) (core.Tx, error) {
	name := nextSavepoint()
	if _, err := t.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return nil, err
	}
	return &savepointTx{tx: t.tx, name: name, engine: t.engine, sysDB: t.sysDB}, nil
}

func (t *pgxTx) Commit(ctx context.Context) error   { return t.tx.Commit() }
func (t *pgxTx) Rollback(ctx context.Context) error { return t.tx.Rollback() }

// savepointTx: nested transaction backed by SAVEPOINT / RELEASE / ROLLBACK TO

type savepointTx struct {
	tx     *sql.Tx
	name   string
	engine string
	sysDB  string
}

func (s *savepointTx) rewrite(q string, args []any) (string, []any) {
	if s.engine == "" {
		return q, args
	}
	q = db.QualifySysTables(q, s.engine, s.sysDB)
	return db.RewritePlaceholders(q, s.engine, args)
}

func (s *savepointTx) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	sql, args = s.rewrite(sql, args)
	return s.tx.QueryRowContext(ctx, sql, args...), nil
}

func (s *savepointTx) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	sql, args = s.rewrite(sql, args)
	rows, err := s.tx.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows: rows}, nil
}

func (s *savepointTx) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	sql, args = s.rewrite(sql, args)
	tag, err := s.tx.ExecContext(ctx, sql, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := tag.RowsAffected()
	return core.CommandTag{RowsAffected: n}, nil
}

func (s *savepointTx) Begin(ctx context.Context) (core.Tx, error) {
	name := nextSavepoint()
	if _, err := s.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return nil, err
	}
	return &savepointTx{tx: s.tx, name: name, engine: s.engine, sysDB: s.sysDB}, nil
}

func (s *savepointTx) Commit(ctx context.Context) error {
	_, err := s.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+s.name)
	return err
}

func (s *savepointTx) Rollback(ctx context.Context) error {
	_, err := s.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+s.name)
	return err
}

// savepointSeq makes savepoint names unique across the process. SAVEPOINTs are
// scoped to a single transaction so collisions would only matter if two
// nested savepoints landed on the same outer tx in the same tick: a counter
// is more than safe enough.
var savepointSeq atomic.Uint64

func nextSavepoint() string {
	return fmt.Sprintf("sp_%d", savepointSeq.Add(1))
}

// configAdapter: wraps *config.Config to implement core.Config

type configAdapter struct{ cfg *config.Config }

// String redacts secret material via the canonical core.SecretKeys set.
// Escalate to engineHost.Secret() for the real value.
func (c *configAdapter) String(key string) string {
	if core.IsSecretKey(key) {
		return ""
	}
	switch key {
	case "api_listen_addr":
		return c.cfg.APIListenAddr
	case "admin_listen_addr":
		return c.cfg.AdminListenAddr
	case "tls_cert_file":
		// Set when the listeners serve TLS. A plugin that calls the engine
		// over loopback needs the scheme and the certificate to verify
		// against.
		return c.cfg.TLSCertFile
	case "jwt_expiry_secs":
		// Without the case this falls through to the raw environment, so an
		// instance that never set the variable would hand plugins an empty
		// string and each would use its own fallback. A plugin that mints its
		// own tokens and reads the expiry directly gets the configured value.
		// The value is seconds, as the key says.
		return strconv.FormatInt(c.cfg.JWTExpirySecs, 10)
	case "instance_id":

		return c.cfg.InstanceID
	case "instance_region":
		// The region the engine booted with, which its own region routing
		// answers from. A plugin that refuses a write by region compares
		// against the same value, not a second read of the setting that a
		// stored override could make disagree with it.
		return c.cfg.InstanceRegion
	case core.ConfigKeyEngineMode:
		return c.cfg.Mode
	case "storage_driver":
		return c.cfg.StorageDriver
	case "storage_s3_region":
		return c.cfg.StorageS3Region
	case "storage_s3_bucket":
		return c.cfg.StorageS3Bucket
	case "storage_s3_endpoint":
		return c.cfg.StorageS3Endpoint
	case "storage_s3_cdn_base_url":
		return c.cfg.StorageS3CDNBaseURL
	case "storage_s3_multipart_mb":
		return strconv.FormatInt(c.cfg.StorageS3MultipartMB, 10)
	case "storage_local_path":
		return c.cfg.StorageLocalPath
	case "storage_base_url":
		return c.cfg.StorageBaseURL
	case "base_url":
		return c.cfg.BaseURL
	case core.ConfigKeyConsoleURL:
		return c.cfg.ConsoleURL
	case "mfa_grace_period_hours":
		return strconv.Itoa(c.cfg.MfaGracePeriodHours)
	case "database_driver":
		return c.cfg.DatabaseDriver
	case "otel_exporter_otlp_endpoint":
		return c.cfg.OTLPEndpoint
	case "otel_service_name":
		return c.cfg.InstanceID // reused as otel service identifier
	default:
		return pluginEnv(key)
	}
}

func (c *configAdapter) Bool(key string) bool {
	switch key {
	case "secure_cookie":
		return c.cfg.SecureCookie
	case "multi_tenant":
		return c.cfg.MultiTenant
	case "s3_use_ssl":
		return c.cfg.StorageS3UseSSL
	case "s3_force_path_style":
		return c.cfg.StorageS3ForcePathStyle
	case "is_production":
		// IsProduction, not a bare compare: it lowercases, so APP_ENV=Production
		// stays production here too. Plugins gate real exposure on this key,
		// such as a reflection service that enumerates every service and
		// message to any caller.
		return c.cfg.IsProduction()
	default:
		return pluginEnvBool(key)
	}
}

func (c *configAdapter) Duration(key string) time.Duration {
	switch key {
	case "graceful_shutdown_timeout":
		return c.cfg.GracefulShutdownTimeout
	case "cache_ttl":
		return c.cfg.CacheTTL
	default:
		return pluginEnvDuration(key)
	}
}

// Strings returns slice values for known list-type config keys.
// Known keys:
//   - cors_origins - allowed CORS origins
//   - trusted_issuers - external OIDC issuer base URLs
//   - ip_allowlist - CIDR ranges for IP allowlisting
//   - plugins - explicit plugin set (LYEVE_PLUGINS)
//
// Secret keys (jwt_secrets, database_url, etc.) are concealed: this method
// returns nil for any key in core.SecretKeys. Callers that need signing
// keys should use core.Host.Secrets() or the SessionTokenSigner interface.
//
// Unknown keys fall through to String(key) and split on "," so any
// comma-separated value exposed by String can be read as a slice without
// further wiring. Returns nil for unset/empty keys.
func (c *configAdapter) Strings(key string) []string {
	if core.SecretKeys[key] {
		return nil
	}
	switch key {
	case "cors_origins":
		return cloneNonEmpty(c.cfg.CORSOrigins)
	case "trusted_issuers":
		return cloneNonEmpty(c.cfg.TrustedIssuers)
	case "ip_allowlist":
		return cloneNonEmpty(c.cfg.IPAllowlist)
	case "plugins":
		return cloneNonEmpty(c.cfg.Plugins)
	}
	raw := c.String(key)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cloneNonEmpty returns a defensive copy of in with empty strings dropped.
// Returns nil when the result would be empty so callers can rely on len() == 0
// meaning "unset".
func cloneNonEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
