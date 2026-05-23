package db

import (
	"context"
	"fmt"
	"strings"
)

// CheckSnapshotReads reports whether SQL Server is serving READ COMMITTED from
// row versions on the current database, and returns the statement that turns it
// on when it is not. It is a no-op on Postgres and MySQL, which both read from
// a snapshot already.
//
// Without READ_COMMITTED_SNAPSHOT, SQL Server takes shared locks for reads, so
// an ordinary SELECT can be chosen as the victim of a deadlock with a
// concurrent write and the request fails. The same workload never deadlocks on
// the other two engines.
//
// This only reports. ALTER DATABASE ... SET READ_COMMITTED_SNAPSHOT waits for
// exclusive access to the database and no lock timeout applies, so issuing it
// against a database that is serving traffic blocks until every other session
// disconnects. Doing that at boot would hang the engine behind its own
// replicas, and the WITH ROLLBACK IMMEDIATE form would kill their connections
// instead. Both are worse than the deadlocks, so the operator is told and
// chooses the window.
func CheckSnapshotReads(ctx context.Context, pool DB) (enabled bool, remedy string, err error) {
	if pool.Engine() != "mssql" {
		return true, "", nil
	}
	row, qErr := pool.QueryRow(ctx, `
		SELECT DB_NAME(),
		       MAX(CASE WHEN database_id = DB_ID() THEN CAST(is_read_committed_snapshot_on AS INT) END),
		       MAX(CASE WHEN name = 'model' THEN CAST(is_read_committed_snapshot_on AS INT) END)
		FROM sys.databases
		WHERE database_id = DB_ID() OR name = 'model'`)
	if qErr != nil {
		return false, "", fmt.Errorf("read snapshot isolation state: %w", qErr)
	}
	var name string
	var here, model int
	if scanErr := row.Scan(&name, &here, &model); scanErr != nil {
		return false, "", fmt.Errorf("read snapshot isolation state: %w", scanErr)
	}
	if here == 1 && model == 1 {
		return true, "", nil
	}

	// Two separate settings. The first covers the shared database, where the
	// sys_* catalog tables live and where the reader/writer deadlocks
	// actually happen. The second covers tenant databases: a new database
	// inherits the setting from model, so doing it there costs nothing per
	// tenant, whereas issuing the ALTER per CREATE DATABASE compounds the
	// exclusive lock provisioning already takes on master.
	var remedies []string
	if here != 1 {
		remedies = append(remedies,
			fmt.Sprintf("ALTER DATABASE [%s] SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE;", name))
	}
	if model != 1 {
		remedies = append(remedies,
			"ALTER DATABASE [model] SET READ_COMMITTED_SNAPSHOT ON; -- tenant databases created after this inherit it")
	}
	return false, strings.Join(remedies, " "), nil
}
