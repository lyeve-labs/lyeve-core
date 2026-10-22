package enginehost

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// A plugin answers a failed write by the sentinel it can import. A status the
// store refuses has to reach it as core.ErrValidation, or the plugin answers
// a write that can never succeed as an outage and its caller retries it.
func TestEngineHost_UpsertContentInvalidStatusIsValidation(t *testing.T) {
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
			const schema = "mirrored_posts"
			def, err := json.Marshal(core.Schema{
				Name:             schema,
				WithDraftPublish: true,
				Fields:           []core.SchemaField{{Name: "title", FieldType: "text"}},
			})
			require.NoError(t, err)
			require.NoError(t, engine.Apply(ctx, schema, def))

			id := uuid.New()
			err = h.UpsertContent(ctx, schema, id, map[string]any{"title": "launch", "_status": "live"})
			require.ErrorIs(t, err, core.ErrValidation)
			assert.Equal(t, http.StatusBadRequest, httpx.StoreStatusFor(err))

			require.NoError(t, h.UpsertContent(ctx, schema, id, map[string]any{"title": "launch"}))
			err = h.UpsertContent(ctx, schema, id, map[string]any{"_status": "live"})
			assert.ErrorIs(t, err, core.ErrValidation, "an update the store refuses is a validation error too")

			assert.ErrorIs(t, h.UpsertContent(ctx, "never_defined", id, map[string]any{"title": "x"}), core.ErrNotFound)

			// A write the database never completes is still an outage the
			// caller retries, not the caller's mistake.
			gone, cancel := context.WithCancel(ctx)
			cancel()
			err = h.UpsertContent(gone, schema, id, map[string]any{"title": "retry"})
			require.Error(t, err)
			assert.NotErrorIs(t, err, core.ErrValidation)
			assert.Equal(t, http.StatusServiceUnavailable, httpx.StoreStatusFor(err))
		})
	}
}

// A write that reaches a soft-deleted row is racing its delete. The writer
// refuses it with a conflict the plugin can tell apart from an outage, and the
// row keeps its data and stays deleted. A row a hard delete removed is
// missing, so the same write inserts it.
func TestEngineHost_UpsertContentRefusesASoftDeletedRow(t *testing.T) {
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
				{Name: "hard_posts", Fields: []core.SchemaField{{Name: "title", FieldType: "text"}}},
				{Name: "soft_posts", WithSoftDelete: true, Fields: []core.SchemaField{{Name: "title", FieldType: "text"}}},
			} {
				def, err := json.Marshal(sc)
				require.NoError(t, err)
				require.NoError(t, engine.Apply(ctx, sc.Name, def))
			}

			id := uuid.New()
			require.NoError(t, h.UpsertContent(ctx, "soft_posts", id, map[string]any{"title": "kept"}))
			require.NoError(t, h.DeleteContent(ctx, "soft_posts", id))
			err := h.UpsertContent(ctx, "soft_posts", id, map[string]any{"title": "late"})
			require.ErrorIs(t, err, core.ErrConflict)
			assert.Equal(t, http.StatusConflict, httpx.StoreStatusFor(err))
			row, err := pool.QueryRow(ctx, `SELECT title, CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END FROM `+core.TableName("soft_posts")+` WHERE id = $1`, id)
			require.NoError(t, err)
			var title string
			var deleted int
			require.NoError(t, row.Scan(&title, &deleted))
			assert.Equal(t, "kept", title, "the write reached the deleted row")
			assert.Equal(t, 1, deleted, "the write brought the deleted row back")

			id = uuid.New()
			require.NoError(t, h.UpsertContent(ctx, "hard_posts", id, map[string]any{"title": "kept"}))
			require.NoError(t, h.DeleteContent(ctx, "hard_posts", id))
			require.NoError(t, h.UpsertContent(ctx, "hard_posts", id, map[string]any{"title": "again"}))
			got, err := h.contentStore.GetByID(ctx, "hard_posts", id)
			require.NoError(t, err)
			assert.Equal(t, "again", got.Data["title"])
		})
	}
}
