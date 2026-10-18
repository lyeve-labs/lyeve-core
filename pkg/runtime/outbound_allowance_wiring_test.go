package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// LYEVE_OUTBOUND_ALLOW_CGNAT does nothing unless the boot applies it. A boot
// that parses the setting and never calls applyOutboundAllowance compiles and
// refuses the range on every install, so the call is the property checked.
func TestRuntime_AppliesOutboundAllowanceAtBoot(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "runtime.go", nil, 0)
	require.NoError(t, err)

	var called bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "RunWithOptions" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "applyOutboundAllowance" {
					called = true
				}
			}
			return true
		})
	}
	require.True(t, called, "RunWithOptions must call applyOutboundAllowance")
}
