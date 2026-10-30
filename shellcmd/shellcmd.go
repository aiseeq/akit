// Package shellcmd находит простые команды в исходном тексте оболочки.
package shellcmd

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Call описывает простой вызов программы в тексте оболочки.
type Call struct {
	Name string
	Args []string
	Raw  string
	Line int
	Via  []string
}

// Calls возвращает вызовы программ во всех частях скрипта.
func Calls(cmd string) ([]Call, error) {
	var calls []Call
	if err := parse(cmd, 0, nil, make(map[string]string), &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

// Find возвращает только вызовы с указанными именами.
func Find(cmd string, names ...string) ([]Call, error) {
	calls, err := Calls(cmd)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	filtered := calls[:0]
	for _, call := range calls {
		if wanted[call.Name] {
			filtered = append(filtered, call)
		}
	}
	return filtered, nil
}

func parse(src string, lineBase int, inherited []string, vars map[string]string, calls *[]Call) error {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		return fmt.Errorf("shellcmd: parse shell: %w", err)
	}
	return walkCalls(file, src, lineBase, inherited, vars, calls)
}

func walkCalls(file *syntax.File, src string, lineBase int, inherited []string, vars map[string]string, calls *[]Call) error {
	timedCalls := timedCallSet(file)
	var shellInputs []*syntax.Stmt
	var parseErr error
	syntax.Walk(file, func(node syntax.Node) bool {
		if parseErr != nil { //nolint:silent-error-handling // Обход прекращается после сохранения ошибки для возврата наружу.
			return false
		}
		if _, ok := node.(*syntax.Redirect); ok {
			return false // Содержимое heredoc — данные, пока оно не передано оболочке.
		}
		if clause, ok := node.(*syntax.TimeClause); ok {
			start, end := int(clause.Pos().Offset()), int(clause.End().Offset())
			if !validSpan(src, start, end) {
				parseErr = fmt.Errorf("shellcmd: invalid source span for time clause")
				return false
			}
			*calls = append(*calls, Call{Name: "time", Raw: src[start:end], Line: int(clause.Pos().Line()) + lineBase, Via: append([]string(nil), inherited...)})
		}
		if stmt, ok := node.(*syntax.Stmt); ok && shellInput(stmt, src, vars) {
			shellInputs = append(shellInputs, stmt)
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		if len(call.Args) == 0 {
			collectAssignments(call, src, vars)
			return true
		}
		args := words(call.Args, src, vars)
		name := commandName(args[0])
		start, end := int(call.Args[0].Pos().Offset()), int(call.Args[len(call.Args)-1].End().Offset())
		if !validSpan(src, start, end) {
			parseErr = fmt.Errorf("shellcmd: invalid source span for command")
			return false
		}
		via := append([]string(nil), inherited...)
		if timedCalls[call] {
			via = append(via, "time")
		}
		entry := Call{Name: name, Args: append([]string(nil), args[1:]...), Raw: src[start:end], Line: int(call.Pos().Line()) + lineBase, Via: via}
		*calls = append(*calls, entry)
		if err := expand(entry, lineBase, vars, calls); err != nil {
			parseErr = err
			return false
		}
		if nested, wrapperVia, ok := unwrap(name, args[1:]); ok {
			child := Call{Name: nested[0], Args: nested[1:], Raw: entry.Raw, Line: entry.Line, Via: append(append([]string(nil), inherited...), wrapperVia...)}
			*calls = append(*calls, child)
			if err := expand(child, lineBase, vars, calls); err != nil {
				parseErr = err
				return false
			}
		}
		return true
	})
	if parseErr != nil {
		return fmt.Errorf("shellcmd: walk syntax tree: %w", parseErr)
	}
	return parseHeredocStmts(shellInputs, src, lineBase, inherited, vars, calls)
}

func timedCallSet(file *syntax.File) map[*syntax.CallExpr]bool {
	timedCalls := make(map[*syntax.CallExpr]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		clause, ok := node.(*syntax.TimeClause)
		if ok {
			syntax.Walk(clause.Stmt, func(child syntax.Node) bool {
				if call, ok := child.(*syntax.CallExpr); ok {
					timedCalls[call] = true
				}
				return true
			})
		}
		return true
	})
	return timedCalls
}

func collectAssignments(call *syntax.CallExpr, src string, vars map[string]string) {
	for _, assign := range call.Assigns {
		if assign.Name == nil || assign.Value == nil {
			continue
		}
		value, literal := wordValue(assign.Value)
		if !literal {
			value = wordText(assign.Value, src)
		}
		vars[assign.Name.Value] = value
	}
}

func commandName(name string) string {
	if strings.Contains(name, "$(") || strings.Contains(name, "`") {
		return ""
	}
	return baseName(name)
}

func validSpan(src string, start, end int) bool {
	return start >= 0 && end >= start && end <= len(src)
}

func parseHeredocStmts(stmts []*syntax.Stmt, src string, lineBase int, inherited []string, vars map[string]string, calls *[]Call) error {
	for _, stmt := range stmts {
		name, shellVia, ok := shellInputName(stmt, src, vars)
		if !ok {
			continue
		}
		for _, redir := range stmt.Redirs {
			if redir.Hdoc == nil {
				continue
			}
			body, literal := wordValue(redir.Hdoc)
			if !literal {
				continue
			}
			via := append(append(append([]string(nil), inherited...), shellVia...), name)
			if err := parseNested(body, int(redir.Hdoc.Pos().Line())+lineBase-1, via, vars, calls); err != nil {
				return err
			}
		}
	}
	return nil
}

func shellInput(stmt *syntax.Stmt, src string, vars map[string]string) bool {
	_, _, ok := shellInputName(stmt, src, vars)
	return ok
}

func shellInputName(stmt *syntax.Stmt, src string, vars map[string]string) (string, []string, bool) {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", nil, false
	}
	args := words(call.Args, src, vars)
	name, args := commandName(args[0]), args[1:]
	var via []string
	for {
		if isShell(name) {
			for _, redir := range stmt.Redirs {
				if redir.Hdoc != nil {
					return name, via, true
				}
			}
			return "", nil, false
		}
		if target, ok := execTarget(args); ok {
			via = append(via, name, "exec")
			name, args = commandName(target[0]), target[1:]
			continue
		}
		wrapped, wrapperVia, ok := unwrap(name, args)
		if !ok {
			return "", nil, false
		}
		via = append(via, wrapperVia...)
		name, args = commandName(wrapped[0]), wrapped[1:]
	}
}

func isShell(name string) bool {
	return name == "bash" || name == "sh" || name == "zsh"
}

type nestedParseError struct {
	script string
	err    error
}

func (e *nestedParseError) Error() string { return e.err.Error() }
func (e *nestedParseError) Unwrap() error { return e.err }

func parseNested(src string, lineBase int, via []string, vars map[string]string, calls *[]Call) error {
	nestedCalls := make([]Call, 0)
	nestedVars := make(map[string]string, len(vars))
	maps.Copy(nestedVars, vars)
	if err := parse(src, lineBase, via, nestedVars, &nestedCalls); err != nil {
		var nestedErr *nestedParseError
		if errors.As(err, &nestedErr) {
			return fmt.Errorf("shellcmd: parse nested shell: %w", err)
		}
		return fmt.Errorf("shellcmd: parse nested shell: %w", &nestedParseError{script: src, err: err})
	}
	*calls = append(*calls, nestedCalls...)
	maps.Copy(vars, nestedVars)
	return nil
}

func words(words []*syntax.Word, src string, vars map[string]string) []string {
	result := make([]string, len(words))
	for i, word := range words {
		if value, ok := wordValue(word); ok {
			result[i] = value
			continue
		}
		start, end := int(word.Pos().Offset()), int(word.End().Offset())
		if start >= 0 && end >= start && end <= len(src) {
			result[i] = src[start:end]
		}
	}
	if len(result) > 0 && strings.HasPrefix(result[0], "$") {
		if value := vars[strings.Trim(result[0], "${}")]; value != "" {
			result[0] = value
		}
	}
	return result
}

func wordText(word *syntax.Word, src string) string {
	start, end := int(word.Pos().Offset()), int(word.End().Offset())
	if start >= 0 && end >= start && end <= len(src) {
		return src[start:end]
	}
	return ""
}

func wordValue(word *syntax.Word) (string, bool) {
	var value strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			value.WriteString(p.Value)
		case *syntax.SglQuoted:
			value.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				value.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return value.String(), true
}

func expand(call Call, lineBase int, vars map[string]string, calls *[]Call) error {
	if target, ok := execTarget(call.Args); ok {
		child := Call{Name: target[0], Args: target[1:], Raw: call.Raw, Line: call.Line, Via: append(append([]string(nil), call.Via...), call.Name, "exec")}
		*calls = append(*calls, child)
		return expand(child, lineBase, vars, calls)
	}
	if isShell(call.Name) {
		for i, arg := range call.Args {
			if !shellCommandOption(arg) {
				continue
			}
			if i+1 >= len(call.Args) || strings.Contains(call.Args[i+1], "$") {
				return nil
			}
			via := append(append([]string(nil), call.Via...), call.Name, "-c")
			return parseNested(call.Args[i+1], call.Line-1, via, vars, calls)
		}
	}
	return nil
}

func shellCommandOption(arg string) bool {
	return strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "c")
}

func execTarget(args []string) ([]string, bool) {
	for i, arg := range args {
		if arg != "--" {
			continue
		}
		exec := slices.Contains(args[:i], "exec")
		if exec && i+1 < len(args) {
			return append([]string{args[i+1]}, args[i+2:]...), true
		}
	}
	return nil, false
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func unwrap(name string, args []string) ([]string, []string, bool) {
	wrappers := map[string]bool{"timeout": true, "env": true, "sudo": true, "nohup": true, "nice": true, "ionice": true, "stdbuf": true, "time": true, "command": true, "builtin": true, "exec": true, "xargs": true}
	var via []string
	for wrappers[name] {
		via = append(via, name)
		i := 0

		if name == "nice" && len(args) > 1 && args[0] == "-n" {
			i = 2
		}
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			if args[i] == "--" {
				i++
				break
			}
			if optionValue(name, args[i]) && i+1 < len(args) {
				i += 2
			} else {
				i++
			}
		}
		if name == "timeout" && i < len(args) && args[i] != "--" {
			i++ // После параметров timeout идёт длительность.
		}
		for name == "env" && i < len(args) && (strings.Contains(args[i], "=") || args[i] == "-i") {
			i++
		}
		if i >= len(args) {
			return nil, via, false
		}
		name, args = baseName(args[i]), args[i+1:]
	}
	if name == "" {
		return nil, via, false
	}
	return append([]string{name}, args...), via, len(via) > 0
}

func optionValue(wrapper, option string) bool {
	switch option {
	case "-I", "-P", "-L", "-d", "-E", "-s", "-a", "-u", "-i", "-t", "-r", "-o", "-T", "-C", "-k", "--kill-after", "--signal":
		return true
	case "-n":
		return wrapper == "xargs" || wrapper == "nice"
	}
	return false
}
