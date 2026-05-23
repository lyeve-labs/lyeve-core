package core

import (
	"context"
	"database/sql"
)

// EngineDBConnProvider hands out a database connection already bound to the
// engine's own database. Implemented by the engine host, forwarded by
// ScopedHost, and reached by type assertion.
//
// A plugin that owns sys_ tables or generated content tables needs this rather
// than a connection from the ordinary pool. The pool is shared with the
// request path, which binds a connection to a tenant database on MySQL and SQL
// Server, and a pooled connection keeps that binding when it is handed back.
// A later caller issuing unqualified DDL then writes into whichever tenant the
// previous request used, or fails with "Unknown database" once that tenant is
// deleted.
//
// The caller closes the connection. On PostgreSQL there is nothing to bind, so
// this returns an ordinary connection.
//
// Assert for the type, then handle the error. Do not branch on the assertion:
// ScopedHost and the test host both satisfy this unconditionally, so a false
// ok means a host that is neither, not a host that cannot serve the request.
// Every refusal, including a missing capability, arrives as an error.
type EngineDBConnProvider interface {
	EngineDBConn(ctx context.Context) (*sql.Conn, error)
}
