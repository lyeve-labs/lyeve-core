package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A many_to_many whose pivot name passes 63 characters keeps its pairs in a
// table named by core.BoundedPivotTableName, which the schema engine creates
// and the content API reads, and one created before names were bounded keeps
// its full name.

const (
	lpSegments    = "campaign_audience_segment_groups"
	lpSubscribers = "newsletter_subscriber_profiles"
	kpWindows     = "contributor_assignment_windows"
	kpEntries     = "editorial_calendar_entries"
)

func init() {
	contentDefaultSchemas["longpivot"] = []string{`{
		"name": "` + lpSubscribers + `",
		"display_name": "Subscribers",
		"fields": [{"name": "name", "field_type": "text", "required": true}]
	}`, `{
		"name": "` + lpSegments + `",
		"display_name": "Segments",
		"fields": [
			{"name": "title", "field_type": "text", "required": true},
			{"name": "subscribers", "field_type": "relation", "relation_to": "` + lpSubscribers + `", "relation_type": "many_to_many"}
		]
	}`}
	contentDefaultSchemas["cutpivot"] = []string{`{
		"name": "` + lpSubscribers + `",
		"display_name": "Subscribers",
		"fields": [{"name": "name", "field_type": "text", "required": true}]
	}`, `{
		"name": "` + lpSegments + `",
		"display_name": "Segments",
		"fields": [{"name": "title", "field_type": "text", "required": true}]
	}`}
	contentDefaultSchemas["keptpivot"] = []string{`{
		"name": "` + kpWindows + `",
		"display_name": "Windows",
		"fields": [{"name": "name", "field_type": "text", "required": true}]
	}`, `{
		"name": "` + kpEntries + `",
		"display_name": "Entries",
		"fields": [{"name": "title", "field_type": "text", "required": true}]
	}`}
}

// dropAfter drops tables when the test ends, ahead of the fixture's own
// cleanup, which drops the types a pivot's keys hold in place.
func dropAfter(t *testing.T, pool db.DB, tables ...string) {
	t.Cleanup(func() {
		for _, table := range tables {
			_, _ = pool.Exec(context.Background(), "DROP TABLE "+table)
		}
	})
}

// tableCount reads how many rows a table holds, or -1 when it is not there.
func tableCount(t *testing.T, pool db.DB, table string) int {
	t.Helper()
	row, err := pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM "+table)
	if err != nil {
		return -1
	}
	var n int
	if err := row.Scan(&n); err != nil {
		return -1
	}
	return n
}

func (fx schemaContentFixture) linkAndList(t *testing.T, schema, id, field string, ids []string) []map[string]any {
	t.Helper()
	params := map[string]string{"schema": schema, "id": id, "field": field}
	target := "/api/v1/content/" + schema + "/" + id + "/relations/" + field
	rr := fx.send(t, fx.ctx, http.MethodPut, target, params, map[string]any{"ids": ids}, fx.content.SetRelations)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	rr = fx.send(t, fx.ctx, http.MethodGet, target, params, nil, fx.content.ListRelations)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), rr.Body.String())
	return out.Data
}

func TestContentLongPivot_SavesLinksPopulatesAndDeletes(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"longpivot"}, func(t *testing.T, fx schemaContentFixture) {
		natural := core.PivotTableName(lpSegments, lpSubscribers)
		bounded := core.BoundedPivotTableName(lpSegments, lpSubscribers)
		require.Greater(t, len(natural), core.PivotNameLimit)
		dropAfter(t, fx.pool, bounded)

		assert.Zero(t, tableCount(t, fx.pool, bounded), "the pivot is created under its bounded name")
		assert.Equal(t, -1, tableCount(t, fx.pool, natural), "no table under the full name")

		ada := createdID(t, fx.create(t, lpSubscribers, map[string]any{"name": "Ada"}))
		bob := createdID(t, fx.create(t, lpSubscribers, map[string]any{"name": "Bob"}))
		segment := createdID(t, fx.create(t, lpSegments, map[string]any{"title": "Weekly"}))

		listed := fx.linkAndList(t, lpSegments, segment, "subscribers", []string{ada, bob})
		assert.Len(t, listed, 2)
		assert.Equal(t, 2, tableCount(t, fx.pool, bounded))

		one := map[string]string{"schema": lpSegments, "id": segment}
		rr := fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/"+lpSegments+"/"+segment+"?populate=subscribers", one, nil, fx.content.Get)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		populated, ok := dataOf(t, rr)["subscribers"].([]any)
		require.True(t, ok, rr.Body.String())
		assert.Len(t, populated, 2)

		gone := map[string]string{"schema": lpSubscribers, "id": bob}
		rr = fx.send(t, fx.ctx, http.MethodDelete, "/api/v1/content/"+lpSubscribers+"/"+bob, gone, nil, fx.content.Delete)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
		assert.Equal(t, 1, tableCount(t, fx.pool, bounded), "deleting a target takes its pair with it")

		gone = map[string]string{"schema": lpSegments, "id": segment}
		rr = fx.send(t, fx.ctx, http.MethodDelete, "/api/v1/content/"+lpSegments+"/"+segment, gone, nil, fx.content.Delete)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
		assert.Zero(t, tableCount(t, fx.pool, bounded), "deleting the source takes its pairs with it")
	})
}

// A pivot an older install created under a full name past 63 characters,
// here 64, the most MySQL takes, is found under that name and never shadowed
// by a second table under the bounded one.
func TestContentLongPivot_KeepsAnExistingFullName(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"keptpivot"}, func(t *testing.T, fx schemaContentFixture) {
		natural := core.PivotTableName(kpEntries, kpWindows)
		bounded := core.BoundedPivotTableName(kpEntries, kpWindows)
		require.Len(t, natural, 64)

		uuidType := map[string]string{"postgres": "UUID", "mysql": "CHAR(36)", "mssql": "UNIQUEIDENTIFIER"}[fx.pool.Engine()]
		aCol, bCol := kpWindows+"_id", kpEntries+"_id"
		_, err := fx.pool.Exec(context.Background(), fmt.Sprintf(
			"CREATE TABLE %s (%s %s NOT NULL, %s %s NOT NULL, tenant_id VARCHAR(64), PRIMARY KEY (%s, %s))",
			natural, aCol, uuidType, bCol, uuidType, aCol, bCol))
		require.NoError(t, err)
		dropAfter(t, fx.pool, natural, bounded)

		withRelation := `{
			"name": "` + kpEntries + `",
			"display_name": "Entries",
			"fields": [
				{"name": "title", "field_type": "text", "required": true},
				{"name": "windows", "field_type": "relation", "relation_to": "` + kpWindows + `", "relation_type": "many_to_many"}
			]
		}`
		require.NoError(t, fx.eng.Apply(fx.ctx, kpEntries, json.RawMessage(withRelation)))
		assert.Equal(t, -1, tableCount(t, fx.pool, bounded), "no second table under the bounded name")

		window := createdID(t, fx.create(t, kpWindows, map[string]any{"name": "Morning"}))
		entry := createdID(t, fx.create(t, kpEntries, map[string]any{"title": "Launch"}))
		listed := fx.linkAndList(t, kpEntries, entry, "windows", []string{window})
		assert.Len(t, listed, 1)
		assert.Equal(t, 1, tableCount(t, fx.pool, natural), "the pair is in the table the install already had")
	})
}

// A table under the pair's full name that does not carry the pair's key
// columns is another relation's: on PostgreSQL, one whose name was cut to the
// same 63 bytes. The pair gets a table of its own under the bounded name.
// MySQL cannot hold a table under a 70 character name, so it has no such
// table to be misled by.
func TestContentLongPivot_LeavesAnotherPairsTableAlone(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"cutpivot"}, func(t *testing.T, fx schemaContentFixture) {
		if fx.pool.Engine() == "mysql" {
			t.Skip("MySQL refuses a table name past 64 characters")
		}
		natural := core.PivotTableName(lpSegments, lpSubscribers)
		bounded := core.BoundedPivotTableName(lpSegments, lpSubscribers)
		_, err := fx.pool.Exec(context.Background(), fmt.Sprintf(
			"CREATE TABLE %s (other_id VARCHAR(36) NOT NULL, tenant_id VARCHAR(64))", natural))
		require.NoError(t, err)
		dropAfter(t, fx.pool, bounded, natural)

		withRelation := `{
			"name": "` + lpSegments + `",
			"display_name": "Segments",
			"fields": [
				{"name": "title", "field_type": "text", "required": true},
				{"name": "subscribers", "field_type": "relation", "relation_to": "` + lpSubscribers + `", "relation_type": "many_to_many"}
			]
		}`
		require.NoError(t, fx.eng.Apply(fx.ctx, lpSegments, json.RawMessage(withRelation)))

		ada := createdID(t, fx.create(t, lpSubscribers, map[string]any{"name": "Ada"}))
		segment := createdID(t, fx.create(t, lpSegments, map[string]any{"title": "Weekly"}))
		assert.Len(t, fx.linkAndList(t, lpSegments, segment, "subscribers", []string{ada}), 1)
		assert.Equal(t, 1, tableCount(t, fx.pool, bounded), "the pair is in a table of its own")
		assert.Zero(t, tableCount(t, fx.pool, natural), "the other relation's table is untouched")
	})
}
