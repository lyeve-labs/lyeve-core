// Command baregoroutines reports every go statement in this repository's
// non-test Go source.
//
// A panic in a goroutine is caught by nothing above it and ends the process.
// The engine starts a goroutine with engine.GoSafe (core.GoSafe for a plugin),
// which recovers and logs the panic with its stack under the name it was
// given. A go statement that has to stay, such as GoSafe's own or an HTTP
// server whose silent death would leave the process up with no listener,
// marks itself with the reason on the line above or at the end of its line:
//
//	// lyeve:bare-goroutine the server must take the process down with it
//
// Usage: baregoroutines <repository root>
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
	"strings"
)

const marker = "lyeve:bare-goroutine"

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" {
		fmt.Fprintln(os.Stderr, "usage: baregoroutines <repository root>")
		os.Exit(2)
	}
	root, err := filepath.Abs(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "baregoroutines: %v\n", err)
		os.Exit(2)
	}
	os.Exit(run(root))
}

func run(root string) int {
	fset := token.NewFileSet()
	var findings, exempt []string
	files, spawns := 0, 0
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		files++
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		reasons := markers(fset, f)
		ast.Inspect(f, func(n ast.Node) bool {
			g, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}
			spawns++
			line := fset.Position(g.Pos()).Line
			where := fmt.Sprintf("%s:%d", rel, line)
			reason, marked := reasons[line]
			if !marked {
				reason, marked = reasons[line-1]
			}
			switch {
			case !marked:
				findings = append(findings, where+" starts a goroutine with no recovery")
			case reason == "":
				findings = append(findings, where+" carries "+marker+" with no reason")
			default:
				exempt = append(exempt, where+": "+reason)
			}
			return true
		})
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
	sort.Strings(exempt)
	for _, e := range exempt {
		fmt.Println("  exempt " + e)
	}
	if len(findings) > 0 {
		fmt.Printf("::error::%d go statement(s) would end the process on a panic:\n", len(findings))
		for _, f := range findings {
			fmt.Println("  VIOLATION: " + f)
		}
		fmt.Println()
		fmt.Println("Start the goroutine with engine.GoSafe(\"<area>-<purpose>\", fn), or submit")
		fmt.Println("the work to a worker pool. A go statement that has to stay names why on")
		fmt.Println("the line above it:")
		fmt.Println()
		fmt.Println("    // " + marker + " <why it cannot go through GoSafe>")
		return 1
	}
	fmt.Printf("OK: %d file(s), %d go statement(s), each marked with its reason.\n", files, spawns)
	return 0
}

// markers maps each line holding the marker to the reason written after it.
func markers(fset *token.FileSet, f *ast.File) map[int]string {
	out := map[int]string{}
	for _, group := range f.Comments {
		for _, c := range group.List {
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if !strings.HasPrefix(text, marker) {
				continue
			}
			out[fset.Position(c.Pos()).Line] = strings.TrimSpace(strings.TrimPrefix(text, marker))
		}
	}
	return out
}
