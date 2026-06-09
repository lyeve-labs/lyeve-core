package jsonpool

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type decodePayload struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

func TestDecodeJSON_Roundtrip(t *testing.T) {
	input := decodePayload{Name: "test-item", Value: 42}
	raw, err := json.Marshal(input)
	require.NoError(t, err)

	var output decodePayload
	err = DecodeJSON(bytes.NewReader(raw), &output)
	require.NoError(t, err)
	assert.Equal(t, input, output)
}

func TestDecodeJSON_SmallPayload(t *testing.T) {
	err := DecodeJSON(strings.NewReader(`{"name":"x","value":7}`), &decodePayload{})
	require.NoError(t, err)
}

func TestDecodeJSON_EmptyBody(t *testing.T) {
	err := DecodeJSON(strings.NewReader(""), &decodePayload{})
	assert.Error(t, err, "empty JSON should fail to decode")
}

func TestDecodeJSON_InvalidJSON(t *testing.T) {
	err := DecodeJSON(strings.NewReader("{bad"), &decodePayload{})
	assert.Error(t, err, "invalid JSON should fail to decode")
}

func TestDecodeJSON_LargePayload(t *testing.T) {
	// Build a payload large enough to exercise the buffer pool but below 64KB.
	items := make([]string, 100)
	for i := range items {
		items[i] = `"value-` + strings.Repeat("x", 50) + `"`
	}
	body := `{"items":[` + strings.Join(items, ",") + `]}`
	var out struct {
		Items []string `json:"items"`
	}
	err := DecodeJSON(strings.NewReader(body), &out)
	require.NoError(t, err)
	assert.Len(t, out.Items, 100)
}

func TestDecodeJSON_NonStructTarget(t *testing.T) {
	var m map[string]any
	err := DecodeJSON(strings.NewReader(`{"k":"v"}`), &m)
	require.NoError(t, err)
	assert.Equal(t, "v", m["k"])
}

// WriteJSON

type writePayload struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func TestWriteJSON_Success(t *testing.T) {
	var buf bytes.Buffer
	err := WriteJSON(&buf, writePayload{ID: "abc", Name: "item"})
	require.NoError(t, err)

	var decoded writePayload
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "abc", decoded.ID)
	assert.Equal(t, "item", decoded.Name)
}

func TestWriteJSON_MarshalError(t *testing.T) {
	// Passing a channel or func should cause sonic to fail.
	err := WriteJSON(&bytes.Buffer{}, make(chan int))
	assert.Error(t, err, "marshaling a channel should fail")
}

func TestWriteJSON_EmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	err := WriteJSON(&buf, struct{}{})
	require.NoError(t, err)
	assert.Equal(t, "{}\n", buf.String())
}

func TestWriteJSON_NilTarget(t *testing.T) {
	var buf bytes.Buffer
	err := WriteJSON(&buf, nil)
	require.NoError(t, err)
	assert.Equal(t, "null\n", buf.String())
}

// PrewarmSonic

func TestPrewarmSonic_DoesNotPanic(t *testing.T) {
	// PrewarmSonic suppresses all errors internally. The contract is that
	// it does not panic.
	assert.NotPanics(t, func() {
		PrewarmSonic()
	})
}

func TestPrewarmSonic_Idempotent(t *testing.T) {
	// Multiple calls should be safe.
	PrewarmSonic()
	PrewarmSonic()
	assert.NotPanics(t, func() {
		PrewarmSonic()
	})
}
