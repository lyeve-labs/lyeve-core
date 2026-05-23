package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseScope_ReadsResourceAndAction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want ScopePair
		ok   bool
	}{
		{"graphql:read", ScopePair{Resource: "graphql", Action: "read"}, true},
		{"  Realtime:Write ", ScopePair{Resource: "realtime", Action: "write"}, true},
		{"*:*", ScopePair{Resource: "*", Action: "*"}, true},
		{"content:blog:read", ScopePair{Resource: "content", Action: "blog:read"}, true},
		{"graphql", ScopePair{}, false},
		{":read", ScopePair{}, false},
		{"graphql:", ScopePair{}, false},
		{"", ScopePair{}, false},
	}
	for _, tc := range cases {
		got, ok := ParseScope(tc.in)
		assert.Equal(t, tc.ok, ok, "%q", tc.in)
		assert.Equal(t, tc.want, got, "%q", tc.in)
	}
}
