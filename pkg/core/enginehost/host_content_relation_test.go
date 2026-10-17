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
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A plugin that writes content through the host, such as a flow's content
// node or the content plugin's projection, gets the refusal a REST write
// gets for a relation to another tenant's row, in a shape it can read:
// a validation error naming the field, the same for a row nobody holds.
func TestEngineHost_UpsertContentRefusesAnotherTenantsRelationTarget(t *testing.T) {
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
			h := NewHost(pool, pool.SQLDB(), nil, hooks.NewRegistry(), "test").(*engineHost)
			engine := schemaengine.New(h)
			h.RegisterSchemaEngine(engine)
			apply := func(sc core.Schema) {
				def, err := json.Marshal(sc)
				require.NoError(t, err)
				require.NoError(t, engine.Apply(context.Background(), sc.Name, def))
			}
			apply(core.Schema{Name: "hr_authors", Fields: []core.SchemaField{{Name: "name", FieldType: "text"}}})
			apply(core.Schema{Name: "hr_posts", Fields: []core.SchemaField{
				{Name: "title", FieldType: "text"},
				{Name: "author", FieldType: "relation", RelationType: core.RelBelongsTo, RelationTo: "hr_authors"},
			}})

			ours := core.WithTenantID(context.Background(), "t1")
			theirs := core.WithTenantID(context.Background(), "t2")
			ourAuthor, theirAuthor := uuid.New(), uuid.New()
			require.NoError(t, h.UpsertContent(ours, "hr_authors", ourAuthor, map[string]any{"name": "ours"}))
			require.NoError(t, h.UpsertContent(theirs, "hr_authors", theirAuthor, map[string]any{"name": "theirs"}))

			post := uuid.New()
			refused := func(target uuid.UUID) error {
				err := h.UpsertContent(ours, "hr_posts", post, map[string]any{"title": "p", "author": target.String()})
				var rel *core.RelationTargetError
				require.True(t, errors.As(err, &rel), "%v", err)
				assert.Equal(t, "author", rel.Field)
				assert.ErrorIs(t, err, core.ErrValidation)
				return err
			}
			theirErr, missingErr := refused(theirAuthor), refused(uuid.New())
			assert.Equal(t, missingErr.Error(), theirErr.Error())
			_, err := h.contentStore.GetByID(ours, "hr_posts", post)
			assert.ErrorIs(t, err, domain.ErrNotFound, "nothing was written")

			require.NoError(t, h.UpsertContent(ours, "hr_posts", post, map[string]any{"title": "p", "author": ourAuthor.String()}))
			require.NoError(t, h.UpsertContent(ours, "hr_posts", post, map[string]any{"title": "p2", "author": ourAuthor.String()}),
				"an update that keeps its relation is checked and accepted")
			refused(theirAuthor)
		})
	}
}
