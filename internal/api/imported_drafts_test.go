package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/schemaimport"
)

// A WordPress post type imports with draft and publish, so a draft post
// written into it the way the plan's notes say stays out of a public read,
// and a published one is served.
func TestContentHandler_ImportedWordPressDraftStaysPrivate(t *testing.T) {
	forEachDialectWithSchemas(t, nil, func(t *testing.T, fx schemaContentFixture) {
		conv, err := schemaimport.FromWordPress([]byte(`[{"title":"Post Fields",
		  "location":[[{"param":"post_type","value":"wp_post"}]],
		  "fields":[{"name":"subtitle","type":"text"}]}]`))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = fx.pool.Exec(context.Background(), "DROP TABLE "+domain.TableName("wp_post"))
		})
		_, err = schemaimport.ApplyBundle(fx.ctx, fx.eng, conv.Bundle)
		require.NoError(t, err)

		fx.content.perms = fakeProvider{writerOnlyChecker{}}
		params := map[string]string{"schema": "wp_post"}
		// Each post carries its slug: the reference engine declares the
		// unique slug as a plain constraint, which SQL Server holds to one
		// NULL.
		write := func(slug, status string) string {
			rr := fx.send(t, fx.ctx, http.MethodPost, "/api/v1/content/wp_post", params,
				map[string]any{"data": map[string]any{"title": slug, "slug": slug, "_status": status}}, fx.content.Create)
			return createdID(t, rr)
		}
		draft := write("a-draft-post", "draft")
		published := write("a-published-post", "published")

		got := fx.send(t, asReader(fx.ctx), http.MethodGet, "/api/v1/content/wp_post", params, nil, fx.content.List)
		ids := listedIDs(t, got)
		assert.NotContains(t, ids, draft, "an imported draft post is public")
		assert.Contains(t, ids, published, "an imported published post is hidden")
	})
}
