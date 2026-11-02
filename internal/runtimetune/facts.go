package runtimetune

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The reasons a fact is unknown.
const (
	reasonNoCPULimit      = "the container sets no CPU limit, or its limit cannot be read"
	reasonNoMemoryLimit   = "the container sets no memory limit, or its limit cannot be read"
	reasonNoHostMemory    = "the machine's memory cannot be read on this system"
	reasonNoDatabase      = "this engine runs without a database"
	reasonUnreadable      = "the database did not answer the read"
	reasonNoServerState   = "the database account lacks the VIEW SERVER STATE permission"
	reasonNoStatsGrant    = "the database account is not a member of pg_read_all_stats"
	reasonCoresNotOverSQL = "this database does not report its CPU count to a client"
	reasonNoRoster        = "no transport that keeps a roster of the replicas is running"
	reasonNotChosenYet    = "the engine has not chosen its cache backend yet"
)

// factsReadLimit bounds the database reads together, and the roster read on
// its own. They run on the primary pool, so without it a saturated pool would
// hold the caller for its whole deadline.
const factsReadLimit = 2 * time.Second

// RuntimeFacts implements core.RuntimeTuning.
func (t *Tuner) RuntimeFacts(ctx context.Context) core.RuntimeFacts {
	f := core.RuntimeFacts{
		CPULimit:    unknown(reasonNoCPULimit),
		MemoryLimit: unknown(reasonNoMemoryLimit),
		HostCores:   known(t.hostCPUs, core.SourceMachine),
		HostMemory:  unknown(reasonNoHostMemory),
		Dialect:     t.dialect,
		Replicas:    unknown(reasonNoRoster),
		SharedCache: unknown(reasonNotChosenYet),
	}
	if n := config.CgroupCPULimit(t.root); n > 0 {
		f.CPULimit = known(n, core.SourceContainer)
	}
	if n := config.CgroupMemoryLimit(t.root); n > 0 {
		f.MemoryLimit = known(n, core.SourceContainer)
	}
	if n := config.MachineMemory(t.root); n > 0 {
		f.HostMemory = known(n, core.SourceMachine)
	}
	dbCtx, cancel := context.WithTimeout(ctx, factsReadLimit)
	f.DatabaseConnLimit, f.DatabaseConnsOpen, f.DatabaseCores = databaseFacts(dbCtx, t.db, t.dialect)
	cancel()
	if t.replicas != nil {
		rosterCtx, cancel := context.WithTimeout(ctx, factsReadLimit)
		if n, ok := t.replicas.Replicas(rosterCtx); ok {
			f.Replicas = known(n, core.SourceCluster)
		}
		cancel()
		f.TransportAttached = t.replicas.Attached()
	}
	if c := t.sharedCache.Load(); c != 0 {
		f.SharedCache = core.Fact{Value: c == 2, Known: true}
	}
	return f
}

// databaseFacts reads the connection limit, the connections open and the
// cores from the database, each bounded by ctx.
func databaseFacts(ctx context.Context, sqlDB *sql.DB, dialect string) (limit, open, cores core.Fact) {
	if sqlDB == nil {
		return unknown(reasonNoDatabase), unknown(reasonNoDatabase), unknown(reasonNoDatabase)
	}
	switch dialect {
	case "postgres", "postgresql":
		return postgresConnLimit(ctx, sqlDB),
			// Without the grant the view hides other accounts' backend type,
			// so only the caller's own sessions would count.
			intFact(ctx, sqlDB, `SELECT CASE WHEN pg_has_role('pg_read_all_stats', 'USAGE')
				THEN (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend') ELSE -1 END`, reasonNoStatsGrant),
			unknown(reasonCoresNotOverSQL)
	case "mysql":
		// One more session stays open to an account holding
		// CONNECTION_ADMIN, which an ordinary pool cannot use.
		return intFact(ctx, sqlDB, `SELECT @@max_connections`, ""),
			intFact(ctx, sqlDB, `SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Threads_connected'`, ""),
			unknown(reasonCoresNotOverSQL)
	case "mssql":
		return mssqlConnLimit(ctx, sqlDB),
			// Without the permission the view lists only the caller's own
			// session, which would read as one connection open.
			intFact(ctx, sqlDB, `SELECT CASE WHEN HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER STATE') = 1
				THEN (SELECT count(*) FROM sys.dm_exec_sessions WHERE is_user_process = 1) ELSE -1 END`, reasonNoServerState),
			// The view refuses the account outright without the permission,
			// even inside a CASE, so the check runs as a branch.
			intFact(ctx, sqlDB, `IF HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER STATE') = 1
				SELECT cpu_count FROM sys.dm_os_sys_info ELSE SELECT -1`, reasonNoServerState)
	default:
		return unknown(reasonUnreadable), unknown(reasonUnreadable), unknown(reasonUnreadable)
	}
}

// postgresConnLimit is max_connections less the slots PostgreSQL keeps for
// superusers and, from version 16, for roles granted pg_use_reserved_connections.
func postgresConnLimit(ctx context.Context, sqlDB *sql.DB) core.Fact {
	rows, err := sqlDB.QueryContext(ctx, `SELECT name, setting FROM pg_settings
		WHERE name IN ('max_connections', 'superuser_reserved_connections', 'reserved_connections')`)
	if err != nil {
		return unknown(reasonUnreadable)
	}
	defer rows.Close()
	limit := -1
	reserved := 0
	for rows.Next() {
		var name, setting string
		if err := rows.Scan(&name, &setting); err != nil {
			return unknown(reasonUnreadable)
		}
		n, err := strconv.Atoi(setting)
		if err != nil {
			return unknown(reasonUnreadable)
		}
		if name == "max_connections" {
			limit = n
		} else {
			reserved += n
		}
	}
	if rows.Err() != nil || limit-reserved <= 0 {
		return unknown(reasonUnreadable)
	}
	return known(limit-reserved, core.SourceDatabase)
}

// mssqlConnLimit reads the user connections option. Its default, 0, lets SQL
// Server open connections on demand up to its maximum, which is no fixed
// limit to size a pool against.
func mssqlConnLimit(ctx context.Context, sqlDB *sql.DB) core.Fact {
	var n int
	err := sqlDB.QueryRowContext(ctx,
		`SELECT CAST(value_in_use AS INT) FROM sys.configurations WHERE name = 'user connections'`).Scan(&n)
	switch {
	case err != nil || n < 0:
		return unknown(reasonUnreadable)
	case n == 0:
		return unknown(core.FactReasonNoFixedLimit)
	default:
		return known(n, core.SourceDatabase)
	}
}

// intFact reads one integer, as an int. A query that checks the account's
// grant answers -1 without it, which reports denied. Any other failure
// reports a read the database did not answer, so a timeout never reads as a
// missing grant. Some servers answer these as text, so the value is scanned
// as one.
func intFact(ctx context.Context, sqlDB *sql.DB, query, denied string) core.Fact {
	var s string
	if err := sqlDB.QueryRowContext(ctx, query).Scan(&s); err != nil {
		return unknown(reasonUnreadable)
	}
	n, err := strconv.Atoi(s)
	switch {
	case err == nil && n == -1 && denied != "":
		return unknown(denied)
	case err != nil || n < 0:
		return unknown(reasonUnreadable)
	}
	return known(n, core.SourceDatabase)
}

func known(v any, source string) core.Fact {
	return core.Fact{Value: v, Known: true, Source: source}
}

func unknown(reason string) core.Fact {
	return core.Fact{Reason: reason}
}
