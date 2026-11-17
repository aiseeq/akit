package shellcmd

import (
	"maps"
	"slices"
	"strings"

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
	// Unknown marks a possible assignment whose value cannot be determined statically, such as
	// an array assignment or a call to a function that may change the variable. This analysis does
	// not track writes through read, printf -v, mapfile, ${S:=…}, (( S=1 )), let, source or ., aliases,
	// namerefs, eval assignments such as eval 'S=…', or unset.
	Unknown bool
	// Offset is the byte offset of the assignment in the command.
	Offset int
	// ScopeStart and ScopeEnd bound the innermost subshell, command or process substitution,
	// pipeline part, background job, coprocess or function body that holds the assignment;
	// both are -1 when the assignment is made by the shell running the command.
	ScopeStart, ScopeEnd int
}

// ScopeKind identifies a shell command region whose assignments do not reach
// the shell running the command. Trap, mapfile -C, aliases and lastpipe behavior
// are not modeled here; consumers can combine Scopes with their own shell rules.
type ScopeKind int

const (
	// ScopeFunctionBody is a declared function's body.
	ScopeFunctionBody ScopeKind = iota
	// ScopeSubshell is a parenthesized command.
	ScopeSubshell
	// ScopeCmdSubst is a command substitution.
	ScopeCmdSubst
	// ScopeProcSubst is a process substitution.
	ScopeProcSubst
	// ScopeCoproc is a coprocess command.
	ScopeCoproc
	// ScopePipePart is one pipeline command.
	ScopePipePart
	// ScopeBackground is a background command.
	ScopeBackground
)

func (k ScopeKind) String() string {
	return [...]string{"FunctionBody", "Subshell", "CmdSubst", "ProcSubst", "Coproc", "PipePart", "Background"}[k]
}

// Scope bounds a command region in which assignments do not reach the shell
// running the command. Function bodies include their name and declaration offset;
// other kinds have an empty Name and DeclOffset of -1. Trap, mapfile -C, aliases
// and lastpipe behavior are left to consumers to infer from these regions.
type Scope struct {
	Start, End int
	Kind       ScopeKind
	Name       string
	DeclOffset int
	Isolated   bool
	Last       bool
}

// VisibleAt reports whether the assignment may be set for a word at byte offset off: it comes
// before off and is made by the shell running the command, or off is inside its scope.
// The check is static: a branch or a loop body that does not run, or a function that is
// called later, is not followed except for recognized function calls. A here-document body
// counts at its place in the text, not when its command runs.
func (a Assign) VisibleAt(off int) bool {
	if a.Offset >= off {
		return false
	}
	return a.ScopeStart < 0 || (a.ScopeStart <= off && off < a.ScopeEnd)
}

// Assigns returns assignments of cmd in text order, including Unknown entries for array and
// element assignments, for-loop variables, and calls to functions whose bodies may change
// variables in the calling shell. Unknown entries have an empty Value and Append=false.
// Prefix assignments (`S=x cmd`) change only the environment of their command and are left out,
// as are declarations without a value (`export T`) and `local` outside a function. Literal,
// quoted and escaped function names are recognized; `eval` checks for function names as separate
// words in literal arguments, and an argument substitution to `eval` may call any function that
// changes a variable. A dynamic command word may also call any such function. Scripts passed as
// quoted arguments (`bash -c '…'`) and declarations behind another
// word (`command export S=x`) are not read. `command f` and `builtin f` are treated as calls to
// those command names, not as calls to f. Writes through `read`, `printf -v`, `mapfile`, `${S:=…}`,
// `(( S=1 ))`, `let`, `source` or `.`, aliases, namerefs, eval assignment strings, and `unset` are
// not tracked. Trap, mapfile -C, aliases and lastpipe are left to consumers of
// Scopes, which reports the command regions and their kinds.
func Assigns(cmd string) ([]Assign, error) {
	file, full, err := parseShellVariant(cmd, false)
	if err != nil {
		return nil, err
	}
	regions := commandScopes(file)
	scopes := make([]span, len(regions.scopes))
	for i, scope := range regions.scopes {
		scopes[i] = span{scope.Start, scope.End}
	}
	functions := regions.functions
	functionChanges := assignFunctionChanges(file, scopes)
	var out []Assign
	add := func(assigns []*syntax.Assign) {
		for _, a := range assigns {
			if a.Name == nil || a.Naked {
				continue
			}
			if a.Index != nil || a.Array != nil {
				off := int(a.Pos().Offset())
				start, end := innermostSpan(scopes, off)
				out = append(out, Assign{Name: a.Name.Value, Unknown: true, Offset: off, ScopeStart: start, ScopeEnd: end})
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
			} else {
				for _, variable := range calledFunctionChanges(n.Args, functionChanges) {
					off := int(n.Args[0].Pos().Offset())
					start, end := innermostSpan(scopes, off)
					out = append(out, Assign{Name: variable, Unknown: true, Offset: off, ScopeStart: start, ScopeEnd: end})
				}
			}
		case *syntax.DeclClause:
			if n.Variant.Value != "local" || insideSpan(functions, int(n.Pos().Offset())) {
				add(n.Args)
			}
		case *syntax.ForClause:
			if loop, ok := n.Loop.(*syntax.WordIter); ok && loop.Name != nil {
				off := int(loop.Name.Pos().Offset())
				start, end := innermostSpan(scopes, off)
				out = append(out, Assign{Name: loop.Name.Value, Unknown: true, Offset: off, ScopeStart: start, ScopeEnd: end})
			}
		}
		return true
	})
	slices.SortStableFunc(out, func(a, b Assign) int { return a.Offset - b.Offset })
	return out, nil
}

// Scopes returns regions whose assignments do not reach the shell running cmd.
// Trap, mapfile -C, aliases and lastpipe are left to consumers to infer from the
// returned kinds and bounds.
func Scopes(cmd string) ([]Scope, error) {
	file, _, err := parseShellVariant(cmd, false)
	if err != nil {
		return nil, err
	}
	return commandScopes(file).scopes, nil
}

// assignFunction contains statically inferred shell-visible writes and known function calls.
type assignFunction struct {
	definitions []assignDefinition
	calls       []string
	vars        map[string]bool
}

type assignDefinition struct {
	body     *syntax.Stmt
	span     span
	isolated bool
}

type localBinding struct {
	name   string
	offset int
}

// assignFunctionChanges returns variables that each function can change in its caller. Calls
// between functions are resolved to a fixed point so a caller inherits its callees' changes.
func assignFunctionChanges(file *syntax.File, scopes []span) map[string][]string {
	funcs := assignFunctionDecls(file)
	for _, name := range slices.Sorted(maps.Keys(funcs)) {
		f := funcs[name]
		for i := range f.definitions {
			definition := &f.definitions[i]
			if !definition.isolated {
				collectFunctionChanges(f, definition, funcs, functionLocals(definition.body, definition.span, scopes), scopes)
			}
		}
	}
	propagateFunctionChanges(funcs)
	out := make(map[string][]string, len(funcs))
	for name, f := range funcs {
		for variable := range f.vars {
			out[name] = append(out[name], variable)
		}
		slices.Sort(out[name])
	}
	return out
}

func assignFunctionDecls(file *syntax.File) map[string]*assignFunction {
	funcs := make(map[string]*assignFunction)
	syntax.Walk(file, func(node syntax.Node) bool {
		f, ok := node.(*syntax.FuncDecl)
		if !ok || f.Body == nil {
			return true
		}
		names := append([]*syntax.Lit(nil), f.Names...)
		if f.Name != nil {
			names = append(names, f.Name)
		}
		for _, name := range names {
			_, isolated := f.Body.Cmd.(*syntax.Subshell)
			function := funcs[name.Value]
			if function == nil {
				function = &assignFunction{vars: make(map[string]bool)}
				funcs[name.Value] = function
			}
			function.definitions = append(function.definitions, assignDefinition{body: f.Body, span: functionBodySpan(f.Body), isolated: isolated})
		}
		return true
	})
	return funcs
}

func functionLocals(body *syntax.Stmt, fn span, scopes []span) []localBinding {
	var locals []localBinding
	collectExecutedLocals(body, true, fn, scopes, &locals)
	return locals
}

func collectExecutedLocals(stmt *syntax.Stmt, executes bool, fn span, scopes []span, locals *[]localBinding) {
	if stmt == nil || !executes {
		return
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.DeclClause:
		collectLocalDeclaration(cmd, fn, scopes, locals)
	case *syntax.Block:
		collectExecutedLocalsIn(cmd.Stmts, executes, fn, scopes, locals)
	case *syntax.BinaryCmd:
		collectBinaryLocals(cmd, executes, fn, scopes, locals)
	case *syntax.IfClause:
		collectIfLocals(cmd, executes, fn, scopes, locals)
	case *syntax.WhileClause:
		collectWhileLocals(cmd, executes, fn, scopes, locals)
	case *syntax.ForClause:
		collectExecutedLocalsIn(cmd.Do, false, fn, scopes, locals)
	case *syntax.CaseClause:
		collectCaseLocals(cmd, fn, scopes, locals)
	}
}

func collectLocalDeclaration(decl *syntax.DeclClause, fn span, scopes []span, locals *[]localBinding) {
	if !isLocalDeclaration(decl) || declHasGlobal(decl) || !inFunctionShell(scopes, fn, int(decl.Pos().Offset())) {
		return
	}
	for _, a := range decl.Args {
		if a.Name != nil {
			*locals = append(*locals, localBinding{name: a.Name.Value, offset: int(a.Pos().Offset())})
		}
	}
}

func collectBinaryLocals(cmd *syntax.BinaryCmd, executes bool, fn span, scopes []span, locals *[]localBinding) {
	if cmd.Op == syntax.AndStmt || cmd.Op == syntax.OrStmt {
		collectExecutedLocals(cmd.X, executes, fn, scopes, locals)
		collectExecutedLocals(cmd.Y, false, fn, scopes, locals)
	}
}

func collectIfLocals(cmd *syntax.IfClause, executes bool, fn span, scopes []span, locals *[]localBinding) {
	collectExecutedLocalsIn(cmd.Cond, executes, fn, scopes, locals)
	collectExecutedLocalsIn(cmd.Then, false, fn, scopes, locals)
}

func collectWhileLocals(cmd *syntax.WhileClause, executes bool, fn span, scopes []span, locals *[]localBinding) {
	collectExecutedLocalsIn(cmd.Cond, executes, fn, scopes, locals)
	collectExecutedLocalsIn(cmd.Do, false, fn, scopes, locals)
}

func collectCaseLocals(cmd *syntax.CaseClause, fn span, scopes []span, locals *[]localBinding) {
	for _, item := range cmd.Items {
		collectExecutedLocalsIn(item.Stmts, false, fn, scopes, locals)
	}
}

func collectExecutedLocalsIn(stmts []*syntax.Stmt, executes bool, fn span, scopes []span, locals *[]localBinding) {
	for _, stmt := range stmts {
		collectExecutedLocals(stmt, executes, fn, scopes, locals)
	}
}

func localShadows(locals []localBinding, name string, offset int) bool {
	for _, local := range locals {
		if local.name == name && local.offset < offset {
			return true
		}
	}
	return false
}

func isLocalDeclaration(decl *syntax.DeclClause) bool {
	return decl.Variant.Value == "local" || decl.Variant.Value == "declare" || decl.Variant.Value == "typeset"
}

func nestedFunctionOf(node syntax.Node, body *syntax.Stmt) bool {
	nested, ok := node.(*syntax.FuncDecl)
	return ok && nested.Body != nil && nested.Body != body
}

func collectFunctionChanges(f *assignFunction, definition *assignDefinition, funcs map[string]*assignFunction, locals []localBinding, scopes []span) {
	syntax.Walk(definition.body, func(node syntax.Node) bool {
		if nestedFunctionOf(node, definition.body) {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			collectCallChanges(f, definition, n, funcs, locals, scopes)
		case *syntax.DeclClause:
			collectDeclChanges(f, definition, n, locals, scopes)
		case *syntax.ForClause:
			collectLoopChanges(f, definition, n, locals, scopes)
		}
		return true
	})
}

func collectCallChanges(f *assignFunction, definition *assignDefinition, call *syntax.CallExpr, funcs map[string]*assignFunction, locals []localBinding, scopes []span) {
	if len(call.Args) == 0 {
		for _, a := range call.Assigns {
			if a.Name != nil && !a.Naked && !localShadows(locals, a.Name.Value, int(a.Pos().Offset())) && inFunctionShell(scopes, definition.span, int(a.Pos().Offset())) {
				f.vars[a.Name.Value] = true
			}
		}
		return
	}
	f.calls = append(f.calls, calledFunctionNames(call.Args, slices.Sorted(maps.Keys(funcs)))...)
}

func calledFunctionChanges(args []*syntax.Word, changes map[string][]string) []string {
	variables := make(map[string]bool)
	for _, function := range calledFunctionNames(args, slices.Sorted(maps.Keys(changes))) {
		for _, variable := range changes[function] {
			variables[variable] = true
		}
	}
	return slices.Sorted(maps.Keys(variables))
}

func calledFunctionNames(args []*syntax.Word, functions []string) []string {
	if len(args) == 0 {
		return nil
	}
	name, literal := wordValue(args[0])
	if !literal {
		return functions
	}
	evalArgs, isEval := evalArguments(args, name)
	if isEval {
		for _, arg := range evalArgs {
			if _, ok := wordValue(arg); !ok {
				return functions
			}
		}
		return evalFunctionNames(evalArgs, functions)
	}
	if slices.Contains(functions, name) {
		return []string{name}
	}
	return nil
}

func evalArguments(args []*syntax.Word, name string) ([]*syntax.Word, bool) {
	if name == "eval" {
		return args[1:], true
	}
	if (name == "builtin" || name == "command") && len(args) > 1 {
		if next, ok := wordValue(args[1]); ok && next == "eval" {
			return args[2:], true
		}
	}
	return nil, false
}

func evalFunctionNames(args []*syntax.Word, functions []string) []string {
	found := make(map[string]bool)
	for _, arg := range args {
		text, _ := wordValue(arg)
		for _, name := range functions {
			if evalTextHasWord(text, name) {
				found[name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(found))
}

func evalTextHasWord(text, name string) bool {
	for from := 0; from < len(text); {
		rel := strings.Index(text[from:], name)
		if rel < 0 {
			return false
		}
		start := from + rel
		end := start + len(name)
		if (start == 0 || evalDelimiter(text[start-1])) && (end == len(text) || evalDelimiter(text[end])) {
			return true
		}
		from = start + 1
	}
	return false
}

func evalDelimiter(char byte) bool {
	return char <= ' ' || strings.ContainsRune(";&|()'\"", rune(char))
}

func collectDeclChanges(f *assignFunction, definition *assignDefinition, decl *syntax.DeclClause, locals []localBinding, scopes []span) {
	global := declHasGlobal(decl)
	for _, a := range decl.Args {
		if a.Name == nil || a.Naked || localShadows(locals, a.Name.Value, int(a.Pos().Offset())) && !global || !inFunctionShell(scopes, definition.span, int(a.Pos().Offset())) {
			continue
		}
		if (decl.Variant.Value == "local" || decl.Variant.Value == "declare" || decl.Variant.Value == "typeset") && !global {
			continue
		}
		f.vars[a.Name.Value] = true
	}
}

func collectLoopChanges(f *assignFunction, definition *assignDefinition, loop *syntax.ForClause, locals []localBinding, scopes []span) {
	words, ok := loop.Loop.(*syntax.WordIter)
	if ok && words.Name != nil && !localShadows(locals, words.Name.Value, int(words.Name.Pos().Offset())) && inFunctionShell(scopes, definition.span, int(words.Name.Pos().Offset())) {
		f.vars[words.Name.Value] = true
	}
}

func propagateFunctionChanges(funcs map[string]*assignFunction) {
	names := slices.Sorted(maps.Keys(funcs))
	changed := true
	for changed {
		changed = false
		for _, name := range names {
			f := funcs[name]
			for _, call := range f.calls {
				changed = mergeFunctionChanges(f, funcs[call]) || changed
			}
		}
	}
}

func mergeFunctionChanges(target, source *assignFunction) bool {
	changed := false
	for name := range source.vars {
		if !target.vars[name] {
			target.vars[name] = true
			changed = true
		}
	}
	return changed
}

func inFunctionShell(scopes []span, fn span, off int) bool {
	start, end := innermostSpan(scopes, off)
	return start == fn.start && end == fn.end
}

func declHasGlobal(decl *syntax.DeclClause) bool {
	for _, a := range decl.Args {
		if a.Name == nil && a.Value != nil {
			flag, literal := wordValue(a.Value)
			if !literal {
				return true
			}
			if flag == "--" {
				return false
			}
			if len(flag) > 1 && flag[0] == '-' && strings.Contains(flag[1:], "g") {
				return true
			}
		}
	}
	return false
}

type span struct{ start, end int }

type scopeSet struct {
	scopes    []Scope
	functions []span
}

// commandScopes collects all assignment scopes once for both Scopes and Assigns.
func commandScopes(file *syntax.File) scopeSet {
	var out scopeSet
	functionSubshells := isolatedFunctionSubshells(file)
	pipeChildren := nestedPipelineNodes(file)
	syntax.Walk(file, func(node syntax.Node) bool {
		binary, ok := node.(*syntax.BinaryCmd)
		if ok && isPipeline(binary) && !pipeChildren[binary] {
			out.scopes = append(out.scopes, pipelineScopes(binary)...)
		}
		return true
	})
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Subshell:
			if !functionSubshells[n] {
				out.scopes = append(out.scopes, nodeScope(n, ScopeSubshell))
			}
		case *syntax.CmdSubst:
			out.scopes = append(out.scopes, nodeScope(n, ScopeCmdSubst))
		case *syntax.ProcSubst:
			out.scopes = append(out.scopes, nodeScope(n, ScopeProcSubst))
		case *syntax.CoprocClause:
			out.scopes = append(out.scopes, nodeScope(n, ScopeCoproc))
		case *syntax.FuncDecl:
			body, scopes := functionScopes(n)
			if body != nil {
				out.functions = append(out.functions, *scopes)
				out.scopes = append(out.scopes, body...)
			}
		case *syntax.Stmt:
			if n.Background {
				out.scopes = append(out.scopes, nodeScope(n, ScopeBackground))
			}
		}
		return true
	})
	sortScopes(out.scopes)
	return out
}

func nodeScope(node syntax.Node, kind ScopeKind) Scope {
	return Scope{Start: int(node.Pos().Offset()), End: int(node.End().Offset()), Kind: kind, DeclOffset: -1}
}

func isPipeline(binary *syntax.BinaryCmd) bool {
	return binary.Op == syntax.Pipe || binary.Op == syntax.PipeAll
}

func nestedPipelineNodes(file *syntax.File) map[*syntax.BinaryCmd]bool {
	children := make(map[*syntax.BinaryCmd]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		binary, ok := node.(*syntax.BinaryCmd)
		if !ok || !isPipeline(binary) {
			return true
		}
		for _, stmt := range []*syntax.Stmt{binary.X, binary.Y} {
			if child, ok := stmt.Cmd.(*syntax.BinaryCmd); ok && isPipeline(child) {
				children[child] = true
			}
		}
		return true
	})
	return children
}

func isolatedFunctionSubshells(file *syntax.File) map[*syntax.Subshell]bool {
	isolated := make(map[*syntax.Subshell]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		function, ok := node.(*syntax.FuncDecl)
		if ok && function.Body != nil {
			if subshell, ok := function.Body.Cmd.(*syntax.Subshell); ok {
				isolated[subshell] = true
			}
		}
		return true
	})
	return isolated
}

func functionScopes(function *syntax.FuncDecl) ([]Scope, *span) {
	if function.Body == nil || function.Body.Cmd == nil {
		return nil, nil
	}
	body := functionBodySpan(function.Body)
	names := function.Names
	if function.Name != nil {
		names = []*syntax.Lit{function.Name}
	}
	decl := int(function.Pos().Offset())
	_, isolated := function.Body.Cmd.(*syntax.Subshell)
	scopes := make([]Scope, 0, len(names))
	for _, name := range names {
		scopes = append(scopes, Scope{Start: body.start, End: body.end, Kind: ScopeFunctionBody, Name: name.Value, DeclOffset: decl, Isolated: isolated})
	}
	return scopes, &body
}

func functionBodySpan(body *syntax.Stmt) span {
	return span{int(body.Cmd.Pos().Offset()), int(body.Cmd.End().Offset())}
}

func pipelineScopes(binary *syntax.BinaryCmd) []Scope {
	var parts []*syntax.Stmt
	appendPipelineParts(binary, &parts)
	scopes := make([]Scope, 0, len(parts))
	for i, part := range parts {
		scopes = append(scopes, Scope{Start: int(part.Pos().Offset()), End: int(part.End().Offset()), Kind: ScopePipePart, DeclOffset: -1, Last: i == len(parts)-1})
	}
	return scopes
}

func appendPipelineParts(binary *syntax.BinaryCmd, parts *[]*syntax.Stmt) {
	for _, stmt := range []*syntax.Stmt{binary.X, binary.Y} {
		if child, ok := stmt.Cmd.(*syntax.BinaryCmd); ok && isPipeline(child) {
			appendPipelineParts(child, parts)
			continue
		}
		*parts = append(*parts, stmt)
	}
}

func sortScopes(scopes []Scope) {
	slices.SortStableFunc(scopes, func(a, b Scope) int {
		if a.Start != b.Start {
			return a.Start - b.Start
		}
		return b.End - a.End
	})
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
