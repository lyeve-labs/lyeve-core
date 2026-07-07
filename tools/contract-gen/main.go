// Contract generator: emits a public fixture of a plugin's observable
// contract (hooks, tables, routes) from AST analysis.
//
//	go run ./tools/contract-gen lyeve-plugin-example/plugin > fixture.json
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: contract-gen <plugin-dir>\n")
		os.Exit(1)
	}
	dir := os.Args[1]

	fixture, err := extractContract(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "contract-gen: %v\n", err)
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(fixture); err != nil {
		fmt.Fprintf(os.Stderr, "contract-gen: marshal: %v\n", err)
		os.Exit(1)
	}
}

// extractContract parses all .go files in dir and builds a PluginFixture.
func extractContract(dir string) (*plugintest.PluginFixture, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), ".go") && !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	var files []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files = append(files, f)
		}
	}

	var (
		pluginName    string
		hooksEmitted  []plugintest.HookEmitted
		tablesCovered []string
		routesExposed []plugintest.RouteExposed
	)

	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.ValueSpec:
				for i, name := range node.Names {
					if name.Name == "Name" && i < len(node.Values) {
						if lit, ok := node.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							pluginName = strings.Trim(lit.Value, `"`)
						}
					}
				}

			case *ast.CallExpr:
				extractHookPublish(node, &hooksEmitted)
				extractCoveredTable(node, &tablesCovered)

			case *ast.FuncDecl:
				if node.Name.Name == "Routes" {
					extractRoutes(node, &routesExposed)
				}
			}
			return true
		})
	}

	if pluginName == "" {
		pluginName = filepath.Base(dir)
	}

	return &plugintest.PluginFixture{
		PluginName:    pluginName,
		HooksEmitted:  hooksEmitted,
		TablesCovered: tablesCovered,
		RoutesExposed: routesExposed,
	}, nil
}

// extractHookPublish handles patterns like:
//
//	host.HookPublisher().Publish(ctx, core.Event{Type: ..., Schema: ...})
//	r.host.HookPublisher().Publish(ctx, event)
func extractHookPublish(call *ast.CallExpr, hooks *[]plugintest.HookEmitted) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Publish" {
		return
	}

	if len(call.Args) < 2 {
		return
	}

	eventArg := call.Args[1]

	if comp, ok := eventArg.(*ast.CompositeLit); ok {
		h := extractEventFromComposite(comp)
		if h.EventType != "" {
			*hooks = append(*hooks, h)
		}
		return
	}

	// Variable-arg Publish calls (e.g. Publish(ctx, evt)) are not traced back
	// to their construction site. Only inline core.Event{...} literals
	// at the call site are extracted.
}

// extractEventFromComposite extracts event type, schema, and payload keys
// from an inline core.Event{...} composite literal.
func extractEventFromComposite(comp *ast.CompositeLit) plugintest.HookEmitted {
	var h plugintest.HookEmitted
	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}

		switch key.Name {
		case "Type":
			h.EventType = core.EventType(extractEventType(kv.Value))
		case "Schema":
			if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				h.Schema = strings.Trim(lit.Value, `"`)
			}
		case "Data":
			h.PayloadKeys = extractPayloadKeys(kv.Value)
		}
	}
	return h
}

// extractEventType resolves PascalCase constants (core.AfterCreate) or
// string literals ("after_create") to a snake_case event type name.
func extractEventType(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.SelectorExpr:
		return toEventType(v.Sel.Name)
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			return strings.Trim(v.Value, `"`)
		}
	case *ast.Ident:
		// Variable reference: cannot resolve statically.
	}
	return ""
}

// toEventType converts a PascalCase constant name to snake_case event type.
func toEventType(name string) string {
	known := map[string]string{
		"BeforeCreate": "before_create",
		"AfterCreate":  "after_create",
		"BeforeUpdate": "before_update",
		"AfterUpdate":  "after_update",
		"BeforeDelete": "before_delete",
		"AfterDelete":  "after_delete",
	}
	if e, ok := known[name]; ok {
		return e
	}
	return ""
}

// extractPayloadKeys extracts string keys from a map[string]any composite literal.
func extractPayloadKeys(expr ast.Expr) []string {
	comp, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	var keys []string
	for _, elt := range comp.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if lit, ok := kv.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				keys = append(keys, strings.Trim(lit.Value, `"`))
			}
		}
	}
	return keys
}

// extractCoveredTable extracts table names from core.RegisterCoveredTable("...") calls.
func extractCoveredTable(call *ast.CallExpr, tables *[]string) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	if sel.Sel.Name != "RegisterCoveredTable" {
		return
	}
	if len(call.Args) < 1 {
		return
	}
	if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
		table := strings.Trim(lit.Value, `"`)
		for _, t := range *tables {
			if t == table {
				return
			}
		}
		*tables = append(*tables, table)
	}
}

// extractRoutes extracts RouteDecl composite literals from the Routes() method.
// Handles both explicit plugin.RouteDecl{...} and inferred {...} literals
// (Go infers the type from the function's return type).
func extractRoutes(fn *ast.FuncDecl, routes *[]plugintest.RouteExposed) {
	if fn.Body == nil {
		return
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		comp, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if !isRouteDeclType(comp.Type) && !isInferredRouteDecl(comp) {
			return true
		}

		r := plugintest.RouteExposed{}
		for _, elt := range comp.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "Method":
				if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					r.Method = strings.Trim(lit.Value, `"`)
				}
			case "Pattern":
				if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					r.Pattern = strings.Trim(lit.Value, `"`)
				}
			case "Group":
				r.Group = core.RouteGroup(extractRouteGroup(kv.Value))
			}
		}
		if r.Method != "" && r.Pattern != "" {
			*routes = append(*routes, r)
		}
		return true
	})
}

// isInferredRouteDecl returns true if the composite literal has fields
// matching a RouteDecl (Method, Pattern) without an explicit type.
func isInferredRouteDecl(comp *ast.CompositeLit) bool {
	hasMethod := false
	hasPattern := false
	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Method":
			hasMethod = true
		case "Pattern":
			hasPattern = true
		}
	}
	return hasMethod && hasPattern
}

// isRouteDeclType reports whether expr is a RouteDecl type reference from the
// core or plugin package (imported as "core" or "plugin").
func isRouteDeclType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); ok {
		if sel.Sel.Name != "RouteDecl" {
			return false
		}
		switch pkg.Name {
		case "core", "plugin":
			return true
		}
	}
	return false
}

// extractRouteGroup resolves a route group selector (pkg.GroupXxx) to its
// snake_case name. Recognizes the core and plugin package aliases.
func extractRouteGroup(expr ast.Expr) string {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if pkg, ok := sel.X.(*ast.Ident); ok {
		switch pkg.Name {
		case "core", "plugin":
			return toRouteGroup(sel.Sel.Name)
		}
	}
	return ""
}

func toRouteGroup(name string) string {
	known := map[string]string{
		"GroupPublic":     "public",
		"GroupAuth":       "auth",
		"GroupAdmin":      "admin",
		"GroupSuperAdmin": "super_admin",
	}
	return known[name]
}
