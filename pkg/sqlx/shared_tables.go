package sqlx

import (
	"sync"
	"sync/atomic"
)

// Shared-table registry.
//
// The engine's own catalog tables carry the sys_ prefix and are isolated by
// a tenant_id column rather than by schema. Plugins own shared tables too: a
// plugin table that lives once in the engine's database is also isolated by
// tenant_id. Those tables do not carry the sys_ prefix, so the query rewriter
// would not qualify them on the database-per-tenant engines (MySQL/MSSQL),
// where an unqualified reference resolves inside the tenant's own database and
// fails with "table doesn't exist". Registering the name here lets the
// rewriter qualify it exactly like a sys_* table.
//
// Call RegisterSharedTable once per table, from init() or Start(). Idempotent
// and safe for concurrent use.
var (
	sharedTables     sync.Map
	sharedTableCount atomic.Int64
)

// RegisterSharedTable marks name as a shared catalog table owned by a plugin.
// It is isolated by a tenant_id column and lives in the engine's own database,
// so the engine qualifies it on MySQL/MSSQL the same way it qualifies sys_*.
func RegisterSharedTable(name string) {
	if _, loaded := sharedTables.LoadOrStore(name, struct{}{}); !loaded {
		sharedTableCount.Add(1)
	}
}

// IsSharedTable reports whether name was registered via RegisterSharedTable.
func IsSharedTable(name string) bool {
	_, ok := sharedTables.Load(name)
	return ok
}

// HasSharedTables reports whether any shared table has been registered. Cheap
// (atomic read) so the query rewriter can consult it on every statement.
func HasSharedTables() bool {
	return sharedTableCount.Load() > 0
}
