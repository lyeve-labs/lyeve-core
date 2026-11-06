package db

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One value is one bound text however it is written, and a plain decimal
// keeps every digit a float64 would round away.
func TestNumberFilter_BindsOneTextPerValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"10", "10"},
		{"10.0", "10"},
		{"010.500", "10.5"},
		{"+5", "5"},
		{".5", "0.5"},
		{"5.", "5"},
		{"-0", "0"},
		{"-0.000", "0"},
		{"-2.50", "-2.5"},
		{"123456789.123456789", "123456789.123456789"},
		{"0.1000000000000000001", "0.1000000000000000001"},
		{"1e2", "100"},
		{"-0e5", "0"},
		{"2.5E-1", "0.25"},
		{json.Number("10.0"), "10"},
		{10.0, "10"},
		{2.5, "2.5"},
		{int64(7), "7"},
	}
	for _, c := range cases {
		got, err := numberFilter(c.in)
		require.NoError(t, err, "%#v", c.in)
		assert.Equal(t, c.want, got, "%#v", c.in)
	}
	for _, in := range []any{"", "abc", "NaN", "Inf", "-infinity", "1e28", "-1e28", "1e400", "1.2.3", "--1", true} {
		_, err := numberFilter(in)
		assert.Error(t, err, "%#v", in)
	}
}
