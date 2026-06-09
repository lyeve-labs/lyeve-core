package plugintest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// A column carrying an index, a default and a unique constraint is the shape
// a tenant migration leaves, and SQL Server refuses to drop it until each of
// those is gone.
func TestDropColumn_RemovesAnIndexedColumnWithADefault(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		table := uniqueTable("drop_col_")
		for _, ddl := range []string{
			`CREATE TABLE ` + table + ` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(64) NOT NULL DEFAULT 'x', name VARCHAR(64) NOT NULL)`,
			`CREATE INDEX ` + table + `_tenant ON ` + table + ` (tenant_id)`,
			`ALTER TABLE ` + table + ` ADD CONSTRAINT ` + table + `_uq UNIQUE (tenant_id, name)`,
		} {
			_, err := q.Exec(ctx, ddl)
			require.NoError(t, err)
		}
		t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+table) })

		plugintest.DropColumn(t, host, table, "tenant_id")

		state, err := core.NewTableSet(nil, []string{table + ".tenant_id"}).Check(ctx, q, host.Dialect())
		require.NoError(t, err)
		assert.Equal(t, core.TableSetUnkeyed, state)
		_, err = q.Exec(ctx, `INSERT INTO `+table+` (id, name) VALUES ('1', 'a'), ('2', 'a')`)
		require.NoError(t, err, "the rest of the table works, and no narrowed unique key is left behind")
	})
}

// A composite index that leads with a foreign key column is the only index
// MySQL can use for that key, and MySQL refuses to drop it. The column still
// has to go, with the foreign key left working.
func TestDropColumn_KeepsTheIndexAForeignKeyNeeds(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		parent, child := uniqueTable("drop_fk_p_"), uniqueTable("drop_fk_c_")
		for _, ddl := range []string{
			`CREATE TABLE ` + parent + ` (id VARCHAR(36) PRIMARY KEY)`,
			`CREATE TABLE ` + child + ` (id VARCHAR(36) PRIMARY KEY, parent_id VARCHAR(36) NOT NULL, tenant_id VARCHAR(64) NOT NULL DEFAULT 'x')`,
			`CREATE INDEX ` + child + `_pt ON ` + child + ` (parent_id, tenant_id)`,
			`ALTER TABLE ` + child + ` ADD CONSTRAINT ` + child + `_fk FOREIGN KEY (parent_id) REFERENCES ` + parent + ` (id)`,
		} {
			_, err := q.Exec(ctx, ddl)
			require.NoError(t, err)
		}
		t.Cleanup(func() {
			_, _ = q.Exec(context.Background(), `DROP TABLE `+child)
			_, _ = q.Exec(context.Background(), `DROP TABLE `+parent)
		})

		plugintest.DropColumn(t, host, child, "tenant_id")

		state, err := core.NewTableSet(nil, []string{child + ".tenant_id"}).Check(ctx, q, host.Dialect())
		require.NoError(t, err)
		assert.Equal(t, core.TableSetUnkeyed, state)
		_, err = q.Exec(ctx, `INSERT INTO `+child+` (id, parent_id) VALUES ('1', 'missing')`)
		assert.Error(t, err, "the foreign key still holds")
	})
}
