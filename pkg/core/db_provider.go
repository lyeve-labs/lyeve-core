package core

import "context"

// StorageConnectedProvider is an optional interface that engine hosts implement
// when a storage provider (S3 or MinIO) has been connected.
// Plugins type-assert the host to this interface to access the storage client.
type StorageConnectedProvider interface {
	// StorageConnected returns the active storage connection or nil.
	StorageConnected() any
}

// AdminQuerierProvider is an optional interface that engine hosts implement
// to grant plugins (admin) an unscoped, non-isolated database querier for
// operational tasks: tenant enumeration, schema inspection, diagnostic
// queries, and administrative DML.
//
// Unlike Querier (which routes through tenant isolation and the primary
// pool) and RawDB (which is DML-guarded), AdminQuerier returns a Querier
// that talks to the engine's real *sql.DB pool. Plugins must gate admin
// endpoints behind RequireSuperAdmin or an equivalent auth check before
// reaching for this querier.
//
// Usage:
//
//	aq, ok := host.(core.AdminQuerierProvider)
//	if ok {
//	    q := aq.AdminQuerier(ctx)
//	    q.Exec(ctx, "UPDATE ...")
//	}
type AdminQuerierProvider interface {
	// AdminQuerier returns an unscoped database querier for admin operations.
	AdminQuerier(ctx context.Context) Querier
}
