package jsonschema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type simpleConfig struct {
	Name     string     `json:"name" jsonschema:"desc=The display name;title=Name"`
	Enabled  bool       `json:"enabled"`
	Count    int        `json:"count" jsonschema:"min=0;max=100;default=10"`
	Labels   []string   `json:"labels,omitempty"`
	Deadline *time.Time `json:"deadline,omitempty"`
	Internal string     `json:"-"`
}

type nestedConfig struct {
	Server   serverConfig `json:"server"`
	Features []string     `json:"features" jsonschema:"enum=a,b,c"`
}

type serverConfig struct {
	Host string `json:"host" jsonschema:"default=localhost;desc=Bind address"`
	Port int    `json:"port" jsonschema:"min=1;max=65535;default=8080"`
}

type configWithUUID struct {
	ID     uuid.UUID  `json:"id"`
	UserID *uuid.UUID `json:"user_id,omitempty"`
}

type configWithEmbedded struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type configWithEmbed struct {
	configWithEmbedded
	Port int `json:"port"`
}

func TestGenerate_SimpleStruct(t *testing.T) {
	s := Generate(reflect.TypeOf(simpleConfig{}))
	require.NotNil(t, s)

	assert.Equal(t, "object", s["type"])

	props, ok := s["properties"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, props, "name")
	require.Contains(t, props, "enabled")
	require.Contains(t, props, "count")
	require.Contains(t, props, "labels")
	require.Contains(t, props, "deadline")

	// Internal field should be excluded.
	assert.NotContains(t, props, "internal")
	assert.NotContains(t, props, "Internal")

	// Check required list (labels and deadline are omitempty).
	req, ok := s["required"].([]string)
	require.True(t, ok)
	assert.Contains(t, req, "name")
	assert.Contains(t, req, "enabled")
	assert.Contains(t, req, "count")
	assert.NotContains(t, req, "labels")
	assert.NotContains(t, req, "deadline")

	// Check descriptions and constraints.
	nameProp := props["name"].(map[string]any)
	assert.Equal(t, "The display name", nameProp["description"])
	assert.Equal(t, "Name", nameProp["title"])

	countProp := props["count"].(map[string]any)
	assert.Equal(t, "integer", countProp["type"])
	assert.Equal(t, float64(0), countProp["minimum"])
	assert.Equal(t, float64(100), countProp["maximum"])
	assert.Equal(t, int64(10), countProp["default"])

	// Deadline should be oneOf [date-time, null].
	deadlineProp := props["deadline"].(map[string]any)
	oneOf, ok := deadlineProp["oneOf"].([]any)
	require.True(t, ok)
	assert.Len(t, oneOf, 2)
}

func TestGenerate_NestedStruct(t *testing.T) {
	s := Generate(reflect.TypeOf(nestedConfig{}))
	require.NotNil(t, s)

	props := s["properties"].(map[string]any)

	serverProp := props["server"].(map[string]any)
	assert.Equal(t, "object", serverProp["type"])
	serverProps := serverProp["properties"].(map[string]any)
	assert.Contains(t, serverProps, "host")
	assert.Contains(t, serverProps, "port")

	// Enum on features.
	featuresProp := props["features"].(map[string]any)
	assert.Equal(t, "array", featuresProp["type"])
	enums := featuresProp["enum"].([]any)
	assert.Len(t, enums, 3)
}

func TestGenerate_UUID(t *testing.T) {
	s := Generate(reflect.TypeOf(configWithUUID{}))
	require.NotNil(t, s)

	props := s["properties"].(map[string]any)

	idProp := props["id"].(map[string]any)
	assert.Equal(t, "string", idProp["type"])
	assert.Equal(t, "uuid", idProp["format"])
	assert.Equal(t, 36, idProp["minLength"])

	userIDProp := props["user_id"].(map[string]any)
	oneOf := userIDProp["oneOf"].([]any)
	assert.Len(t, oneOf, 2)
}

func TestGenerate_EmbeddedStruct(t *testing.T) {
	s := Generate(reflect.TypeOf(configWithEmbed{}))
	require.NotNil(t, s)

	props := s["properties"].(map[string]any)
	assert.Contains(t, props, "name")
	assert.Contains(t, props, "enabled")
	assert.Contains(t, props, "port")
}

func TestGenerate_NilType(t *testing.T) {
	assert.Nil(t, Generate(nil))
}

func TestGenerate_NonStruct(t *testing.T) {
	assert.Nil(t, Generate(reflect.TypeOf(42)))
	assert.Nil(t, Generate(reflect.TypeOf("string")))
}

func TestGenerateWithOptions(t *testing.T) {
	s := GenerateWithOptions(reflect.TypeOf(simpleConfig{}), Options{
		Title:       "SMTP Configuration",
		Description: "SMTP server settings for email delivery.",
	})
	require.NotNil(t, s)

	assert.Equal(t, "SMTP Configuration", s["title"])
	assert.Equal(t, "SMTP server settings for email delivery.", s["description"])
}

func TestGenerate_JSONRawMessage(t *testing.T) {
	type configWithRaw struct {
		Payload json.RawMessage `json:"payload,omitempty"`
		Meta    json.RawMessage `json:"meta"`
	}
	s := Generate(reflect.TypeOf(configWithRaw{}))
	require.NotNil(t, s)

	props := s["properties"].(map[string]any)

	// json.RawMessage should be an open schema {}.
	payloadProp := props["payload"].(map[string]any)
	assert.Empty(t, payloadProp)
}

func TestSchemaRoundTrip(t *testing.T) {
	// Ensure generated schema is valid JSON.
	s := Generate(reflect.TypeOf(simpleConfig{}))
	b, err := json.MarshalIndent(s, "", "  ")
	require.NoError(t, err)

	// And that it deserializes back.
	var result map[string]any
	err = json.Unmarshal(b, &result)
	require.NoError(t, err)
	assert.Equal(t, "object", result["type"])

	// Ensure it's proper JSON by checking it starts with {.
	assert.True(t, strings.HasPrefix(string(b), "{"))
}
