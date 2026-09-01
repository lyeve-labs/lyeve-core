package schemaimport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

func TestFromJSONSchema_MapsTypes(t *testing.T) {
	conv, err := FromJSONSchema([]byte(`{
	  "title": "Article",
	  "type": "object",
	  "required": ["title"],
	  "properties": {
	    "title":     {"type": "string"},
	    "body":      {"type": "string", "maxLength": 50000},
	    "views":     {"type": "integer"},
	    "rating":    {"type": "number"},
	    "published": {"type": "boolean"},
	    "contact":   {"type": "string", "format": "email"},
	    "homepage":  {"type": "string", "format": "uri"},
	    "born":      {"type": "string", "format": "date"},
	    "seen_at":   {"type": "string", "format": "date-time"},
	    "ref":       {"type": "string", "format": "uuid"},
	    "avatar":    {"type": "string", "contentMediaType": "image/png"},
	    "meta":      {"type": "object"}
	  }
	}`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "article")
	assert.Equal(t, "Article", s.DisplayName)

	want := map[string]string{
		"title": "text", "body": "rich_text", "views": "number", "rating": "number",
		"published": "boolean", "contact": "email", "homepage": "url",
		"born": "date", "seen_at": "datetime", "ref": "uid",
		"avatar": "media", "meta": "json",
	}
	for name, fieldType := range want {
		assert.Equal(t, fieldType, fieldByName(t, s, name).FieldType, "field %q", name)
	}
	assert.True(t, fieldByName(t, s, "title").Required)
	assert.False(t, fieldByName(t, s, "body").Required)
}

func TestFromJSONSchema_ReferenceBecomesARelation(t *testing.T) {
	conv, err := FromJSONSchema([]byte(`{
	  "Article": {"type":"object","properties":{
	     "author": {"$ref": "#/components/schemas/Person"},
	     "tags":   {"type":"array","items":{"$ref":"#/components/schemas/Tag"}}
	  }},
	  "Person": {"type":"object","properties":{"name":{"type":"string"}}},
	  "Tag":    {"type":"object","properties":{"label":{"type":"string"}}}
	}`))
	require.NoError(t, err)

	article := schemaByName(t, conv.Bundle, "article")

	author := fieldByName(t, article, "author")
	assert.Equal(t, "relation", author.FieldType)
	assert.Equal(t, "person", author.RelationTo)
	assert.Equal(t, domain.RelBelongsTo, author.RelationType)

	tags := fieldByName(t, article, "tags")
	assert.Equal(t, "relation", tags.FieldType)
	assert.Equal(t, "tag", tags.RelationTo)
	assert.Equal(t, domain.RelManyToMany, tags.RelationType, "a list of references is many-to-many")
}

func TestFromJSONSchema_ReferenceOutsideTheImportIsReported(t *testing.T) {
	conv, err := FromJSONSchema([]byte(`{
	  "title":"Article","type":"object",
	  "properties":{"author":{"$ref":"#/components/schemas/Person"}}
	}`))
	require.NoError(t, err)

	assert.Equal(t, "json", fieldByName(t, schemaByName(t, conv.Bundle, "article"), "author").FieldType)
	require.NotEmpty(t, conv.Notes)
	assert.Contains(t, conv.Notes[0].Message, "no schema by that name is being imported")
}

func TestFromJSONSchema_NullableUnwraps(t *testing.T) {
	conv, err := FromJSONSchema([]byte(`{
	  "title":"Article","type":"object","properties":{
	    "subtitle": {"anyOf":[{"type":"string"},{"type":"null"}]},
	    "count":    {"type":["integer","null"]}
	  }
	}`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "article")
	assert.Equal(t, "text", fieldByName(t, s, "subtitle").FieldType)
	assert.Equal(t, "number", fieldByName(t, s, "count").FieldType)
}

func TestFromJSONSchema_CombinedSchemaIsReported(t *testing.T) {
	conv, err := FromJSONSchema([]byte(`{
	  "title":"Article","type":"object","properties":{
	    "either": {"oneOf":[{"type":"string"},{"type":"integer"}]}
	  }
	}`))
	require.NoError(t, err)

	assert.Equal(t, "json", fieldByName(t, schemaByName(t, conv.Bundle, "article"), "either").FieldType)
	require.NotEmpty(t, conv.Notes)
	assert.Contains(t, conv.Notes[0].Message, "no single column type")
}

func TestFromJSONSchema_RenamesCamelCaseProperties(t *testing.T) {
	conv, err := FromJSONSchema([]byte(`{
	  "title":"BlogPost","type":"object","properties":{"publishedAt":{"type":"string","format":"date-time"}}
	}`))
	require.NoError(t, err)

	s := schemaByName(t, conv.Bundle, "blog_post")
	assert.Equal(t, "datetime", fieldByName(t, s, "published_at").FieldType)
	assert.Equal(t, "publishedAt -> published_at", conv.Renamed["blog_post.published_at"])
}

func TestFromJSONSchema_ReadsYAML(t *testing.T) {
	conv, err := FromJSONSchema([]byte(`
title: Article
type: object
properties:
  title:
    type: string
`))
	require.NoError(t, err)
	assert.Equal(t, "text", fieldByName(t, schemaByName(t, conv.Bundle, "article"), "title").FieldType)
}

func TestFromJSONSchema_RejectsInputWithNoObjectSchema(t *testing.T) {
	_, err := FromJSONSchema([]byte(`{"type":"string"}`))
	require.Error(t, err)
}

func TestFromOpenAPI_ReadsComponentSchemas(t *testing.T) {
	conv, err := FromOpenAPI([]byte(`
openapi: 3.0.0
info: {title: Example, version: "1.0"}
paths: {}
components:
  schemas:
    Article:
      type: object
      required: [title]
      properties:
        title: {type: string}
        author: {$ref: "#/components/schemas/Person"}
    Person:
      type: object
      properties:
        name: {type: string}
`))
	require.NoError(t, err)
	require.Len(t, conv.Bundle.Schemas, 2)

	article := schemaByName(t, conv.Bundle, "article")
	assert.True(t, fieldByName(t, article, "title").Required)
	assert.Equal(t, "person", fieldByName(t, article, "author").RelationTo)

	// A relation only imports if the referenced table is created first.
	ordered, _ := conv.Bundle.Ordered()
	assert.Equal(t, "person", ordered[0].Name)
}

func TestFromOpenAPI_ReadsSwagger2Definitions(t *testing.T) {
	conv, err := FromOpenAPI([]byte(`
swagger: "2.0"
definitions:
  Article:
    type: object
    properties:
      title: {type: string}
`))
	require.NoError(t, err)
	assert.Equal(t, "text", fieldByName(t, schemaByName(t, conv.Bundle, "article"), "title").FieldType)
}

func TestFromOpenAPI_RejectsADocumentWithNoComponents(t *testing.T) {
	_, err := FromOpenAPI([]byte(`{"openapi":"3.0.0","paths":{}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no component schemas")
}

func TestPrimaryType(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{name: "a plain type", in: "string", want: "string"},
		{name: "a nullable union takes the real type", in: []any{"string", "null"}, want: "string"},
		{name: "null first still finds the real type", in: []any{"null", "integer"}, want: "integer"},
		{name: "only null", in: []any{"null"}, want: "null"},
		{name: "absent", in: nil, want: ""},
		{name: "an empty list", in: []any{}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, primaryType(tc.in))
		})
	}
}

func TestRefTarget(t *testing.T) {
	assert.Equal(t, "Person", refTarget("#/components/schemas/Person"))
	assert.Equal(t, "Person", refTarget("#/definitions/Person"))
	assert.Equal(t, "Person", refTarget("Person"))
	assert.Equal(t, "", refTarget(""))
}
