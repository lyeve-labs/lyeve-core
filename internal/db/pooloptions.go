package db

// PoolerMode selects the connection pooler strategy. "none" means the
// built-in *sql.DB pool is used directly: no external pooler involved.
// "pgbouncer" and "proxysql" route through the respective external poolers.
//
// When PgBouncer or ProxySQL is used, the Go-side pool is configured with
// conservative settings (small MaxOpenConns) because the external pooler
// absorbs the bulk of connection management.
type PoolerMode string

const (
	// PoolerModeNone uses the built-in *sql.DB pool directly without an external pooler.
	PoolerModeNone PoolerMode = "none"
	// PoolerModePgBouncer routes connections through an external PgBouncer pooler.
	PoolerModePgBouncer PoolerMode = "pgbouncer"
	// PoolerModeProxySQL routes connections through an external ProxySQL pooler.
	PoolerModeProxySQL PoolerMode = "proxysql"
)
