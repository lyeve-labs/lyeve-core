package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A list honors the documented floor of 1, so a request for three records
// returns three.
func TestClampLimit_ContentListHonorsTheDocumentedFloor(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{"a small page is served as asked", 3, 3},
		{"the documented minimum is kept", 1, 1},
		{"a value in range is kept", 50, 50},
		{"the maximum is kept", 200, 200},
		{"over the maximum is capped", 500, contentListMaxLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, clampLimit(tc.in, contentListMinLimit, contentListMaxLimit))
		})
	}
}

// The clamp and the published contract are the same numbers, so they cannot
// drift apart.
func TestOpenAPI_ContentListLimitMatchesTheClamp(t *testing.T) {
	doc := buildOpenAPIDoc([]string{"posts"}, nil, openAPIOptions{})

	path, ok := doc.Paths["/api/v1/content/{schema}"]
	require.True(t, ok, "the spec does not document the content list")
	op, ok := path["get"]
	require.True(t, ok, "no get operation on the content list")

	var schema map[string]any
	for _, p := range op.Parameters {
		if p.Name == "limit" {
			schema = p.Schema
		}
	}
	require.NotNil(t, schema, "limit is not documented")

	assert.Equal(t, contentListMinLimit, schema["minimum"])
	assert.Equal(t, contentListMaxLimit, schema["maximum"])
	assert.Equal(t, contentListDefaultLimit, schema["default"])
}
