package enginehost

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A plugin that updates or deletes a content row through the host reaches
// the before_update and before_delete subscribers an update or a delete
// through the engine's own routes reaches, stamped with the plugin's name, so
// a subscriber that guards a content type guards it against every writer. A
// refusal leaves the row as it was. The content plugin's projection of an
// entry whose own write asked them asks nobody.
func TestPluginContentEdits_AskTheBeforeSubscribers(t *testing.T) {
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
			ctx := core.WithTenantID(context.Background(), "tenant-a")
			h := NewHost(pool, pool.SQLDB(), nil, hooks.NewRegistry(), "test").(*engineHost)
			engine := schemaengine.New(h)
			h.RegisterSchemaEngine(engine)
			const schema = "guarded_records"
			def, err := json.Marshal(core.Schema{Name: schema, Fields: []core.SchemaField{{Name: "title", FieldType: "text"}}})
			require.NoError(t, err)
			require.NoError(t, engine.Apply(ctx, schema, def))

			var mu sync.Mutex
			var seen []core.Event
			refuse := true
			guard := func(_ context.Context, e core.Event) error {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, e)
				if refuse {
					return errors.New("not here")
				}
				return nil
			}
			for _, et := range []core.EventType{core.BeforeUpdate, core.BeforeDelete} {
				sub := h.Hooks().Subscribe(schema, et, guard)
				defer sub.Unsubscribe()
			}
			events := func() []core.Event {
				mu.Lock()
				defer mu.Unlock()
				out := append([]core.Event(nil), seen...)
				seen = nil
				return out
			}
			title := func(id uuid.UUID) string {
				t.Helper()
				row, err := pool.QueryRow(context.Background(), `SELECT title FROM `+core.TableName(schema)+` WHERE id = $1`, id)
				require.NoError(t, err)
				var s string
				require.NoError(t, row.Scan(&s))
				return s
			}

			id := uuid.New()
			require.NoError(t, h.contentStore.UpsertContent(ctx, schema, id, map[string]any{"title": "kept"}))
			flow := core.NewScopedHost(h, "flow", core.CapAll)

			err = flow.UpdateContent(ctx, schema, id, map[string]any{"title": "rewritten"})
			require.ErrorIs(t, err, core.ErrUpdateRefused)
			assert.ErrorIs(t, err, core.ErrValidation)
			assert.Equal(t, "kept", title(id), "a refused update wrote the row")
			got := events()
			require.Len(t, got, 1)
			assert.Equal(t, core.BeforeUpdate, got[0].Type)
			assert.Equal(t, "flow", got[0].Plugin)
			assert.Equal(t, id.String(), got[0].RecordID)
			assert.Equal(t, "tenant-a", got[0].TenantID)
			assert.Equal(t, "rewritten", got[0].Data["title"])
			assert.Equal(t, "kept", got[0].OldData["title"])

			// An upsert that lands on an existing row is an update.
			err = flow.UpsertContent(ctx, schema, id, map[string]any{"title": "rewritten"})
			require.ErrorIs(t, err, core.ErrUpdateRefused)
			assert.Equal(t, "kept", title(id))
			got = events()
			require.Len(t, got, 1)
			assert.Equal(t, core.BeforeUpdate, got[0].Type)
			assert.Equal(t, "flow", got[0].Plugin)

			err = flow.DeleteContent(ctx, schema, id)
			require.ErrorIs(t, err, core.ErrDeleteRefused)
			assert.Equal(t, "kept", title(id), "a refused delete removed the row")
			got = events()
			require.Len(t, got, 1)
			assert.Equal(t, core.BeforeDelete, got[0].Type)
			assert.Equal(t, "flow", got[0].Plugin)
			assert.Equal(t, "kept", got[0].Data["title"])

			// The content plugin projects an entry whose own write asked.
			content := core.NewScopedHost(h, core.ContentPluginName, core.CapAll)
			require.NoError(t, content.UpdateContent(ctx, schema, id, map[string]any{"title": "projected"}))
			require.NoError(t, content.UpsertContent(ctx, schema, id, map[string]any{"title": "projected again"}))
			assert.Equal(t, "projected again", title(id))
			assert.Empty(t, events())

			// A subscriber that lets the write through sees it land.
			refuse = false
			require.NoError(t, flow.UpdateContent(ctx, schema, id, map[string]any{"title": "allowed"}))
			assert.Equal(t, "allowed", title(id))
			require.NoError(t, flow.DeleteContent(ctx, schema, id))
			assert.Len(t, events(), 2)

			// A row that is gone asks nobody: an update finds nothing and a
			// delete of it is the success it always was.
			require.ErrorIs(t, flow.UpdateContent(ctx, schema, id, map[string]any{"title": "late"}), core.ErrNotFound)
			require.NoError(t, flow.DeleteContent(ctx, schema, id))
			assert.Empty(t, events())

			gone := uuid.New()
			require.NoError(t, h.contentStore.UpsertContent(ctx, schema, gone, map[string]any{"title": "projected"}))
			require.NoError(t, content.DeleteContent(ctx, schema, gone))
			assert.Empty(t, events())
		})
	}
}
