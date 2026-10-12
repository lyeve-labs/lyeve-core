package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A relation names a row by id, and the database checks only that some row
// holds that id: every tenant's rows share the target table, and its foreign
// key is not tenant scoped. A write that sets a relation is held to the
// caller's tenant, and a row another tenant holds is refused exactly as a row
// nobody holds, so the answer says nothing about other tenants.

// relationTenancyFixture is the essays set with rows planted in a second
// tenant.
type relationTenancyFixture struct {
	schemaContentFixture
	other context.Context

	ownAuthor, ownTopic                 string
	theirAuthor, theirTopic, theirEssay string
	nowhere                             string
}

func newRelationTenancyFixture(t *testing.T, fx schemaContentFixture) relationTenancyFixture {
	t.Helper()
	rf := relationTenancyFixture{
		schemaContentFixture: fx,
		other:                asSuperAdmin(tenant.WithID(context.Background(), "t2")),
		nowhere:              uuid.NewString(),
	}
	rf.ownAuthor = createdID(t, fx.create(t, "authors", map[string]any{"name": "Ours"}))
	rf.ownTopic = createdID(t, fx.create(t, "topics", map[string]any{"name": "Ours"}))
	rf.theirAuthor = createdID(t, rf.createAs(t, rf.other, "authors", map[string]any{"name": "Theirs"}))
	rf.theirTopic = createdID(t, rf.createAs(t, rf.other, "topics", map[string]any{"name": "Theirs"}))
	rf.theirEssay = createdID(t, rf.createAs(t, rf.other, "essays", map[string]any{"title": "Theirs"}))
	return rf
}

func (rf relationTenancyFixture) createAs(t *testing.T, ctx context.Context, schema string, data map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return rf.send(t, ctx, http.MethodPost, "/api/v1/content/"+schema, map[string]string{"schema": schema},
		map[string]any{"data": data}, rf.content.Create)
}

func (rf relationTenancyFixture) bulk(t *testing.T, schema string, items []map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return rf.send(t, rf.ctx, http.MethodPost, "/api/v1/content/"+schema+"/bulk", map[string]string{"schema": schema},
		map[string]any{"items": items}, rf.content.BulkCreate)
}

func (rf relationTenancyFixture) setRelations(t *testing.T, schema, id, field string, ids []string) *httptest.ResponseRecorder {
	t.Helper()
	return rf.send(t, rf.ctx, http.MethodPut, "/api/v1/content/"+schema+"/"+id+"/relations/"+field,
		map[string]string{"schema": schema, "id": id, "field": field}, map[string]any{"ids": ids}, rf.content.SetRelations)
}

// restoreNaming restores a revision of essay whose snapshot names author,
// through a handler whose revision store holds that snapshot.
func (rf relationTenancyFixture) restoreNaming(t *testing.T, essay, author string) *httptest.ResponseRecorder {
	t.Helper()
	revs := &memRevisions{}
	require.NoError(t, revs.SaveRecordRevision(rf.ctx, "essays", uuid.MustParse(essay),
		map[string]any{"title": "Restored", "author_id": author}, nil))
	h := *rf.content
	h.revisions = &revisionProvider{s: revs}
	return rf.restoreRevision(t, &h, "essays", essay, revs.rows[0].ID.String())
}

// essayAuthor reads the stored key of an essay through the caller's tenant.
func (rf relationTenancyFixture) essayAuthor(t *testing.T, essay string) any {
	t.Helper()
	got, err := rf.content.store.GetByID(rf.ctx, "essays", uuid.MustParse(essay))
	require.NoError(t, err)
	return got.Data["author_id"]
}

// essayTitles lists the titles of every essay the caller's tenant holds.
func (rf relationTenancyFixture) essayTitles(t *testing.T) []string {
	t.Helper()
	items, err := rf.content.store.List(rf.ctx, "essays", 100, 0, nil)
	require.NoError(t, err)
	var out []string
	for _, it := range items {
		out = append(out, it.Data["title"].(string))
	}
	return out
}

// pivotRows counts the pivot rows that name an essay, in any tenant, read
// from the table itself rather than through a join that would hide a row
// pointing at another tenant.
func (rf relationTenancyFixture) pivotRows(t *testing.T, essay string) int {
	t.Helper()
	row, err := rf.pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM "+core.PivotTableName("essays", "topics")+" WHERE essays_id = $1", essay)
	require.NoError(t, err)
	var n int
	require.NoError(t, row.Scan(&n))
	return n
}

// assertRelationRefused checks the refusal names the field and carries no
// detail of the row it was asked about.
func assertRelationRefused(t *testing.T, rr *httptest.ResponseRecorder, field string) {
	t.Helper()
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	var body struct {
		Errors []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
			Rule  string `json:"rule"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), rr.Body.String())
	require.Len(t, body.Errors, 1, rr.Body.String())
	assert.Equal(t, field, body.Errors[0].Field)
	assert.Equal(t, "VALIDATION_RELATION_TARGET", body.Errors[0].Code)
	assert.Equal(t, "relation", body.Errors[0].Rule)
}

func TestContentRelations_AnotherTenantsRowIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"essays"}, func(t *testing.T, fx schemaContentFixture) {
		rf := newRelationTenancyFixture(t, fx)

		t.Run("create under the field name", func(t *testing.T) {
			rr := rf.create(t, "essays", map[string]any{"title": "Linked", "author": rf.theirAuthor})
			assertRelationRefused(t, rr, "author")
			assert.NotContains(t, rf.essayTitles(t), "Linked")
		})

		t.Run("create under the key column", func(t *testing.T) {
			rr := rf.create(t, "essays", map[string]any{"title": "Keyed", "author_id": rf.theirAuthor})
			assertRelationRefused(t, rr, "author_id")
			assert.NotContains(t, rf.essayTitles(t), "Keyed")
		})

		t.Run("update", func(t *testing.T) {
			essay := createdID(t, rf.create(t, "essays", map[string]any{"title": "Unlinked"}))
			rr := rf.update(t, "essays", essay, map[string]any{"author": rf.theirAuthor})
			assertRelationRefused(t, rr, "author")
			assert.Nil(t, rf.essayAuthor(t, essay))
		})

		t.Run("bulk create", func(t *testing.T) {
			rr := rf.bulk(t, "essays", []map[string]any{
				{"title": "Bulk one"},
				{"title": "Bulk two", "author": rf.theirAuthor},
			})
			assertRelationRefused(t, rr, "author")
			titles := rf.essayTitles(t)
			assert.NotContains(t, titles, "Bulk one", "the batch lands whole or not at all")
			assert.NotContains(t, titles, "Bulk two")
		})

		t.Run("restore of a revision", func(t *testing.T) {
			essay := createdID(t, rf.create(t, "essays", map[string]any{"title": "Before"}))
			rr := rf.restoreNaming(t, essay, rf.theirAuthor)
			assertRelationRefused(t, rr, "author_id")
			assert.Nil(t, rf.essayAuthor(t, essay))
		})

		t.Run("many_to_many target", func(t *testing.T) {
			essay := createdID(t, rf.create(t, "essays", map[string]any{"title": "Tagged"}))
			rr := rf.setRelations(t, "essays", essay, "topics", []string{rf.ownTopic, rf.theirTopic})
			assertRelationRefused(t, rr, "topics")
			assert.Equal(t, 0, rf.pivotRows(t, essay), "nothing is linked when one target is refused")
		})

		t.Run("many_to_many source", func(t *testing.T) {
			rr := rf.setRelations(t, "essays", rf.theirEssay, "topics", []string{rf.ownTopic})
			require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
			assert.Equal(t, 0, rf.pivotRows(t, rf.theirEssay))
		})
	})
}

// The same request with a row another tenant holds and with an id nobody
// holds answers byte for byte the same, on every writer.
func TestContentRelations_AnotherTenantsRowAnswersAsAMissingRow(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"essays"}, func(t *testing.T, fx schemaContentFixture) {
		rf := newRelationTenancyFixture(t, fx)
		essay := createdID(t, rf.create(t, "essays", map[string]any{"title": "Mine"}))

		writers := map[string]func(id string) *httptest.ResponseRecorder{
			"create": func(id string) *httptest.ResponseRecorder {
				return rf.create(t, "essays", map[string]any{"title": "Probe", "author": id})
			},
			"update": func(id string) *httptest.ResponseRecorder {
				return rf.update(t, "essays", essay, map[string]any{"author_id": id})
			},
			"bulk create": func(id string) *httptest.ResponseRecorder {
				return rf.bulk(t, "essays", []map[string]any{{"title": "Probe", "author": id}})
			},
			"restore of a revision": func(id string) *httptest.ResponseRecorder {
				return rf.restoreNaming(t, essay, id)
			},
			"many_to_many target": func(id string) *httptest.ResponseRecorder {
				return rf.setRelations(t, "essays", essay, "topics", []string{id})
			},
		}
		probes := map[string]string{
			"create": rf.theirAuthor, "update": rf.theirAuthor, "bulk create": rf.theirAuthor,
			"restore of a revision": rf.theirAuthor, "many_to_many target": rf.theirTopic,
		}
		for name, write := range writers {
			t.Run(name, func(t *testing.T) {
				theirs := write(probes[name])
				missing := write(rf.nowhere)
				assert.Equal(t, missing.Code, theirs.Code)
				assert.Equal(t, strings.TrimSpace(missing.Body.String()), strings.TrimSpace(theirs.Body.String()))
				assert.Equal(t, http.StatusUnprocessableEntity, theirs.Code, theirs.Body.String())
			})
		}

		t.Run("many_to_many source", func(t *testing.T) {
			theirs := rf.setRelations(t, "essays", rf.theirEssay, "topics", []string{rf.ownTopic})
			missing := rf.setRelations(t, "essays", rf.nowhere, "topics", []string{rf.ownTopic})
			assert.Equal(t, missing.Code, theirs.Code)
			assert.Equal(t, strings.TrimSpace(missing.Body.String()), strings.TrimSpace(theirs.Body.String()))
			assert.Equal(t, http.StatusNotFound, theirs.Code, theirs.Body.String())
		})
	})
}

func TestContentRelations_OwnTenantsRowIsAccepted(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"essays"}, func(t *testing.T, fx schemaContentFixture) {
		rf := newRelationTenancyFixture(t, fx)
		same := func(t *testing.T, want string, got any) {
			t.Helper()
			s, _ := got.(string)
			assert.True(t, strings.EqualFold(want, s), "want %s, got %v", want, got)
		}

		rr := rf.create(t, "essays", map[string]any{"title": "Linked", "author": rf.ownAuthor})
		essay := createdID(t, rr)
		same(t, rf.ownAuthor, rf.essayAuthor(t, essay))

		other := createdID(t, rf.create(t, "authors", map[string]any{"name": "Also ours"}))
		rr = rf.update(t, "essays", essay, map[string]any{"author_id": other})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		same(t, other, rf.essayAuthor(t, essay))

		rr = rf.update(t, "essays", essay, map[string]any{"author": nil})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Nil(t, rf.essayAuthor(t, essay), "a null clears the relation and is not checked")

		rr = rf.bulk(t, "essays", []map[string]any{{"title": "Bulk", "author": rf.ownAuthor}})
		require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

		rr = rf.restoreNaming(t, essay, rf.ownAuthor)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		same(t, rf.ownAuthor, rf.essayAuthor(t, essay))

		rr = rf.setRelations(t, "essays", essay, "topics", []string{rf.ownTopic})
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
		assert.Equal(t, 1, rf.pivotRows(t, essay))
	})
}
