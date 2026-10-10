package enginehost

import (
	"context"
	"encoding/json"
	"errors"
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

// A plugin that keeps its own record of entries finds the rows another
// writer left in the table by paging through it. Every row has to come back
// once, drafts included, with its id on the row.
func TestEngineHost_ContentPageReadsEveryRowOnce(t *testing.T) {
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
			const schema = "paged_posts"
			def, err := json.Marshal(core.Schema{
				Name:             schema,
				WithDraftPublish: true,
				Fields:           []core.SchemaField{{Name: "title", FieldType: "text"}},
			})
			require.NoError(t, err)
			require.NoError(t, engine.Apply(ctx, schema, def))

			want := []string{}
			for _, title := range []string{"one", "two", "three"} {
				id := uuid.New()
				require.NoError(t, h.UpsertContent(ctx, schema, id, map[string]any{"title": title}))
				want = append(want, id.String())
			}
			draft := uuid.MustParse(want[1])
			require.NoError(t, h.ProjectContentStatus(ctx, schema, draft, core.ContentStatusDraft))

			var cr core.ContentReader = h
			var got []string
			statuses := map[string]any{}
			after := uuid.Nil
			for {
				page, err := cr.ContentPage(ctx, schema, after, 2)
				require.NoError(t, err)
				if len(page) == 0 {
					break
				}
				assert.LessOrEqual(t, len(page), 2)
				for _, row := range page {
					id := row["id"].(string)
					got = append(got, id)
					statuses[id] = row["_status"]
					assert.NotEmpty(t, row["title"])
					after = uuid.MustParse(id)
				}
			}
			// SQL Server orders a uniqueidentifier by its own byte groups, so the
			// order is the database's and only membership is portable.
			assert.ElementsMatch(t, want, got, "every row exactly once")
			assert.Equal(t, core.ContentStatusDraft, statuses[draft.String()], "a draft is read like any other row")

			_, err = cr.ContentPage(ctx, "never_defined", uuid.Nil, 10)
			assert.True(t, errors.Is(err, core.ErrNotFound), "an undefined schema is ErrNotFound, got %v", err)
		})
	}
}
