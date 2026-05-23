//go:build !mutest

package core_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// probeDDL builds a table shaped like the plugin catalog tables that read
// InsertDoNothing's row count: a surrogate key, a tenant-scoped unique key,
// and a JSON payload. The JSON and timestamp columns are here on purpose,
// because they are where an insert that routes its values through a subquery
// stops agreeing with one that lists them in a VALUES clause.
func probeDDL(dialect, table string) string {
	switch dialect {
	case "mysql":
		return fmt.Sprintf(`CREATE TABLE %s (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			tenant_id VARCHAR(255) NOT NULL,
			entry_id VARCHAR(64) NOT NULL,
			locale VARCHAR(10) NOT NULL,
			body JSON NOT NULL,
			created_at DATETIME(6) NOT NULL,
			UNIQUE KEY %s_key (tenant_id, entry_id, locale)
		) ENGINE=InnoDB`, table, table)
	case "mssql":
		return fmt.Sprintf(`CREATE TABLE %s (
			id NVARCHAR(64) NOT NULL PRIMARY KEY,
			tenant_id NVARCHAR(255) NOT NULL,
			entry_id NVARCHAR(64) NOT NULL,
			locale NVARCHAR(10) NOT NULL,
			body NVARCHAR(MAX) NOT NULL,
			created_at DATETIME2(7) NOT NULL,
			CONSTRAINT %s_key UNIQUE (tenant_id, entry_id, locale)
		)`, table, table)
	default:
		return fmt.Sprintf(`CREATE TABLE %s (
			id TEXT NOT NULL PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			entry_id TEXT NOT NULL,
			locale TEXT NOT NULL,
			body JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT %s_key UNIQUE (tenant_id, entry_id, locale)
		)`, table, table)
	}
}

var probeCols = []string{"id", "tenant_id", "entry_id", "locale", "body", "created_at"}

var probeConflict = []string{"tenant_id", "entry_id", "locale"}

func probeArgs(id string) []any {
	return []any{id, "acme", "entry-1", "en", `{"k":"v"}`, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func probeCount(t *testing.T, ctx context.Context, host core.Host, table string) int {
	t.Helper()
	row, err := host.Querier(ctx).QueryRow(ctx, "SELECT COUNT(*) FROM "+table)
	require.NoError(t, err)
	var n int
	require.NoError(t, row.Scan(&n))
	return n
}

// A second insert of a row that is already there has to report that it wrote
// nothing. Callers across the codebase read that count as the only signal
// telling a fresh insert from a duplicate, and answer 409 on the strength of
// it.
//
// MySQL connects with CLIENT_FOUND_ROWS so that an idempotent UPDATE reports
// the row it matched rather than the zero rows it changed. That flag also
// makes ON DUPLICATE KEY UPDATE count a duplicate it left untouched, so an
// insert-or-ignore built on a self-assignment reports one row for a duplicate
// and one row for a fresh insert. Duplicate detection has to survive the flag.
func TestInsertDoNothing_DuplicateReportsNoRowInserted(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		table := "idn_dup_probe"

		_, err := host.Querier(ctx).Exec(ctx, probeDDL(host.Dialect(), table))
		require.NoError(t, err)

		stmt := core.InsertDoNothing(host.Dialect(), table, probeCols, probeConflict)

		tag, err := host.Querier(ctx).Exec(ctx, stmt, probeArgs("row-1")...)
		require.NoError(t, err)
		assert.Equal(t, int64(1), tag.RowsAffected, "a fresh insert must report one row on %s", host.Dialect())

		tag, err = host.Querier(ctx).Exec(ctx, stmt, probeArgs("row-1")...)
		require.NoError(t, err)
		assert.Equal(t, int64(0), tag.RowsAffected, "an identical duplicate must report zero rows on %s", host.Dialect())

		// The production shape: the caller mints a new surrogate id per
		// attempt, so only the unique key repeats.
		tag, err = host.Querier(ctx).Exec(ctx, stmt, probeArgs("row-2")...)
		require.NoError(t, err)
		assert.Equal(t, int64(0), tag.RowsAffected, "a duplicate unique key must report zero rows on %s", host.Dialect())

		assert.Equal(t, 1, probeCount(t, ctx, host, table), "the duplicate must not have been written on %s", host.Dialect())
	})
}

// The row that was already there keeps its own values. An insert-or-ignore
// that quietly upgraded itself to an upsert would overwrite the winner's
// payload with the loser's.
func TestInsertDoNothing_DuplicateLeavesTheStoredRowAlone(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		table := "idn_preserve_probe"

		_, err := host.Querier(ctx).Exec(ctx, probeDDL(host.Dialect(), table))
		require.NoError(t, err)

		stmt := core.InsertDoNothing(host.Dialect(), table, probeCols, probeConflict)
		_, err = host.Querier(ctx).Exec(ctx, stmt, probeArgs("original")...)
		require.NoError(t, err)

		later := []any{"replacement", "acme", "entry-1", "en", `{"k":"overwritten"}`, time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)}
		_, err = host.Querier(ctx).Exec(ctx, stmt, later...)
		require.NoError(t, err)

		row, err := host.Querier(ctx).QueryRow(ctx,
			"SELECT id FROM "+table+" WHERE tenant_id = $1 AND entry_id = $2 AND locale = $3",
			"acme", "entry-1", "en")
		require.NoError(t, err)
		var gotID string
		require.NoError(t, row.Scan(&gotID))
		assert.Equal(t, "original", gotID, "the stored row must survive the duplicate on %s", host.Dialect())
	})
}

// InsertIfAbsent is the contract callers actually want: it reports whether
// this call created the row, on every dialect, whatever the connection flags.
func TestInsertIfAbsent_ReportsCreatedThenAlreadyPresent(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		table := "idn_helper_probe"

		_, err := host.Querier(ctx).Exec(ctx, probeDDL(host.Dialect(), table))
		require.NoError(t, err)

		inserted, err := core.InsertIfAbsent(ctx, host.Querier(ctx), host.Dialect(), table,
			probeCols, probeConflict, probeArgs("first")...)
		require.NoError(t, err)
		assert.True(t, inserted, "the first call must report that it created the row on %s", host.Dialect())

		inserted, err = core.InsertIfAbsent(ctx, host.Querier(ctx), host.Dialect(), table,
			probeCols, probeConflict, probeArgs("second")...)
		require.NoError(t, err)
		assert.False(t, inserted, "the second call must report the row as already present on %s", host.Dialect())

		assert.Equal(t, 1, probeCount(t, ctx, host, table))
	})
}

// A write the database rejected is not a duplicate. InsertIfAbsent maps a
// duplicate key onto "already present", and nothing else: a NOT NULL violation
// still has to reach the caller, or lost data reads as a row someone else
// already wrote.
func TestInsertIfAbsent_RejectedWriteIsNotADuplicate(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		table := "idn_reject_probe"

		_, err := host.Querier(ctx).Exec(ctx, probeDDL(host.Dialect(), table))
		require.NoError(t, err)

		args := probeArgs("nulled")
		args[3] = nil // locale is NOT NULL

		inserted, err := core.InsertIfAbsent(ctx, host.Querier(ctx), host.Dialect(), table,
			probeCols, probeConflict, args...)
		require.Error(t, err, "a NOT NULL violation must reach the caller on %s", host.Dialect())
		assert.False(t, inserted)
		assert.Equal(t, 0, probeCount(t, ctx, host, table))
	})
}

// A key the statement does not probe for still comes back as "already
// present" rather than an error, which is the only reading that holds on all
// three engines: the row was not created, and what stopped it was a key
// someone else holds. This is also the path a MySQL caller takes when it loses
// the race to a concurrent writer, so it is worth pinning down on its own
// rather than waiting for a race to schedule itself.
func TestInsertIfAbsent_DuplicateOnAnotherUniqueKeyIsAlreadyPresent(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		table := "idn_second_key_probe"

		_, err := host.Querier(ctx).Exec(ctx, probeDDL(host.Dialect(), table))
		require.NoError(t, err)

		// id carries a unique key of its own, and InsertDoNothing is told
		// nothing about it.
		inserted, err := core.InsertIfAbsent(ctx, host.Querier(ctx), host.Dialect(), table,
			probeCols, probeConflict, probeArgs("shared-id")...)
		require.NoError(t, err)
		require.True(t, inserted)

		clash := probeArgs("shared-id")
		clash[3] = "fr" // a different conflict key, the same primary key

		inserted, err = core.InsertIfAbsent(ctx, host.Querier(ctx), host.Dialect(), table,
			probeCols, probeConflict, clash...)
		require.NoError(t, err, "a duplicate key must not reach the caller as an error on %s", host.Dialect())
		assert.False(t, inserted, "a duplicate key means the row was not created on %s", host.Dialect())

		assert.Equal(t, 1, probeCount(t, ctx, host, table))
	})
}

// Concurrent callers racing for the same key must agree on exactly one
// creator. On MySQL the emitted statement probes for the key and inserts in
// two steps, so a loser can find the key absent and still collide. The
// duplicate that comes back means the row exists, which is the same answer the
// probe would have given a moment later.
func TestInsertIfAbsent_ConcurrentCallersAgreeOnOneCreator(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		table := "idn_race_probe"

		_, err := host.Querier(ctx).Exec(ctx, probeDDL(host.Dialect(), table))
		require.NoError(t, err)

		const racers = 8
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			created  int
			failures []error
		)
		start := make(chan struct{})
		for i := range racers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				ok, err := core.InsertIfAbsent(ctx, host.Querier(ctx), host.Dialect(), table,
					probeCols, probeConflict, probeArgs(fmt.Sprintf("racer-%d", i))...)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failures = append(failures, err)
					return
				}
				if ok {
					created++
				}
			}(i)
		}
		close(start)
		wg.Wait()

		require.Empty(t, failures, "no racer may fail on %s", host.Dialect())
		assert.Equal(t, 1, created, "exactly one racer may report creating the row on %s", host.Dialect())
		assert.Equal(t, 1, probeCount(t, ctx, host, table))
	})
}
