package plugin

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPluginMigrate_InvalidTableName(t *testing.T) {
	// nil DB exercises only the validation guard, not the DB path
	cases := []struct {
		name      string
		tableName string
		wantErr   bool
	}{
		{"valid canonical", "plugin_content_schema_migrations", false},
		{"valid minimal", "m", false},
		{"valid max length", "a123456789_123456789_123456789_123456789_123456789_123456789_123", false}, // 63 chars
		{"valid underscores", "plugin_test_migrations", false},
		{"empty", "", true},
		{"starts with digit", "0plugin_migrations", true},
		{"contains hyphen", "plugin-migrations", true},
		{"contains space", "plugin migrations", true},
		{"contains semicolon", "plugin;drop", true},
		{"contains single quote", "plugin'migrations", true},
		{"too long", "a123456789_123456789_123456789_123456789_123456789_123456789_1234", true}, // 64 chars
		{"uppercase", "Plugin_Migrations", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// nil DB -> skips DB operations, but validation runs first
			err := PluginMigrate(context.Background(), nil, "postgres", fstest.MapFS{}, tc.tableName)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "invalid table name")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
