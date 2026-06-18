package sqldialect

import (
	"reflect"
	"sort"
	"testing"
)

// Syntax is the query-building half of the dialect surface. The DDL surface
// belongs to the schema engine, which is its only caller. The method set is
// pinned so the DDL layer cannot grow into this interface unnoticed.
func TestSyntax_ExposesOnlyTheQueryBuildingSurface(t *testing.T) {
	want := []string{"Name", "NowFunc", "Placeholder", "QuoteIdentifier"}

	typ := reflect.TypeOf((*Syntax)(nil)).Elem()
	got := make([]string, 0, typ.NumMethod())
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Syntax method set = %v, want %v", got, want)
	}
}
