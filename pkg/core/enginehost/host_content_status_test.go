package enginehost

import (
	"context"
	"encoding/json"
	"sync/atomic"
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

// A plugin that announces its own status changes projects them through
// ProjectContentStatus, so the write has to land exactly where
// SetContentStatus writes and announce nothing. SetContentStatus keeps its
// one after_update per write.
func TestEngineHost_ProjectContentStatusWritesAndAnnouncesNothing(t *testing.T) {
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
			const schema = "projected_posts"
			def, err := json.Marshal(core.Schema{
				Name:             schema,
				WithDraftPublish: true,
				Fields:           []core.SchemaField{{Name: "title", FieldType: "text"}},
			})
			require.NoError(t, err)
			require.NoError(t, engine.Apply(ctx, schema, def))

			var updates atomic.Int32
			sub := h.Hooks().Subscribe("*", core.AfterUpdate, func(context.Context, core.Event) error {
				updates.Add(1)
				return nil
			})
			defer sub.Unsubscribe()

			id := uuid.New()
			require.NoError(t, h.UpsertContent(ctx, schema, id, map[string]any{"title": "launch"}))
			status := func() string {
				row, err := h.contentStore.GetByID(ctx, schema, id)
				require.NoError(t, err)
				return row.Data["_status"].(string)
			}

			var pj core.ContentStatusProjector = h
			require.NoError(t, pj.ProjectContentStatus(ctx, schema, id, core.ContentStatusDraft))
			assert.Equal(t, core.ContentStatusDraft, status())
			require.NoError(t, pj.ProjectContentStatus(ctx, schema, id, core.ContentStatusPublished))
			assert.Equal(t, core.ContentStatusPublished, status())
			assert.Zero(t, updates.Load(), "a projection announces nothing")

			require.NoError(t, h.SetContentStatus(ctx, schema, id, core.ContentStatusArchived))
			assert.Equal(t, core.ContentStatusArchived, status())
			assert.Equal(t, int32(1), updates.Load(), "SetContentStatus still announces its write once")

			assert.ErrorIs(t, pj.ProjectContentStatus(ctx, schema, uuid.New(), core.ContentStatusDraft), core.ErrNotFound)
			assert.ErrorIs(t, pj.ProjectContentStatus(ctx, schema, id, "live"), core.ErrValidation)
			assert.ErrorIs(t, pj.ProjectContentStatus(ctx, "never_defined", id, core.ContentStatusDraft), core.ErrNotFound)
		})
	}
}

// A plugin receives a scoped host, so the projection has to reach the engine
// through it.
func TestScopedHost_ForwardsProjectContentStatus(t *testing.T) {
	var pj core.ContentStatusProjector = core.NewScopedHost(&engineHost{}, "content", core.CapDBWrite)
	err := pj.ProjectContentStatus(context.Background(), "posts", uuid.New(), core.ContentStatusDraft)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "content writer not wired", "the call reached the engine host")
}
