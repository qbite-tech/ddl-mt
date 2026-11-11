package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestAllFunctionsHaveDocumentation keeps production functions, methods, tests,
// and named helpers documented as the repository grows. Anonymous callbacks use
// their enclosing function's contract and local comments instead of Go doc groups.
func TestAllFunctionsHaveDocumentation(t *testing.T) {
	count := 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor" || entry.Name() == "bin" || entry.Name() == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		for _, declaration := range source.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			count++
			if function.Doc == nil || !strings.HasPrefix(strings.TrimSpace(function.Doc.Text()), function.Name.Name+" ") {
				t.Errorf("%s: %s needs a Go doc comment beginning with its name", path, function.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Checked documentation for %d named functions and methods", count)
}
