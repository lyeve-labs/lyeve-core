package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIndexRefusal_JSONOnly(t *testing.T) {
	cases := []struct {
		name    string
		field   SchemaField
		refused bool
	}{
		{"indexed json", SchemaField{FieldType: "json", Indexed: true}, true},
		{"unique json", SchemaField{FieldType: "json", Unique: true}, true},
		{"plain json", SchemaField{FieldType: "json"}, false},
		{"indexed text", SchemaField{FieldType: "text", Indexed: true}, false},
		{"unique email", SchemaField{FieldType: "email", Unique: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.refused, IndexRefusal(tc.field) != "")
		})
	}
}
