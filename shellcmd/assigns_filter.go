package shellcmd

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// closedAssignScript is the gate for trusting a function body's write order.
// Unknown syntax fails closed; this list intentionally contains only constructs
// whose effects Assigns can model for this purpose.
func closedAssignScript(file *syntax.File, src string) bool {
	functions := make(map[string]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil {
			return true
		}
		if fn, ok := node.(*syntax.FuncDecl); ok && fn.Name != nil {
			functions[fn.Name.Value] = true
		}
		return true
	})
	valid := true
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil {
			return true
		}
		if !valid {
			return false
		}
		if !validAssignNode(node, src) || !validAssignNodeProperties(node, src, functions) {
			valid = false
			return false
		}
		return true
	})
	return valid
}

func validAssignNode(node syntax.Node, src string) bool {
	switch n := node.(type) {
	case *syntax.File, *syntax.Stmt, *syntax.Assign, *syntax.Redirect,
		*syntax.FuncDecl, *syntax.Subshell, *syntax.Block, *syntax.IfClause,
		*syntax.WhileClause, *syntax.ForClause, *syntax.WordIter,
		*syntax.CaseClause, *syntax.CaseItem, *syntax.BinaryCmd,
		*syntax.CallExpr, *syntax.Word, *syntax.Lit, *syntax.SglQuoted,
		*syntax.DblQuoted, *syntax.CmdSubst, *syntax.ParamExp:
		return true
	case *syntax.DeclClause:
		return validDeclaration(n, src)
	case *syntax.ArithmCmd, *syntax.ArithmExp, *syntax.LetClause,
		*syntax.CStyleLoop, *syntax.TestClause, *syntax.BinaryTest,
		*syntax.UnaryTest, *syntax.ParenTest, *syntax.ArrayExpr,
		*syntax.ArrayElem, *syntax.ProcSubst, *syntax.CoprocClause,
		*syntax.TimeClause, *syntax.BraceExp, *syntax.TestDecl:
		return false
	default:
		// This also rejects syntax added by a future parser version.
		return false
	}
}

func validAssignNodeProperties(node syntax.Node, src string, functions map[string]bool) bool {
	return validAssignStructuralProperties(node) && validAssignValueProperties(node, src, functions)
}

func validAssignStructuralProperties(node syntax.Node) bool {
	switch n := node.(type) {
	case *syntax.Stmt:
		return !n.Negated && !n.Background && !n.Coprocess && !n.Disown
	case *syntax.BinaryCmd:
		return n.Op == syntax.AndStmt || n.Op == syntax.OrStmt || n.Op == syntax.Pipe || n.Op == syntax.PipeAll
	case *syntax.FuncDecl:
		return n.Name != nil && shellVarName.MatchString(n.Name.Value) && !bashBuiltinNames[n.Name.Value] && !allowedAssignCommand[n.Name.Value]
	case *syntax.ForClause:
		return n.Loop != nil && !n.Select && !n.Braces
	case *syntax.WhileClause:
		return !n.Until
	case *syntax.CaseClause:
		return !n.Braces
	default:
		return true
	}
}

func validAssignValueProperties(node syntax.Node, src string, functions map[string]bool) bool {
	switch n := node.(type) {
	case *syntax.Assign:
		return n.Name != nil && n.Index == nil && n.Array == nil && !n.Append && (n.Naked || validWord(n.Value, src))
	case *syntax.Redirect:
		return validWord(n.Word, src) && validWord(n.Hdoc, src)
	case *syntax.CallExpr:
		return validCall(n, src, functions)
	case *syntax.CaseItem:
		return validWords(n.Patterns, src)
	case *syntax.WordIter:
		return validWords(n.Items, src)
	case *syntax.ParamExp:
		return validParam(n)
	case *syntax.CaseClause:
		return validWord(n.Word, src)
	default:
		return true
	}
}

func validWords(words []*syntax.Word, src string) bool {
	for _, word := range words {
		if !validWord(word, src) {
			return false
		}
	}
	return true
}

func validWord(word *syntax.Word, src string) bool {
	if word == nil {
		return true
	}
	if wordTildeOptions(word, src, false, false, false, false) != 0 ||
		wordTildeOptions(word, src, true, false, false, false) != 0 {
		return false
	}
	valid := true
	syntax.Walk(word, func(node syntax.Node) bool {
		if node == nil {
			return true
		}
		switch n := node.(type) {
		case *syntax.Word, *syntax.Lit:
		case *syntax.CmdSubst:
			if n.Backquotes || n.TempFile || n.ReplyVar {
				valid = false
			}
			return false
		case *syntax.SglQuoted:
			if n.Dollar {
				valid = false
			}
		case *syntax.DblQuoted:
			if n.Dollar {
				valid = false
			}
		case *syntax.ParamExp:
			if !validParam(n) {
				valid = false
			}
		default:
			valid = false
		}
		return valid
	})
	return valid
}

func validParam(p *syntax.ParamExp) bool {
	return p != nil && p.Param != nil && shellVarName.MatchString(p.Param.Value) && !p.Excl && !p.Length && !p.Width && !p.IsSet &&
		p.Flags == nil && p.NestedParam == nil && p.Index == nil && p.Slice == nil &&
		p.Repl == nil && p.Names == 0 && p.Split == syntax.OptUnset &&
		p.GlobSubst == syntax.OptUnset && p.RcExpand == syntax.OptUnset &&
		len(p.Modifiers) == 0 && (p.Exp == nil || validExpansion(p.Exp))
}

func validExpansion(e *syntax.Expansion) bool {
	return e != nil && (e.Op == syntax.DefaultUnset || e.Op == syntax.DefaultUnsetOrNull ||
		e.Op == syntax.RemSmallPrefix || e.Op == syntax.RemLargePrefix ||
		e.Op == syntax.RemSmallSuffix || e.Op == syntax.RemLargeSuffix)
}

func validDeclaration(d *syntax.DeclClause, src string) bool {
	if d.Variant == nil || !strings.Contains(" local export declare readonly ", " "+d.Variant.Value+" ") {
		return false
	}
	hasAssignment := false
	for _, arg := range d.Args {
		if arg.Name == nil || arg.Index != nil || arg.Array != nil || arg.Append {
			return false
		}
		if arg.Naked {
			option := arg.Name.Value
			if option != "--" && !validDeclarationOption(option) {
				return false
			}
			continue
		}
		if !shellVarName.MatchString(arg.Name.Value) || !validWord(arg.Value, src) {
			return false
		}
		hasAssignment = true
	}
	return hasAssignment
}

func validDeclarationOption(option string) bool {
	if len(option) < 2 || (option[0] != '-' && option[0] != '+') {
		return false
	}
	for _, flag := range option[1:] {
		if !strings.ContainsRune("gxr", flag) || (flag == 'g' && option[0] == '+') {
			return false
		}
	}
	return true
}

func validCall(c *syntax.CallExpr, src string, functions map[string]bool) bool {
	for _, a := range c.Assigns {
		if a.Name == nil || a.Index != nil || a.Array != nil || a.Append || a.Naked || !validWord(a.Value, src) {
			return false
		}
	}
	if len(c.Args) == 0 {
		return true
	}
	name, ok := wordValue(c.Args[0])
	if !ok || name == "" {
		return false
	}
	for _, arg := range c.Args {
		if !validWord(arg, src) {
			return false
		}
	}
	return validCallName(name, c.Args, functions)
}

func validCallName(name string, args []*syntax.Word, functions map[string]bool) bool {
	if name == "trap" {
		return validTrapCall(args)
	}
	if name == "printf" && !validPrintfCall(args) {
		return false
	}
	if functions[name] {
		return !bashBuiltinNames[name] && !allowedAssignCommand[name]
	}
	if allowedAssignCommand[name] {
		return true
	}
	return false
}

func validTrapCall(args []*syntax.Word) bool {
	action := 1
	if len(args) > action && args[action] != nil {
		if separator, literal := wordValue(args[action]); literal && separator == "--" {
			action++
		}
	}
	if len(args) < action+2 {
		return false
	}
	for _, signalWord := range args[action+1:] {
		signal, literal := wordValue(signalWord)
		if !literal || (signal != "EXIT" && signal != "0") {
			return false
		}
	}
	return true
}

func validPrintfCall(args []*syntax.Word) bool {
	if len(args) <= 1 {
		return true
	}
	opt, literal := wordValue(args[1])
	return literal && (opt == "--" || !strings.HasPrefix(opt, "-v"))
}

var allowedAssignCommand = func() map[string]bool {
	names := strings.Fields(`curl wget echo printf cat jq grep sed awk head tail tr cut sort uniq wc base64 date mktemp rm mkdir cp mv ls sleep true false : return exit test [`)
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}()

func neverStableFunctionName(name string) bool {
	if strings.Contains(" PWD OLDPWD OPTARG OPTIND REPLY MAPFILE BASH_REMATCH COPROC DIRSTACK RANDOM SECONDS LINENO _ ", " "+name+" ") {
		return true
	}
	return strings.HasPrefix(name, "BASH_") || name == "FUNCNAME" || name == "PIPESTATUS"
}

var bashBuiltinNames = func() map[string]bool {
	names := strings.Fields(`: . [ alias bg bind break builtin caller cd command compgen complete compopt continue declare dirs disown echo enable eval exec exit export false fc fg getopts hash help history jobs kill let local logout mapfile popd printf pushd pwd read readarray readonly return set shift shopt source suspend test times trap true type typeset ulimit umask unalias unset wait`)
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}()
