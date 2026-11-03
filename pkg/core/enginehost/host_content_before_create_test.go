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

// A plugin that writes a content table through the host reaches the
// before_create subscribers a create through the engine's own routes reaches,
// so a subscriber that guards a content type guards it against every writer.
// An update of a row that exists asks no before_create subscriber, and the
// content plugin's projection of an entry it created asks nobody.
func TestUpsertContent_PublishesBeforeCreate(t *testing.T) {
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
			const schema = "guarded_pages"
			def, err := json.Marshal(core.Schema{Name: schema, Fields: []core.SchemaField{{Name: "title", FieldType: "text"}}})
			require.NoError(t, err)
			require.NoError(t, engine.Apply(ctx, schema, def))

			var mu sync.Mutex
			var seen []core.Event
			refuse := true
			sub := h.Hooks().Subscribe(schema, core.BeforeCreate, func(_ context.Context, e core.Event) error {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, e)
				if refuse {
					return errors.New("not here")
				}
				return nil
			})
			defer sub.Unsubscribe()
			events := func() []core.Event {
				mu.Lock()
				defer mu.Unlock()
				out := append([]core.Event(nil), seen...)
				seen = nil
				return out
			}
			rows := func(id uuid.UUID) int {
				t.Helper()
				row, err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM `+core.TableName(schema)+` WHERE id = $1`, id)
				require.NoError(t, err)
				var n int
				require.NoError(t, row.Scan(&n))
				return n
			}

			flow := core.NewScopedHost(h, "flow", core.CapAll)
			id := uuid.New()
			err = flow.UpsertContent(ctx, schema, id, map[string]any{"title": "outside"})
			require.ErrorIs(t, err, core.ErrCreateRefused)
			assert.ErrorIs(t, err, core.ErrValidation)
			assert.Equal(t, 0, rows(id), "a refused insert wrote a row")
			got := events()
			require.Len(t, got, 1)
			assert.Equal(t, "flow", got[0].Plugin)
			assert.Equal(t, core.SourceEngine, got[0].Source)
			assert.Equal(t, id.String(), got[0].RecordID)
			assert.Equal(t, "tenant-a", got[0].TenantID)

			// Another tenant's row under the same id fails the update and
			// falls back to the insert, which asks the subscribers before it
			// meets the key the other tenant holds.
			refuse = false
			taken := uuid.New()
			require.NoError(t, flow.UpsertContent(ctx, schema, taken, map[string]any{"title": "a's"}))
			require.Len(t, events(), 1)
			refuse = true
			other := core.WithTenantID(context.Background(), "tenant-b")
			err = flow.UpsertContent(other, schema, taken, map[string]any{"title": "b's"})
			require.ErrorIs(t, err, core.ErrCreateRefused, "the fallback insert did not ask the subscribers")
			assert.Len(t, events(), 1)

			// An update of a row that exists creates nothing and asks no
			// before_create subscriber.
			require.NoError(t, flow.UpsertContent(ctx, schema, taken, map[string]any{"title": "edited"}))
			assert.Empty(t, events())

			// The content plugin's projection asks nobody and is not refused.
			content := core.NewScopedHost(h, core.ContentPluginName, core.CapAll)
			projected := uuid.New()
			require.NoError(t, content.ProjectContent(ctx, schema, projected, map[string]any{"title": "projected"}))
			assert.Equal(t, 1, rows(projected))
			assert.Empty(t, events())

			// The content plugin's own upsert asks, and names it.
			err = content.UpsertContent(ctx, schema, uuid.New(), map[string]any{"title": "created"})
			require.ErrorIs(t, err, core.ErrCreateRefused)
			got = events()
			require.Len(t, got, 1)
			assert.Equal(t, core.ContentPluginName, got[0].Plugin)
		})
	}
}
