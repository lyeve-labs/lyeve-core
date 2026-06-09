package plugintest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

func uniqueTable(prefix string) string {
	return prefix + strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
}

func eraseByEmail(table string, called *bool) compliance.SubjectEraseFunc {
	return func(ctx context.Context, host core.Host, identifier string) (int64, error) {
		*called = true
		tag, err := host.Querier(ctx).Exec(ctx, `DELETE FROM `+table+` WHERE email = $1`, identifier)
		return tag.RowsAffected, err
	}
}

// A plugin that never ran here has no tables. Its eraser reports nothing
// erased rather than failing every erasure request with a driver error.
func TestDeclaredEraser_AbsentTablesEraseNothing(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		called := false
		e := compliance.NewDeclaredEraser("absent", host, compliance.SubjectErasure{
			Tables: []string{uniqueTable("never_people_")},
			Erase:  eraseByEmail("unused", &called),
		})
		require.NotNil(t, e)
		n, err := e.EraseSubject(context.Background(), "ada@example.com")
		require.NoError(t, err)
		assert.Zero(t, n)
		assert.False(t, called)
		assert.Equal(t, "absent", compliance.EraserName(e))
	})
}

func TestDeclaredEraser_PresentTablesErase(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		table := uniqueTable("erase_people_")
		_, err := q.Exec(ctx, `CREATE TABLE `+table+` (id VARCHAR(36) PRIMARY KEY, email VARCHAR(255) NOT NULL)`)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+table) })
		_, err = q.Exec(ctx, `INSERT INTO `+table+` (id, email) VALUES ('1', 'ada@example.com'), ('2', 'bob@example.com')`)
		require.NoError(t, err)

		called := false
		n := plugintest.RunSubjectErasures(t, host, erasingPlugin{erasures: []compliance.SubjectErasure{{
			Tables: []string{table},
			Erase:  eraseByEmail(table, &called),
		}}}, "ada@example.com")
		assert.True(t, called)
		assert.Equal(t, int64(1), n)

		row, err := q.QueryRow(ctx, `SELECT COUNT(*) FROM `+table)
		require.NoError(t, err)
		var left int
		require.NoError(t, row.Scan(&left))
		assert.Equal(t, 1, left, "another subject's row stays")
	})
}

// Where the plugin last ran before the migration that added the column the
// eraser keys by, the function would fail on its first statement, so the set
// is skipped. A set partly present reports an error naming what is missing.
func TestDeclaredEraser_ColumnsDecideWhetherItRuns(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		old := uniqueTable("erase_old_")
		cur := uniqueTable("erase_cur_")
		for _, ddl := range []string{
			`CREATE TABLE ` + old + ` (id VARCHAR(36) PRIMARY KEY)`,
			`CREATE TABLE ` + cur + ` (id VARCHAR(36) PRIMARY KEY, email VARCHAR(255))`,
		} {
			_, err := q.Exec(ctx, ddl)
			require.NoError(t, err)
		}
		t.Cleanup(func() {
			_, _ = q.Exec(context.Background(), `DROP TABLE `+old)
			_, _ = q.Exec(context.Background(), `DROP TABLE `+cur)
		})

		called := false
		unkeyed := compliance.NewDeclaredEraser("old", host, compliance.SubjectErasure{
			Columns: []string{old + ".email"},
			Erase:   eraseByEmail(old, &called),
		})
		require.NotNil(t, unkeyed, "a column's table belongs to the set")
		n, err := unkeyed.EraseSubject(ctx, "ada@example.com")
		require.NoError(t, err)
		assert.Zero(t, n)
		assert.False(t, called)

		// An older shape the plugin can still erase from runs Unkeyed.
		olderShape := false
		fallback := compliance.NewDeclaredEraser("old", host, compliance.SubjectErasure{
			Columns: []string{old + ".email"},
			Erase:   eraseByEmail(old, &called),
			Unkeyed: func(context.Context, core.Host, string) (int64, error) {
				olderShape = true
				return 3, nil
			},
		})
		n, err = fallback.EraseSubject(ctx, "ada@example.com")
		require.NoError(t, err)
		assert.Equal(t, int64(3), n)
		assert.True(t, olderShape)
		assert.False(t, called)

		partial := compliance.NewDeclaredEraser("mixed", host, compliance.SubjectErasure{
			Columns: []string{old + ".email", cur + ".email"},
			Erase:   eraseByEmail(cur, &called),
		})
		_, err = partial.EraseSubject(ctx, "ada@example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), old+".email")
		assert.False(t, called)
	})
}

type erasingPlugin struct{ erasures []compliance.SubjectErasure }

func (erasingPlugin) Name() string                           { return "erasing" }
func (erasingPlugin) Start(context.Context, core.Host) error { return nil }
func (erasingPlugin) Stop(context.Context) error             { return nil }

func (p erasingPlugin) SubjectErasures() []compliance.SubjectErasure { return p.erasures }
