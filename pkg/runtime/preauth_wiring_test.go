package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Both routers accept pre-auth middleware. A plugin implementing
// core.PreAuthMiddlewareProvider gets mounted nowhere
// unless the runtime asks the activator for it and passes the answer on, and
// a runtime that asks and forgets one router leaves the public surface broken
// on that port alone.
//
// This reads the source rather than booting a server because the wiring is
// the property: the interface, the accessor and the option all compile
// whether or not anything joins them up.
func TestRuntime_PassesPluginPreAuthMiddlewareToBothRouters(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "runtime.go", nil, 0)
	require.NoError(t, err)

	var builders []string
	var preAuthBuilder *ast.AssignStmt

	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		name, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		switch name.Name {
		case "preAuthMW":
			preAuthBuilder = assign
		case "adminBaseOpts", "apiBaseOpts":
			if callsSelector(assign.Rhs[0], "WithPreAuthMiddleware", "preAuthMW") {
				builders = append(builders, name.Name)
			}
		}
		return true
	})

	assert.ElementsMatch(t, []string{"adminBaseOpts", "apiBaseOpts"}, builders,
		"both routers must be given the pre-auth chain, or a plugin that resolves "+
			"the tenant is mounted on one port and missing from the other")

	require.NotNil(t, preAuthBuilder, "runtime.go declares no preAuthMW builder")
	assert.True(t, callsSelector(preAuthBuilder.Rhs[0], "PreAuthMiddleware"),
		"preAuthMW must ask the activator for what the plugins contribute, or the "+
			"capability is an interface nothing ever reads")
	assert.True(t, callsSelector(preAuthBuilder.Rhs[0], "Middleware"),
		"preAuthMW must keep the request logging middleware ahead of the plugin chain")
}

// callsSelector reports whether node contains a call to every named selector.
func callsSelector(node ast.Node, names ...string) bool {
	found := map[string]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			found[fn.Sel.Name] = true
		case *ast.Ident:
			found[fn.Name] = true
		}
		return true
	})
	for _, name := range names {
		if !found[name] {
			return false
		}
	}
	return true
}
