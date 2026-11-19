package shellcmd

import (
	"maps"
	"slices"
	"sort"
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
	// Tilde marks expansion at the start of Value and after each unquoted colon,
	// or in each array element. Bash applies this to declaration values and S+= values,
	// including in POSIX mode.
	Tilde Tilde
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
// the shell running the command. Bash's ${ ...; } command substitutions execute
// in the current shell; local declarations inside them are tracked separately.
// Trap, mapfile -C, aliases and lastpipe behavior are not modeled here.
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
// running the command. Pipeline, background, and coprocess scopes also include
// separate ranges for their here-document bodies. Function bodies include their
// name and declaration offset; other kinds have an empty Name and DeclOffset of
// -1. Bash funsubs have no general assignment scope, though local declarations
// inside them are tracked by Assigns. Trap, mapfile -C, aliases and lastpipe
// behavior are left to consumers to infer from these regions.
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
// counts at its place in the text, not when its command runs. Bodies expanded by an external
// command outside a pipeline, background job, or coprocess cannot be modeled precisely.
func (a Assign) VisibleAt(off int) bool {
	if a.Offset >= off {
		return false
	}
	return a.ScopeStart < 0 || (a.ScopeStart <= off && off < a.ScopeEnd)
}

// Assigns returns assignments of cmd in text order, including Unknown entries for array and
// element assignments, for-loop variables, calls to functions whose bodies may change variables
// in the calling shell, and assignments under a redirect that may fail. Unknown entries have an
// empty Value and Append=false.
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
	ctx := newAssignContext(file, regions)
	ctx.assignmentModes = assignmentShellModes(file, full)
	ctx.declarations = declarationAssignments(file)
	uncertainRedirects := uncertainRedirectStatements(file)
	var out []Assign
	syntax.Walk(file, func(node syntax.Node) bool {
		assignments := assignmentsForNode(node, full, ctx)
		for i := range assignments {
			if insideAny(uncertainRedirects, assignments[i].Offset) {
				assignments[i].Value = ""
				assignments[i].Tilde = 0
				assignments[i].Append = false
				assignments[i].Unknown = true
			}
		}
		out = append(out, assignments...)
		return true
	})
	slices.SortStableFunc(out, func(a, b Assign) int { return a.Offset - b.Offset })
	return out, nil
}

type assignContext struct {
	scopes            []span
	scopeIndex        spanIndex
	functionIndex     spanIndex
	funsubs           []span
	funsubLocals      []localBinding
	replySubs         []span
	functionChanges   map[string][]string
	functionDefs      map[string][]assignDefinition
	functionNames     []string
	functionNameSet   map[string]bool
	functionDocIndex  spanIndex
	functionDocBySpan map[span]functionDocScope
	deferredIndex     spanIndex
	// assignmentModes and declarations give the tilde mark of a value: the
	// shell modes at the assignment and whether a declaration command makes it.
	assignmentModes map[*syntax.Assign]shellModes
	declarations    map[*syntax.Assign]bool
	uncertainDecls  map[*syntax.DeclClause]bool
}

func newAssignContext(file *syntax.File, regions scopeSet) assignContext {
	uncertainDecls := uncertainDeclarationRedirects(file)
	rawScopes := make([]span, len(regions.scopes))
	for i, scope := range regions.scopes {
		rawScopes[i] = span{scope.Start, scope.End}
	}
	docBodies := make(map[span]bool, len(regions.functionDocs))
	for _, doc := range regions.functionDocs {
		if doc.owner.start <= doc.doc.start && doc.doc.end <= doc.owner.end {
			docBodies[doc.doc] = true
		}
	}
	scopes := make([]span, 0, len(regions.scopes))
	for _, scope := range regions.scopes {
		if scope.Kind == ScopeFunctionBody && docBodies[span{scope.Start, scope.End}] {
			continue
		}
		scopes = append(scopes, span{scope.Start, scope.End})
	}
	functionChanges, functionDefs := assignFunctionChanges(regions, rawScopes)
	scopeIndex := newSpanIndex(scopes)
	ctx := assignContext{
		scopes:            scopes,
		scopeIndex:        scopeIndex,
		functionIndex:     newSpanIndex(regions.functions),
		funsubs:           funsubSpans(file),
		funsubLocals:      funsubLocalBindings(file, scopeIndex, uncertainDecls),
		functionChanges:   functionChanges,
		functionDefs:      functionDefs,
		functionNames:     slices.Sorted(maps.Keys(functionChanges)),
		functionNameSet:   functionNameSet(functionChanges),
		deferredIndex:     newSpanIndex(regions.index.deferredCalls),
		functionDocBySpan: make(map[span]functionDocScope, len(regions.functionDocs)),
		uncertainDecls:    uncertainDecls,
	}
	for _, doc := range regions.functionDocs {
		ctx.functionDocBySpan[doc.doc] = doc
		ctx.functionDocIndex.spans = append(ctx.functionDocIndex.spans, doc.doc)
	}
	ctx.functionDocIndex = newSpanIndex(ctx.functionDocIndex.spans)
	syntax.Walk(file, func(node syntax.Node) bool {
		if subst, ok := node.(*syntax.CmdSubst); ok && subst.ReplyVar {
			ctx.replySubs = append(ctx.replySubs, span{int(subst.Pos().Offset()), int(subst.End().Offset())})
		}
		return true
	})
	return ctx
}

func assignmentsForNode(node syntax.Node, full string, ctx assignContext) []Assign {
	switch n := node.(type) {
	case *syntax.CallExpr:
		if len(n.Args) == 0 {
			return assignmentsFromSyntax(n.Assigns, full, ctx, false)
		}
		return unknownFunctionAssignments(n, ctx)
	case *syntax.DeclClause:
		declOff := int(n.Pos().Offset())
		if n.Variant.Value == "local" && !ctx.functionIndex.contains(declOff) && !insideAny(ctx.funsubs, declOff) {
			return nil
		}
		assignments := assignmentsFromSyntax(n.Args, full, ctx, declHasGlobal(n))
		if ctx.uncertainDecls[n] {
			for i := range assignments {
				assignments[i].Value = ""
				assignments[i].Tilde = 0
				assignments[i].Append = false
				assignments[i].Unknown = true
			}
		}
		return assignments
	case *syntax.ForClause:
		return unknownLoopAssignment(n, ctx)
	}
	return nil
}

func assignmentsFromSyntax(assigns []*syntax.Assign, full string, ctx assignContext, forceGlobal bool) []Assign {
	var out []Assign
	for _, a := range assigns {
		if a.Name == nil || a.Naked {
			continue
		}
		off := int(a.Pos().Offset())
		start, end := assignmentScope(ctx, a.Name.Value, off, forceGlobal)
		uncertainFunsub := false
		if !forceGlobal {
			if binding, ok := funsubBinding(ctx.funsubLocals, ctx.replySubs, a.Name.Value, off, true); ok {
				uncertainFunsub = binding.uncertain
			}
		}
		mode, ok := ctx.assignmentModes[a]
		if !ok {
			mode.brace = true
		}
		if a.Index != nil || a.Array != nil {
			var tilde Tilde
			if a.Array != nil {
				tilde = arrayAssignmentTilde(a.Array, full, mode)
			}
			out = append(out, Assign{Name: a.Name.Value, Tilde: tilde, Unknown: true, Offset: off, ScopeStart: start, ScopeEnd: end})
			continue
		}
		value := ""
		var tilde Tilde
		if a.Value != nil {
			value = assignValue(a, full)
			tilde = assignmentValueTilde(a, full, mode, ctx.declarations[a])
		}
		assignment := Assign{Name: a.Name.Value, Value: value, Tilde: tilde, Append: a.Append, Unknown: uncertainFunsub, Offset: off, ScopeStart: start, ScopeEnd: end}
		if uncertainFunsub {
			assignment.Value = ""
			assignment.Tilde = 0
			assignment.Append = false
		}
		out = append(out, assignment)
		if execution, ok := functionDocExecution(assignment, off, ctx); ok {
			out = append(out, execution)
		}
	}
	return out
}

func assignmentScope(ctx assignContext, name string, off int, forceGlobal bool) (int, int) {
	start, end := ctx.scopeIndex.innermost(off)
	if localScope, ok := funsubBinding(ctx.funsubLocals, ctx.replySubs, name, off, true); ok && !forceGlobal {
		if localScope.uncertain {
			if certain, found := certainFunsubBinding(ctx.funsubLocals, name, off); found {
				localScope = certain
			} else {
				return start, end
			}
		}
		// A funsub-local binding applies only when the funsub itself performs
		// the assignment. Preserve any narrower execution scope nested inside it.
		if start < localScope.start || end > localScope.end {
			start, end = localScope.start, localScope.end
		}
	}
	return start, end
}

// certainFunsubBinding finds the narrowest non-uncertain funsub local active at off.
func certainFunsubBinding(locals []localBinding, name string, offset int) (localBinding, bool) {
	var found localBinding
	for _, local := range locals {
		if local.name == name && !local.uncertain && local.offset <= offset && local.start <= offset && offset < local.end &&
			funsubBindingPreferred(local, found) {
			found = local
		}
	}
	return found, found.end != 0
}

func unknownFunctionAssignments(call *syntax.CallExpr, ctx assignContext) []Assign {
	if len(call.Args) == 0 {
		return nil
	}
	off := int(call.Args[0].Pos().Offset())
	possibleNames := calledFunctionNames(call.Args, ctx.functionNames, ctx.functionNameSet)
	deferred := ctx.deferredIndex.contains(off)
	knownFunction := false
	for _, name := range possibleNames {
		definitions := ctx.functionDefs[name]
		if sort.Search(len(definitions), func(i int) bool { return definitions[i].declOffset > off }) > 0 {
			knownFunction = true
			break
		}
	}
	if !knownFunction && !deferred {
		return nil
	}
	var out []Assign
	for _, variable := range calledFunctionChangesAt(call.Args, ctx.functionNames, ctx.functionNameSet, ctx.functionChanges) {
		start, end := assignmentScope(ctx, variable, off, false)
		assignment := Assign{Name: variable, Unknown: true, Offset: off, ScopeStart: start, ScopeEnd: end}
		out = append(out, assignment)
		if execution, ok := functionDocExecution(assignment, off, ctx); ok {
			out = append(out, execution)
		}
	}
	return out
}

func functionDocExecution(assignment Assign, off int, ctx assignContext) (Assign, bool) {
	doc, ok := ctx.functionDocContaining(off)
	if !ok || !doc.functionCommandRedirect {
		return Assign{}, false
	}
	assignment.Offset = doc.owner.start
	assignment.ScopeStart, assignment.ScopeEnd = doc.owner.start, doc.owner.end
	return assignment, true
}

func (ctx assignContext) functionDocContaining(off int) (functionDocScope, bool) {
	start, end := ctx.functionDocIndex.innermost(off)
	if start < 0 {
		return functionDocScope{}, false
	}
	doc, ok := ctx.functionDocBySpan[span{start, end}]
	return doc, ok
}

func unknownLoopAssignment(loop *syntax.ForClause, ctx assignContext) []Assign {
	words, ok := loop.Loop.(*syntax.WordIter)
	if !ok || words.Name == nil {
		return nil
	}
	off := int(words.Name.Pos().Offset())
	start, end := assignmentScope(ctx, words.Name.Value, off, false)
	return []Assign{{Name: words.Name.Value, Unknown: true, Offset: off, ScopeStart: start, ScopeEnd: end}}
}

func declarationAssignments(file *syntax.File) map[*syntax.Assign]bool {
	declarations := make(map[*syntax.Assign]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		decl, ok := node.(*syntax.DeclClause)
		if ok && decl.Variant != nil && slices.Contains(declarationCommands[:], decl.Variant.Value) {
			for _, assign := range decl.Args {
				declarations[assign] = true
			}
		}
		return true
	})
	return declarations
}

func uncertainDeclarationRedirects(node syntax.Node) map[*syntax.DeclClause]bool {
	uncertainStatements := uncertainRedirectStatements(node)
	uncertain := make(map[*syntax.DeclClause]bool)
	syntax.Walk(node, func(node syntax.Node) bool {
		decl, ok := node.(*syntax.DeclClause)
		if !ok || decl.Variant == nil || !isLocalDeclaration(decl) {
			return true
		}
		off := int(decl.Pos().Offset())
		for _, stmt := range uncertainStatements {
			if stmt.start <= off && off < stmt.end {
				uncertain[decl] = true
				break
			}
		}
		return true
	})
	return uncertain
}

// uncertainRedirectStatements returns command spans whose named command may not run because
// a redirect can fail. Assignment-only statements expand and assign before redirects are applied.
func uncertainRedirectStatements(node syntax.Node) []span {
	var uncertain []span
	syntax.Walk(node, func(node syntax.Node) bool {
		stmt, ok := node.(*syntax.Stmt)
		if !ok || len(stmt.Redirs) == 0 || stmtHasNoCommand(stmt) {
			return true
		}
		for _, redir := range stmt.Redirs {
			if !redirectDefinitelySucceeds(redir) {
				uncertain = append(uncertain, span{int(stmt.Pos().Offset()), int(stmt.End().Offset())})
				break
			}
		}
		return true
	})
	return uncertain
}

func redirectDefinitelySucceeds(redir *syntax.Redirect) bool {
	switch redir.Op {
	case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		return true
	case syntax.DplIn, syntax.DplOut:
		if redir.Word == nil {
			return false
		}
		target, literal := wordValue(redir.Word)
		if !literal {
			return false
		}
		if target == "-" || target == "/dev/null" {
			return true
		}
		if before, ok := strings.CutSuffix(target, "-"); ok {
			target = before
		}
		return target == "0" || target == "1" || target == "2"
	default:
		if redir.Word == nil {
			return false
		}
		target, literal := wordValue(redir.Word)
		return literal && target == "/dev/null"
	}
}

// assignmentValueTilde marks the tilde expansion of a scalar value in the
// shell modes at the assignment; a declaration command also brace-expands it.
func assignmentValueTilde(a *syntax.Assign, full string, mode shellModes, declaration bool) Tilde {
	tilde := wordTildeWithMode(a.Value, full, true, false, mode.posix, mode.brace, mode.posixUnknown, mode.braceUnknown)
	if declaration && assignmentTildeInsideBrace(a.Value, full) {
		if mode.brace && !mode.braceUnknown {
			tilde = 0
		} else if mode.braceUnknown {
			tilde |= TildeModeUnknown
		}
	}
	return tilde
}

func arrayAssignmentTilde(array *syntax.ArrayExpr, src string, mode shellModes) Tilde {
	var marks Tilde
	for _, elem := range array.Elems {
		if elem.Value != nil {
			marks |= wordTildeWithMode(elem.Value, src, false, false, mode.posix, mode.brace, mode.posixUnknown, mode.braceUnknown)
		}
	}
	return marks
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
	body           *syntax.Stmt
	span           span
	shells         []functionShell
	isolated       bool
	declOffset     int
	vars           []string
	calls          []string
	uncertainDecls map[*syntax.DeclClause]bool
}

type functionShell struct {
	span span
	node syntax.Node
}

type functionDocScope struct {
	redir                   *syntax.Redirect
	hdoc                    *syntax.Word
	doc                     span
	owner                   span
	functionCommandRedirect bool
	statementOwned          bool
	noCommand               bool
}

type localBinding struct {
	name      string
	offset    int
	start     int
	end       int
	uncertain bool
}

// assignFunctionChanges returns variables that each function can change in its caller. Calls
// between functions are resolved to a fixed point so a caller inherits its callees' changes.
func assignFunctionChanges(regions scopeSet, scopes []span) (map[string][]string, map[string][]assignDefinition) {
	scopeIndex := newSpanIndex(scopes)
	funcs := assignFunctionDecls(regions, scopeIndex)
	functionNames := slices.Sorted(maps.Keys(funcs))
	functionSet := functionNameSet(funcs)
	for _, name := range functionNames {
		f := funcs[name]
		for i := range f.definitions {
			collectDefinitionChanges(f, &f.definitions[i], functionNames, functionSet, scopeIndex)
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
	definitions := make(map[string][]assignDefinition, len(funcs))
	for name, function := range funcs {
		definitions[name] = function.definitions
	}
	return out, definitions
}

func collectDefinitionChanges(function *assignFunction, definition *assignDefinition, functionNames []string, functionSet map[string]bool, scopes spanIndex) {
	if definition.isolated {
		return
	}
	locals := functionLocals(definition.body, definition.span, scopes)
	for _, shell := range definition.shells {
		scopedDefinition := *definition
		scopedDefinition.span = shell.span
		shellLocals := locals
		if shell.node != definition.body {
			// Here-doc expansion order relative to local declarations is not
			// statically reliable. Keep possible writes as unknown at the caller.
			shellLocals = nil
		}
		changes := &assignFunction{vars: make(map[string]bool)}
		collectFunctionChanges(shell.node, changes, &scopedDefinition, functionNames, functionSet, shellLocals, scopes)
		mergeDefinitionChanges(function, definition, changes)
	}
	sortUnique(&definition.vars)
	sortUnique(&definition.calls)
}

func mergeDefinitionChanges(function *assignFunction, definition *assignDefinition, changes *assignFunction) {
	for variable := range changes.vars {
		definition.vars = append(definition.vars, variable)
		function.vars[variable] = true
	}
	definition.calls = append(definition.calls, changes.calls...)
	function.calls = append(function.calls, changes.calls...)
}

func sortUnique(values *[]string) {
	slices.Sort(*values)
	*values = slices.Compact(*values)
}

func assignFunctionDecls(regions scopeSet, scopes spanIndex) map[string]*assignFunction {
	funcs := make(map[string]*assignFunction)
	for _, f := range regions.index.functions {
		if f.Body == nil {
			continue
		}
		for _, name := range funcDeclNames(f) {
			_, isolated := f.Body.Cmd.(*syntax.Subshell)
			function := funcs[name.Value]
			if function == nil {
				function = &assignFunction{vars: make(map[string]bool)}
				funcs[name.Value] = function
			}
			bodySpan := functionBodySpan(f.Body)
			shells := append([]functionShell{{span: bodySpan, node: f.Body}}, functionHereDocShells(regions.functionDocsByFunction[f], scopes)...)
			function.definitions = append(function.definitions, assignDefinition{
				body: f.Body, span: bodySpan, shells: shells, isolated: isolated, declOffset: int(f.Pos().Offset()),
				uncertainDecls: uncertainDeclarationRedirects(f.Body),
			})
		}
	}
	return funcs
}

func functionLocals(body *syntax.Stmt, fn span, scopes spanIndex) []localBinding {
	var locals []localBinding
	uncertain := uncertainDeclarationRedirects(body)
	collectExecutedLocals(body, true, fn, scopes, uncertain, &locals)
	return append(locals, funsubLocalBindingsInFunction(body, fn, scopes, uncertain)...)
}

func collectExecutedLocals(stmt *syntax.Stmt, executes bool, fn span, scopes spanIndex, uncertain map[*syntax.DeclClause]bool, locals *[]localBinding) {
	if stmt == nil || !executes {
		return
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.DeclClause:
		collectLocalDeclaration(cmd, fn, scopes, uncertain, locals)
	case *syntax.Block:
		collectExecutedLocalsIn(cmd.Stmts, executes, fn, scopes, uncertain, locals)
	case *syntax.BinaryCmd:
		collectBinaryLocals(cmd, executes, fn, scopes, uncertain, locals)
	case *syntax.IfClause:
		collectIfLocals(cmd, executes, fn, scopes, uncertain, locals)
	case *syntax.WhileClause:
		collectWhileLocals(cmd, executes, fn, scopes, uncertain, locals)
	case *syntax.ForClause:
		collectExecutedLocalsIn(cmd.Do, false, fn, scopes, uncertain, locals)
	case *syntax.CaseClause:
		collectCaseLocals(cmd, fn, scopes, uncertain, locals)
	}
}

func collectLocalDeclaration(decl *syntax.DeclClause, fn span, scopes spanIndex, uncertain map[*syntax.DeclClause]bool, locals *[]localBinding) {
	if uncertain[decl] || !isLocalDeclaration(decl) || declHasGlobal(decl) || !inFunctionShell(scopes, fn, int(decl.Pos().Offset())) {
		return
	}
	for _, a := range decl.Args {
		if a.Name != nil {
			*locals = append(*locals, localBinding{name: a.Name.Value, offset: int(a.Pos().Offset()), start: fn.start, end: fn.end})
		}
	}
}

func collectBinaryLocals(cmd *syntax.BinaryCmd, executes bool, fn span, scopes spanIndex, uncertain map[*syntax.DeclClause]bool, locals *[]localBinding) {
	if cmd.Op == syntax.AndStmt || cmd.Op == syntax.OrStmt {
		collectExecutedLocals(cmd.X, executes, fn, scopes, uncertain, locals)
		collectExecutedLocals(cmd.Y, false, fn, scopes, uncertain, locals)
	}
}

func collectIfLocals(cmd *syntax.IfClause, executes bool, fn span, scopes spanIndex, uncertain map[*syntax.DeclClause]bool, locals *[]localBinding) {
	collectExecutedLocalsIn(cmd.Cond, executes, fn, scopes, uncertain, locals)
	collectExecutedLocalsIn(cmd.Then, false, fn, scopes, uncertain, locals)
}

func collectWhileLocals(cmd *syntax.WhileClause, executes bool, fn span, scopes spanIndex, uncertain map[*syntax.DeclClause]bool, locals *[]localBinding) {
	collectExecutedLocalsIn(cmd.Cond, executes, fn, scopes, uncertain, locals)
	collectExecutedLocalsIn(cmd.Do, false, fn, scopes, uncertain, locals)
}

func collectCaseLocals(cmd *syntax.CaseClause, fn span, scopes spanIndex, uncertain map[*syntax.DeclClause]bool, locals *[]localBinding) {
	for _, item := range cmd.Items {
		collectExecutedLocalsIn(item.Stmts, false, fn, scopes, uncertain, locals)
	}
}

func collectExecutedLocalsIn(stmts []*syntax.Stmt, executes bool, fn span, scopes spanIndex, uncertain map[*syntax.DeclClause]bool, locals *[]localBinding) {
	for _, stmt := range stmts {
		collectExecutedLocals(stmt, executes, fn, scopes, uncertain, locals)
	}
}

func localShadows(locals []localBinding, name string, offset int) bool {
	for _, local := range locals {
		if local.name == name && !local.uncertain && local.offset < offset && local.start <= offset && offset < local.end {
			return true
		}
	}
	return false
}

func funsubSpans(node syntax.Node) []span {
	var spans []span
	syntax.Walk(node, func(node syntax.Node) bool {
		cmd, ok := node.(*syntax.CmdSubst)
		if ok && (cmd.TempFile || cmd.ReplyVar) {
			spans = append(spans, span{start: int(cmd.Pos().Offset()), end: int(cmd.End().Offset())})
		}
		return true
	})
	return spans
}

func funsubLocalBindings(node syntax.Node, scopes spanIndex, uncertain map[*syntax.DeclClause]bool) []localBinding {
	return funsubLocalBindingsInFunction(node, span{-1, -1}, scopes, uncertain)
}

func funsubLocalBindingsInFunction(node syntax.Node, fn span, scopes spanIndex, uncertain map[*syntax.DeclClause]bool) []localBinding {
	var funsubs []span
	var declarations []*syntax.DeclClause
	syntax.Walk(node, func(node syntax.Node) bool {
		return collectFunsubLocalNode(node, fn, &funsubs, &declarations)
	})
	return bindFunsubLocalDeclarations(funsubs, declarations, scopes, uncertain)
}

func collectFunsubLocalNode(node syntax.Node, fn span, funsubs *[]span, declarations *[]*syntax.DeclClause) bool {
	if function, ok := node.(*syntax.FuncDecl); ok && fn.start >= 0 {
		off := int(function.Pos().Offset())
		if fn.start <= off && off < fn.end {
			return false
		}
	}
	switch n := node.(type) {
	case *syntax.CmdSubst:
		if n.TempFile || n.ReplyVar {
			*funsubs = append(*funsubs, span{start: int(n.Pos().Offset()), end: int(n.End().Offset())})
		}
	case *syntax.DeclClause:
		if isLocalDeclaration(n) && !declHasGlobal(n) {
			*declarations = append(*declarations, n)
		}
	}
	return true
}

func bindFunsubLocalDeclarations(funsubs []span, declarations []*syntax.DeclClause, scopes spanIndex, uncertain map[*syntax.DeclClause]bool) []localBinding {
	slices.SortFunc(funsubs, func(a, b span) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return b.end - a.end
	})
	slices.SortFunc(declarations, func(a, b *syntax.DeclClause) int {
		return int(a.Pos().Offset()) - int(b.Pos().Offset())
	})
	var locals []localBinding
	active := make([]span, 0, len(funsubs))
	nextFunsub := 0
	for _, decl := range declarations {
		off := int(decl.Pos().Offset())
		for len(active) > 0 && active[len(active)-1].end <= off {
			active = active[:len(active)-1]
		}
		for nextFunsub < len(funsubs) && funsubs[nextFunsub].start <= off {
			active = append(active, funsubs[nextFunsub])
			nextFunsub++
		}
		for len(active) > 0 && active[len(active)-1].end <= off {
			active = active[:len(active)-1]
		}
		if len(active) == 0 {
			continue
		}
		fn := active[len(active)-1]
		scopeStart, scopeEnd := scopes.innermost(off)
		if scopeStart >= fn.start && scopeEnd <= fn.end {
			continue
		}
		for _, a := range decl.Args {
			if a.Name != nil {
				locals = append(locals, localBinding{
					name: a.Name.Value, offset: int(a.Pos().Offset()), start: fn.start, end: fn.end,
					uncertain: uncertain[decl],
				})
			}
		}
	}
	return locals
}

func funsubBinding(locals []localBinding, replySubs []span, name string, offset int, includeDeclaration bool) (localBinding, bool) {
	var found localBinding
	for _, local := range locals {
		atOrAfter := local.offset < offset || includeDeclaration && local.offset == offset
		if local.name == name && atOrAfter && local.start <= offset && offset < local.end && local.end-local.start > 0 &&
			funsubBindingPreferred(local, found) {
			found = local
		}
	}
	if name == "REPLY" {
		for _, reply := range replySubs {
			if reply.start <= offset && offset < reply.end &&
				(found.end == 0 || reply.end-reply.start < found.end-found.start) {
				found = localBinding{start: reply.start, end: reply.end}
			}
		}
	}
	if found.end != 0 {
		return found, true
	}
	return localBinding{}, false
}

// funsubBindingPreferred prefers the innermost active funsub, then a certain binding
// over an uncertain one with the same span, and finally the later declaration.
func funsubBindingPreferred(candidate, current localBinding) bool {
	if current.end == 0 {
		return true
	}
	candidateSpan, currentSpan := candidate.end-candidate.start, current.end-current.start
	if candidateSpan != currentSpan {
		return candidateSpan < currentSpan
	}
	if candidate.uncertain != current.uncertain {
		return !candidate.uncertain
	}
	return candidate.offset > current.offset
}

func insideAny(spans []span, off int) bool {
	for _, s := range spans {
		if s.start <= off && off < s.end {
			return true
		}
	}
	return false
}

func isLocalDeclaration(decl *syntax.DeclClause) bool {
	return decl.Variant.Value == "local" || decl.Variant.Value == "declare" || decl.Variant.Value == "typeset"
}

func nestedFunctionOf(node syntax.Node, definition *assignDefinition) bool {
	nested, ok := node.(*syntax.FuncDecl)
	if !ok || nested.Body == nil || nested.Body == definition.body {
		return false
	}
	off := int(nested.Pos().Offset())
	return definition.span.start <= off && off < definition.span.end
}

func collectFunctionChanges(root syntax.Node, f *assignFunction, definition *assignDefinition, functionNames []string, functionSet map[string]bool, locals []localBinding, scopes spanIndex) {
	syntax.Walk(root, func(node syntax.Node) bool {
		if nestedFunctionOf(node, definition) {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			if inFunctionShell(scopes, definition.span, int(n.Pos().Offset())) {
				collectCallChanges(f, definition, n, functionNames, functionSet, locals, scopes)
			}
		case *syntax.DeclClause:
			collectDeclChanges(f, definition, n, locals, scopes)
		case *syntax.ForClause:
			collectLoopChanges(f, definition, n, locals, scopes)
		}
		return true
	})
}

func collectCallChanges(f *assignFunction, definition *assignDefinition, call *syntax.CallExpr, functionNames []string, functionSet map[string]bool, locals []localBinding, scopes spanIndex) {
	if len(call.Args) == 0 {
		for _, a := range call.Assigns {
			if a.Name != nil && !a.Naked && !localShadows(locals, a.Name.Value, int(a.Pos().Offset())) && inFunctionShell(scopes, definition.span, int(a.Pos().Offset())) {
				f.vars[a.Name.Value] = true
			}
		}
		return
	}
	f.calls = append(f.calls, calledFunctionNames(call.Args, functionNames, functionSet)...)
}

func calledFunctionChangesAt(args []*syntax.Word, functionNames []string, functionSet map[string]bool, changes map[string][]string) []string {
	variables := make(map[string]bool)
	for _, name := range calledFunctionNames(args, functionNames, functionSet) {
		for _, variable := range changes[name] {
			variables[variable] = true
		}
	}
	return slices.Sorted(maps.Keys(variables))
}

func calledFunctionNames(args []*syntax.Word, functions []string, functionSet map[string]bool) []string {
	if len(args) == 0 {
		return nil
	}
	name, literal := wordValue(args[0])
	if !literal {
		return functions
	}
	if functionSet[name] {
		return []string{name}
	}
	switch name {
	case "eval":
		return evalCallFunctionNames(args[1:], functions)
	case "command":
		return wrappedBuiltinFunctionNames(args[1:], true, functions)
	case "builtin":
		return wrappedBuiltinFunctionNames(args[1:], false, functions)
	}
	return nil
}

func wrappedBuiltinFunctionNames(args []*syntax.Word, command bool, functions []string) []string {
	for len(args) > 0 {
		option, literal := wordValue(args[0])
		if !literal {
			return functions
		}
		if option == "--" {
			args = args[1:]
			break
		}
		if command && len(option) > 1 && option[0] == '-' {
			valid := true
			query := false
			for _, flag := range option[1:] {
				switch flag {
				case 'p':
				case 'v', 'V':
					query = true
				default:
					valid = false
				}
			}
			if !valid || query {
				return nil
			}
			args = args[1:]
			continue
		}
		if !command && strings.HasPrefix(option, "-") {
			return nil
		}
		break
	}
	if len(args) == 0 {
		return nil
	}
	name, literal := wordValue(args[0])
	if !literal {
		return functions
	}
	switch name {
	case "eval":
		return evalCallFunctionNames(args[1:], functions)
	case "command":
		return wrappedBuiltinFunctionNames(args[1:], true, functions)
	case "builtin":
		return wrappedBuiltinFunctionNames(args[1:], false, functions)
	default:
		return nil
	}
}

func evalCallFunctionNames(evalArgs []*syntax.Word, functions []string) []string {
	for _, arg := range evalArgs {
		if _, ok := wordValue(arg); !ok {
			return functions
		}
	}
	return evalFunctionNames(evalArgs, functions)
}

func functionNameSet[V any](functions map[string]V) map[string]bool {
	set := make(map[string]bool, len(functions))
	for name := range functions {
		set[name] = true
	}
	return set
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

func collectDeclChanges(f *assignFunction, definition *assignDefinition, decl *syntax.DeclClause, locals []localBinding, scopes spanIndex) {
	uncertainRedirect := definition.uncertainDecls[decl]
	global := declHasGlobal(decl)
	for _, a := range decl.Args {
		if a.Name == nil || a.Naked || localShadows(locals, a.Name.Value, int(a.Pos().Offset())) && !global || !inFunctionShell(scopes, definition.span, int(a.Pos().Offset())) {
			continue
		}
		if (decl.Variant.Value == "local" || decl.Variant.Value == "declare" || decl.Variant.Value == "typeset") && !global && !uncertainRedirect {
			continue
		}
		f.vars[a.Name.Value] = true
	}
}

func collectLoopChanges(f *assignFunction, definition *assignDefinition, loop *syntax.ForClause, locals []localBinding, scopes spanIndex) {
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

func inFunctionShell(scopes spanIndex, fn span, off int) bool {
	start, end := scopes.innermost(off)
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

type spanIndex struct {
	spans        []span
	prefixMaxEnd []int
}

func newSpanIndex(spans []span) spanIndex {
	ordered := slices.Clone(spans)
	slices.SortFunc(ordered, func(a, b span) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return b.end - a.end
	})
	index := spanIndex{spans: ordered, prefixMaxEnd: make([]int, len(ordered))}
	for i, current := range ordered {
		index.prefixMaxEnd[i] = current.end
		if i > 0 && index.prefixMaxEnd[i-1] > index.prefixMaxEnd[i] {
			index.prefixMaxEnd[i] = index.prefixMaxEnd[i-1]
		}
	}
	return index
}

func (index spanIndex) innermost(off int) (start, end int) {
	start, end = -1, -1
	for i := sort.Search(len(index.spans), func(i int) bool { return index.spans[i].start > off }) - 1; i >= 0 && index.prefixMaxEnd[i] > off; i-- {
		candidate := index.spans[i]
		if candidate.end > off && (start < 0 || candidate.end-candidate.start < end-start) {
			start, end = candidate.start, candidate.end
		}
	}
	return start, end
}

func (index spanIndex) contains(off int) bool {
	for i := sort.Search(len(index.spans), func(i int) bool { return index.spans[i].start > off }) - 1; i >= 0 && index.prefixMaxEnd[i] > off; i-- {
		if index.spans[i].end > off {
			return true
		}
	}
	return false
}

type scopeSet struct {
	scopes                 []Scope
	functions              []span
	functionDocs           []functionDocScope
	functionDocsByFunction map[*syntax.FuncDecl][]functionDocScope
	index                  *syntaxIndex
}

type syntaxIndex struct {
	functions         []*syntax.FuncDecl
	redirects         []*syntax.Redirect
	stmts             map[syntax.Command][]*syntax.Stmt
	noCommand         map[*syntax.Redirect]bool
	functionRedirects map[*syntax.FuncDecl]map[*syntax.Redirect]bool
	deferredCalls     []span
}

func indexSyntax(file *syntax.File) *syntaxIndex {
	index := &syntaxIndex{
		stmts:     make(map[syntax.Command][]*syntax.Stmt),
		noCommand: make(map[*syntax.Redirect]bool),
	}
	seenRedirs := make(map[*syntax.Redirect]bool)
	addRedirect := func(redir *syntax.Redirect) {
		if redir != nil && !seenRedirs[redir] {
			seenRedirs[redir] = true
			index.redirects = append(index.redirects, redir)
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.FuncDecl:
			index.functions = append(index.functions, n)
			if n.Body != nil && n.Body.Cmd != nil {
				index.deferredCalls = append(index.deferredCalls, functionBodySpan(n.Body))
			}
		case *syntax.ForClause, *syntax.WhileClause:
			index.deferredCalls = append(index.deferredCalls, span{int(n.Pos().Offset()), int(n.End().Offset())})
		case *syntax.Redirect:
			addRedirect(n)
		case *syntax.Stmt:
			if n.Cmd != nil {
				index.stmts[n.Cmd] = append(index.stmts[n.Cmd], n)
			}
			for _, redir := range n.Redirs {
				addRedirect(redir)
				if stmtHasNoCommand(n) {
					index.noCommand[redir] = true
				}
			}
		}
		return true
	})
	slices.SortFunc(index.redirects, func(a, b *syntax.Redirect) int {
		return int(a.Pos().Offset()) - int(b.Pos().Offset())
	})
	slices.SortFunc(index.functions, func(a, b *syntax.FuncDecl) int {
		return int(a.Pos().Offset()) - int(b.Pos().Offset())
	})
	index.functionRedirects = make(map[*syntax.FuncDecl]map[*syntax.Redirect]bool, len(index.functions))
	for _, function := range index.functions {
		all := make(map[*syntax.Redirect]bool)
		addStmtRedirs := func(stmts []*syntax.Stmt) {
			for _, stmt := range stmts {
				for _, redir := range stmt.Redirs {
					all[redir] = true
				}
			}
		}
		addStmtRedirs(index.stmts[function])
		if function.Body != nil && function.Body.Cmd != nil {
			addStmtRedirs(index.stmts[function.Body.Cmd])
		}
		index.functionRedirects[function] = all
	}
	return index
}

// commandScopes collects all assignment scopes once for both Scopes and Assigns.
func commandScopes(file *syntax.File) scopeSet {
	index := indexSyntax(file)
	out := scopeSet{index: index, functionDocsByFunction: make(map[*syntax.FuncDecl][]functionDocScope)}
	collectPipelineScopes(file, &out)
	collectExecutionScopes(file, &out, index)
	collectFunctionDocScopes(&out, index)
	sortScopes(out.scopes)
	return out
}

func collectPipelineScopes(file *syntax.File, out *scopeSet) {
	pipeChildren := nestedPipelineNodes(file)
	syntax.Walk(file, func(node syntax.Node) bool {
		binary, ok := node.(*syntax.BinaryCmd)
		if ok && isPipeline(binary) && !pipeChildren[binary] {
			out.scopes = append(out.scopes, pipelineScopes(binary, out.index)...)
		}
		return true
	})
}

func collectExecutionScopes(file *syntax.File, out *scopeSet, index *syntaxIndex) {
	functionSubshells := isolatedFunctionSubshells(index.functions)
	syntax.Walk(file, func(node syntax.Node) bool {
		appendExecutionScope(out, index, node, functionSubshells)
		return true
	})
}

func appendExecutionScope(out *scopeSet, index *syntaxIndex, node syntax.Node, functionSubshells map[*syntax.Subshell]bool) {
	switch n := node.(type) {
	case *syntax.Subshell:
		if !functionSubshells[n] {
			out.scopes = appendScopeAndParentHereDocs(out.scopes, index, n, nodeScope(n, ScopeSubshell))
		}
	case *syntax.CmdSubst:
		if !n.TempFile && !n.ReplyVar {
			out.scopes = appendScopeAndHereDocs(out.scopes, index, n, nodeScope(n, ScopeCmdSubst))
		}
	case *syntax.ProcSubst:
		out.scopes = appendScopeAndHereDocs(out.scopes, index, n, nodeScope(n, ScopeProcSubst))
	case *syntax.CoprocClause:
		out.scopes = appendScopeAndHereDocs(out.scopes, index, n, scopeWithEnd(n, ScopeCoproc, statementEnd(n.Stmt)))
	case *syntax.FuncDecl:
		body, scopes := functionScopes(n)
		if body != nil {
			out.functions = append(out.functions, *scopes)
			out.scopes = append(out.scopes, body...)
		}
	case *syntax.Stmt:
		appendStatementScopes(out, n)
	}
}

func appendStatementScopes(out *scopeSet, stmt *syntax.Stmt) {
	if stmtHasNoCommand(stmt) {
		out.scopes = appendDirectStmtHereDocRanges(out.scopes, stmt, Scope{Kind: ScopeSubshell, DeclOffset: -1})
	}
	if stmt.Background {
		out.scopes = appendScopeAndHereDocs(out.scopes, out.index, stmt, scopeWithEnd(stmt, ScopeBackground, statementEnd(stmt)))
	}
}

func collectFunctionDocScopes(out *scopeSet, index *syntaxIndex) {
	type functionBody struct {
		function *syntax.FuncDecl
		span     span
		scopes   []Scope
	}
	bodies := make([]functionBody, 0, len(index.functions))
	bodyByFunction := make(map[*syntax.FuncDecl]functionBody, len(index.functions))
	directOwners := make(map[*syntax.Redirect]*syntax.FuncDecl)
	for _, function := range index.functions {
		for redir := range index.functionRedirects[function] {
			directOwners[redir] = function
		}
		if function.Body == nil || function.Body.Cmd == nil {
			continue
		}
		body, _ := functionScopes(function)
		info := functionBody{function: function, span: functionBodySpan(function.Body), scopes: body}
		bodies = append(bodies, info)
		bodyByFunction[function] = info
	}
	slices.SortFunc(bodies, func(a, b functionBody) int {
		if a.span.start != b.span.start {
			return a.span.start - b.span.start
		}
		return b.span.end - a.span.end
	})
	var active []functionBody
	nextBody := 0
	for _, redir := range index.redirects {
		if redir.Hdoc == nil {
			continue
		}
		pos := int(redir.Pos().Offset())
		function := directOwners[redir]
		direct := function != nil
		if !direct {
			for len(active) > 0 && active[len(active)-1].span.end <= pos {
				active = active[:len(active)-1]
			}
			for nextBody < len(bodies) && bodies[nextBody].span.start <= pos {
				active = append(active, bodies[nextBody])
				nextBody++
			}
			for len(active) > 0 && active[len(active)-1].span.end <= pos {
				active = active[:len(active)-1]
			}
			if len(active) == 0 {
				continue
			}
			function = active[len(active)-1].function
		}
		info := bodyByFunction[function]
		owner, body := info.span, info.scopes
		doc := span{int(redir.Hdoc.Pos().Offset()), int(redir.Hdoc.End().Offset())}
		docInfo := functionDocScope{
			redir: redir, hdoc: redir.Hdoc, doc: doc, owner: owner,
			functionCommandRedirect: direct,
			statementOwned:          direct,
			noCommand:               index.noCommand[redir],
		}
		out.functionDocsByFunction[function] = append(out.functionDocsByFunction[function], docInfo)
		out.functionDocs = append(out.functionDocs, docInfo)
		if direct {
			index.deferredCalls = append(index.deferredCalls, doc)
		}
		for _, scope := range body {
			docScope := scope
			docScope.Start, docScope.End = doc.start, doc.end
			out.scopes = append(out.scopes, docScope)
		}
	}
}

func appendScopeAndHereDocs(scopes []Scope, index *syntaxIndex, node syntax.Node, scope Scope) []Scope {
	scopes = append(scopes, scope)
	return appendHereDocRanges(scopes, index, node, scope)
}

func appendScopeAndParentHereDocs(scopes []Scope, index *syntaxIndex, node syntax.Node, scope Scope) []Scope {
	scopes = appendScopeAndHereDocs(scopes, index, node, scope)
	return appendParentStmtHereDocs(scopes, index, node, scope)
}

func appendParentStmtHereDocs(scopes []Scope, index *syntaxIndex, node syntax.Node, scope Scope) []Scope {
	command, ok := node.(syntax.Command)
	if !ok {
		return scopes
	}
	for _, stmt := range index.stmts[command] {
		scopes = appendDirectStmtHereDocRanges(scopes, stmt, scope)
	}
	return scopes
}

func stmtHasNoCommand(stmt *syntax.Stmt) bool {
	if stmt.Cmd == nil {
		return true
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	return ok && len(call.Args) == 0
}

func functionHereDocShells(docs []functionDocScope, scopes spanIndex) []functionShell {
	var shells []functionShell
	for _, doc := range docs {
		if doc.noCommand || doc.hdoc == nil || doc.redir == nil {
			continue
		}
		pos := int(doc.redir.Pos().Offset())
		if pos < doc.owner.start || pos >= doc.owner.end {
			if doc.statementOwned {
				shells = append(shells, functionShell{span: doc.doc, node: doc.hdoc})
			}
			continue
		}
		if start, end := scopes.innermost(pos); start == doc.owner.start && end == doc.owner.end {
			shells = append(shells, functionShell{span: doc.doc, node: doc.hdoc})
		}
	}
	return shells
}

func appendStmtHereDocRanges(scopes []Scope, stmt *syntax.Stmt, scope Scope) []Scope {
	syntax.Walk(stmt, func(node syntax.Node) bool {
		child, ok := node.(*syntax.Stmt)
		if !ok {
			return true
		}
		for _, redir := range child.Redirs {
			scopes = appendHereDocScope(scopes, redir, scope)
		}
		return true
	})
	return scopes
}

func appendDirectStmtHereDocRanges(scopes []Scope, stmt *syntax.Stmt, scope Scope) []Scope {
	for _, redir := range stmt.Redirs {
		scopes = appendHereDocScope(scopes, redir, scope)
	}
	return scopes
}

func appendHereDocScope(scopes []Scope, redir *syntax.Redirect, scope Scope) []Scope {
	if redir.Hdoc == nil {
		return scopes
	}
	body := scope
	body.Start = int(redir.Hdoc.Pos().Offset())
	body.End = int(redir.Hdoc.End().Offset())
	return append(scopes, body)
}

func appendHereDocRanges(scopes []Scope, index *syntaxIndex, node syntax.Node, scope Scope) []Scope {
	if stmt, ok := node.(*syntax.Stmt); ok {
		return appendStmtHereDocRanges(scopes, stmt, scope)
	}
	start, end := int(node.Pos().Offset()), int(node.End().Offset())
	lo := sort.Search(len(index.redirects), func(i int) bool {
		return int(index.redirects[i].Pos().Offset()) >= start
	})
	for i := lo; i < len(index.redirects); i++ {
		redir := index.redirects[i]
		if int(redir.Pos().Offset()) >= end {
			break
		}
		if redir.Hdoc == nil {
			continue
		}
		scopes = appendHereDocScope(scopes, redir, scope)
	}
	return scopes
}

func nodeScope(node syntax.Node, kind ScopeKind) Scope {
	return Scope{Start: int(node.Pos().Offset()), End: int(node.End().Offset()), Kind: kind, DeclOffset: -1}
}

func scopeWithEnd(node syntax.Node, kind ScopeKind, end int) Scope {
	return Scope{Start: int(node.Pos().Offset()), End: end, Kind: kind, DeclOffset: -1}
}

// statementEnd returns the source end of a statement's command syntax, excluding
// here-document bodies. mvdan includes those bodies in Redirect.End and hence in
// Stmt.End when the statement has no trailing control operator.
func statementEnd(stmt *syntax.Stmt) int {
	if stmt.Semicolon.IsValid() {
		return int(stmt.End().Offset())
	}
	end := int(stmt.Position.Offset())
	if stmt.Negated {
		end++
	}
	if stmt.Cmd != nil {
		end = max(end, int(stmt.Cmd.End().Offset()))
	}
	for _, redir := range stmt.Redirs {
		redirEnd := redir.End()
		if redir.Hdoc != nil && redir.Word != nil {
			redirEnd = redir.Word.End()
		}
		end = max(end, int(redirEnd.Offset()))
	}
	return end
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

func isolatedFunctionSubshells(functions []*syntax.FuncDecl) map[*syntax.Subshell]bool {
	isolated := make(map[*syntax.Subshell]bool)
	for _, function := range functions {
		if function.Body != nil {
			if subshell, ok := function.Body.Cmd.(*syntax.Subshell); ok {
				isolated[subshell] = true
			}
		}
	}
	return isolated
}

func functionScopes(function *syntax.FuncDecl) ([]Scope, *span) {
	if function.Body == nil || function.Body.Cmd == nil {
		return nil, nil
	}
	body := functionBodySpan(function.Body)
	names := funcDeclNames(function)
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

func pipelineScopes(binary *syntax.BinaryCmd, index *syntaxIndex) []Scope {
	var parts []*syntax.Stmt
	appendPipelineParts(binary, &parts)
	scopes := make([]Scope, 0, len(parts))
	for i, part := range parts {
		scope := Scope{Start: int(part.Pos().Offset()), End: statementEnd(part), Kind: ScopePipePart, DeclOffset: -1, Last: i == len(parts)-1}
		scopes = appendScopeAndHereDocs(scopes, index, part, scope)
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
