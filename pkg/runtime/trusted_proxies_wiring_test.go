package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Both routers need TRUSTED_PROXIES. A runtime that parses it and hands it only
// to the API-key audit logger changes nothing a request can observe: proxy
// headers are stripped no matter what an operator configured, and every per-IP
// limit counts the load balancer rather than the caller behind it.
//
// This reads the source rather than booting a server because the wiring is the
// property: a runtime that parses the setting and forgets to pass it on is
// exactly as broken as one that never read it, and both compile.
func TestRuntime_PassesTrustedProxiesToBothRouters(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "runtime.go", nil, 0)
	require.NoError(t, err)

	var builders []string
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 {
			return true
		}
		name, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if name.Name != "adminBaseOpts" && name.Name != "apiBaseOpts" {
			return true
		}
		if callsWithTrustedProxies(assign.Rhs[0]) {
			builders = append(builders, name.Name)
		}
		return true
	})

	assert.ElementsMatch(t, []string{"adminBaseOpts", "apiBaseOpts"}, builders,
		"both routers must be given the configured trusted proxies, or TRUSTED_PROXIES "+
			"is a setting the request path never sees")
}

func callsWithTrustedProxies(node ast.Node) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "WithTrustedProxies" {
			found = true
			return false
		}
		return true
	})
	return found
}
