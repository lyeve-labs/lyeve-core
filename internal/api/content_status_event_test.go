package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/hooks"
)

// Publishing or withdrawing an entry changes what a reader sees, so a
// subscriber that keeps a copy (a search index, a response cache, a webhook)
// has to hear about it the way it hears about any other update.
func TestContentHandler_StatusChangeFiresAfterUpdate(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"drafts"}, func(t *testing.T, fx schemaContentFixture) {
		id := createdID(t, fx.create(t, "drafts", map[string]any{"title": "First"}))

		var mu sync.Mutex
		var seen []hooks.Event
		fx.content.hooks.Register("drafts", hooks.AfterUpdate, func(_ context.Context, e hooks.Event) error {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, e)
			return nil
		})

		for _, step := range []struct {
			path    string
			handler func(http.ResponseWriter, *http.Request)
		}{
			{"publish", fx.content.Publish},
			{"unpublish", fx.content.Unpublish},
		} {
			r := httptest.NewRequest(http.MethodPut, "/api/v1/content/drafts/"+id+"/"+step.path, nil)
			r = chiCtx(r.WithContext(fx.ctx), map[string]string{"schema": "drafts", "id": id})
			rr := httptest.NewRecorder()
			step.handler(rr, r)
			require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
		}

		mu.Lock()
		defer mu.Unlock()
		require.Len(t, seen, 2, "publish and unpublish each fire one after-update event")
		for _, e := range seen {
			assert.Equal(t, "drafts", e.Schema)
		}
	})
}
