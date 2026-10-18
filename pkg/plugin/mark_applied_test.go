package plugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// lostRecord migrates a one-script tree and then deletes its bookkeeping row,
// which leaves the schema holding a change nothing records.
func lostRecord(t *testing.T, dialect, table string) (core.MigrationTree, string) {
	t.Helper()
	script := createTableSQL(dialect, table+"_items")
	return core.MigrationTree{Table: table, Files: scriptFS(dialect, map[string]string{"001_init": script})}, script
}

func TestMarkMigrationApplied_RecordsALostVersion(t *testing.T) {
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()
			tree, script := lostRecord(t, d.name, "mk_lost_migrations")
			require.NoError(t, PluginMigrate(ctx, db, d.name, tree.Files, tree.Table))
			_, err := db.ExecContext(ctx, "DELETE FROM mk_lost_migrations")
			require.NoError(t, err)

			require.NoError(t, MarkMigrationApplied(ctx, db, d.name, tree.Files, tree.Table, "001_init"))

			recorded, err := recordedVersions(ctx, db, tree.Table)
			require.NoError(t, err)
			assert.Equal(t, map[string]recordedVersion{
				"001_init": {completed: true, checksum: scriptChecksum(script)},
			}, recorded, "the row reads as if the script had run")
			assert.NoError(t, PluginMigrate(ctx, db, d.name, tree.Files, tree.Table),
				"the next boot skips the version instead of creating the table again")
		})
	}
}

func TestMarkMigrationApplied_RefusesAVersionOutsideTheTree(t *testing.T) {
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()
			tree, _ := lostRecord(t, d.name, "mk_outside_migrations")
			require.NoError(t, PluginMigrate(ctx, db, d.name, tree.Files, tree.Table))

			err := MarkMigrationApplied(ctx, db, d.name, tree.Files, tree.Table, "002_missing")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "002_missing")

			recorded, err := recordedVersions(ctx, db, tree.Table)
			require.NoError(t, err)
			assert.NotContains(t, recorded, "002_missing")
		})
	}
}

func TestMarkMigrationApplied_RefusesAVersionAlreadyApplied(t *testing.T) {
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()
			tree, _ := lostRecord(t, d.name, "mk_applied_migrations")
			require.NoError(t, PluginMigrate(ctx, db, d.name, tree.Files, tree.Table))

			err := MarkMigrationApplied(ctx, db, d.name, tree.Files, tree.Table, "001_init")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "already records 001_init")
		})
	}
}

func TestMarkMigrationApplied_RefusesWhileTheMigrationLockIsHeld(t *testing.T) {
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()
			tree, _ := lostRecord(t, d.name, "mk_locked_migrations")
			require.NoError(t, PluginMigrate(ctx, db, d.name, tree.Files, tree.Table))
			_, err := db.ExecContext(ctx, "DELETE FROM mk_locked_migrations")
			require.NoError(t, err)

			release, err := acquireMigrationLock(ctx, db, d.name)
			require.NoError(t, err)
			err = MarkMigrationApplied(ctx, db, d.name, tree.Files, tree.Table, "001_init")
			release()
			require.Error(t, err, "an engine migrating at the same moment would race the write")
			assert.Contains(t, err.Error(), "migrating")

			recorded, err := recordedVersions(ctx, db, tree.Table)
			require.NoError(t, err)
			assert.Empty(t, recorded, "a refused run writes nothing")

			assert.NoError(t, MarkMigrationApplied(ctx, db, d.name, tree.Files, tree.Table, "001_init"),
				"the same run goes through once the lock is free")
		})
	}
}

func TestMarkPluginMigrationApplied_RefusesAPluginItCannotResolve(t *testing.T) {
	ctx := context.Background()
	RegisterPlugin("mark-undeclared", func() core.Plugin { return newFakePlugin("mark-undeclared") })
	t.Cleanup(func() { UnregisterPlugin("mark-undeclared") })

	_, err := MarkPluginMigrationApplied(ctx, nil, "postgres", "mark-absent", "001_init")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not compiled into this binary")

	_, err = MarkPluginMigrationApplied(ctx, nil, "postgres", "mark-undeclared", "001_init")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not declare its migrations")
}
