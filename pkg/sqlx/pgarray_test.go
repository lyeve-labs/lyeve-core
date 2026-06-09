package sqlx

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPGArray(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty braces", raw: "{}", want: []string{}},
		{name: "empty with spaces", raw: "{   }", want: []string{}},
		{name: "single element", raw: "{a}", want: []string{"a"}},
		{name: "multiple elements", raw: "{a,b,c}", want: []string{"a", "b", "c"}},
		{name: "quoted element with comma", raw: "{\"hello,world\",b}", want: []string{"hello,world", "b"}},
		{name: "escaped quotes", raw: "{\"say \\\"hi\\\"\",b}", want: []string{`say "hi"`, "b"}},
		{name: "empty element", raw: "{,a}", want: []string{"", "a"}},
		{name: "too short", raw: "x", want: nil},
		{name: "no braces", raw: "abc", want: nil},
		{name: "empty string", raw: "", want: []string{}},
		{name: "open brace only", raw: "{", want: nil},
		{name: "close brace only", raw: "}", want: nil},
		{name: "unclosed", raw: "{a,b", want: nil},
		{name: "json array", raw: `["a","b"]`, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PGArray(tt.raw))
		})
	}

	t.Run("consistent with ParsePGArray", func(t *testing.T) {
		input := "{a,\"b c\",d}"
		assert.Equal(t, ParsePGArray(input), PGArray(input))
		assert.Equal(t, []string{"a", "b c", "d"}, PGArray(input))
	})
}

// A value too short to hold the braces holds no elements. ParsePGArray
// answers it with nil and PGArray with an empty array.
func TestPGArray_EmptyStringIsAnEmptyArray(t *testing.T) {
	for _, raw := range []string{"", "{"} {
		assert.NotPanics(t, func() { ParsePGArray(raw) })
		assert.Nil(t, ParsePGArray(raw))
	}
	assert.NotPanics(t, func() { PGArray("") })
	assert.Equal(t, []string{}, PGArray(""))
	assert.NotNil(t, PGArray(""))
}
