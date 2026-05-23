package domain

import (
	"reflect"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The domain names are aliases of the pkg/core definition types.
// An alias is the same type, so a value crosses the two packages with no
// conversion and no copy. A redeclared struct would compile at most call sites
// and then fail wherever one type is assigned to the other, which is the
// failure this guards.
func TestSchemaTypes_AreAliasesOfTheCoreTypes(t *testing.T) {
	cases := []struct {
		name          string
		domain, other reflect.Type
	}{
		{"Schema", reflect.TypeOf(Schema{}), reflect.TypeOf(core.Schema{})},
		{"SchemaField", reflect.TypeOf(SchemaField{}), reflect.TypeOf(core.SchemaField{})},
		{"ValidationRule", reflect.TypeOf(ValidationRule{}), reflect.TypeOf(core.ValidationRule{})},
		{"CrossFieldRule", reflect.TypeOf(CrossFieldRule{}), reflect.TypeOf(core.CrossFieldRule{})},
		{"CustomValidator", reflect.TypeOf(CustomValidator{}), reflect.TypeOf(core.CustomValidator{})},
		{"PopulateConfig", reflect.TypeOf(PopulateConfig{}), reflect.TypeOf(core.PopulateConfig{})},
	}
	for _, tc := range cases {
		if tc.domain != tc.other {
			t.Errorf("%s: domain type %v is not the core type %v", tc.name, tc.domain, tc.other)
		}
	}

	// Passing a domain value where a core value is wanted, with no conversion,
	// is what callers actually depend on.
	readCore := func(s *core.Schema) string { return s.Name }
	if got := readCore(&Schema{Name: "articles"}); got != "articles" {
		t.Fatalf("Name = %q, want %q", got, "articles")
	}
}
