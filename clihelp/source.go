package clihelp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

// TestReporter is the subset of testing.T used by validation helpers.
type TestReporter interface {
	Helper()
	Errorf(string, ...any)
	Fatalf(string, ...any)
}

// CheckFlags checks that declared keys parse and derives value flags from missing-value behavior.
func CheckFlags(t TestReporter, help Help, argsFor, argsWithoutValue func(string) []string, parse func([]string) error) {
	t.Helper()
	if len(help.Commands) != 1 {
		t.Fatalf("flag check requires one root command, got %d", len(help.Commands))
		return
	}
	missingValueErrors := make(map[string]error)
	for _, flag := range help.Commands[0].Flags {
		if err := parse(argsFor(flag)); err != nil {
			t.Errorf("declared flag %q was rejected: %v", flag, err)
		}
		missingValueErrors[flag] = parse(argsWithoutValue(flag))
	}
	declaredValues := make(map[string]bool)
	for _, flag := range help.Commands[0].ValueFlags {
		declaredValues[flag] = true
		if missingValueErrors[flag] == nil {
			t.Errorf("value flag %q parses without a value", flag)
		}
	}
	for flag, err := range missingValueErrors {
		if err != nil && !declaredValues[flag] {
			t.Errorf("parser rejects %q without a value (%v) but help does not declare it as a value flag", flag, err)
		}
	}
}

var keyCallNames = map[string]bool{"HasPrefix": true, "TrimPrefix": true, "CutPrefix": true, "EqualFold": true}

// CheckSource compares declared flags with keys found in named parser functions and their local helpers.
func CheckSource(t TestReporter, dir string, help Help, parserFunctions []string, exceptions map[string]string) {
	t.Helper()
	known := sourceKeys(t, dir, parserFunctions)
	if len(help.Commands) != 1 {
		t.Fatalf("source check requires one root command, got %d", len(help.Commands))
		return
	}
	declared := make(map[string]bool)
	for _, flag := range help.Commands[0].Flags {
		declared[flag] = true
	}
	for key := range known {
		if declared[key] || exceptions[key] != "" {
			continue
		}
		t.Errorf("source recognizes %q but help does not declare it", key)
	}
	for key := range declared {
		if known[key] || exceptions[key] != "" {
			continue
		}
		t.Errorf("help declares %q but source does not recognize it", key)
	}
}

func sourceKeys(t TestReporter, dir string, parserFunctions []string) map[string]bool {
	functions, values := packageDeclarations(t, dir)
	queue := append([]string(nil), parserFunctions...)
	visited := make(map[string]bool)
	keys := make(map[string]bool)
	usedValues := make(map[string]bool)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if visited[name] {
			continue
		}
		visited[name] = true
		for _, fn := range functions[name] {
			queue = append(queue, inspectParserFunction(fn, keys, usedValues, functions, visited)...)
		}
	}
	for name := range usedValues {
		if value := values[name]; value != nil {
			collectStrings(value, keys)
		}
	}
	return keys
}

func packageDeclarations(t TestReporter, dir string) (map[string][]*ast.FuncDecl, map[string]ast.Expr) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("list source: %v", err)
		return nil, nil
	}
	fset := token.NewFileSet()
	functions := make(map[string][]*ast.FuncDecl)
	values := make(map[string]ast.Expr)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
			return nil, nil
		}
		recordDeclarations(f.Decls, functions, values)
	}
	return functions, values
}

func recordDeclarations(decls []ast.Decl, functions map[string][]*ast.FuncDecl, values map[string]ast.Expr) {
	for _, decl := range decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			functions[d.Name.Name] = append(functions[d.Name.Name], d)
		case *ast.GenDecl:
			recordValues(d, values)
		}
	}
}

func recordValues(decl *ast.GenDecl, values map[string]ast.Expr) {
	for _, spec := range decl.Specs {
		v, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range v.Names {
			if i < len(v.Values) {
				values[name.Name] = v.Values[i]
			}
		}
	}
}

func inspectParserFunction(fn *ast.FuncDecl, keys, usedValues map[string]bool, functions map[string][]*ast.FuncDecl, visited map[string]bool) []string {
	var calls []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		collectParserNode(n, keys, usedValues)
		if id, ok := n.(*ast.Ident); ok {
			// Help text is not parser input. Ignore references encountered when
			// checking main functions that print usage or machine-readable help.
			if id.Name != "usage" && id.Name != "machineHelp" {
				usedValues[id.Name] = true
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		called := calledName(call.Fun)
		if len(functions[called]) > 0 && !visited[called] {
			calls = append(calls, called)
		}
		return true
	})
	return calls
}

func calledName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	default:
		return ""
	}
}

func collectParserNode(n ast.Node, keys, usedValues map[string]bool) {
	add := func(e ast.Expr) {
		collectStrings(e, keys)
		ast.Inspect(e, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				usedValues[id.Name] = true
			}
			return true
		})
	}
	switch n := n.(type) {
	case *ast.CaseClause:
		for _, e := range n.List {
			add(e)
		}
	case *ast.BinaryExpr:
		if n.Op == token.EQL || n.Op == token.NEQ {
			add(n.X)
			add(n.Y)
		}
	case *ast.CompositeLit:
		collectStrings(n, keys)
	case *ast.CallExpr:
		name := calledName(n.Fun)
		if keyCallNames[name] {
			for _, arg := range n.Args {
				add(arg)
			}
		}
	}
}

func collectStrings(node ast.Node, keys map[string]bool) {
	ast.Inspect(node, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err == nil && strings.HasPrefix(value, "-") {
			key, _, _ := strings.Cut(value, "=")
			keys[key] = true
		}
		return true
	})
}
