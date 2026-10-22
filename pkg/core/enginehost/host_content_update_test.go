package enginehost

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A plugin that reads a row and then writes it can lose a race with a delete.
// The update-only write has to report the delete and leave the table as the
// delete left it: no row recreated under the old id holding only the patch,
// and no patch written into a soft-deleted row. The store's own update, which
// the Content API's PUT route runs, skips a soft-deleted row the same way.
func TestEngineHost_UpdateContentRefusesADeletedRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, name := range []string{"postgres", "mysql", "mssql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			var pool db.DB
			switch name {
			case "mysql":
				pool = testdb.MySQL(t)
			case "mssql":
				pool = testdb.MSSQL(t)
			default:
				pool = testdb.Postgres(t)
			}
			ctx := context.Background()
			h := NewHost(pool, pool.SQLDB(), nil, hooks.NewRegistry(), "test").(*engineHost)
			engine := schemaengine.New(h)
			h.RegisterSchemaEngine(engine)
			for _, sc := range []core.Schema{
				{Name: "hard_posts", Fields: []core.SchemaField{{Name: "title", FieldType: "text"}, {Name: "body", FieldType: "text"}}},
				{Name: "soft_posts", WithSoftDelete: true, Fields: []core.SchemaField{{Name: "title", FieldType: "text"}, {Name: "body", FieldType: "text"}}},
			} {
				def, err := json.Marshal(sc)
				require.NoError(t, err)
				require.NoError(t, engine.Apply(ctx, sc.Name, def))
			}

			id := uuid.New()
			require.NoError(t, h.UpsertContent(ctx, "hard_posts", id, map[string]any{"title": "kept", "body": "kept"}))
			require.NoError(t, h.UpdateContent(ctx, "hard_posts", id, map[string]any{"title": "patched"}))
			got, err := h.contentStore.GetByID(ctx, "hard_posts", id)
			require.NoError(t, err)
			require.Equal(t, "patched", got.Data["title"])
			require.Equal(t, "kept", got.Data["body"])

			require.NoError(t, h.DeleteContent(ctx, "hard_posts", id))
			require.ErrorIs(t, h.UpdateContent(ctx, "hard_posts", id, map[string]any{"title": "late"}), core.ErrNotFound)
			_, err = h.contentStore.GetByID(ctx, "hard_posts", id)
			require.ErrorIs(t, err, domain.ErrNotFound, "the update recreated the deleted row")

			id = uuid.New()
			require.NoError(t, h.UpsertContent(ctx, "soft_posts", id, map[string]any{"title": "kept"}))
			require.NoError(t, h.DeleteContent(ctx, "soft_posts", id))
			require.ErrorIs(t, h.UpdateContent(ctx, "soft_posts", id, map[string]any{"title": "late"}), core.ErrNotFound)
			_, err = h.contentStore.Update(ctx, "soft_posts", id, map[string]any{"title": "late"}, time.Time{})
			require.ErrorIs(t, err, db.ErrContentConflict, "the store's update matched a soft-deleted row")

			row, err := pool.QueryRow(ctx, `SELECT title FROM `+core.TableName("soft_posts")+` WHERE id = $1`, id)
			require.NoError(t, err)
			var title string
			require.NoError(t, row.Scan(&title))
			require.Equal(t, "kept", title, "the patch was written into the soft-deleted row")
		})
	}
}
