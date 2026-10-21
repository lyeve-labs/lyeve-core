package compliance_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// changeProbe creates a table with a note and a NULL-able memo, keyed by a
// UUID column when uuidKey is set and by CHAR(36) otherwise, and drops it
// after the test.
func changeProbe(t *testing.T, host core.Host, uuidKey bool) string {
	t.Helper()
	ctx := context.Background()
	q := host.Querier(ctx)
	table := "change_probe_" + uuid.NewString()[:8]
	text, key := "VARCHAR(255)", "CHAR(36)"
	switch {
	case host.Dialect() == "mssql" && uuidKey:
		text, key = "NVARCHAR(255)", "UNIQUEIDENTIFIER"
	case host.Dialect() == "mssql":
		text = "NVARCHAR(255)"
	case host.Dialect() == "postgres" && uuidKey:
		key = "UUID"
	}
	_, err := q.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (id %s PRIMARY KEY, grp INT NOT NULL, note %s NOT NULL, memo %s NULL)", table, key, text, text))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := q.Exec(context.Background(), "DROP TABLE "+table)
		assert.NoError(t, err)
	})
	return table
}

func cleared(t *testing.T, host core.Host, table string, grp int) (done, kept int) {
	t.Helper()
	ctx := context.Background()
	row, err := host.Querier(ctx).QueryRow(ctx,
		"SELECT COUNT(CASE WHEN note = '' THEN 1 END), COUNT(CASE WHEN note <> '' THEN 1 END) FROM "+table+" WHERE grp = $1", grp)
	require.NoError(t, err)
	require.NoError(t, row.Scan(&done, &kept))
	return done, kept
}

// A clear takes the rows Go confirmed and no other: a row edited after the
// read keeps its edit, even when its new text would pass the read again.
func TestChangeByKey_KeepsAnEditMadeAfterTheRead(t *testing.T) {
	t.Run("text key", func(t *testing.T) { keepsAnEditMadeAfterTheRead[string](t, false, "CHAR(36)") })
	t.Run("uuid key", func(t *testing.T) { keepsAnEditMadeAfterTheRead[uuid.UUID](t, true, "") })
}

func keepsAnEditMadeAfterTheRead[K string | uuid.UUID](t *testing.T, uuidKey bool, keyCast string) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		dialect := host.Dialect()
		table := changeProbe(t, host, uuidKey)

		ids := make([]string, 4)
		for i := range ids {
			ids[i] = uuid.NewString()
			memo := any(nil)
			if i%2 == 0 {
				memo = "for ann@x.example"
			}
			_, err := q.Exec(ctx, "INSERT INTO "+table+" (id, grp, note, memo) VALUES ($1, 1, $2, $3)", ids[i], "ann@x.example", memo)
			require.NoError(t, err)
		}

		rows, err := compliance.ConfirmedRows[K](ctx, q, dialect,
			"SELECT id, note, memo FROM "+table+" WHERE grp = $1", []any{1}, 2,
			func(texts []string) bool { return compliance.NamesAddress(texts[0], "ann@x.example") })
		require.NoError(t, err)
		require.Len(t, rows, 4)
		read := make([]string, len(rows))
		for i, row := range rows {
			read[i] = fmt.Sprint(row.Key)
		}
		assert.ElementsMatch(t, ids, read, "each key reads back as written")

		_, err = q.Exec(ctx, "UPDATE "+table+" SET note = $1 WHERE id = $2", "joann@x.example", ids[1])
		require.NoError(t, err)
		_, err = q.Exec(ctx, "UPDATE "+table+" SET memo = $1 WHERE id = $2", "edited", ids[2])
		require.NoError(t, err)

		n, err := compliance.ChangeByKey(ctx, q, dialect, compliance.KeyedChange{
			Change:  "UPDATE " + table + " SET note = $1, memo = NULL WHERE grp = $2 AND ",
			Args:    []any{"", 1},
			Key:     "id",
			KeyCast: keyCast,
			Columns: []string{"note", "memo"},
		}, rows)
		require.NoError(t, err)
		assert.EqualValues(t, 2, n, "the two rows nobody edited")
		done, kept := cleared(t, host, table, 1)
		assert.Equal(t, 2, done)
		assert.Equal(t, 2, kept, "an edited note and an edited memo both keep their edit")
	})
}

// One clear binds at most EraseParams parameters: with two text columns that
// is 666 rows, so 1000 rows take two statements and every one is cleared.
func TestChangeByKey_ClearsMoreRowsThanOneStatementBinds(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		dialect := host.Dialect()
		table := changeProbe(t, host, false)

		const total = 1000
		for range total {
			_, err := q.Exec(ctx, "INSERT INTO "+table+" (id, grp, note, memo) VALUES ($1, 2, $2, $3)",
				uuid.NewString(), "ann@x.example", "for ann@x.example")
			require.NoError(t, err)
		}
		rows, err := compliance.ConfirmedRows[string](ctx, q, dialect,
			"SELECT id, note, memo FROM "+table+" WHERE grp = $1", []any{2}, 2,
			func([]string) bool { return true })
		require.NoError(t, err)
		require.Len(t, rows, total)

		n, err := compliance.ChangeByKey(ctx, q, dialect, compliance.KeyedChange{
			Change:  "UPDATE " + table + " SET note = '', memo = NULL WHERE ",
			Key:     "id",
			KeyCast: "CHAR(36)",
			Columns: []string{"note", "memo"},
		}, rows)
		require.NoError(t, err)
		assert.EqualValues(t, total, n)
		done, kept := cleared(t, host, table, 2)
		assert.Equal(t, total, done)
		assert.Zero(t, kept)
	})
}

// MySQL's and SQL Server's binary collations pad with spaces, so an edit that
// only appends spaces is cleared there. PostgreSQL compares every byte.
func TestBinaryEquals_TrailingSpacesFollowTheCollation(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		table := changeProbe(t, host, false)
		_, err := q.Exec(ctx, "INSERT INTO "+table+" (id, grp, note) VALUES ($1, 3, $2)", uuid.NewString(), "ann@x.example  ")
		require.NoError(t, err)

		var n int
		row, err := q.QueryRow(ctx, "SELECT COUNT(*) FROM "+table+" WHERE grp = 3 AND "+
			compliance.BinaryEquals(host.Dialect(), "note", "$1"), "ann@x.example")
		require.NoError(t, err)
		require.NoError(t, row.Scan(&n))
		want := 1
		if host.Dialect() == "postgres" {
			want = 0
		}
		assert.Equal(t, want, n)
	})
}

// A change whose own arguments leave no room for one row's key and texts is
// refused before any statement runs. Before, the batch came out empty or the
// slice bound went negative, and the loop never ended or panicked.
func TestChangeByKey_RefusesAChangeThatCannotBindOneRow(t *testing.T) {
	rows := []compliance.ConfirmedRow[string]{{Key: uuid.NewString()}}
	for _, args := range []int{compliance.EraseParams - 1, compliance.EraseParams, compliance.EraseParams + 1} {
		c := compliance.KeyedChange{
			Change:  "UPDATE t SET note = '' WHERE ",
			Args:    make([]any, args),
			Key:     "id",
			Columns: []string{"note"},
		}
		var q core.Querier
		n, err := compliance.ChangeByKey(context.Background(), q, "postgres", c, rows)
		require.ErrorIs(t, err, compliance.ErrChangeTooWide, "%d arguments", args)
		assert.Zero(t, n)
	}
}
