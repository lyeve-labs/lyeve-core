package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCloneJSONValue_SharesNoContainer(t *testing.T) {
	src := map[string]any{
		"title":  "kept",
		"nested": map[string]any{"depth": float64(2)},
		"list":   []any{"a", map[string]any{"in": "list"}},
		"rows":   []map[string]any{{"id": "r1"}},
		"none":   nil,
	}
	want := map[string]any{
		"title":  "kept",
		"nested": map[string]any{"depth": float64(2)},
		"list":   []any{"a", map[string]any{"in": "list"}},
		"rows":   []map[string]any{{"id": "r1"}},
		"none":   nil,
	}

	got, ok := CloneJSONValue(src).(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, want, got)

	got["title"] = "edited"
	got["nested"].(map[string]any)["depth"] = float64(9)
	got["list"].([]any)[0] = "edited"
	got["list"].([]any)[1].(map[string]any)["in"] = "edited"
	got["rows"].([]map[string]any)[0]["id"] = "edited"
	assert.Equal(t, want, src, "editing the copy reached the original")

	assert.Nil(t, CloneJSONMap(nil))
	assert.Equal(t, "scalar", CloneJSONValue("scalar"))
}
