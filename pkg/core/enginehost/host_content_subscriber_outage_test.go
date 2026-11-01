package enginehost

import (
	"context"
	"encoding/json"
	"fmt"
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

// A before subscriber that cannot read its own store has not refused the
// write. Its failure reaches the plugin as the outage it is, so the plugin
// answers 503 and not the 422 of a refusal, and nothing is written.
func TestPluginContentWrites_SubscriberOutageIsNotARefusal(t *testing.T) {
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
			const schema = "guarded_outage"
			def, err := json.Marshal(core.Schema{Name: schema, Fields: []core.SchemaField{{Name: "title", FieldType: "text"}}})
			require.NoError(t, err)
			require.NoError(t, engine.Apply(ctx, schema, def))

			down := func(context.Context, core.Event) error {
				return fmt.Errorf("read the guard's rows: %w", core.ErrServiceUnavailable)
			}
			for _, et := range []core.EventType{core.BeforeCreate, core.BeforeUpdate, core.BeforeDelete} {
				sub := h.Hooks().Subscribe(schema, et, down)
				defer sub.Unsubscribe()
			}
			rows := func() map[string]string {
				t.Helper()
				r, err := pool.Query(context.Background(), `SELECT id, title FROM `+core.TableName(schema))
				require.NoError(t, err)
				defer r.Close()
				out := map[string]string{}
				for r.Next() {
					var id, title string
					require.NoError(t, r.Scan(&id, &title))
					out[id] = title
				}
				require.NoError(t, r.Err())
				return out
			}

			id := uuid.New()
			require.NoError(t, h.contentStore.UpsertContent(ctx, schema, id, map[string]any{"title": "kept"}))
			before := rows()
			flow := core.NewScopedHost(h, "flow", core.CapAll)

			for _, w := range []struct {
				name  string
				write func() error
			}{
				{"update", func() error { return flow.UpdateContent(ctx, schema, id, map[string]any{"title": "rewritten"}) }},
				{"upsert onto a row", func() error { return flow.UpsertContent(ctx, schema, id, map[string]any{"title": "rewritten"}) }},
				{"delete", func() error { return flow.DeleteContent(ctx, schema, id) }},
				{"create", func() error { return flow.UpsertContent(ctx, schema, uuid.New(), map[string]any{"title": "new"}) }},
			} {
				err := w.write()
				require.ErrorIs(t, err, core.ErrServiceUnavailable, w.name)
				assert.NotErrorIs(t, err, core.ErrValidation, "%s: an outage read as a refusal", w.name)
			}
			assert.Equal(t, before, rows(), "a write went through while its subscriber was down")
		})
	}
}
