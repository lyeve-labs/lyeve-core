package schemaimport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// noteFor returns the message recorded against one field, or "".
func noteFor(conv *Conversion, schema, field string) string {
	for _, n := range conv.Notes {
		if n.Schema == schema && n.Field == field {
			return n.Message
		}
	}
	return ""
}

// Strapi

func TestFromStrapi_MapsAttributeTypes(t *testing.T) {
	conv, err := FromStrapi([]byte(`{
	  "kind": "collectionType",
	  "collectionName": "articles",
	  "info": {"singularName": "article", "displayName": "Article"},
	  "attributes": {
	    "title":     {"type": "string", "required": true},
	    "body":      {"type": "richtext"},
	    "contact":   {"type": "email"},
	    "slug":      {"type": "uid"},
	    "views":     {"type": "integer"},
	    "price":     {"type": "decimal"},
	    "live":      {"type": "boolean"},
	    "born":      {"type": "date"},
	    "seen":      {"type": "datetime"},
	    "payload":   {"type": "json"},
	    "cover":     {"type": "media"},
	    "kind":      {"type": "enumeration"}
	  }
	}`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "article")
	assert.Equal(t, "Article", s.DisplayName)

	want := map[string]string{
		"title": "text", "body": "rich_text", "contact": "email", "slug": "text",
		"views": "number", "price": "number", "live": "boolean", "born": "date",
		"seen": "datetime", "payload": "json", "cover": "media", "kind": "text",
	}
	for name, fieldType := range want {
		assert.Equal(t, fieldType, fieldByName(t, s, name).FieldType, "attribute %q", name)
	}
	assert.True(t, fieldByName(t, s, "title").Required)
	assert.True(t, fieldByName(t, s, "slug").Unique, "a Strapi uid is a unique slug")
	assert.Contains(t, noteFor(conv, "article", "kind"), "not enforced by the column")
}

func TestFromStrapi_MapsRelationCardinality(t *testing.T) {
	conv, err := FromStrapi([]byte(`[
	  {"info":{"singularName":"article"},"attributes":{
	     "author":   {"type":"relation","relation":"manyToOne","target":"api::person.person"},
	     "cover":    {"type":"relation","relation":"oneToOne","target":"api::person.person"},
	     "comments": {"type":"relation","relation":"oneToMany","target":"api::person.person"},
	     "tags":     {"type":"relation","relation":"manyToMany","target":"api::person.person"}
	  }},
	  {"info":{"singularName":"person"},"attributes":{"name":{"type":"string"}}}
	]`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "article")
	assert.Equal(t, domain.RelBelongsTo, fieldByName(t, s, "author").RelationType)
	assert.Equal(t, domain.RelBelongsTo, fieldByName(t, s, "cover").RelationType)
	assert.Equal(t, domain.RelHasMany, fieldByName(t, s, "comments").RelationType)
	assert.Equal(t, domain.RelManyToMany, fieldByName(t, s, "tags").RelationType)
	assert.Equal(t, "person", fieldByName(t, s, "author").RelationTo, "the target UID reduces to a name")
}

func TestFromStrapi_DropsPasswords(t *testing.T) {
	conv, err := FromStrapi([]byte(`{"info":{"singularName":"account"},"attributes":{
	   "email":{"type":"email"},"password":{"type":"password"}}}`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "account")
	assert.NotContains(t, fieldNames(s), "password")
	assert.Contains(t, noteFor(conv, "account", "password"), "not carried between systems")
}

func TestFromStrapi_ReportsComponentsAndDynamicZones(t *testing.T) {
	conv, err := FromStrapi([]byte(`{"info":{"singularName":"page"},"attributes":{
	   "hero":   {"type":"component","component":"blocks.hero"},
	   "blocks": {"type":"dynamiczone"}}}`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "page")
	assert.Equal(t, "json", fieldByName(t, s, "hero").FieldType)
	assert.Equal(t, "json", fieldByName(t, s, "blocks").FieldType)
	assert.Contains(t, noteFor(conv, "page", "hero"), "not as related tables")
	assert.Contains(t, noteFor(conv, "page", "blocks"), "no fixed shape")
}

func TestFromStrapi_SingleTypeIsReported(t *testing.T) {
	// This engine has no single-type constraint, so importing one silently
	// would leave the operator expecting a rule that is not there.
	conv, err := FromStrapi([]byte(`{"kind":"singleType","info":{"singularName":"homepage"},
	   "attributes":{"title":{"type":"string"}}}`))
	require.NoError(t, err)
	assert.Contains(t, noteFor(conv, "homepage", ""), "limit it to one entry")
}

func TestFromStrapi_RejectsUnrecognizedInput(t *testing.T) {
	_, err := FromStrapi([]byte(`"just a string"`))
	require.Error(t, err)
}

func TestStrapiTarget(t *testing.T) {
	assert.Equal(t, "person", strapiTarget("api::person.person"))
	assert.Equal(t, "author", strapiTarget("api::blog.author"))
	assert.Equal(t, "person", strapiTarget("person"))
	assert.Equal(t, "", strapiTarget(""))
}

// Contentful

func TestFromContentful_MapsFieldTypes(t *testing.T) {
	conv, err := FromContentful([]byte(`{"contentTypes":[{
	  "sys": {"id": "article"},
	  "name": "Article",
	  "fields": [
	    {"id":"title","type":"Symbol","required":true},
	    {"id":"body","type":"Text"},
	    {"id":"rich","type":"RichText"},
	    {"id":"views","type":"Integer"},
	    {"id":"score","type":"Number"},
	    {"id":"live","type":"Boolean"},
	    {"id":"seen","type":"Date"},
	    {"id":"meta","type":"Object"},
	    {"id":"place","type":"Location"},
	    {"id":"cover","type":"Link","linkType":"Asset"}
	  ]}]}`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "article")
	want := map[string]string{
		"title": "text", "body": "rich_text", "rich": "json", "views": "number",
		"score": "number", "live": "boolean", "seen": "datetime",
		"meta": "json", "place": "json", "cover": "media",
	}
	for name, fieldType := range want {
		assert.Equal(t, fieldType, fieldByName(t, s, name).FieldType, "field %q", name)
	}
	assert.True(t, fieldByName(t, s, "title").Required)
}

func TestFromContentful_LinksBecomeRelations(t *testing.T) {
	conv, err := FromContentful([]byte(`[
	  {"sys":{"id":"article"},"fields":[
	    {"id":"author","type":"Link","linkType":"Entry",
	     "validations":[{"linkContentType":["person"]}]},
	    {"id":"tags","type":"Array","items":{"type":"Link","linkType":"Entry",
	     "validations":[{"linkContentType":["person"]}]}}
	  ]},
	  {"sys":{"id":"person"},"fields":[{"id":"name","type":"Symbol"}]}
	]`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "article")
	assert.Equal(t, domain.RelBelongsTo, fieldByName(t, s, "author").RelationType)
	assert.Equal(t, "person", fieldByName(t, s, "author").RelationTo)
	assert.Equal(t, domain.RelManyToMany, fieldByName(t, s, "tags").RelationType)
}

func TestFromContentful_AmbiguousLinkIsReported(t *testing.T) {
	// A link accepting several content types has no single relation target.
	conv, err := FromContentful([]byte(`[
	  {"sys":{"id":"article"},"fields":[
	    {"id":"related","type":"Link","linkType":"Entry",
	     "validations":[{"linkContentType":["person","company"]}]}]},
	  {"sys":{"id":"person"},"fields":[{"id":"name","type":"Symbol"}]},
	  {"sys":{"id":"company"},"fields":[{"id":"name","type":"Symbol"}]}
	]`))
	require.NoError(t, err)

	assert.Equal(t, "json", fieldByName(t, schemaByName(t, conv.Bundle, "article"), "related").FieldType)
	assert.Contains(t, noteFor(conv, "article", "related"), "several content types")
}

func TestFromContentful_LocalizedTurnsOnLocalization(t *testing.T) {
	conv, err := FromContentful([]byte(`[{"sys":{"id":"article"},"fields":[
	   {"id":"title","type":"Symbol","localized":true}]}]`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "article")
	assert.True(t, s.WithLocalization)
	assert.Contains(t, noteFor(conv, "article", ""), "one row per locale")
}

// Contentful marks translation per field, and so does this engine. A field
// it cannot translate keeps its value and loses the mark, with a note naming
// why, because one refused field would otherwise fail the whole import.
func TestFromContentful_LocalizedMarksTheField(t *testing.T) {
	conv, err := FromContentful([]byte(`[{"sys":{"id":"article"},"fields":[
	   {"id":"title","type":"Symbol","localized":true},
	   {"id":"body","type":"Text","localized":true},
	   {"id":"slug","type":"Symbol","localized":true,"validations":[{"unique":true}]},
	   {"id":"rank","type":"Integer","localized":true},
	   {"id":"summary","type":"Symbol"}]}]`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "article")
	assert.True(t, fieldByName(t, s, "title").Localized)
	assert.True(t, fieldByName(t, s, "body").Localized)
	assert.False(t, fieldByName(t, s, "summary").Localized)

	assert.False(t, fieldByName(t, s, "slug").Localized)
	assert.Contains(t, noteFor(conv, "article", "slug"), "unique")
	assert.False(t, fieldByName(t, s, "rank").Localized)
	assert.Contains(t, noteFor(conv, "article", "rank"), "number")

	require.NoError(t, domain.ValidateSchema(&s), "the converted definition must pass the validator it is applied through")
}

func TestFromContentful_OmittedFieldsAreSkipped(t *testing.T) {
	conv, err := FromContentful([]byte(`[{"sys":{"id":"article"},"fields":[
	   {"id":"title","type":"Symbol"},
	   {"id":"legacy","type":"Symbol","omitted":true}]}]`))
	require.NoError(t, err)

	assert.NotContains(t, fieldNames(schemaByName(t, conv.Bundle, "article")), "legacy")
}

func TestFromContentful_UniqueValidationCarriesOver(t *testing.T) {
	conv, err := FromContentful([]byte(`[{"sys":{"id":"article"},"fields":[
	   {"id":"slug","type":"Symbol","validations":[{"unique":true}]}]}]`))
	require.NoError(t, err)

	assert.True(t, fieldByName(t, schemaByName(t, conv.Bundle, "article"), "slug").Unique)
}

// WordPress

func TestFromWordPress_AddsCoreFieldsAndACFFields(t *testing.T) {
	conv, err := FromWordPress([]byte(`[{
	  "key": "group_1",
	  "title": "Article Fields",
	  "location": [[{"param":"post_type","operator":"==","value":"article"}]],
	  "fields": [
	    {"name":"subtitle","type":"text","required":1},
	    {"name":"summary","type":"textarea"},
	    {"name":"details","type":"wysiwyg"},
	    {"name":"rank","type":"number"},
	    {"name":"featured","type":"true_false"},
	    {"name":"contact","type":"email"},
	    {"name":"hero","type":"image"},
	    {"name":"event_on","type":"date_picker"},
	    {"name":"shade","type":"color_picker"}
	  ]}]`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "article")

	// Without the core columns an imported type holds custom fields hanging off
	// nothing.
	for _, core := range []string{"title", "slug", "content", "excerpt", "status", "published_at"} {
		assert.Contains(t, fieldNames(s), core, "core WordPress field %q", core)
	}

	want := map[string]string{
		"subtitle": "text", "summary": "text", "details": "rich_text",
		"rank": "number", "featured": "boolean", "contact": "email",
		"hero": "media", "event_on": "date", "shade": "text",
	}
	for name, fieldType := range want {
		assert.Equal(t, fieldType, fieldByName(t, s, name).FieldType, "ACF field %q", name)
	}
	assert.True(t, fieldByName(t, s, "subtitle").Required, "ACF writes required as 1")
}

func TestFromWordPress_RelationFields(t *testing.T) {
	conv, err := FromWordPress([]byte(`[
	  {"title":"A","location":[[{"param":"post_type","operator":"==","value":"article"}]],
	   "fields":[
	     {"name":"author","type":"post_object","post_type":["person"]},
	     {"name":"related","type":"relationship","post_type":["person"]},
	     {"name":"many","type":"post_object","post_type":["person"],"multiple":1}
	   ]},
	  {"title":"P","location":[[{"param":"post_type","operator":"==","value":"person"}]],
	   "fields":[{"name":"bio","type":"textarea"}]}
	]`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "article")
	assert.Equal(t, domain.RelBelongsTo, fieldByName(t, s, "author").RelationType)
	assert.Equal(t, domain.RelManyToMany, fieldByName(t, s, "related").RelationType,
		"a relationship field always holds several")
	assert.Equal(t, domain.RelManyToMany, fieldByName(t, s, "many").RelationType,
		"multiple makes a post_object hold several")
}

func TestFromWordPress_LayoutFieldsStoreNothing(t *testing.T) {
	conv, err := FromWordPress([]byte(`[{"title":"A",
	  "location":[[{"param":"post_type","value":"article"}]],
	  "fields":[{"name":"tab1","type":"tab"},{"name":"note","type":"message"},
	            {"name":"real","type":"text"}]}]`))
	require.NoError(t, err)

	names := fieldNames(schemaByName(t, conv.Bundle, "article"))
	assert.Contains(t, names, "real")
	assert.NotContains(t, names, "tab1")
	assert.NotContains(t, names, "note")
}

func TestFromWordPress_NonPostTypeLocationIsReported(t *testing.T) {
	// A group attached to an options page has no table to land in.
	_, err := FromWordPress([]byte(`[{"title":"Site Options",
	  "location":[[{"param":"options_page","operator":"==","value":"theme-settings"}]],
	  "fields":[{"name":"logo","type":"image"}]}]`))
	require.Error(t, err, "nothing importable means nothing to import")
	assert.Contains(t, err.Error(), "no post types found")
}

func TestFromWordPress_ReadsTypeListingAlongsideGroups(t *testing.T) {
	conv, err := FromWordPress([]byte(`{
	  "types": {"article": {"slug":"article","name":"Articles"},
	            "page":    {"slug":"page","name":"Pages"}},
	  "acf": [{"title":"A","location":[[{"param":"post_type","value":"article"}]],
	           "fields":[{"name":"subtitle","type":"text"}]}]
	}`))
	require.NoError(t, err)

	require.Len(t, conv.Bundle.Schemas, 2)
	assert.Equal(t, "Articles", schemaByName(t, conv.Bundle, "article").DisplayName)
	assert.Contains(t, fieldNames(schemaByName(t, conv.Bundle, "article")), "subtitle")
	assert.Contains(t, fieldNames(schemaByName(t, conv.Bundle, "page")), "title",
		"a post type with no field group still gets the core columns")
}

func TestTruthy(t *testing.T) {
	tests := []struct {
		in   any
		want bool
	}{
		{true, true}, {false, false},
		{1.0, true}, {0.0, false},
		{"1", true}, {"0", false},
		{"true", true}, {"yes", true}, {"", false},
		{nil, false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, truthy(tc.in), "truthy(%#v)", tc.in)
	}
}
