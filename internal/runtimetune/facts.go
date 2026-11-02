package runtimetune

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The facts that are unknown, each with its reason as a code and in words.
var (
	noCPULimit      = unknown(core.FactReasonNoCgroup, "the container sets no CPU limit, or its limit cannot be read")
	noMemoryLimit   = unknown(core.FactReasonNoCgroup, "the container sets no memory limit, or its limit cannot be read")
	noHostMemory    = unknown(core.FactReasonNoProc, "the machine's memory cannot be read on this system")
	noDatabase      = unknown(core.FactReasonNoDatabase, "this engine runs without a database")
	unreadable      = unknown(core.FactReasonUnreadable, "the database did not answer the read")
	noFixedLimit    = unknown(core.FactReasonNoFixedLimit, "the database sets no fixed connection limit and accepts connections on demand")
	coresNotOverSQL = unknown(core.FactReasonNotOverSQL, "this database does not report its CPU count to a client")
	noRoster        = unknown(core.FactReasonNoRoster, "no transport that keeps a roster of the replicas is running")
	notChosenYet    = unknown(core.FactReasonNotChosenYet, "the engine has not chosen its cache backend yet")
	noServerState   = missingGrant("VIEW SERVER STATE", "the database account lacks the VIEW SERVER STATE permission")
	noStatsGrant    = missingGrant("pg_read_all_stats", "the database account is not a member of pg_read_all_stats")
)

// factsReadLimit bounds the database reads together, and the roster read on
// its own. They run on the primary pool, so without it a saturated pool would
// hold the caller for its whole deadline.
const factsReadLimit = 2 * time.Second

// RuntimeFacts implements core.RuntimeTuning.
func (t *Tuner) RuntimeFacts(ctx context.Context) core.RuntimeFacts {
	f := core.RuntimeFacts{
		CPULimit:    noCPULimit,
		MemoryLimit: noMemoryLimit,
		HostCores:   known(t.hostCPUs, core.SourceMachine),
		HostMemory:  noHostMemory,
		Dialect:     t.dialect,
		Replicas:    noRoster,
		SharedCache: notChosenYet,
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
		return noDatabase, noDatabase, noDatabase
	}
	switch dialect {
	case "postgres", "postgresql":
		return connLimit(ctx, sqlDB, dialect),
			// Without the grant the view hides other accounts' backend type,
			// so only the caller's own sessions would count.
			intFact(ctx, sqlDB, `SELECT CASE WHEN pg_has_role('pg_read_all_stats', 'USAGE')
				THEN (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend') ELSE -1 END`, noStatsGrant),
			coresNotOverSQL
	case "mysql":
		return connLimit(ctx, sqlDB, dialect),
			intFact(ctx, sqlDB, `SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Threads_connected'`, core.Fact{}),
			coresNotOverSQL
	case "mssql":
		return connLimit(ctx, sqlDB, dialect),
			// Without the permission the view lists only the caller's own
			// session, which would read as one connection open.
			intFact(ctx, sqlDB, `SELECT CASE WHEN HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER STATE') = 1
				THEN (SELECT count(*) FROM sys.dm_exec_sessions WHERE is_user_process = 1) ELSE -1 END`, noServerState),
			// The view refuses the account outright without the permission,
			// even inside a CASE, so the check runs as a branch.
			intFact(ctx, sqlDB, `IF HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER STATE') = 1
				SELECT cpu_count FROM sys.dm_os_sys_info ELSE SELECT -1`, noServerState)
	default:
		return unreadable, unreadable, unreadable
	}
}

// connLimit reads the connections the server accepts from ordinary accounts,
// by the read the engine's crowding check uses.
func connLimit(ctx context.Context, sqlDB *sql.DB, dialect string) core.Fact {
	n, err := db.ServerConnLimit(ctx, sqlDB, dialect)
	switch {
	case errors.Is(err, db.ErrNoFixedConnLimit):
		return noFixedLimit
	case err != nil:
		return unreadable
	default:
		return known(n, core.SourceDatabase)
	}
}

// intFact reads one integer, as an int. A query that checks the account's
// grant answers -1 without it, which reports denied. Any other failure
// reports a read the database did not answer, so a timeout never reads as a
// missing grant. Some servers answer these as text, so the value is scanned
// as one.
func intFact(ctx context.Context, sqlDB *sql.DB, query string, denied core.Fact) core.Fact {
	var s string
	if err := sqlDB.QueryRowContext(ctx, query).Scan(&s); err != nil {
		return unreadable
	}
	n, err := strconv.Atoi(s)
	switch {
	case err == nil && n == -1 && denied.ReasonCode != "":
		return denied
	case err != nil || n < 0:
		return unreadable
	}
	return known(n, core.SourceDatabase)
}

func known(v any, source string) core.Fact {
	return core.Fact{Value: v, Known: true, Source: source}
}

func unknown(code core.FactReason, reason string) core.Fact {
	return core.Fact{ReasonCode: code, Reason: reason}
}

func missingGrant(grant, reason string) core.Fact {
	return core.Fact{ReasonCode: core.FactReasonMissingGrant, Grant: grant, Reason: reason}
}
