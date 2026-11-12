// Command logrestore reports every test function in this repository that
// makes a logger the default without putting back the log package's output.
//
// slog.SetDefault with any handler but the process's first one also points
// the log package at that handler and clears its flags. Making the old logger
// the default again does not undo either, so every later line of the default
// logger in the test binary goes into the handler of a test that has ended:
// usually a buffer nobody reads, so a later failure prints none of its own
// log lines. A function that calls slog.SetDefault in a test file, or in any
// file when it registers a Cleanup, also calls log.SetOutput, or a function of
// its own directory that does, which is how it puts the writer back:
//
//	prev, writer, flags := slog.Default(), log.Writer(), log.Flags()
//	slog.SetDefault(logger)
//	t.Cleanup(func() {
//		slog.SetDefault(prev)
//		log.SetOutput(writer)
//		log.SetFlags(flags)
//	})
//
// A function that leaves the output on purpose, such as a TestMain that sets
// the default for the whole binary, marks itself with the reason on the line
// above the call or at the end of its line:
//
//	// lyeve:log-output-kept the whole binary logs through this handler
//
// Usage: logrestore <repository root>
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

const marker = "lyeve:log-output-kept"

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" {
		fmt.Fprintln(os.Stderr, "usage: logrestore <repository root>")
		os.Exit(2)
	}
	root, err := filepath.Abs(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "logrestore: %v\n", err)
		os.Exit(2)
	}
	os.Exit(run(filepath.Dir(root), []string{filepath.Base(root)}))
}

// parsed is one Go file the scan read.
type parsed struct {
	rel    string
	isTest bool
	file   *ast.File
}

func run(workspace string, roots []string) int {
	fset := token.NewFileSet()
	byDir := map[string][]parsed{}
	files := 0
	for _, name := range roots {
		root := filepath.Join(workspace, name)
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				base := d.Name()
				if path != root && (strings.HasPrefix(base, ".") || base == "vendor" || base == "node_modules" || base == "testdata") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			files++
			rel, _ := filepath.Rel(workspace, path)
			rel = filepath.ToSlash(rel)
			byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], parsed{rel: rel, isTest: strings.HasSuffix(path, "_test.go"), file: f})
			return nil
		})
		if walkErr != nil {
			fmt.Printf("::error::%v\n", walkErr)
			return 1
		}
	}
	if files == 0 {
		fmt.Printf("::error::no Go source under %s, refusing to report a clean run\n", workspace)
		return 1
	}

	var findings, exempt []string
	sets := 0
	for _, dir := range byDir {
		// A function that puts the output back is a restorer, and a function
		// that calls one of its own directory's restorers by name counts as
		// putting it back too, which is how a helper does it for its callers.
		restorers := map[string]bool{}
		for _, p := range dir {
			logName := importName(p.file, "log", "log")
			for _, decl := range p.file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil && fn.Recv == nil && callsSetOutput(fn.Body, logName) {
					restorers[fn.Name.Name] = true
				}
			}
		}
		for _, p := range dir {
			slogName, logName := importName(p.file, "log/slog", "slog"), importName(p.file, "log", "log")
			if slogName == "" {
				continue
			}
			reasons := markers(fset, p.file)
			for _, decl := range p.file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				var setAt []token.Pos
				restores, cleanup := false, false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if id, ok := call.Fun.(*ast.Ident); ok && restorers[id.Name] {
						restores = true
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					pkg, _ := sel.X.(*ast.Ident)
					switch {
					case pkg != nil && pkg.Name == slogName && sel.Sel.Name == "SetDefault":
						setAt = append(setAt, call.Pos())
					case pkg != nil && logName != "" && pkg.Name == logName && sel.Sel.Name == "SetOutput":
						restores = true
					case sel.Sel.Name == "Cleanup":
						cleanup = true
					}
					return true
				})
				if len(setAt) == 0 || !(p.isTest || cleanup) {
					continue
				}
				sets += len(setAt)
				if restores {
					continue
				}
				line := fset.Position(setAt[0]).Line
				where := fmt.Sprintf("%s:%d", p.rel, line)
				reason, marked := reasons[line]
				if !marked {
					reason, marked = reasons[line-1]
				}
				switch {
				case !marked:
					findings = append(findings, where+" "+fn.Name.Name+" makes a logger the default and never puts back the log package's output")
				case reason == "":
					findings = append(findings, where+" carries "+marker+" with no reason")
				default:
					exempt = append(exempt, where+": "+reason)
				}
			}
		}
	}

	sort.Strings(findings)
	sort.Strings(exempt)
	for _, e := range exempt {
		fmt.Println("  exempt " + e)
	}
	if len(findings) > 0 {
		fmt.Printf("::error::%d function(s) leave the log package writing to a logger a test made the default:\n", len(findings))
		for _, f := range findings {
			fmt.Println("  VIOLATION: " + f)
		}
		fmt.Println()
		fmt.Println("Save log.Writer() and log.Flags() before slog.SetDefault, and put both back")
		fmt.Println("with the old default when the test ends. A function that keeps the output on")
		fmt.Println("purpose names why on the line above the call:")
		fmt.Println()
		fmt.Println("    // " + marker + " <why>")
		return 1
	}
	fmt.Printf("OK: %d file(s), %d slog.SetDefault call(s) in tests, each putting back the log output.\n", files, sets)
	return 0
}

// callsSetOutput reports whether body calls the log package's SetOutput.
func callsSetOutput(body *ast.BlockStmt, logName string) bool {
	if logName == "" {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == logName && sel.Sel.Name == "SetOutput" {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// importName is the name f refers to path by, or "" when f does not import
// it. A blank or dot import counts as not imported.
func importName(f *ast.File, path, def string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != path {
			continue
		}
		if imp.Name == nil {
			return def
		}
		if imp.Name.Name == "_" || imp.Name.Name == "." {
			return ""
		}
		return imp.Name.Name
	}
	return ""
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
