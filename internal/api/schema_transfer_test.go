package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertUpload_SelectsTheConverter(t *testing.T) {
	tests := []struct {
		name       string
		from       string
		body       string
		wantSchema string
	}{
		{
			name:       "a bundle is the default",
			from:       "",
			body:       "schemas:\n  - name: article\n    fields: [{name: title, field_type: text}]\n",
			wantSchema: "article",
		},
		{
			name:       "lyeve names it explicitly",
			from:       "lyeve",
			body:       "schemas:\n  - name: article\n    fields: [{name: title, field_type: text}]\n",
			wantSchema: "article",
		},
		{
			name:       "json schema",
			from:       "json-schema",
			body:       `{"title":"Article","type":"object","properties":{"title":{"type":"string"}}}`,
			wantSchema: "article",
		},
		{
			name:       "openapi",
			from:       "openapi",
			body:       `{"openapi":"3.0.0","components":{"schemas":{"Article":{"type":"object","properties":{"title":{"type":"string"}}}}}}`,
			wantSchema: "article",
		},
		{
			name:       "strapi",
			from:       "strapi",
			body:       `{"info":{"singularName":"article"},"attributes":{"title":{"type":"string"}}}`,
			wantSchema: "article",
		},
		{
			name:       "contentful",
			from:       "contentful",
			body:       `[{"sys":{"id":"article"},"fields":[{"id":"title","type":"Symbol"}]}]`,
			wantSchema: "article",
		},
		{
			name:       "wordpress",
			from:       "wordpress",
			body:       `[{"title":"A","location":[[{"param":"post_type","value":"article"}]],"fields":[{"name":"subtitle","type":"text"}]}]`,
			wantSchema: "article",
		},
		{
			name:       "case and spacing do not matter",
			from:       "  OpenAPI  ",
			body:       `{"openapi":"3.0.0","components":{"schemas":{"Article":{"type":"object","properties":{"t":{"type":"string"}}}}}}`,
			wantSchema: "article",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bundle, _, err := convertUpload(tc.from, []byte(tc.body))
			require.NoError(t, err)
			require.NotNil(t, bundle)

			var names []string
			for _, s := range bundle.Schemas {
				names = append(names, s.Name)
			}
			assert.Contains(t, names, tc.wantSchema)
		})
	}
}

func TestConvertUpload_ReportsAnUnknownSource(t *testing.T) {
	_, _, err := convertUpload("drupal", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drupal")
	assert.Contains(t, err.Error(), "wordpress", "the message lists what is accepted")
}

func TestConvertUpload_ForeignSourcesCarryTheirNotes(t *testing.T) {
	// The notes are the whole point of converting rather than guessing: an
	// operator has to see what did not come across.
	_, conv, err := convertUpload("strapi", []byte(
		`{"info":{"singularName":"page"},"attributes":{
		   "title":{"type":"string"},
		   "blocks":{"type":"dynamiczone"}}}`))
	require.NoError(t, err)
	require.NotNil(t, conv)
	assert.NotEmpty(t, conv.Notes)
}

func TestConvertUpload_BundleHasNoConversionNotes(t *testing.T) {
	// A bundle is already in this engine's own terms, so nothing was approximated.
	_, conv, err := convertUpload("lyeve",
		[]byte("schemas:\n  - name: article\n    fields: [{name: title, field_type: text}]\n"))
	require.NoError(t, err)
	assert.Nil(t, conv)
}

func TestConvertUpload_ReportsUnreadableInput(t *testing.T) {
	_, _, err := convertUpload("json-schema", []byte(`{"type":"string"}`))
	require.Error(t, err)
}

func TestSchemaEngineFrom_NilHostIsNotAPanic(t *testing.T) {
	// An install with no schema engine wires none, and the handler answers
	// 503 rather than crashing the process.
	assert.Nil(t, schemaEngineFrom(nil))
}

func TestInstanceSource_FallsBackWithoutAHost(t *testing.T) {
	assert.Equal(t, "lyeve", instanceSource(nil))
}
