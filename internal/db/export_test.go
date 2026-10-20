package db

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
