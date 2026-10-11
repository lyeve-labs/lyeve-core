package jsonpool

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The read cache tags a response with a hash of its bytes, so one value has to
// encode to one sequence of bytes. A content entry's fields are a map, and Go
// walks a map in a different order on each pass.
func TestEncoding_SameValueSameBytes(t *testing.T) {
	data := map[string]any{}
	for i := 0; i < 24; i++ {
		data[fmt.Sprintf("field_%02d", i)] = map[string]any{"a": i, "b": "x", "c": []int{i}}
	}
	value := map[string]any{"id": "e1", "data": data}

	var first bytes.Buffer
	require.NoError(t, WriteJSON(&first, value))
	marshaled, err := MarshalJSON(value)
	require.NoError(t, err)
	assert.Equal(t, first.Bytes(), marshaled, "both helpers write the same bytes")

	for i := 0; i < 50; i++ {
		var again bytes.Buffer
		require.NoError(t, WriteJSON(&again, value))
		assert.Equal(t, first.String(), again.String(), "encode %d wrote different bytes", i)
	}
}
