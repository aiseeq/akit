package shellcmd

import (
	"slices"

	"mvdan.cc/sh/v3/syntax"
)

// Assign is a variable assignment of a command: a command made only of assignments (`S=x`),
// an `export`, `declare`, `typeset` or `readonly` declaration, or `local` inside a function body.
type Assign struct {
	// Name is the variable name.
	Name string
	// Value is the literal value without quotes; a value with an expansion keeps its source
	// text without quotes (`"$S/x"` gives `$S/x`). An empty assignment (`S=`) gives "".
	// A tilde is kept as written, quoted or not, and `$'…'` escapes are not decoded.
	Value string
	// Append marks `S+=x`: Value is added to the previous value.
	Append bool
	// Offset is the byte offset of the assignment in the command.
	Offset int
	// ScopeStart and ScopeEnd bound the innermost subshell, command or process substitution,
	// pipeline part, background job, coprocess or function body that holds the assignment;
	// both are -1 when the assignment is made by the shell running the command.
	ScopeStart, ScopeEnd int
}

// VisibleAt reports whether the assignment may be set for a word at byte offset off: it comes
// before off and is made by the shell running the command, or off is inside its scope.
// The check is static: a branch or a loop body that does not run, or a function that is
// called later, is not followed, and a here-document body counts at its place in the text,
// not when its command runs.
func (a Assign) VisibleAt(off int) bool {
	if a.Offset >= off {
		return false
	}
	return a.ScopeStart < 0 || (a.ScopeStart <= off && off < a.ScopeEnd)
}

// Assigns returns the assignments of cmd in text order, each with the scope that holds it.
// Prefix assignments (`S=x cmd`) change only the environment of their command and are left out,
// as are declarations without a value (`export T`), arrays, array elements, `for` loop variables
// and `local` outside a function. Scripts passed as quoted arguments (`bash -c '…'`) and
// declarations behind another word (`command export S=x`) are not read.
func Assigns(cmd string) ([]Assign, error) {
	file, full, err := parseShellVariant(cmd, false)
	if err != nil {
		return nil, err
	}
	scopes, functions := assignScopes(file)
	var out []Assign
	add := func(assigns []*syntax.Assign) {
		for _, a := range assigns {
			if a.Name == nil || a.Naked || a.Index != nil || a.Array != nil {
				continue
			}
			value := ""
			if a.Value != nil {
				value = assignValue(a, full)
			}
			off := int(a.Pos().Offset())
			start, end := innermostSpan(scopes, off)
			out = append(out, Assign{Name: a.Name.Value, Value: value, Append: a.Append, Offset: off, ScopeStart: start, ScopeEnd: end})
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			if len(n.Args) == 0 {
				add(n.Assigns)
			}
		case *syntax.DeclClause:
			if n.Variant.Value != "local" || insideSpan(functions, int(n.Pos().Offset())) {
				add(n.Args)
			}
		}
		return true
	})
	slices.SortStableFunc(out, func(a, b Assign) int { return a.Offset - b.Offset })
	return out, nil
}

type span struct{ start, end int }

// assignScopes collects the spans whose assignments do not reach the shell running the command,
// and the function bodies among them.
func assignScopes(file *syntax.File) (scopes, functions []span) {
	of := func(n syntax.Node) span { return span{int(n.Pos().Offset()), int(n.End().Offset())} }
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst, *syntax.CoprocClause:
			scopes = append(scopes, of(n))
		case *syntax.FuncDecl:
			if n.Body != nil {
				scopes = append(scopes, of(n.Body))
				functions = append(functions, of(n.Body))
			}
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				scopes = append(scopes, of(n.X), of(n.Y))
			}
		case *syntax.Stmt:
			if n.Background || n.Coprocess || n.Disown {
				scopes = append(scopes, of(n))
			}
		}
		return true
	})
	return scopes, functions
}

// innermostSpan is the smallest span holding off, or -1, -1 when none does.
func innermostSpan(spans []span, off int) (start, end int) {
	start, end = -1, -1
	for _, s := range spans {
		if s.start <= off && off < s.end && (start < 0 || s.end-s.start < end-start) {
			start, end = s.start, s.end
		}
	}
	return start, end
}

func insideSpan(spans []span, off int) bool {
	start, _ := innermostSpan(spans, off)
	return start >= 0
}
