package plugin

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlaceholderFor(t *testing.T) {
	assert.Equal(t, "?", placeholderFor("mysql"))
	assert.Equal(t, "@p1", placeholderFor("mssql"))
	assert.Equal(t, "$1", placeholderFor("postgres"))
	assert.Equal(t, "$1", placeholderFor(""))
}

func TestPluginRollback_NilDB(t *testing.T) {
	ctx := context.Background()

	// Rollback with a nil DB is a documented no-op.
	require.NoError(t, PluginMigrateRollback(ctx, nil, "postgres", fstest.MapFS{}, "tbl", 3))

	// AppliedVersions with a nil DB yields an empty (non-nil) set.
	applied, err := PluginAppliedVersions(ctx, nil, "tbl")
	require.NoError(t, err)
	assert.Empty(t, applied)
}

func TestSetMigrationSigCache(t *testing.T) {
	orig := migrationSigCache
	t.Cleanup(func() { migrationSigCache = orig })

	SetMigrationSigCache(nil) // documented: nil disables the cache
	assert.Nil(t, migrationSigCache)
}
