package core

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Help text and the No AI mark are stored with the definition, so they have
// to survive the trip through JSON that every store and decoder makes.
func TestSchemaField_HelpTextAndNoAIRoundTrip(t *testing.T) {
	in := SchemaField{Name: "summary", FieldType: "text", HelpText: "One sentence for the home page.", NoAI: true}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"help_text":"One sentence for the home page."`)
	assert.Contains(t, string(raw), `"no_ai":true`)

	var out SchemaField
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, in, out)
}

// A stored definition's version is a hash of its JSON, so a field that
// carries neither key must serialize exactly as it did before the keys
// existed, or every stored version would change.
func TestSchemaField_UnmarkedFieldKeepsItsJSON(t *testing.T) {
	raw, err := json.Marshal(SchemaField{Name: "title", FieldType: "text", Required: true})
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"title","field_type":"text","required":true,"unique":false,"indexed":false}`, string(raw))
}
