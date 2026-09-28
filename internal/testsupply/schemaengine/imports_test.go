package schemaengine_test

import (
	"go/build"
	"strings"
	"testing"
)

// The point of this package is that a third party could have written it. A
// third party cannot import internal/, so neither may this. Without this
// check the fixture would drift into using engine internals and the extension
// point it demonstrates would quietly stop being one.
func TestFixture_ImportsOnlyThePublicSurface(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("read package: %v", err)
	}

	const module = "github.com/lyeve-labs/lyeve-core/"
	for _, imp := range pkg.Imports {
		if !strings.HasPrefix(imp, module) {
			continue // stdlib, which a third party also has
		}
		if imp != module+"pkg/core" {
			t.Errorf("fixture imports %s; a third party can import only %spkg/core", imp, module)
		}
	}
}
