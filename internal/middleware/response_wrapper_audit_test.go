package middleware_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A type that wraps http.ResponseWriter and defines WriteHeader is a response
// wrapper, and every wrapper in a chain has to pass the connection through.
// One that forgets Hijack breaks every WebSocket handshake mounted below it,
// and one that forgets Flush stalls every SSE stream. Both fail a layer away
// from the cause: the client sees a generic error event, the server logs
// "feature not supported", and nothing names the middleware responsible. The
// audit reads every Go file in this module, so a wrapper anywhere in the
// engine is held to it.
//
// wrapperAuditExempt names wrappers that can never front a live connection,
// each with the reason, keyed "dir::Type".
var wrapperAuditExempt = map[string]string{}

// A dot-prefixed directory is skipped too, by wrapperAuditSkipDir: those hold
// caches and second copies of trees the walk already reaches at their real
// path.
var wrapperAuditSkipDirs = map[string]bool{
	"vendor": true, "node_modules": true, "testdata": true,
}

func wrapperAuditSkipDir(name string) bool {
	return wrapperAuditSkipDirs[name] || strings.HasPrefix(name, ".")
}

type wrapperMethods struct {
	methods map[string]bool
	file    string
	line    int
}

func TestResponseWrappers_ForwardHijackAndFlush(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// Methods of one type can be spread over several files, so collect the
	// whole package before judging any type in it.
	seen := map[string]*wrapperMethods{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if wrapperAuditSkipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // not our job to police unparseable files
		}
		rel, _ := filepath.Rel(root, path)
		dir := filepath.ToSlash(filepath.Dir(rel))

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			typ := fn.Recv.List[0].Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}
			id, ok := typ.(*ast.Ident)
			if !ok {
				continue
			}
			key := dir + "::" + id.Name
			w := seen[key]
			if w == nil {
				w = &wrapperMethods{methods: map[string]bool{}}
				seen[key] = w
			}
			w.methods[fn.Name.Name] = true
			if fn.Name.Name == "WriteHeader" {
				w.file, w.line = filepath.ToSlash(rel), fset.Position(fn.Pos()).Line
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	var findings []string
	for key, w := range seen {
		if !w.methods["WriteHeader"] {
			continue
		}
		if reason, exempt := wrapperAuditExempt[key]; exempt {
			t.Logf("exempt %s: %s", key, reason)
			continue
		}
		var missing []string
		for _, m := range []string{"Hijack", "Flush"} {
			if !w.methods[m] {
				missing = append(missing, m)
			}
		}
		if len(missing) > 0 {
			findings = append(findings, w.file+":"+itoa(w.line)+" "+key+" does not forward "+strings.Join(missing, " or "))
		}
	}
	sort.Strings(findings)

	if len(findings) > 0 {
		t.Fatalf("response wrapper(s) that would break upgrades or streaming below them:\n  %s\n\n"+
			"Forward the method to the embedded writer, or add the type to wrapperAuditExempt with the reason it can never front a live connection.",
			strings.Join(findings, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// moduleRoot walks up from the test's directory to the go.mod of the module
// under test.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("response wrapper audit: no go.mod above the test directory")
		}
		dir = parent
	}
}
