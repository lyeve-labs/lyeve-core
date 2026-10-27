package db

import (
	"context"

	"github.com/google/uuid"
)

// WriteLockName exposes the server-side lock name so a test can ask the
// server whether a session still holds it.
var WriteLockName = writeLockName

// AdminSeatLock is the key the seat guard locks, so a test can hold it.
const AdminSeatLock = adminSeatLock

// MySQLSchemaLockName is the engine's migration lock name on a MySQL
// connection, so a test can compare it with the name the server computed.
var MySQLSchemaLockName = mysqlSchemaLockName

// EnsureMySQLCollation is the boot step that gives a new MySQL database the
// collation the engine's tables need.
var EnsureMySQLCollation = ensureMySQLCollation

// AccountEmailFormLock is the lock the boot rewrite takes, so a test can hold
// it as another replica would.
const AccountEmailFormLock = accountEmailFormLock

// RewriteAccountAddress is the rewrite's single write, given the bytes it read,
// so a test can change the row between that read and the write. It reports
// whether the row took the form.
func RewriteAccountAddress(ctx context.Context, pool DB, column, tenant string, id uuid.UUID, read, form string) (bool, error) {
	outcome, err := rewriteAccountAddress(ctx, pool, column, accountAddressRow{id: id, tenant: tenant, value: read}, form)
	return outcome == addressRewritten, err
}
