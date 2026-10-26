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

	_, err := MarkPluginMigrationApplied(ctx, nil, "postgres", "mark-absent", "001_init", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not compiled into this binary")

	_, err = MarkPluginMigrationApplied(ctx, nil, "postgres", "mark-undeclared", "001_init", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not declare its migrations")
}

// declaringPlugin declares its migration trees the way a shipped plugin does,
// from an instance that never starts.
type declaringPlugin struct {
	name  string
	trees []core.MigrationTree
}

func (p declaringPlugin) Name() string                         { return p.name }
func (declaringPlugin) Start(context.Context, core.Host) error { return nil }
func (declaringPlugin) Stop(context.Context) error             { return nil }
func (p declaringPlugin) MigrationTrees() []core.MigrationTree { return p.trees }

func registerDeclaringPlugin(t *testing.T, name string, trees ...core.MigrationTree) {
	t.Helper()
	RegisterPlugin(name, func() core.Plugin { return declaringPlugin{name: name, trees: trees} })
	t.Cleanup(func() { UnregisterPlugin(name) })
}

func TestMarkPluginMigrationApplied_MarksTheTreeThatShipsTheVersion(t *testing.T) {
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()
			lost, script := lostRecord(t, d.name, "mkp_lost_migrations")
			other := core.MigrationTree{
				Table: "mkp_other_migrations",
				Files: scriptFS(d.name, map[string]string{"001_other": createTableSQL(d.name, "mkp_other_items")}),
			}
			registerDeclaringPlugin(t, "mark-declared-"+d.name, other, lost)
			require.NoError(t, PluginMigrate(ctx, db, d.name, other.Files, other.Table))
			require.NoError(t, PluginMigrate(ctx, db, d.name, lost.Files, lost.Table))
			_, err := db.ExecContext(ctx, "DELETE FROM mkp_lost_migrations")
			require.NoError(t, err)

			table, err := MarkPluginMigrationApplied(ctx, db, d.name, "mark-declared-"+d.name, "001_init", "")
			require.NoError(t, err)
			assert.Equal(t, "mkp_lost_migrations", table, "the tree that ships the version is the one marked")

			recorded, err := recordedVersions(ctx, db, lost.Table)
			require.NoError(t, err)
			assert.Equal(t, map[string]recordedVersion{
				"001_init": {completed: true, checksum: scriptChecksum(script)},
			}, recorded)
			others, err := recordedVersions(ctx, db, other.Table)
			require.NoError(t, err)
			assert.NotContains(t, others, "001_init", "a tree that does not ship the version is left alone")
			assert.NoError(t, PluginMigrate(ctx, db, d.name, lost.Files, lost.Table),
				"the next boot skips the version instead of creating the table again")
		})
	}
}

func TestMarkPluginMigrationApplied_RefusesAVersionInMoreThanOneTree(t *testing.T) {
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()
			first, _ := lostRecord(t, d.name, "mkp_first_migrations")
			second, _ := lostRecord(t, d.name, "mkp_second_migrations")
			registerDeclaringPlugin(t, "mark-ambiguous-"+d.name, first, second)

			_, err := MarkPluginMigrationApplied(ctx, db, d.name, "mark-ambiguous-"+d.name, "001_init", "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "mkp_first_migrations, mkp_second_migrations")
			assert.Contains(t, err.Error(), "--table", "the refusal names the flag that resolves it")

			for _, table := range []string{first.Table, second.Table} {
				_, err := recordedVersions(ctx, db, table)
				assert.True(t, IsTableNotExistError(err), "a refused run writes nothing to %s: %v", table, err)
			}
		})
	}
}

// Two trees of one plugin can both ship 001, as a plugin with a store of its
// own beside a retention store does. Naming the table marks that tree and no
// other, and the boot failure for either table names the table.
func TestMarkPluginMigrationApplied_TheTableNamesOneOfTwoTreesWithTheVersion(t *testing.T) {
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()
			first, _ := lostRecord(t, d.name, "mkt_first_migrations")
			second, script := lostRecord(t, d.name, "mkt_second_migrations")
			name := "mark-table-" + d.name
			registerDeclaringPlugin(t, name, first, second)
			require.NoError(t, PluginMigrate(ctx, db, d.name, first.Files, first.Table))
			require.NoError(t, PluginMigrate(ctx, db, d.name, second.Files, second.Table))
			_, err := db.ExecContext(ctx, "DELETE FROM mkt_second_migrations")
			require.NoError(t, err)

			assert.Equal(t, "lyeve migrate mark-applied --plugin "+name+" --version 001_init --table mkt_second_migrations",
				markAppliedCommand(second.Table, "001_init"), "the boot failure names the table it stopped on")

			table, err := MarkPluginMigrationApplied(ctx, db, d.name, name, "001_init", second.Table)
			require.NoError(t, err)
			assert.Equal(t, second.Table, table)
			recorded, err := recordedVersions(ctx, db, second.Table)
			require.NoError(t, err)
			assert.Equal(t, map[string]recordedVersion{
				"001_init": {completed: true, checksum: scriptChecksum(script)},
			}, recorded)
			assert.NoError(t, PluginMigrate(ctx, db, d.name, second.Files, second.Table))

			_, err = MarkPluginMigrationApplied(ctx, db, d.name, name, "001_init", "mkt_unknown_migrations")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "declares no tree with the table mkt_unknown_migrations")
		})
	}
}

// A plugin with one tree keeps the short command.
func TestMarkAppliedCommand_OneTreeNeedsNoTable(t *testing.T) {
	tree, _ := lostRecord(t, "postgres", "mkt_single_migrations")
	registerDeclaringPlugin(t, "mark-single", tree)
	assert.Equal(t, "lyeve migrate mark-applied --plugin mark-single --version 001_init",
		markAppliedCommand(tree.Table, "001_init"))
}
