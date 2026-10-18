package runtime

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/core/enginehost"
)

// lockOnlyHost offers the lock pool setter alone, so the lock pool's open
// is the first one attachSessionPools tries.
type lockOnlyHost struct {
	core.Host
	locks *sql.DB
}

func (h *lockOnlyHost) WithLockDB(d *sql.DB) { h.locks = d }

// An engine whose DML guard or lock pool cannot open would hand plugins the
// unguarded pool, or hold leader sessions where requests run, for the life
// of the process. The start has to fail instead, naming the pool.
func TestAttachSessionPools_OpenFailureFailsStart_Postgres(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}
	pool, dsn := testdb.PostgresWithDSN(t)
	testSessionPoolOpenFailure(t, pool, dsn)
}

func TestAttachSessionPools_OpenFailureFailsStart_MySQL(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}
	pool, dsn := testdb.MySQLWithDSN(t)
	testSessionPoolOpenFailure(t, pool, dsn)
}

func TestAttachSessionPools_OpenFailureFailsStart_MSSQL(t *testing.T) {
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}
	pool, dsn := testdb.MSSQLWithDSN(t)
	testSessionPoolOpenFailure(t, pool, dsn)
}

func testSessionPoolOpenFailure(t *testing.T, pool db.DB, dsn string) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	unreachable := unreachableDSN(t, dsn)

	host := enginehost.NewHost(pool, sqlDBOf(t, pool), &config.Config{}, nil, "test")
	err := attachSessionPools(context.Background(), host, pool.Engine(), unreachable)
	require.ErrorContains(t, err, "DML guard pool")
	require.Same(t, sqlDBOf(t, pool), host.RawDB(), "no guard pool may be wired after a failed open")

	lockHost := &lockOnlyHost{Host: host}
	err = attachSessionPools(context.Background(), lockHost, pool.Engine(), unreachable)
	require.ErrorContains(t, err, "lock session pool")
	require.Nil(t, lockHost.locks)

	require.NoError(t, attachSessionPools(context.Background(), host, pool.Engine(), dsn),
		"the real DSN must still open both pools")
	require.NotSame(t, sqlDBOf(t, pool), host.RawDB())
}

// unreachableDSN points dsn at a port nothing listens on, keeping its user,
// database and options. A MySQL DSN names its address as tcp(host:port).
func unreachableDSN(t *testing.T, dsn string) string {
	t.Helper()
	if i := strings.Index(dsn, "tcp("); i >= 0 {
		j := strings.Index(dsn[i:], ")")
		require.Positive(t, j)
		return dsn[:i] + "tcp(127.0.0.1:1" + dsn[i+j:]
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Host = "127.0.0.1:1"
	return u.String()
}
