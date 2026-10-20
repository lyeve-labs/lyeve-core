// Command dialectgates reports every database server a test starts without
// first asking whether the run covers that server's dialect.
//
// CI_DIALECT names the one dialect a run covers. The CI matrix runs each
// dialect in its own job, so a job holds one kind of server, and a run limited
// to PostgreSQL that starts MySQL or SQL Server spends minutes and gigabytes on
// a server nobody asked for. On a machine that other runs share, that memory is
// what makes SQL Server abort mid-suite, which reads as a product failure.
//
// testdb and plugintest ask on every caller's behalf. A test that starts its own
// server, with a testcontainers module (postgres, mysql or mssql) or with the
// generic testcontainers.Run or GenericContainer and a database image, has to
// ask itself, earlier in the same top-level function, with one of:
//
//	testdialect.Require(t, "mysql")
//	testdialect.Enabled("mysql")
//	testdb.ShouldTest("mysql")
//	plugintest.SkipDialect(t, "mysql")
//
// A dialect named as a literal has to be the one the call starts, and it has to
// be one of the three CI_DIALECT names: a check for a name that is none of them
// skips its test in every run limited to one dialect, so the test never runs in
// CI and nothing says so. Every Go file is read, whatever its build tags,
// because a tagged suite starts servers too.
//
// Usage: dialectgates <repository root>
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// servers maps each testcontainers module that starts a database server to
// the dialect name CI_DIALECT uses for it.
var servers = map[string]string{
	"github.com/testcontainers/testcontainers-go/modules/postgres": "postgres",
	"github.com/testcontainers/testcontainers-go/modules/mysql":    "mysql",
	"github.com/testcontainers/testcontainers-go/modules/mssql":    "mssql",
}

// known are the dialect names CI_DIALECT takes.
var known = map[string]bool{"postgres": true, "mysql": true, "mssql": true}

// starts are the module functions that start a server.
var starts = map[string]bool{"Run": true, "RunContainer": true}

// generic is the testcontainers package, whose Run and GenericContainer start
// whatever image they are given.
const generic = "github.com/testcontainers/testcontainers-go"

// genericStarts are the generic functions that start a container.
var genericStarts = map[string]bool{"Run": true, "GenericContainer": true}

// images names the dialect of a database image by a word in its name.
var images = []struct{ word, dialect string }{
	{"mysql", "mysql"},
	{"mariadb", "mysql"},
	{"mssql", "mssql"},
	{"sql-edge", "mssql"},
	{"sqlserver", "mssql"},
	{"postgres", "postgres"},
	{"postgis", "postgres"},
}

// gate is a function that answers whether the run covers a dialect, and the
// position of its dialect argument.
type gate struct {
	path string
	fn   string
	arg  int
}

// gates are the checks a server start may sit behind.
var gates = []gate{
	{"github.com/lyeve-labs/lyeve-core/internal/testdialect", "Require", 1},
	{"github.com/lyeve-labs/lyeve-core/internal/testdialect", "Enabled", 0},
	{"github.com/lyeve-labs/lyeve-core/internal/testdb", "ShouldTest", 0},
	{"github.com/lyeve-labs/lyeve-core/pkg/plugintest", "SkipDialect", 1},
}

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" {
		fmt.Fprintln(os.Stderr, "usage: dialectgates <repository root>")
		os.Exit(2)
	}
	root, err := filepath.Abs(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "dialectgates: %v\n", err)
		os.Exit(2)
	}
	os.Exit(run(root))
}

// check is one dialect check found in a declaration: the dialect it names,
// empty when the argument is not a literal, and where it is.
type check struct {
	dialect string
	pos     token.Pos
}

func run(root string) int {
	fset := token.NewFileSet()
	var findings []string
	files, started, asks := 0, 0, 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		files++
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		modules, genericName, gateFns := imports(f)
		if len(modules) == 0 && genericName == "" && len(gateFns) == 0 {
			return nil
		}
		for _, decl := range f.Decls {
			var checks []check
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pkg, fn, ok := qualified(call)
				if !ok {
					return true
				}
				if g, ok := gateFns[pkg+"."+fn]; ok {
					asks++
					d := literal(call, g.arg)
					if d != "" && !known[d] {
						findings = append(findings, fmt.Sprintf("%s:%d asks about %q, which is not a dialect CI_DIALECT names",
							rel, fset.Position(call.Pos()).Line, d))
					}
					checks = append(checks, check{dialect: d, pos: call.Pos()})
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pkg, fn, ok := qualified(call)
				if !ok {
					return true
				}
				dialect := ""
				switch {
				case starts[fn] && modules[pkg] != "":
					dialect = modules[pkg]
				case genericStarts[fn] && pkg == genericName:
					dialect = imageDialect(call)
				}
				if dialect == "" {
					return true
				}
				started++
				if !asked(checks, dialect, call.Pos()) {
					findings = append(findings, fmt.Sprintf("%s:%d starts a %s server without asking whether the run covers %s",
						rel, fset.Position(call.Pos()).Line, dialect, dialect))
				}
				return true
			})
		}
		return nil
	})
	if walkErr != nil {
		fmt.Printf("::error::%v\n", walkErr)
		return 1
	}
	if files == 0 {
		fmt.Printf("::error::no Go source under %s, refusing to report a clean run\n", root)
		return 1
	}

	sort.Strings(findings)
	if len(findings) > 0 {
		fmt.Printf("::error::%d database server start(s) or dialect check(s) ignore CI_DIALECT:\n", len(findings))
		for _, f := range findings {
			fmt.Println("  VIOLATION: " + f)
		}
		fmt.Println()
		fmt.Println("A run limited to one dialect starts that dialect's server alone. Earlier")
		fmt.Println("in the same function, skip when the run does not cover the dialect:")
		fmt.Println()
		fmt.Println("    testdialect.Require(t, \"mysql\")")
		fmt.Println()
		fmt.Println("or ask testdb.ShouldTest, or plugintest.SkipDialect where internal packages")
		fmt.Println("are out of reach. testdb and plugintest hosts already ask.")
		return 1
	}
	fmt.Printf("OK: %d file(s), %d database server start(s), each behind a dialect check, and %d dialect check(s), each naming a dialect.\n", files, started, asks)
	return 0
}

// imports returns the local names of the server modules a file imports, with
// the dialect each starts, the local name of the generic testcontainers
// package, and the gate functions it can call, keyed by
// "<local name>.<function>".
func imports(f *ast.File) (map[string]string, string, map[string]gate) {
	modules := map[string]string{}
	genericName := ""
	gateFns := map[string]gate{}
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		local := path[strings.LastIndex(path, "/")+1:]
		if path == generic {
			// The package is named testcontainers, not after its path.
			local = "testcontainers"
		}
		if spec.Name != nil {
			local = spec.Name.Name
		}
		if dialect, ok := servers[path]; ok {
			modules[local] = dialect
		}
		if path == generic {
			genericName = local
		}
		for _, g := range gates {
			if g.path == path {
				gateFns[local+"."+g.fn] = g
			}
		}
	}
	return modules, genericName, gateFns
}

// imageDialect returns the dialect of the database image a generic start
// names in any string literal among its arguments, or "" for another image.
func imageDialect(call *ast.CallExpr) string {
	dialect := ""
	for _, arg := range call.Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			if dialect != "" {
				return false
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			s = strings.ToLower(s)
			for _, im := range images {
				if strings.Contains(s, im.word) {
					dialect = im.dialect
					break
				}
			}
			return true
		})
	}
	return dialect
}

// qualified splits a call of the form pkg.Fn(...).
func qualified(call *ast.CallExpr) (string, string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	return id.Name, sel.Sel.Name, true
}

// literal returns the string literal at argument i, or "" when it is not one.
func literal(call *ast.CallExpr, i int) string {
	if i >= len(call.Args) {
		return ""
	}
	lit, ok := call.Args[i].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}

// asked reports whether a check before pos covers dialect. A check whose
// dialect is not a literal, as in a loop over dialects, counts for any.
func asked(checks []check, dialect string, pos token.Pos) bool {
	for _, c := range checks {
		if c.pos < pos && (c.dialect == "" || c.dialect == dialect) {
			return true
		}
	}
	return false
}
