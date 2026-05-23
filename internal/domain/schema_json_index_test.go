package domain

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A json column cannot be an index key on two of the three databases: MySQL
// refuses one with error 3152, and SQL Server stores the value as
// NVARCHAR(MAX), which no index can take. The schema is refused at save with
// the field named, rather than failing on the database with its own words.
func TestValidateSchema_JSONFieldCannotBeIndexed(t *testing.T) {
	cases := []struct {
		name  string
		field SchemaField
		ok    bool
	}{
		{"indexed json", SchemaField{Name: "meta", FieldType: "json", Indexed: true}, false},
		{"unique json", SchemaField{Name: "meta", FieldType: "json", Unique: true}, false},
		{"plain json", SchemaField{Name: "meta", FieldType: "json"}, true},
		{"indexed text", SchemaField{Name: "meta", FieldType: "text", Indexed: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSchema(&Schema{Name: "article", Fields: []SchemaField{tc.field}})
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			assert.True(t, errors.Is(err, ErrValidation), "err = %v", err)
			assert.Contains(t, err.Error(), `"meta"`)
		})
	}
}
