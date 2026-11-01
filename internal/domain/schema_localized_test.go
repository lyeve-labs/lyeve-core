package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The mark travels in the definition JSON under the name the admin and the
// translation panel read. An unmarked field leaves the key out, so a stored
// definition that never used it reads back byte for byte unchanged.
func TestSchemaField_LocalizedWireName(t *testing.T) {
	raw, err := json.Marshal(SchemaField{Name: "title", FieldType: "text", Localized: true})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"localized":true`)

	raw, err = json.Marshal(SchemaField{Name: "title", FieldType: "text"})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "localized")

	var back SchemaField
	require.NoError(t, json.Unmarshal([]byte(`{"name":"title","field_type":"text","localized":true}`), &back))
	assert.True(t, back.Localized)
}
