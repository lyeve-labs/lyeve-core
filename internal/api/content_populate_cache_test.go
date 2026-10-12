package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// readAs sends a content read to the handler as a session holding one role.
func readAs(t *testing.T, fx schemaContentFixture, role, schema, id, query string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/content/" + schema + "/" + id
	if query != "" {
		path += "?" + query
	}
	claims := &auth.Claims{UserID: "00000000-0000-0000-0000-000000000002", Roles: []string{role}}
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r = chiCtx(r.WithContext(context.WithValue(fx.ctx, auth.ClaimsKey, claims)), map[string]string{"schema": schema, "id": id})
	rr := httptest.NewRecorder()
	fx.content.Get(rr, r)
	return rr
}

// relationIn reports whether field of the response's entry, or of the object
// at the path inside it, was served as a populated record.
func relationIn(t *testing.T, rr *httptest.ResponseRecorder, path ...string) bool {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	node := dataOf(t, rr)
	for _, step := range path[:len(path)-1] {
		next, ok := node[step].(map[string]any)
		require.True(t, ok, "%s was not populated: %s", step, rr.Body.String())
		node = next
	}
	_, populated := node[path[len(path)-1]].(map[string]any)
	return populated
}

// A relation populated for one caller belongs to that caller's response. The
// walk reads related rows through the item cache, and when it wrote into the
// cached row, the next caller to read that row was served the relation too,
// past its own rules. Here a role that may not read regions is the next
// caller, through a plain read of the office and through a populate that
// stops at the office.
func TestContentHandler_PopulateLeavesCachedRowsAsStored(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"chain"}, func(t *testing.T, fx schemaContentFixture) {
		regionID := createdID(t, fx.create(t, "regions", map[string]any{"name": "north", "secret": "hq code"}))
		officeID := createdID(t, fx.create(t, "offices", map[string]any{"name": "oslo", "region": regionID}))
		staffID := createdID(t, fx.create(t, "staff", map[string]any{"name": "ada", "office": officeID}))

		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{
				"reader":     {"staff", "offices", "regions"},
				"no-regions": {"staff", "offices"},
			},
			mask: map[string][]string{"regions": {"secret"}},
		}}

		// Two hops read the office through the batch path, which is the read
		// that fills the item cache, and fill in its region.
		require.True(t, relationIn(t, readAs(t, fx, "reader", "staff", staffID, "populate=office.region"), "office", "region"),
			"the reader's populate did not reach the region")

		for _, role := range []string{"reader", "no-regions"} {
			assert.False(t, relationIn(t, readAs(t, fx, role, "offices", officeID, ""), "region"),
				"%s: a plain read of the office was served the region another request populated", role)
		}

		rr := readAs(t, fx, "no-regions", "staff", staffID, "populate=office")
		assert.False(t, relationIn(t, rr, "office", "region"),
			"a role that may not read regions was served one through the office")
		assert.NotContains(t, rr.Body.String(), "north")

		// The same holds the other way round: the office now sits in the
		// cache, so this populate reads it on a hit.
		require.True(t, relationIn(t, readAs(t, fx, "reader", "staff", staffID, "populate=office.region"), "office", "region"))
		assert.False(t, relationIn(t, readAs(t, fx, "no-regions", "offices", officeID, ""), "region"),
			"a plain read of the office was served the region a populate wrote into the cached row")
	})
}

// Readers populating the same cached rows at once, to different depths. When
// the walk wrote into the cached row, one request filling in the office's
// region while another copied the office ended the process with a fatal
// concurrent map error, which no recover catches. Under -race the detector
// reports the first such pair. Every shallow response is checked as well, so
// the test also fails on a build without the detector, where the crash itself
// depends on timing.
func TestContentHandler_ConcurrentPopulateOfCachedRows(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"chain"}, func(t *testing.T, fx schemaContentFixture) {
		regionID := createdID(t, fx.create(t, "regions", map[string]any{"name": "north"}))
		officeID := createdID(t, fx.create(t, "offices", map[string]any{"name": "oslo", "region": regionID}))
		deepID := createdID(t, fx.create(t, "staff", map[string]any{"name": "ada", "office": officeID}))
		shallowID := createdID(t, fx.create(t, "staff", map[string]any{"name": "bo", "office": officeID}))

		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{"reader": {"staff", "offices", "regions"}},
		}}

		// One of each first, so every row sits in the item cache and the run
		// below reads memory alone.
		require.True(t, relationIn(t, readAs(t, fx, "reader", "staff", deepID, "populate=office.region"), "office", "region"))
		require.Equal(t, http.StatusOK, readAs(t, fx, "reader", "staff", shallowID, "populate=office").Code)

		const workers = 8
		const rounds = 150
		var failed, leaked atomic.Int64
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range rounds {
					id, query := deepID, "populate=office.region"
					if w%2 == 1 {
						id, query = shallowID, "populate=office"
					}
					rr := readAs(t, fx, "reader", "staff", id, query)
					var out struct {
						Data struct {
							Office map[string]any `json:"office"`
						} `json:"data"`
					}
					if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &out) != nil {
						failed.Add(1)
						continue
					}
					if _, populated := out.Data.Office["region"].(map[string]any); populated && w%2 == 1 {
						leaked.Add(1)
					}
				}
			}()
		}
		wg.Wait()

		assert.Zero(t, failed.Load(), "concurrent populates answered other than a 200 with an entry")
		assert.Zero(t, leaked.Load(), "populates that stop at the office were served the region a concurrent request filled in")
	})
}
