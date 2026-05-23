package core_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// The purge coverage check compares this list with the covered tables, so a
// table missing from it is one whose rows a tenant delete may leave behind,
// and nobody is told.
func TestListTenantScopedTables_ListsEveryTenantColumnOutsideTenantSchemas(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		dialect := host.Dialect()
		suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
		slugKeyed := "scoped_slug_" + suffix
		uuidKeyed := "scoped_uuid_" + suffix
		unscoped := "unscoped_" + suffix
		tenantSchema := "tenant_probe_" + suffix
		inTenantSchema := "inner_" + suffix

		uuidType := map[string]string{"postgres": "UUID", "mysql": "CHAR(36)", "mssql": "UNIQUEIDENTIFIER"}[dialect]
		require.NotEmpty(t, uuidType, "dialect %q", dialect)
		exec := func(stmt string) {
			t.Helper()
			_, err := q.Exec(ctx, stmt)
			require.NoError(t, err, stmt)
		}
		dropLater := func(stmt string) {
			t.Cleanup(func() { _, _ = q.Exec(context.Background(), stmt) })
		}

		exec(`CREATE TABLE ` + slugKeyed + ` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL)`)
		dropLater(`DROP TABLE ` + slugKeyed)
		exec(`CREATE TABLE ` + uuidKeyed + ` (id VARCHAR(36) PRIMARY KEY, tenant_id ` + uuidType + ` NOT NULL)`)
		dropLater(`DROP TABLE ` + uuidKeyed)
		exec(`CREATE TABLE ` + unscoped + ` (id VARCHAR(36) PRIMARY KEY, name VARCHAR(255) NOT NULL)`)
		dropLater(`DROP TABLE ` + unscoped)

		// A per-tenant schema is a database of its own on MySQL.
		if dialect == "mysql" {
			exec(`CREATE DATABASE ` + tenantSchema)
			dropLater(`DROP DATABASE ` + tenantSchema)
		} else {
			exec(`CREATE SCHEMA ` + tenantSchema)
			dropLater(`DROP SCHEMA ` + tenantSchema)
		}
		exec(`CREATE TABLE ` + tenantSchema + `.` + inTenantSchema + ` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL)`)
		dropLater(`DROP TABLE ` + tenantSchema + `.` + inTenantSchema)

		// A MySQL server can hold another install's database beside this
		// one, and information_schema there spans every database the server
		// has. That install's tables are not this engine's to purge.
		otherInstall := "other_install_" + suffix
		inOtherInstall := "elsewhere_" + suffix
		if dialect == "mysql" {
			exec(`CREATE DATABASE ` + otherInstall)
			dropLater(`DROP DATABASE ` + otherInstall)
			exec(`CREATE TABLE ` + otherInstall + `.` + inOtherInstall + ` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL)`)
		}

		tables, err := core.ListTenantScopedTables(ctx, q, dialect)
		require.NoError(t, err)
		assert.NotContains(t, tables, inOtherInstall, "a table in another database on the same server")
		assert.Contains(t, tables, slugKeyed, "a table keyed by the tenant slug")
		assert.Contains(t, tables, uuidKeyed, "a table keyed by a tenant UUID")
		assert.Contains(t, tables, "sys_users", "the engine's own tables")
		assert.NotContains(t, tables, unscoped, "a table with no tenant column")
		assert.NotContains(t, tables, inTenantSchema, "a table dropping the tenant's schema removes")
	})
}
