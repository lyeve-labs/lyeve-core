package core_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// uuidColumnType is the native UUID column for each engine. sys_users.id uses
// the same types, so a round trip here is the round trip every plugin store
// performs on a foreign key into it.
func uuidColumnType(dialect string) string {
	switch dialect {
	case "mysql":
		return "CHAR(36)"
	case "mssql":
		return "UNIQUEIDENTIFIER"
	default:
		return "UUID"
	}
}

// A uuid written and read back must be the same uuid. MSSQL returns
// UNIQUEIDENTIFIER in mixed-endian byte order, so a plain uuid.UUID
// destination reads back a different value that matches no row.
func TestScanUUID_RoundTripsOnEveryDialect(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		dialect := host.Dialect()
		table := "uuid_roundtrip_probe"

		_, err := host.Querier(ctx).Exec(ctx, fmt.Sprintf(
			"CREATE TABLE %s (id %s NOT NULL, ref %s NULL)", table,
			uuidColumnType(dialect), uuidColumnType(dialect)))
		require.NoError(t, err)

		id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
		ref := uuid.MustParse("d7e8f9a0-b1c2-4d3e-8f67-89abcdef0123")
		_, err = host.Querier(ctx).Exec(ctx,
			fmt.Sprintf("INSERT INTO %s (id, ref) VALUES ($1, $2)", table), id, ref)
		require.NoError(t, err)

		row, err := host.Querier(ctx).QueryRow(ctx,
			fmt.Sprintf("SELECT id, ref FROM %s WHERE id = $1", table), id)
		require.NoError(t, err)

		var gotID uuid.UUID
		var gotRef *uuid.UUID
		require.NoError(t, row.Scan(core.ScanUUID(dialect, &gotID), core.ScanNullUUID(dialect, &gotRef)))

		assert.Equal(t, id, gotID, "id must survive the round trip on %s", dialect)
		require.NotNil(t, gotRef)
		assert.Equal(t, ref, *gotRef, "nullable ref must survive the round trip on %s", dialect)

		// The decoded value has to be usable as a key again, which is the
		// failure the byte swap actually causes downstream.
		var n int
		countRow, err := host.Querier(ctx).QueryRow(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = $1", table), gotID)
		require.NoError(t, err)
		require.NoError(t, countRow.Scan(&n))
		assert.Equal(t, 1, n, "re-querying by the decoded id must find the row on %s", dialect)
	})
}

// A NULL column reads back as a nil pointer rather than an error or a zero uuid.
func TestScanNullUUID_NullColumnOnEveryDialect(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		dialect := host.Dialect()
		table := "uuid_null_probe"

		_, err := host.Querier(ctx).Exec(ctx, fmt.Sprintf(
			"CREATE TABLE %s (id %s NOT NULL, ref %s NULL)", table,
			uuidColumnType(dialect), uuidColumnType(dialect)))
		require.NoError(t, err)

		id := uuid.New()
		_, err = host.Querier(ctx).Exec(ctx,
			fmt.Sprintf("INSERT INTO %s (id, ref) VALUES ($1, NULL)", table), id)
		require.NoError(t, err)

		row, err := host.Querier(ctx).QueryRow(ctx,
			fmt.Sprintf("SELECT ref FROM %s WHERE id = $1", table), id)
		require.NoError(t, err)

		var gotRef *uuid.UUID
		require.NoError(t, row.Scan(core.ScanNullUUID(dialect, &gotRef)))
		assert.Nil(t, gotRef)
	})
}
