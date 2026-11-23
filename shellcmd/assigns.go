package shellcmd

import (
	"regexp"
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
	// Unknown marks a value that Assigns cannot determine. Global triggers include eval, dynamic
	// command words, defining aliases or BASH_ALIASES writes, command traps, mapfile/readarray -C,
	// source/dot, nameref declarations, and recognized nested shell scripts. Per-name triggers
	// include arithmetic and integer declarations; `${name:=…}`; write targets of read, printf -v,
	// getopts, mapfile/readarray, unset, wait -p, coproc, and `{name}>…`; readonly/case-transform
	// declarations; special-builtin prefix assignments; uncertain function-body writes, including
	// nested-scope records selectable by a later reference; and writes in
	// if/elif/else and case arms, right sides of &&/||, loop conditions and bodies, conditional
	// parameter expansions, case patterns, and here-document/here-string expansions. A name read
	// in a function body with multiple assignments in the script is also Unknown. Implicit writes
	// may not have a corresponding returned assignment.
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
// Scopes is syntactic and does not model trap, callback, alias, or lastpipe effects.
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
// inside them are tracked by Assigns. These regions do not encode runtime effects
// from traps, callbacks, aliases, or lastpipe.
type Scope struct {
	Start, End int
	Kind       ScopeKind
	Name       string
	DeclOffset int
	Isolated   bool
	Last       bool
}

// VisibleAt reports whether the assignment is in source order before off and either belongs to
// the command's shell or off is inside its syntactic scope. It does not determine execution order
// across functions, branches, loop conditions/bodies, traps, callbacks, pipelines, or deferred
// here-document and here-string expansions. Assigns marks affected names Unknown instead of
// moving offsets; callers must not treat a known source-order value as proof that a command ran.
func (a Assign) VisibleAt(off int) bool {
	if a.Offset >= off {
		return false
	}
	return a.ScopeStart < 0 || (a.ScopeStart <= off && off < a.ScopeEnd)
}

// Assigns returns assignments in source order. Prefix assignments and declarations without a
// value are omitted; a function called with a prefix sees its value, so a name with a prefix and a
// reference inside a function body is Unknown. It marks all assignments Unknown when the script contains eval, a dynamic
// command word, a defining alias or BASH_ALIASES write, a command trap, mapfile/readarray -C,
// source/dot, a declare/local/typeset -n option, or a recognized nested shell script from a string
// or stdin. Per-name triggers include arithmetic and -i declarations; `${name:=…}`; write targets
// of read, printf -v, getopts, mapfile/readarray, unset, wait -p, coproc, and `{name}>…`;
// readonly/case-transform declarations; special-builtin prefix assignments; uncertain function-body writes,
// including nested-scope records selectable by a later reference;
// and assignments in if/elif/else/case arms, right sides of &&/||, loop conditions and bodies,
// conditional parameter expansions, case patterns, and here-document/here-string expansions. A
// name read inside a function body is also Unknown when there are multiple
// assignments to it. Arithmetic involving a write target or a non-integer/Unknown value makes all
// assignments Unknown. Offsets and scopes remain syntactic. The analysis does not execute generated
// text, inspect source files, resolve arbitrary indirection, model trap/callback order or lastpipe,
// and may omit implicit write records. Malformed top-level input is an error; malformed script
// text in a recognized nested shell marks names Unknown without returning the nested parse error.
func Assigns(cmd string) ([]Assign, error) {
	file, full, err := parseShellVariant(cmd, false)
	if err != nil {
		return nil, err
	}
	regions := commandScopes(file)
	ctx := newAssignContext(file, regions)
	ctx.assignmentModes = assignmentShellModes(file, full)
	ctx.declarations = declarationAssignments(file)
	uncertainRedirects := newSpanIndex(uncertainRedirectStatements(file))
	var out []Assign
	syntax.Walk(file, func(node syntax.Node) bool {
		assignments := assignmentsForNode(node, full, ctx)
		for i := range assignments {
			if uncertainRedirects.contains(assignments[i].Offset) {
				assignments[i].Value = ""
				assignments[i].Tilde = 0
				assignments[i].Append = false
				assignments[i].Unknown = true
			}
		}
		out = append(out, assignments...)
		return true
	})
	suspicious, functionUnknown, global, err := suspiciousAssignNames(file, cmd, out)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if global || suspicious[out[i].Name] || functionUnknown[out[i].Offset] {
			out[i].Value, out[i].Append, out[i].Unknown = "", false, true
		}
	}
	slices.SortStableFunc(out, func(a, b Assign) int { return a.Offset - b.Offset })
	return out, nil
}

var arithmeticNames = regexp.MustCompile(`[[:alpha:]_][[:alnum:]_]*`)
var shellVarName = regexp.MustCompile(`^[[:alpha:]_][[:alnum:]_]*$`)
var numericValue = regexp.MustCompile(`^[+-]?[0-9]+$`)
var integerDeclOption = regexp.MustCompile(`(^|[[:space:]])-[[:alpha:]]*i[[:alpha:]]*([[:space:]]|$)`)
var namerefDeclOption = regexp.MustCompile(`(^|[[:space:]])-[[:alpha:]]*n[[:alpha:]]*([[:space:]]|$)`)
var caseTransformDeclOption = regexp.MustCompile(`(^|[[:space:]])-[[:alpha:]]*[ulc][[:alpha:]]*([[:space:]]|$)`)

// suspiciousAssignNames makes one syntax-tree pass and records names whose value can be
// affected by constructs Assigns deliberately does not execute or interpret.
func suspiciousAssignNames(file *syntax.File, src string, assigns []Assign) (map[string]bool, map[int]bool, bool, error) {
	scan := suspiciousAssignScan{
		src: src, assigns: assigns, names: make(map[string]bool), arithmetic: make(map[string]bool),
		integerVars: make(map[string]bool), writeNames: make(map[string]bool),
		functionCounts: make(map[string]int), functionRefs: make(map[string][]int), functionNames: make(map[span]string), functionUnknown: make(map[int]bool),
		definedFunctions: make(map[string]bool), functionCalls: make(map[string][]int),
		// Any mention of posix (set -o posix, shopt -so posix, POSIXLY_CORRECT) may switch bash to posix mode,
		// where a prefix assignment before a special builtin persists. Checked once: per command it was quadratic.
		posix: strings.Contains(src, "posix") || strings.Contains(src, "POSIX"),
	}
	syntax.Walk(file, scan.visit)
	for _, call := range scan.callSites {
		if scan.definedFunctions[call.name] {
			scan.functionCalls[call.name] = append(scan.functionCalls[call.name], call.offset)
		}
	}
	names, all := scan.result()
	return names, scan.functionUnknown, all, nil
}

type suspiciousAssignScan struct {
	src              string
	posix            bool
	assigns          []Assign
	names            map[string]bool
	arithmetic       map[string]bool
	integerVars      map[string]bool
	writeNames       map[string]bool
	functionCounts   map[string]int
	functionRefs     map[string][]int
	definedFunctions map[string]bool
	functionCalls    map[string][]int
	callSites        []callSite
	functionNames    map[span]string
	functionBodies   []span
	functionIsolated []span
	functionUnknown  map[int]bool
	mathSpans        []span
	valueSpans       map[string][]span
	uncertain        []span
	all              bool
}

type callSite struct {
	name   string
	offset int
}

func (s *suspiciousAssignScan) markText(lo, hi int) {
	if lo < 0 || hi > len(s.src) || lo >= hi {
		return
	}
	s.mathSpans = append(s.mathSpans, span{lo, hi})
}

func (s *suspiciousAssignScan) markStmts(stmts []*syntax.Stmt) {
	for _, stmt := range stmts {
		s.uncertain = append(s.uncertain, span{int(stmt.Pos().Offset()), int(stmt.End().Offset())})
	}
}

func (s *suspiciousAssignScan) visit(node syntax.Node) bool {
	if !s.visitPrimary(node) {
		s.visitSecondary(node)
	}
	return true
}

func (s *suspiciousAssignScan) visitPrimary(node syntax.Node) bool {
	switch n := node.(type) {
	case *syntax.CallExpr:
		s.call(n)
	case *syntax.DeclClause:
		s.declaration(n)
	case *syntax.FuncDecl:
		if n.Name != nil {
			s.definedFunctions[n.Name.Value] = true
		}
		body := span{-1, -1}
		if n.Body != nil && n.Body.Cmd != nil {
			body = functionBodySpan(n.Body)
		}
		s.functionBodies = append(s.functionBodies, body)
		if n.Name != nil {
			s.functionNames[body] = n.Name.Value
		}
		if n.Body != nil {
			if _, ok := n.Body.Cmd.(*syntax.Subshell); ok {
				s.functionIsolated = append(s.functionIsolated, nodeSpan(n.Body.Cmd))
			}
		}
	case *syntax.ArithmCmd, *syntax.ArithmExp, *syntax.LetClause, *syntax.CStyleLoop:
		s.markText(int(n.Pos().Offset()), int(n.End().Offset()))
	case *syntax.Assign:
		s.visitAssign(n)
	case *syntax.ParamExp:
		s.parameter(n)
	case *syntax.IfClause:
		s.visitIf(n)
	case *syntax.CaseClause:
		s.visitCase(n)
	case *syntax.BinaryTest:
		s.visitBinaryTest(n)
	default:
		return false
	}
	return true
}

func (s *suspiciousAssignScan) visitSecondary(node syntax.Node) {
	switch n := node.(type) {
	case *syntax.WhileClause:
		s.markStmts(n.Do)
		s.markStmts(n.Cond)
	case *syntax.ForClause:
		s.markStmts(n.Do)
		if words, ok := n.Loop.(*syntax.WordIter); ok && words.Name != nil {
			s.markName(words.Name.Value)
		}
	case *syntax.BinaryCmd:
		s.binary(n)
	case *syntax.Redirect:
		s.redirect(n)
	case *syntax.Stmt:
		s.visitStmt(n)
	case *syntax.CoprocClause:
		s.visitCoproc(n)
	}
}

func nodeSpan(node syntax.Node) span {
	return span{int(node.Pos().Offset()), int(node.End().Offset())}
}

func (s *suspiciousAssignScan) visitAssign(assign *syntax.Assign) {
	if assign.Name != nil {
		if assign.Value != nil {
			if s.valueSpans == nil {
				s.valueSpans = make(map[string][]span)
			}
			s.valueSpans[assign.Name.Value] = append(s.valueSpans[assign.Name.Value], nodeSpan(assign.Value))
		}
		if assign.Name.Value == "BASH_ALIASES" {
			s.all = true
		}
	}
	if assign.Index != nil {
		s.markText(int(assign.Index.Pos().Offset()), int(assign.Index.End().Offset()))
	}
}

func (s *suspiciousAssignScan) visitIf(clause *syntax.IfClause) {
	s.markStmts(clause.Then)
	if clause.Else != nil {
		s.markStmts(clause.Else.Then)
		s.markStmts(clause.Else.Cond)
	}
}

func (s *suspiciousAssignScan) visitCase(clause *syntax.CaseClause) {
	for _, item := range clause.Items {
		s.markStmts(item.Stmts)
		for _, pattern := range item.Patterns {
			s.uncertain = append(s.uncertain, nodeSpan(pattern))
		}
	}
}

func (s *suspiciousAssignScan) visitBinaryTest(test *syntax.BinaryTest) {
	switch test.Op {
	case syntax.TsEql, syntax.TsNeq, syntax.TsLeq, syntax.TsGeq, syntax.TsLss, syntax.TsGtr:
		s.markText(int(test.X.Pos().Offset()), int(test.X.End().Offset()))
		s.markText(int(test.Y.Pos().Offset()), int(test.Y.End().Offset()))
	}
}

func (s *suspiciousAssignScan) visitStmt(stmt *syntax.Stmt) {
	for _, redir := range stmt.Redirs {
		if (redir.Op == syntax.Hdoc || redir.Op == syntax.DashHdoc) && redir.Word != nil && redir.Hdoc != nil {
			lo, hi := int(redir.Word.End().Offset()), int(redir.Hdoc.Pos().Offset())
			if lo < hi {
				s.uncertain = append(s.uncertain, span{lo, hi})
			}
		}
	}
	if call, ok := stmt.Cmd.(*syntax.CallExpr); ok && stmtHasHeredoc(stmt) {
		s.nestedShellHeredoc(call)
	}
}

func (s *suspiciousAssignScan) visitCoproc(clause *syntax.CoprocClause) {
	if clause.Name == nil {
		return
	}
	name, ok := wordValue(clause.Name)
	if !ok {
		s.all = true
		return
	}
	s.markWriteName(name)
}

func (s *suspiciousAssignScan) call(call *syntax.CallExpr) {
	if len(call.Args) > 0 {
		if word, ok := wordValue(call.Args[0]); ok {
			s.callSites = append(s.callSites, callSite{name: word, offset: int(call.Pos().Offset())})
		}
	}
	for _, assign := range call.Assigns {
		if assign.Name != nil && len(call.Args) > 0 {
			// A prefix before a function call is seen inside the function body, though Assigns omits it.
			s.functionCounts[assign.Name.Value]++
		}
		if assign.Name != nil && assign.Value != nil && s.integerVars[assign.Name.Value] {
			s.markText(int(assign.Value.Pos().Offset()), int(assign.Value.End().Offset()))
		}
	}
	if len(call.Args) == 0 {
		return
	}
	name, ok := wordValue(call.Args[0])
	if !ok {
		s.all = true
		return
	}
	args := make([]string, len(call.Args))
	for i, word := range call.Args {
		args[i], _ = wordValue(word)
	}
	name, args = unwrapBuiltinCommand(name, args)
	s.callEffects(name, args, call)
	s.nestedShellInput(call)
	if len(call.Assigns) > 0 && (s.specialBuiltin(name) || strings.Contains(" declare local typeset ", " "+name+" ")) {
		for _, assign := range call.Assigns {
			if assign.Name != nil {
				s.markWriteName(assign.Name.Value)
			}
		}
	}
}

func unwrapBuiltinCommand(name string, args []string) (string, []string) {
	for name == "command" || name == "builtin" {
		next := 1
		for next < len(args) && strings.HasPrefix(args[next], "-") && args[next] != "--" {
			next++
		}
		if next < len(args) && args[next] == "--" {
			next++
		}
		if next >= len(args) {
			break
		}
		name, args = args[next], args[next:]
	}
	return name, args
}

func (s *suspiciousAssignScan) callEffects(name string, args []string, call *syntax.CallExpr) {
	switch name {
	case "eval", "source", ".":
		s.all = true
	case "alias":
		s.alias(args)
	case "trap":
		s.trap(args)
	case "mapfile", "readarray":
		s.mapfile(args, call)
	case "read":
		s.readTargets(args)
	case "getopts":
		if len(args) > 2 {
			s.markWriteName(args[2])
		} else {
			s.all = true
		}
	case "printf":
		s.printfTarget(args)
	case "let":
		for _, word := range call.Args[1:] {
			s.markText(int(word.Pos().Offset()), int(word.End().Offset()))
		}
	case "declare", "local", "typeset", "export", "readonly":
		s.wrappedDeclaration(name, args)
	case "unset":
		s.unsetTargets(args)
	case "wait":
		for i := 1; i+1 < len(args); i++ {
			if args[i] == "-p" {
				s.markWriteName(args[i+1])
			}
		}
	}
}

func (s *suspiciousAssignScan) alias(args []string) {
	for _, arg := range args[1:] {
		if strings.Contains(arg, "=") {
			s.all = true
			return
		}
	}
}

func (s *suspiciousAssignScan) trap(args []string) {
	action := 1
	if action < len(args) && args[action] == "--" {
		action++
	}
	if action < len(args) && args[action] != "" && !strings.HasPrefix(args[action], "-") {
		s.all = true
	}
}

func (s *suspiciousAssignScan) markName(name string) {
	if shellVarName.MatchString(name) {
		s.names[name] = true
	}
}

func (s *suspiciousAssignScan) markWriteName(name string) {
	name = strings.Trim(name, "{}")
	if i := strings.IndexByte(name, '['); i >= 0 {
		if j := strings.LastIndexByte(name, ']'); j > i {
			s.markTextByText(name[i+1 : j])
			name = name[:i]
		}
	}
	if shellVarName.MatchString(name) {
		s.names[name], s.writeNames[name] = true, true
	} else {
		s.all = true
	}
}

func (s *suspiciousAssignScan) markTextByText(text string) {
	for _, name := range arithmeticNames.FindAllString(text, -1) {
		s.names[name], s.arithmetic[name] = true, true
	}
}

func (s *suspiciousAssignScan) mapfile(args []string, call *syntax.CallExpr) {
	for i, arg := range args[1:] {
		if strings.HasPrefix(arg, "-") && strings.Contains(arg, "C") && (arg != "-C" || i+2 < len(args)) {
			s.all = true
		}
	}
	s.mapfileTarget(args, call)
}

func (s *suspiciousAssignScan) mapfileTarget(args []string, call *syntax.CallExpr) {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			s.markWriteName(arg)
			return
		}
		if arg == "--" {
			if i+1 < len(args) {
				s.markWriteName(args[i+1])
			}
			return
		}
		i += s.mapfileOption(args, call, i)
	}
}

func (s *suspiciousAssignScan) mapfileOption(args []string, call *syntax.CallExpr, i int) int {
	arg := args[i]
	key := strings.TrimLeft(arg, "-")
	if eq := strings.IndexByte(key, '='); eq >= 0 {
		key = key[:eq]
	}
	if key == "n" || key == "d" || key == "u" || key == "O" || key == "C" || key == "c" {
		return s.mapfileOptionArgument(args, call, i, arg, key)
	}
	return mapfileClusterArgument(args, i, arg)
}

func (s *suspiciousAssignScan) mapfileOptionArgument(args []string, call *syntax.CallExpr, i int, arg, key string) int {
	if !strings.Contains(arg, "=") && len(key) == len(strings.TrimLeft(arg, "-")) && i+1 < len(args) {
		if key == "O" && i+1 < len(call.Args) {
			s.markWord(call.Args[i+1])
		}
		return 1
	}
	if key == "O" && strings.Contains(arg, "=") && i < len(call.Args) {
		s.markWord(call.Args[i])
	}
	return 0
}

func mapfileClusterArgument(args []string, i int, arg string) int {
	options := strings.TrimLeft(arg, "-")
	for _, option := range options {
		if strings.ContainsRune("nduOCc", option) && len(options) == 1 && i+1 < len(args) {
			return 1
		}
	}
	return 0
}

func (s *suspiciousAssignScan) markWord(word *syntax.Word) {
	s.markText(int(word.Pos().Offset()), int(word.End().Offset()))
}

func (s *suspiciousAssignScan) readTargets(args []string) {
	found := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			found = s.markReadTargets(args, i+1) || found
			return
		}
		if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			consumed, targetFound := s.readOptionTarget(args, i, arg)
			i += consumed
			found = found || targetFound
			continue
		}
		if arg == "" {
			s.all = true
			found = true
			continue
		}
		s.markWriteName(arg)
		found = true
	}
	if !found {
		s.markWriteName("REPLY")
	}
}

func (s *suspiciousAssignScan) markReadTargets(args []string, start int) bool {
	found := start < len(args)
	for _, target := range args[start:] {
		s.markWriteName(target)
	}
	return found
}

func (s *suspiciousAssignScan) readOptionTarget(args []string, i int, arg string) (int, bool) {
	option := strings.TrimLeft(arg, "-")
	if option == "" {
		return 0, false
	}
	if option[0] == 'a' {
		if len(option) > 1 {
			s.markWriteName(option[1:])
			return 0, true
		}
		if i+1 < len(args) {
			s.markWriteName(args[i+1])
			return 1, true
		}
		return 1, false
	}
	if strings.ContainsRune("d n N p t u i", rune(option[0])) && len(option) == 1 {
		return 1, false
	}
	return 0, false
}

func (s *suspiciousAssignScan) printfTarget(args []string) {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "-v" {
			if i+1 < len(args) {
				s.markWriteName(args[i+1])
			} else {
				s.all = true
			}
			return
		}
		if strings.HasPrefix(arg, "-v") && len(arg) > 2 {
			s.markWriteName(arg[2:])
			return
		}
	}
}

func (s *suspiciousAssignScan) unsetTargets(args []string) {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "-v" || arg == "-f" {
			continue
		}
		if arg == "--" {
			i++
			if i < len(args) {
				s.markWriteName(args[i])
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		s.markWriteName(arg)
	}
}

func (s *suspiciousAssignScan) wrappedDeclaration(name string, args []string) {
	markAttributes := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flags := strings.TrimLeft(arg, "-")
			if strings.Contains(flags, "n") && (name == "declare" || name == "local" || name == "typeset") {
				s.all = true
			}
			if strings.ContainsAny(flags, "iulc") {
				markAttributes = true
			}
			continue
		}
		if n, _, ok := strings.Cut(arg, "="); ok {
			s.markWriteName(n)
		} else if markAttributes {
			s.markWriteName(arg)
		}
	}
}

func (s *suspiciousAssignScan) specialBuiltin(name string) bool {
	if strings.Contains(" export readonly ", " "+name+" ") {
		return true
	}
	if !s.posix {
		return false
	}
	return strings.Contains(" : . break continue eval exec exit return set shift trap unset ", " "+name+" ")
}

func (s *suspiciousAssignScan) declaration(decl *syntax.DeclClause) {
	if decl.Variant == nil {
		return
	}
	cmd := decl.Variant.Value
	// Only the option words: the text of the whole clause holds nested values, and matching it at every nested
	// declaration was quadratic.
	declText := declOptions(decl)
	integer := integerDeclOption.MatchString(declText)
	attribute := caseTransformDeclOption.MatchString(declText)
	if namerefDeclOption.MatchString(declText) && (cmd == "declare" || cmd == "local" || cmd == "typeset") {
		s.all = true
	}
	s.declarationArgs(decl, integer, attribute)
}

// declOptions — the literal option words of a declaration (-i, -n, -u, ...) joined by spaces.
func declOptions(decl *syntax.DeclClause) string {
	var b strings.Builder
	for _, assign := range decl.Args {
		if !assign.Naked {
			continue
		}
		word := ""
		if assign.Name != nil {
			word = assign.Name.Value
		} else if assign.Value != nil {
			word, _ = wordValue(assign.Value)
		}
		if strings.HasPrefix(word, "-") {
			b.WriteString(" " + word)
		}
	}
	return b.String()
}

func (s *suspiciousAssignScan) declarationArgs(decl *syntax.DeclClause, integer, attribute bool) {
	cmd := decl.Variant.Value
	for _, assign := range decl.Args {
		s.declarationArg(assign, cmd, integer, attribute)
	}
}

func (s *suspiciousAssignScan) declarationArg(assign *syntax.Assign, cmd string, integer, attribute bool) {
	if assign.Name == nil {
		s.markNakedDeclaration(assign)
		return
	}
	if assign.Naked && strings.HasPrefix(assign.Name.Value, "-") {
		return
	}
	name := assign.Name.Value
	if integer {
		s.integerVars[name], s.names[name] = true, true
	}
	if attribute {
		s.names[name] = true
	}
	if cmd == "readonly" {
		s.markWriteName(name)
	}
	if integer && assign.Value != nil {
		s.markWord(assign.Value)
	}
}

func (s *suspiciousAssignScan) markNakedDeclaration(assign *syntax.Assign) {
	if assign.Value == nil {
		return
	}
	word, ok := wordValue(assign.Value)
	if !ok {
		return
	}
	if name, _, hasValue := strings.Cut(word, "="); hasValue {
		s.markWriteName(name)
	}
}

func (s *suspiciousAssignScan) nestedShellInput(call *syntax.CallExpr) {
	if len(call.Args) == 0 {
		return
	}
	values := make([]string, len(call.Args))
	for i, word := range call.Args {
		values[i], _ = wordValue(word)
	}
	if i, ok := shellCommandIndex(values); ok && shellReadsInlineScript(values[i+1:]) {
		s.all = true
	}
}

func (s *suspiciousAssignScan) nestedShellHeredoc(call *syntax.CallExpr) {
	if len(call.Args) == 0 {
		return
	}
	values := make([]string, len(call.Args))
	for i, word := range call.Args {
		values[i], _ = wordValue(word)
	}
	i, ok := shellCommandIndex(values)
	if ok && (shellReadsInlineScript(values[i+1:]) || !shellHasScriptFile(values[i+1:])) {
		s.all = true
	}
}

// shellCommandIndex follows the package's registered wrapper chain using its existing option
// parsers, then returns the first shell command. Arguments to ordinary commands are not candidates.
func shellCommandIndex(args []string) (int, bool) {
	for i := 0; i < len(args); {
		name := baseName(args[i])
		if IsShell(name) {
			return i, true
		}
		if !wrappers[name] {
			return 0, false
		}
		rest := args[i+1:]
		start := wrapperStart(name, rest)
		if name == "timeout" && start < len(rest) && rest[start] != "--" {
			start++
		}
		if name == "env" {
			start = envCommandStart(rest, start)
		}
		if name == "parallel" {
			var ok bool
			start, ok = parallelCommandStart(rest)
			if !ok {
				return 0, false
			}
		}
		if start >= len(rest) {
			return 0, false
		}
		i += start + 1
	}
	return 0, false
}

func shellReadsInlineScript(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if arg == "-c" || arg == "-s" {
			return true
		}
		if arg == "--command" || strings.HasPrefix(arg, "--command=") {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") {
			if strings.ContainsAny(arg[1:], "cs") {
				return true
			}
			if arg == "-o" || arg == "-O" {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--") {
			if arg == "--rcfile" || arg == "--init-file" || arg == "--startup-file" {
				i++
			}
			continue
		}
		return false
	}
	return false
}

func shellHasScriptFile(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return i+1 < len(args)
		}
		if arg == "-c" || arg == "-s" || strings.HasPrefix(arg, "--command=") {
			return false
		}
		if strings.HasPrefix(arg, "-") {
			if arg == "-o" || arg == "-O" || arg == "--rcfile" || arg == "--init-file" || arg == "--startup-file" {
				i++
			}
			continue
		}
		return true
	}
	return false
}

func (s *suspiciousAssignScan) parameter(param *syntax.ParamExp) {
	if param.Index != nil || param.Slice != nil {
		s.markText(int(param.Pos().Offset()), int(param.End().Offset()))
	}
	if param.Param != nil && param.Exp != nil && (param.Exp.Op == syntax.AssignUnset || param.Exp.Op == syntax.AssignUnsetOrNull) {
		s.names[param.Param.Value] = true
	}
	if param.Exp != nil && (param.Exp.Op == syntax.AlternateUnset || param.Exp.Op == syntax.AlternateUnsetOrNull || param.Exp.Op == syntax.DefaultUnset || param.Exp.Op == syntax.DefaultUnsetOrNull) {
		s.uncertain = append(s.uncertain, span{int(param.Pos().Offset()), int(param.End().Offset())})
	}
	if param.Param != nil {
		s.functionRefs[param.Param.Value] = append(s.functionRefs[param.Param.Value], int(param.Pos().Offset()))
	}
}

func (s *suspiciousAssignScan) binary(binary *syntax.BinaryCmd) {
	if binary.Op == syntax.AndStmt || binary.Op == syntax.OrStmt {
		s.uncertain = append(s.uncertain, span{int(binary.Y.Pos().Offset()), int(binary.Y.End().Offset())})
	}
}

func (s *suspiciousAssignScan) redirect(redir *syntax.Redirect) {
	if redir.Hdoc != nil {
		s.uncertain = append(s.uncertain, span{int(redir.Hdoc.Pos().Offset()), int(redir.Hdoc.End().Offset())})
	}
	if redir.Op == syntax.WordHdoc && redir.Word != nil {
		s.uncertain = append(s.uncertain, span{int(redir.Word.Pos().Offset()), int(redir.Word.End().Offset())})
	}
	if redir.N != nil {
		name := strings.Trim(redir.N.Value, "{}")
		if shellVarName.MatchString(name) && !numericValue.MatchString(name) {
			s.markWriteName(name)
		}
	}
}

func (s *suspiciousAssignScan) result() (map[string]bool, bool) {
	s.prepareArithmeticNames()
	order := assignmentOrder(s.assigns)
	functions := mergeSpans(s.functionBodies)
	if len(functions) > 0 {
		s.markFunctionBodyNames(order, functions)
	}
	markAssignmentsInSpans(s.assigns, order, mergeSpans(s.uncertain), s.names)
	return s.names, s.all
}

func (s *suspiciousAssignScan) prepareArithmeticNames() {
	for name := range s.integerVars {
		s.names[name] = true
		for _, value := range s.valueSpans[name] {
			s.mathSpans = append(s.mathSpans, value)
		}
	}
	s.collectArithmeticNames()
	for _, assign := range s.assigns {
		if s.integerVars[assign.Name] && (assign.Unknown || assign.Value != "" && !numericValue.MatchString(assign.Value)) {
			s.all = true
		}
		if s.arithmetic[assign.Name] && (assign.Unknown || assign.Value != "" && !numericValue.MatchString(assign.Value)) {
			s.all = true
		}
	}
	for name := range s.arithmetic {
		if s.writeNames[name] {
			s.all = true
		}
	}
}

func (s *suspiciousAssignScan) markFunctionBodyNames(order []int, functions []span) {
	for _, assign := range s.assigns {
		s.functionCounts[assign.Name]++
	}
	// A single assignment followed only by reads in its own body has a stable
	// value for those reads. Another textual assignment or call prefix defeats it.
	safeBodyNames := s.safeFunctionBodyNames(functions)
	for _, index := range order {
		a := s.assigns[index]
		if containsMergedSpan(functions, a.Offset) && !safeBodyNames[a.Name] {
			s.markFunctionBodyAssignment(a)
		}
	}
	s.markFunctionReferenceNames(functions)
}

func (s *suspiciousAssignScan) markFunctionBodyAssignment(a Assign) {
	if s.nestedFunctionWrite(a) {
		inBody, outside := s.referencesOutsideScope(a)
		if inBody || outside && strings.Contains(s.src, "lastpipe") {
			s.names[a.Name] = true
		} else if outside || s.inFunctionIsolatedScope(a.Offset) {
			s.functionUnknown[a.Offset] = true
		}
		return
	}
	s.names[a.Name] = true
}

func (s *suspiciousAssignScan) markFunctionReferenceNames(functions []span) {
	for name, offsets := range s.functionRefs {
		if s.functionCounts[name] < 2 {
			continue
		}
		for _, off := range offsets {
			if containsMergedSpan(functions, off) {
				s.names[name] = true
				break
			}
		}
	}
}

func (s *suspiciousAssignScan) inFunctionIsolatedScope(off int) bool {
	for _, isolated := range s.functionIsolated {
		if isolated.start <= off && off < isolated.end {
			return true
		}
	}
	return false
}

func (s *suspiciousAssignScan) safeFunctionBodyNames(functions []span) map[string]bool {
	counts := make(map[string]int)
	var only map[string]int
	for i := range s.assigns {
		a := s.assigns[i]
		if containsMergedSpan(functions, a.Offset) {
			counts[a.Name]++
			if only == nil {
				only = make(map[string]int)
			}
			if counts[a.Name] == 1 {
				only[a.Name] = a.Offset
			}
		}
	}
	safe := make(map[string]bool)
	for name, off := range only {
		if s.safeFunctionBodyName(name, off, counts[name], functions) {
			safe[name] = true
		}
	}
	return safe
}

func (s *suspiciousAssignScan) safeFunctionBodyName(name string, off, bodyWrites int, functions []span) bool {
	if bodyWrites != 1 || s.functionCounts[name] != 1 {
		return false
	}
	owner, bodyName, owners := s.functionBodyOwner(off)
	calls := s.functionCalls[bodyName]
	if bodyName == "" || owners != 1 || len(calls) != 1 || calls[0] <= owner.end {
		return false
	}
	if !s.assignmentMatchesFunctionScope(name, off, owner) || s.inFunctionIsolatedScope(off) {
		return false
	}
	if !s.referencesFollowBodyAssignment(name, off, owner, functions) {
		return false
	}
	if s.functionCalledBetweenAssignmentAndReference(name, off) {
		return false
	}
	return !s.functionBodyHasDynamicEffects(off, functions)
}

func (s *suspiciousAssignScan) functionBodyOwner(off int) (span, string, int) {
	var owner span
	name, count := "", 0
	for _, body := range s.functionBodies {
		if body.start <= off && off < body.end {
			owner, name, count = body, s.functionNames[body], count+1
		}
	}
	return owner, name, count
}

func (s *suspiciousAssignScan) assignmentMatchesFunctionScope(name string, off int, owner span) bool {
	if off < owner.start || off >= owner.end {
		return false
	}
	for i := range s.assigns {
		a := s.assigns[i]
		if a.Name == name && a.Offset == off && (a.ScopeStart != owner.start || a.ScopeEnd != owner.end) {
			return false
		}
	}
	return true
}

func (s *suspiciousAssignScan) referencesFollowBodyAssignment(name string, off int, owner span, functions []span) bool {
	seen := false
	for _, ref := range s.functionRefs[name] {
		if !containsMergedSpan(functions, ref) || ref <= off || ref < owner.start || ref >= owner.end {
			return false
		}
		seen = true
	}
	return seen
}

func (s *suspiciousAssignScan) functionCalledBetweenAssignmentAndReference(name string, off int) bool {
	for _, calls := range s.functionCalls {
		for _, called := range calls {
			if callBeforeReference(called, off, s.functionRefs[name]) {
				return true
			}
		}
	}
	return false
}

func callBeforeReference(called, assignment int, refs []int) bool {
	for _, ref := range refs {
		if assignment < called && called < ref {
			return true
		}
	}
	return false
}

func (s *suspiciousAssignScan) functionBodyHasDynamicEffects(off int, functions []span) bool {
	for _, body := range functions {
		if body.start <= off && off < body.end {
			text := s.src[body.start:body.end]
			if strings.Contains(text, "eval") || strings.Contains(text, "trap") || strings.Contains(text, "source") || strings.Contains(text, " . ") {
				return true
			}
		}
	}
	return false
}

func (s *suspiciousAssignScan) nestedFunctionWrite(a Assign) bool {
	insideFunction, matchingBody := false, false
	for _, body := range s.functionBodies {
		if body.start <= a.Offset && a.Offset < body.end {
			insideFunction = true
			if a.ScopeStart == body.start && a.ScopeEnd == body.end {
				matchingBody = true
			}
		}
	}
	if !insideFunction {
		return false
	}
	for _, isolated := range s.functionIsolated {
		if isolated.start <= a.Offset && a.Offset < isolated.end {
			return true
		}
	}
	return !matchingBody
}

func (s *suspiciousAssignScan) referencesOutsideScope(a Assign) (inBody, outside bool) {
	for _, ref := range s.functionRefs[a.Name] {
		if ref <= a.Offset || (a.ScopeStart <= ref && ref < a.ScopeEnd) {
			continue
		}
		found := false
		for _, body := range s.functionBodies {
			if body.start <= ref && ref < body.end {
				inBody = true
				found = true
				break
			}
		}
		if !found {
			outside = true
		}
	}
	return inBody, outside
}

func assignmentOrder(assigns []Assign) []int {
	order := make([]int, len(assigns))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return assigns[order[i]].Offset < assigns[order[j]].Offset })
	return order
}

func mergeSpans(spans []span) []span {
	if len(spans) < 2 {
		return spans
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start == spans[j].start {
			return spans[i].end < spans[j].end
		}
		return spans[i].start < spans[j].start
	})
	merged := spans[:1]
	for _, current := range spans[1:] {
		last := &merged[len(merged)-1]
		if current.start > last.end {
			merged = append(merged, current)
		} else if current.end > last.end {
			last.end = current.end
		}
	}
	return merged
}

func markAssignmentsInSpans(assigns []Assign, order []int, spans []span, names map[string]bool) {
	spanIndex := 0
	for _, assignmentIndex := range order {
		assignment := assigns[assignmentIndex]
		for spanIndex < len(spans) && spans[spanIndex].end <= assignment.Offset {
			spanIndex++
		}
		if spanIndex < len(spans) && spans[spanIndex].start <= assignment.Offset {
			names[assignment.Name] = true
		}
	}
}

func containsMergedSpan(spans []span, off int) bool {
	i := sort.Search(len(spans), func(i int) bool { return spans[i].start > off }) - 1
	return i >= 0 && off < spans[i].end
}

func (s *suspiciousAssignScan) collectArithmeticNames() {
	sort.Slice(s.mathSpans, func(i, j int) bool {
		if s.mathSpans[i].start == s.mathSpans[j].start {
			return s.mathSpans[i].end < s.mathSpans[j].end
		}
		return s.mathSpans[i].start < s.mathSpans[j].start
	})
	if len(s.mathSpans) == 0 {
		return
	}
	merged := s.mathSpans[:0]
	for _, current := range s.mathSpans {
		if len(merged) == 0 || current.start > merged[len(merged)-1].end {
			merged = append(merged, current)
			continue
		}
		if current.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = current.end
		}
	}
	for _, source := range merged {
		for _, name := range arithmeticNames.FindAllString(s.src[source.start:source.end], -1) {
			s.names[name], s.arithmetic[name] = true, true
		}
	}
}

func stmtHasHeredoc(stmt *syntax.Stmt) bool {
	for _, redir := range stmt.Redirs {
		if redir.Op == syntax.Hdoc || redir.Op == syntax.DashHdoc || redir.Op == syntax.WordHdoc {
			return true
		}
	}
	return false
}

type assignContext struct {
	scopes        []span
	scopeIndex    spanIndex
	functionIndex spanIndex
	funsubs       []span
	funsubLocals  []localBinding
	replySubs     []span
	// assignmentModes and declarations give the tilde mark of a value: the
	// shell modes at the assignment and whether a declaration command makes it.
	assignmentModes map[*syntax.Assign]shellModes
	declarations    map[*syntax.Assign]bool
	uncertainDecls  map[*syntax.DeclClause]bool
}

func newAssignContext(file *syntax.File, regions scopeSet) assignContext {
	uncertainDecls := uncertainDeclarationRedirects(file)
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
	scopeIndex := newSpanIndex(scopes)
	ctx := assignContext{
		scopes:         scopes,
		scopeIndex:     scopeIndex,
		functionIndex:  newSpanIndex(regions.functions),
		funsubs:        funsubSpans(file),
		funsubLocals:   funsubLocalBindings(file, scopeIndex, uncertainDecls),
		uncertainDecls: uncertainDecls,
	}
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
	uncertainStatements := newSpanIndex(uncertainRedirectStatements(node))
	uncertain := make(map[*syntax.DeclClause]bool)
	syntax.Walk(node, func(node syntax.Node) bool {
		decl, ok := node.(*syntax.DeclClause)
		if !ok || decl.Variant == nil || !isLocalDeclaration(decl) {
			return true
		}
		off := int(decl.Pos().Offset())
		if uncertainStatements.contains(off) {
			uncertain[decl] = true
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

func stmtHasNoCommand(stmt *syntax.Stmt) bool {
	if stmt.Cmd == nil {
		return true
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	return ok && len(call.Args) == 0
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

type functionDocScope struct {
	doc   span
	owner span
}

type localBinding struct {
	name      string
	offset    int
	start     int
	end       int
	uncertain bool
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
	scopes       []Scope
	functions    []span
	functionDocs []functionDocScope
	index        *syntaxIndex
}

type syntaxIndex struct {
	functions         []*syntax.FuncDecl
	redirects         []*syntax.Redirect
	stmts             map[syntax.Command][]*syntax.Stmt
	functionRedirects map[*syntax.FuncDecl]map[*syntax.Redirect]bool
}

func indexSyntax(file *syntax.File) *syntaxIndex {
	index := &syntaxIndex{stmts: make(map[syntax.Command][]*syntax.Stmt)}
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
		case *syntax.Redirect:
			addRedirect(n)
		case *syntax.Stmt:
			if n.Cmd != nil {
				index.stmts[n.Cmd] = append(index.stmts[n.Cmd], n)
			}
			for _, redir := range n.Redirs {
				addRedirect(redir)
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
	out := scopeSet{index: index}
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
		docInfo := functionDocScope{doc: doc, owner: owner}
		out.functionDocs = append(out.functionDocs, docInfo)
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
