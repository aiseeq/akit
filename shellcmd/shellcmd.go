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
	// Offset — байтовое смещение имени программы (после снятых обёрток) в исходной команде;
	// -1 — его нет в исходном тексте как слова: вызов из вложенного скрипта или argv0 из переменной.
	Offset int
	// Redirs — перенаправления простой команды по порядку (у снятой обёртки — те же, что у неё).
	Redirs []Redir
	// Loop — вызов в теле цикла while, until или for (и во вложенном скрипте такого тела).
	Loop bool
}

// cmd — вызов вместе со словами его командной строки: words[0] — argv0, дальше — Args по порядку.
type cmd struct {
	call  Call
	words []word
}

// word — место слова в исходной команде и его текст как скрипта.
type word struct {
	offset int    // смещение в исходной команде; -1 — слово не литерал
	script string // текст, который выполнит оболочка, получив слово скриптом; "" — не виден до запуска
}

// Redir — перенаправление: `> f`, `2>>log`, `<<EOF` с телом.
type Redir struct {
	Op     string // оператор: >, >>, <, <<, <<-, <<<, &>, >& …
	N      string // номер дескриптора перед оператором ("2" в 2>), пусто — по умолчанию
	Target string // цель: значение литерального слова, иначе его исходный текст; у heredoc — ограничитель
	Body   string // тело heredoc как в исходном тексте
	// BodyStart, BodyEnd — байтовые границы тела heredoc в исходной команде, без строки ограничителя;
	// -1 — heredoc нет, тело пустое или heredoc во вложенном скрипте.
	BodyStart, BodyEnd int
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
	for i := range calls {
		if wanted[calls[i].Name] {
			filtered = append(filtered, calls[i])
		}
	}
	return filtered, nil
}

func parse(src string, lineBase int, inherited []string, vars map[string]string, calls *[]Call) error {
	file, full, err := parseShell(src)
	if err != nil {
		return fmt.Errorf("shellcmd: parse shell: %w", err)
	}
	first := len(*calls)
	if err := walkCalls(file, full, lineBase, inherited, vars, calls); err != nil {
		return err
	}
	// Тело дочитанного heredoc кончается на конце исходного текста, а не дописанного ограничителя.
	for _, c := range (*calls)[first:] {
		for i := range c.Redirs {
			if r := &c.Redirs[i]; r.BodyEnd > len(src) {
				r.BodyEnd, r.Body = len(src), src[r.BodyStart:]
			}
		}
	}
	return nil
}

// parseShell разбирает текст как bash. Незакрытый heredoc bash дочитывает до конца текста с предупреждением,
// а разбор считает ошибкой — тогда ограничитель дописывается в конец; full — разобранный текст.
func parseShell(src string) (file *syntax.File, full string, err error) {
	full = src
	for range 8 {
		file, err = syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(full), "")
		var pe syntax.ParseError
		if !errors.As(err, &pe) {
			return file, full, err
		}
		delim, ok := strings.CutPrefix(pe.Text, "unclosed here-document `")
		if !ok {
			return nil, full, err
		}
		full += "\n" + strings.TrimSuffix(delim, "`") + "\n"
	}
	return nil, full, err
}

func walkCalls(file *syntax.File, src string, lineBase int, inherited []string, vars map[string]string, calls *[]Call) error {
	timedCalls := timedCallSet(file)
	redirs := make(map[*syntax.CallExpr][]Redir)
	loopCalls := loopCallSet(file)
	var shellInputs []*syntax.Stmt
	var parseErr error
	var visit func(node syntax.Node) bool
	visit = func(node syntax.Node) bool {
		if parseErr != nil { //nolint:silent-error-handling // Обход прекращается после сохранения ошибки для возврата наружу.
			return false
		}
		if r, ok := node.(*syntax.Redirect); ok {
			// Цель перенаправления выполняется (`< /proc/$(pgrep x)/cmdline`), тело heredoc — данные,
			// пока оно не передано оболочке.
			if r.Word != nil {
				syntax.Walk(r.Word, visit)
			}
			return false
		}
		if clause, ok := node.(*syntax.TimeClause); ok {
			start, end := int(clause.Pos().Offset()), int(clause.End().Offset())
			if !validSpan(src, start, end) {
				parseErr = fmt.Errorf("shellcmd: invalid source span for time clause")
				return false
			}
			*calls = append(*calls, Call{Name: "time", Raw: src[start:end], Line: int(clause.Pos().Line()) + lineBase, Via: append([]string(nil), inherited...), Offset: start})
		}
		if stmt, ok := node.(*syntax.Stmt); ok {
			if call, ok := stmt.Cmd.(*syntax.CallExpr); ok && len(stmt.Redirs) > 0 {
				redirs[call] = stmtRedirs(stmt.Redirs, src)
			}
			if shellInput(stmt, src, vars) {
				shellInputs = append(shellInputs, stmt)
			}
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
		start, end := int(call.Args[0].Pos().Offset()), int(call.Args[len(call.Args)-1].End().Offset())
		if !validSpan(src, start, end) {
			parseErr = fmt.Errorf("shellcmd: invalid source span for command")
			return false
		}
		via := append([]string(nil), inherited...)
		if timedCalls[call] {
			via = append(via, "time")
		}
		ws := cmdWords(call.Args, src)
		c := cmd{call: Call{Name: commandName(args[0]), Args: args[1:], Raw: src[start:end], Line: int(call.Pos().Line()) + lineBase, Via: via, Offset: ws[0].offset, Redirs: redirs[call], Loop: loopCalls[call]}, words: ws}
		if err := emit(c, lineBase, vars, calls); err != nil {
			parseErr = err
			return false
		}
		return true
	}
	syntax.Walk(file, visit)
	if parseErr != nil {
		return fmt.Errorf("shellcmd: walk syntax tree: %w", parseErr)
	}
	return parseHeredocStmts(shellInputs, src, lineBase, inherited, vars, loopCalls, calls)
}

func loopCallSet(file *syntax.File) map[*syntax.CallExpr]bool {
	loopCalls := make(map[*syntax.CallExpr]bool)
	mark := func(stmts []*syntax.Stmt) {
		for _, stmt := range stmts {
			syntax.Walk(stmt, func(node syntax.Node) bool {
				if call, ok := node.(*syntax.CallExpr); ok {
					loopCalls[call] = true
				}
				return true
			})
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch clause := node.(type) {
		case *syntax.WhileClause:
			mark(clause.Do)
		case *syntax.ForClause:
			mark(clause.Do)
		}
		return true
	})
	return loopCalls
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
			value = nodeText(assign.Value, src)
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

func parseHeredocStmts(stmts []*syntax.Stmt, src string, lineBase int, inherited []string, vars map[string]string, loopCalls map[*syntax.CallExpr]bool, calls *[]Call) error {
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
			call, _ := stmt.Cmd.(*syntax.CallExpr)
			if err := parseNested(body, int(redir.Hdoc.Pos().Line())+lineBase-1, via, loopCalls[call], vars, calls); err != nil {
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

// IsShell сообщает, входит ли команда в список поддерживаемых оболочек.
func IsShell(name string) bool { return isShell(name) }

type nestedParseError struct {
	script string
	err    error
}

func (e *nestedParseError) Error() string { return e.err.Error() }
func (e *nestedParseError) Unwrap() error { return e.err }

func parseNested(src string, lineBase int, via []string, inLoop bool, vars map[string]string, calls *[]Call) error {
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
	// Смещения во вложенном тексте не указывают в исходную команду.
	for i := range nestedCalls {
		nestedCalls[i].Offset = -1
		nestedCalls[i].Loop = nestedCalls[i].Loop || inLoop
		redirs := slices.Clone(nestedCalls[i].Redirs)
		for j := range redirs {
			redirs[j].BodyStart, redirs[j].BodyEnd = -1, -1
		}
		nestedCalls[i].Redirs = redirs
	}
	*calls = append(*calls, nestedCalls...)
	maps.Copy(vars, nestedVars)
	return nil
}

// stmtRedirs — перенаправления команды с телами heredoc и их границами в src.
func stmtRedirs(rs []*syntax.Redirect, src string) []Redir {
	out := make([]Redir, 0, len(rs))
	for _, r := range rs {
		red := Redir{Op: r.Op.String(), BodyStart: -1, BodyEnd: -1}
		if r.N != nil {
			red.N = r.N.Value
		}
		if r.Word != nil {
			if value, literal := wordValue(r.Word); literal {
				red.Target = value
			} else {
				red.Target = nodeText(r.Word, src)
			}
		}
		if r.Hdoc != nil {
			start, end := int(r.Hdoc.Pos().Offset()), int(r.Hdoc.End().Offset())
			if validSpan(src, start, end) {
				end = start + heredocBodyLen(src[start:end], red.Target)
				red.Body, red.BodyStart, red.BodyEnd = src[start:end], start, end
			}
		}
		out = append(out, red)
	}
	return out
}

// heredocBodyLen — длина тела heredoc: разбор отдаёт его вместе со строкой ограничителя.
func heredocBodyLen(text, delim string) int {
	i := strings.LastIndexByte(text, '\n')
	if strings.TrimLeft(text[i+1:], "\t") != delim {
		return len(text)
	}
	return i + 1
}

// cmdWords — место и текст скрипта каждого слова команды.
func cmdWords(args []*syntax.Word, src string) []word {
	ws := make([]word, len(args))
	for i, arg := range args {
		ws[i] = word{offset: -1, script: wordScript(arg, src)}
		if _, literal := wordValue(arg); literal {
			ws[i].offset = int(arg.Pos().Offset())
		}
	}
	return ws
}

// wordScript — текст слова, который оболочка разберёт как скрипт (`bash -c СЛОВО`, удалённая команда ssh).
// Литерал — его значение. Слово в двойных кавычках — текст внутри без экранирования: подстановки в нём
// ($x, $(…)) остаются текстом, а строй скрипта (циклы, вызовы) виден. Слово только из подстановки
// ("$cmd", $cmd) — "": что выполнится, видно только при запуске.
func wordScript(w *syntax.Word, src string) string {
	if value, literal := wordValue(w); literal {
		return value
	}
	if len(w.Parts) != 1 {
		return ""
	}
	quoted, ok := w.Parts[0].(*syntax.DblQuoted)
	if !ok || !slices.ContainsFunc(quoted.Parts, func(p syntax.WordPart) bool { _, lit := p.(*syntax.Lit); return lit }) {
		return ""
	}
	start, end := int(quoted.Pos().Offset()), int(quoted.End().Offset())
	if !validSpan(src, start, end) || end-start < 2 {
		return ""
	}
	return unescapeDouble.Replace(src[start+1 : end-1])
}

// unescapeDouble снимает экранирование в двойных кавычках: обратная косая значима только перед " \ $ ` и переводом строки.
var unescapeDouble = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`", "\\\n", "")

// unescapeBare снимает экранирование вне кавычек: `\x` — x, `\`+перевод строки — продолжение строки.
func unescapeBare(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// words — значения слов команды; у слов с подстановками — их текст. argv0 из переменной, присвоенной
// раньше литералом, — её значение; другой argv0 с подстановками — текст без кавычек ("$HOME/bin/x" → $HOME/bin/x).
func words(ws []*syntax.Word, src string, vars map[string]string) []string {
	result := make([]string, len(ws))
	for i, w := range ws {
		if value, ok := wordValue(w); ok {
			result[i] = value
			continue
		}
		result[i] = nodeText(w, src)
	}
	if len(ws) > 0 {
		if name, ok := paramName(ws[0]); ok && vars[name] != "" {
			result[0] = vars[name]
		} else if _, literal := wordValue(ws[0]); !literal {
			result[0] = unquotedText(ws[0], src)
		}
	}
	return result
}

// unquotedText — текст слова без кавычек; подстановки остаются текстом.
func unquotedText(w *syntax.Word, src string) string {
	var text strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			text.WriteString(unescapeBare(p.Value))
		case *syntax.SglQuoted:
			text.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				if lit, ok := inner.(*syntax.Lit); ok {
					text.WriteString(unescapeDouble.Replace(lit.Value))
				} else {
					text.WriteString(nodeText(inner, src))
				}
			}
		default:
			text.WriteString(nodeText(part, src))
		}
	}
	return text.String()
}

func nodeText(n syntax.Node, src string) string {
	start, end := int(n.Pos().Offset()), int(n.End().Offset())
	if validSpan(src, start, end) {
		return src[start:end]
	}
	return ""
}

// paramName — имя переменной у слова из одной простой подстановки: $R, ${R}, "$R", "${R}".
func paramName(w *syntax.Word) (string, bool) {
	if len(w.Parts) != 1 {
		return "", false
	}
	part := w.Parts[0]
	if quoted, ok := part.(*syntax.DblQuoted); ok {
		if len(quoted.Parts) != 1 {
			return "", false
		}
		part = quoted.Parts[0]
	}
	p, ok := part.(*syntax.ParamExp)
	if !ok || !simpleParamExp(p) {
		return "", false
	}
	return p.Param.Value, true
}

func simpleParamExp(p *syntax.ParamExp) bool {
	return p.Param != nil && p.Flags == nil && !p.Excl && !p.Length && !p.Width && !p.IsSet &&
		p.Split == syntax.OptUnset && p.GlobSubst == syntax.OptUnset && p.RcExpand == syntax.OptUnset &&
		p.NestedParam == nil && p.Index == nil && len(p.Modifiers) == 0 && p.Slice == nil && p.Repl == nil && p.Names == 0 && p.Exp == nil
}

// wordValue — значение слова без кавычек, если в нём нет подстановок.
func wordValue(w *syntax.Word) (string, bool) {
	var value strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			value.WriteString(unescapeBare(p.Value))
		case *syntax.SglQuoted:
			value.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				value.WriteString(unescapeDouble.Replace(lit.Value))
			}
		default:
			return "", false
		}
	}
	return value.String(), true
}

// sub — вызов программы из слова i командной строки c (i ≥ 1): под обёрткой, после `--`, после ssh.
func (c cmd) sub(i int, via []string) cmd {
	return cmd{call: Call{Name: commandName(c.call.Args[i-1]), Args: c.call.Args[i:], Raw: c.call.Raw, Line: c.call.Line,
		Via: via, Offset: c.words[i].offset, Redirs: c.call.Redirs, Loop: c.call.Loop}, words: c.words[i:]}
}

func (c cmd) via(more ...string) []string {
	return append(slices.Clone(c.call.Via), more...)
}

// emit добавляет вызов и всё, что он запускает: программу под обёрткой, цель `X exec … --`, скрипт `bash -c`.
func emit(c cmd, lineBase int, vars map[string]string, calls *[]Call) error {
	*calls = append(*calls, c.call)
	if err := expand(c, lineBase, vars, calls); err != nil {
		return err
	}
	if nested, wrapperVia, ok := unwrap(c.call.Name, c.call.Args); ok {
		return emit(c.sub(len(c.words)-len(nested), c.via(wrapperVia...)), lineBase, vars, calls)
	}
	return nil
}

// expand раскрывает то, что вызов запускает, кроме программы под обёрткой (её раскрывает emit).
func expand(c cmd, lineBase int, vars map[string]string, calls *[]Call) error {
	name, args := c.call.Name, c.call.Args
	if target, ok := execTarget(args); ok {
		return emit(c.sub(len(c.words)-len(target), c.via(name, "exec")), lineBase, vars, calls)
	}
	switch {
	case isShell(name):
		for i, arg := range args {
			if !shellCommandOption(arg) {
				continue
			}
			if i+1 >= len(args) || c.words[i+2].script == "" {
				return nil // скрипта нет или он виден только при запуске
			}
			return parseNested(c.words[i+2].script, c.call.Line-1, c.via(name, "-c"), c.call.Loop, vars, calls)
		}
	case name == "ssh":
		start, ok := sshCommand(args)
		if !ok {
			return nil
		}
		parts := make([]string, 0, len(args)-start)
		for _, w := range c.words[start+1:] {
			if w.script == "" {
				return nil
			}
			parts = append(parts, w.script)
		}
		return parseNested(strings.Join(parts, " "), c.call.Line-1, c.via("ssh"), c.call.Loop, vars, calls)
	case !wrappers[name] && !dataArgs[name]:
		// Оболочка с -c среди аргументов незнакомой программы (`X exec dir bash -c '…'`): та её запускает.
		for i, arg := range args {
			if isShell(baseName(arg)) && i+1 < len(args) && shellCommandOption(args[i+1]) {
				return emit(c.sub(i+1, c.via(name)), lineBase, vars, calls)
			}
		}
	}
	return nil
}

// dataArgs — программы, чьи аргументы — данные: `echo bash -c x` ничего не запускает.
var dataArgs = set("echo", "printf")

var sshValueOptions = set("-b", "-c", "-D", "-E", "-e", "-F", "-I", "-i", "-J", "-L", "-l", "-m", "-O", "-o", "-p", "-Q", "-R", "-S", "-W", "-w")

// sshCommand — индекс в args первого слова удалённой команды ssh; false — команды нет.
func sshCommand(args []string) (int, bool) {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if args[i] == "--" {
			i++
			break
		}
		if sshValueOptions[args[i]] {
			i += 2
			continue
		}
		i++
	}
	i++ // хост
	return i, i < len(args)
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
	var via []string
	for wrappers[name] {
		via = append(via, name)
		i := 0
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

// wrappers — программы, которые запускают программу из своих аргументов.
var wrappers = set("timeout", "env", "sudo", "nohup", "nice", "ionice", "stdbuf", "time", "command", "builtin", "exec", "xargs")

// valueOptions — опции обёрток, следующее слово после которых — их значение, а не программа.
var valueOptions = map[string]map[string]bool{
	"timeout": set("-s", "--signal", "-k", "--kill-after"),
	"env":     set("-u", "--unset", "-C", "--chdir"),
	"sudo":    set("-u", "--user", "-g", "--group", "-C", "--close-from", "-D", "--chdir", "-h", "--host", "-p", "--prompt", "-r", "--role", "-t", "--type", "-T", "--command-timeout", "-U", "--other-user"),
	"nice":    set("-n", "--adjustment"),
	"ionice":  set("-c", "--class", "-n", "--classdata", "-p", "--pid", "-P", "--pgid", "-u", "--uid"),
	"stdbuf":  set("-i", "--input", "-o", "--output", "-e", "--error"),
	"time":    set("-f", "--format", "-o", "--output"),
	"exec":    set("-a"),
	"xargs":   set("-I", "-n", "--max-args", "-P", "--max-procs", "-L", "--max-lines", "-d", "--delimiter", "-E", "-s", "--max-chars", "-a", "--arg-file"),
}

func set(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

func optionValue(wrapper, option string) bool {
	return valueOptions[wrapper][option]
}
