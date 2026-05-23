package core

import (
	"context"
	"fmt"
	"sync"
)

// TenantPurgeHandler is invoked during tenant deletion for GDPR-compliant
// per-tenant data removal. The handler receives a transaction-bound Querier
// (so all purges participate in the delete transaction) and the tenant slug.
// Return an error to abort the deletion and roll back the transaction.
type TenantPurgeHandler func(ctx context.Context, q Querier, slug string) error

var (
	purgeHandlersMu sync.Mutex
	purgeHandlers   []TenantPurgeHandler
)

// RegisterTenantPurgeHandler registers a handler that is called during tenant
// deletion. A plugin that owns tenant-scoped tables declares them through
// TenantPurger instead, so they are purged whether or not the plugin starts.
// This call is for handlers that clear state no table holds, and for
// the engine's own tables. Handlers are invoked in registration order inside
// the delete transaction.
//
// Safe for concurrent use.
func RegisterTenantPurgeHandler(fn TenantPurgeHandler) {
	purgeHandlersMu.Lock()
	purgeHandlers = append(purgeHandlers, fn)
	purgeHandlersMu.Unlock()
}

// TenantPurgeHandlers returns a copy of all registered purge handlers.
// The returned slice is safe to iterate and mutate without affecting the
// registry.
func TenantPurgeHandlers() []TenantPurgeHandler {
	purgeHandlersMu.Lock()
	defer purgeHandlersMu.Unlock()
	out := make([]TenantPurgeHandler, len(purgeHandlers))
	copy(out, purgeHandlers)
	return out
}

// TenantPurgeHandlerCount returns the number of registered purge handlers.
// Used by the boot-time purge registration validator to detect plugins that
// own tenant_id-bearing tables but forgot to register a purge handler.
func TenantPurgeHandlerCount() int {
	purgeHandlersMu.Lock()
	defer purgeHandlersMu.Unlock()
	return len(purgeHandlers)
}

// Covered table registry (parallel to purge handlers)

var (
	coveredTablesMu sync.Mutex
	coveredTables   = make(map[string]bool)
)

// RegisterCoveredTable declares that a purge handler covers the given table,
// so the boot-time validator can confirm every tenant_id-bearing table has a
// handler. RegisterTenantPurge calls it for every table a TenantPurge names.
// Duplicate registrations are idempotent (safe for hot-reload / test cycles).
func RegisterCoveredTable(tableName string) {
	coveredTablesMu.Lock()
	coveredTables[tableName] = true
	coveredTablesMu.Unlock()
}

// CoveredTableSet returns a copy of all registered covered table names.
// The returned map is safe to iterate without holding the lock.
func CoveredTableSet() map[string]bool {
	coveredTablesMu.Lock()
	defer coveredTablesMu.Unlock()
	out := make(map[string]bool, len(coveredTables))
	for k, v := range coveredTables {
		out[k] = v
	}
	return out
}

// ResetCoveredTables clears the covered table registry.
// Intended for tests that need a clean registry between tests.
func ResetCoveredTables() {
	coveredTablesMu.Lock()
	coveredTables = make(map[string]bool)
	coveredTablesMu.Unlock()
}

// ResetTenantPurgeHandlers clears the registered purge handler list.
// Intended for tests that need a clean registry between tests.
func ResetTenantPurgeHandlers() {
	purgeHandlersMu.Lock()
	purgeHandlers = nil
	purgeHandlersMu.Unlock()
}

// TenantPurger is a plugin that owns tenant-scoped tables. The engine reads
// it from every plugin compiled into the build at boot, whether or not the
// plugin is allowed to start, and registers each TenantPurge it returns.
//
// A plugin that stops running keeps its tables, because nothing drops a table
// that may hold the only copy of a customer's data. Registering from Start
// would leave those tables without a purge path the moment the plugin stops
// starting, so a tenant delete would keep their rows.
type TenantPurger interface {
	TenantPurges() []TenantPurge
}

// TenantPurge is one set of a plugin's tenant-scoped tables and the handler
// that clears a tenant's rows from them.
//
// The handler runs on an instance that has never started, so it reads
// nothing Start builds: only the querier and the slug it is handed.
//
// Tables names every tenant-scoped table the handler clears, including one
// whose rows go by a cascade. A set whose tables are all absent is skipped,
// because the plugin never ran here. A set with only some of them present is
// refused with an error naming the missing ones, because its handler would
// fail on them and roll the delete back without saying why. A table a later
// migration adds therefore belongs in a set of its own.
//
// Columns names, as "table.column", the tenant column a migration added to a
// table after it was created. Where the plugin last ran before that
// migration, the tables exist and the column does not, and every row there
// was written without a tenant, so no tenant owns it. The set is then
// skipped, or handed to Unkeyed when that is set. A set with some of its
// columns present and others absent is refused, for the same reason as a
// partial set of tables. A column a later migration adds to only some of a
// set's tables therefore moves those tables to a set of their own.
//
// Unkeyed runs instead of Handler when none of Columns exists. It is for a
// table whose tenant migration assigns its existing rows to a tenant rather
// than to none, so the delete of that tenant has to clear them as well.
type TenantPurge struct {
	Tables  []string
	Columns []string
	Handler TenantPurgeHandler
	Unkeyed TenantPurgeHandler
}

// RegisterTenantPurge registers tp's handler behind the check for its tables
// and columns, and declares each table covered. dialect is the engine
// database's, which the check reads the catalog in.
func RegisterTenantPurge(dialect string, tp TenantPurge) {
	if tp.Handler == nil || len(tp.Tables) == 0 {
		return
	}
	set := NewTableSet(tp.Tables, tp.Columns)
	handler, unkeyed := tp.Handler, tp.Unkeyed
	RegisterTenantPurgeHandler(func(ctx context.Context, q Querier, slug string) error {
		state, err := set.Check(ctx, q, dialect)
		if err != nil {
			return fmt.Errorf("tenant purge: %w", err)
		}
		switch state {
		case TableSetReady:
			return handler(ctx, q, slug)
		case TableSetUnkeyed:
			if unkeyed != nil {
				return unkeyed(ctx, q, slug)
			}
		}
		return nil
	})
	for _, t := range set.Tables() {
		RegisterCoveredTable(t)
	}
}

// PurgeRegistrationValidator compares the registered covered tables
// against the set of tables that have a tenant_id column (from
// information_schema). It returns the names of any tenant-scoped tables
// that are NOT covered by a registered purge handler: these tables would
// silently leak data on tenant deletion.
//
// This is a best-effort safety net, not a formal proof. The canonical
// ground truth is the set of registered handlers: this validator exists
// to catch the case where a developer adds a tenant_id column to a new
// table but forgets to register the corresponding purge handler.
type PurgeRegistrationValidator struct{}

// NewPurgeRegistrationValidator creates a validator.
func NewPurgeRegistrationValidator() *PurgeRegistrationValidator {
	return &PurgeRegistrationValidator{}
}

// MissingTables returns the set of tenant-scoped tables (from
// information_schema) that have zero registered purge handler coverage.
// An empty slice means every scoped table is covered.
func (v *PurgeRegistrationValidator) MissingTables(scopedTables []string) []string {
	covered := CoveredTableSet()
	var missing []string
	for _, t := range scopedTables {
		if !covered[t] {
			missing = append(missing, t)
		}
	}
	return missing
}

// ListTenantScopedTables lists every table that has a tenant_id column, read
// from information_schema on the dialect q speaks, for the validator to
// compare with the covered tables. A table the list holds that no handler
// covers keeps a deleted tenant's rows.
//
// Every column type counts, whether the table keys its tenant by the slug or
// by a UUID, because either way a delete that skips the table leaks it. The
// per-tenant schemas and databases, whose names begin with tenant_, are left
// out: dropping the schema removes their tables, not a purge handler.
//
// On MySQL information_schema spans every database on the server, so the list
// is held to the database q is connected to. Another install sharing the
// server is not this engine's to purge. q must therefore be the engine's own
// connection, never one a tenant request has bound to its database.
func ListTenantScopedTables(ctx context.Context, q Querier, dialect string) ([]string, error) {
	var query string
	switch dialect {
	case "mysql":
		query = `SELECT DISTINCT table_name FROM information_schema.columns
WHERE column_name='tenant_id'
  AND table_schema = DATABASE()
ORDER BY table_name`
	case "mssql":
		query = `SELECT DISTINCT table_name FROM information_schema.columns
WHERE column_name='tenant_id'
  AND table_schema NOT IN ('INFORMATION_SCHEMA','sys')
  AND table_schema NOT LIKE 'tenant_%'
ORDER BY table_name`
	default:
		query = `SELECT DISTINCT table_name FROM information_schema.columns
WHERE column_name='tenant_id'
  AND table_schema NOT IN ('information_schema','pg_catalog')
  AND table_schema NOT LIKE 'tenant_%'
ORDER BY table_name`
	}

	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list tenant-scoped tables: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan tenant-scoped table: %w", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenant-scoped tables: %w", err)
	}
	return tables, nil
}
