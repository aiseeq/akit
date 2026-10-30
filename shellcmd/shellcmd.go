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
	// Name — имя программы без каталога; "" — в слове имени есть `$(` или обратная кавычка.
	Name string
	Args []string
	Raw  string
	Line int
	Via  []string
	// Offset — байтовое смещение имени программы (после снятых обёрток) в исходной команде, в том числе
	// внутри вложенного скрипта (`bash -c '…'`, `ssh host …`, heredoc на вход оболочке); -1 — его нет
	// в исходном тексте как слова: argv0 из переменной или с подстановкой, байт скрипта из $'…'.
	Offset int
	// Redirs — перенаправления простой команды по порядку (у снятой обёртки — те же, что у неё).
	Redirs []Redir
	// Loop — вызов в теле цикла while, until или for (и во вложенном скрипте такого тела).
	Loop bool
	// Background — вызов в задании, чей оператор или объемлющий оператор заканчивается `&`.
	Background bool
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
	pos    []int  // pos[i] — смещение байта i текста script в исходной команде; -1 — такого байта нет
}

// Redir — перенаправление: `> f`, `2>>log`, `<<EOF` с телом.
type Redir struct {
	Op     string // оператор: >, >>, <, <<, <<-, <<<, &>, >& …
	N      string // номер дескриптора перед оператором ("2" в 2>), пусто — по умолчанию
	Target string // цель: значение литерального слова, иначе его исходный текст; у heredoc — ограничитель
	Body   string // тело heredoc как в исходном тексте
	// BodyStart, BodyEnd — байтовые границы тела heredoc в исходной команде, без строки ограничителя;
	// -1 — heredoc нет, тело пустое или его края не в исходном тексте ($'…'). Во вложенном скрипте
	// (`bash -c "…"`) диапазон охватывает тело, но текст в нём может отличаться от Body кавычками и
	// экранированием; вне вложенного скрипта он равен Body.
	BodyStart, BodyEnd int
}

// Calls возвращает найденные вызовы и ошибки разбора вложенных скриптов. При ошибке внешнего
// разбора или обхода возвращает nil. Find сохраняет то же правило при фильтрации.
func Calls(cmd string) ([]Call, error) {
	var calls []Call
	if err := parse(cmd, 0, nil, make(map[string]string), &calls); err != nil {
		var nested *NestedError
		if !errors.As(err, &nested) {
			return nil, err
		}
		return calls, err
	}
	return calls, nil
}

// Find возвращает только вызовы с указанными именами.
func Find(cmd string, names ...string) ([]Call, error) {
	calls, err := Calls(cmd)
	var nested *NestedError
	if err != nil && !errors.As(err, &nested) {
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
	return filtered, err
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
	info := collectWalkInfo(file, src, vars)
	walker := callWalker{src: src, lineBase: lineBase, inherited: inherited, vars: vars, calls: calls, info: info}
	syntax.Walk(file, walker.visit)
	if walker.parseErr != nil {
		return fmt.Errorf("shellcmd: walk syntax tree: %w", walker.parseErr)
	}
	if err := parseHeredocStmts(info.shellInputs, src, lineBase, inherited, vars, info.loopCalls, info.backgroundCalls, calls); err != nil {
		walker.nestedErrs = append(walker.nestedErrs, err)
	}
	return errors.Join(walker.nestedErrs...)
}

type walkInfo struct {
	timedCalls      map[*syntax.CallExpr]bool
	redirs          map[*syntax.CallExpr][]Redir
	loopCalls       map[*syntax.CallExpr]bool
	backgroundCalls map[*syntax.CallExpr]bool
	shellInputs     []*syntax.Stmt
	shellHdocs      map[*syntax.Redirect]bool
}

func collectWalkInfo(file *syntax.File, src string, vars map[string]string) walkInfo {
	info := walkInfo{
		timedCalls:      timedCallSet(file),
		redirs:          make(map[*syntax.CallExpr][]Redir),
		loopCalls:       loopCallSet(file),
		backgroundCalls: backgroundCallSet(file),
		shellHdocs:      make(map[*syntax.Redirect]bool),
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		stmt, ok := node.(*syntax.Stmt)
		if !ok || !shellInput(stmt, src, vars) {
			return true
		}
		info.shellInputs = append(info.shellInputs, stmt)
		for _, redirect := range stmt.Redirs {
			if redirect.Hdoc != nil {
				info.shellHdocs[redirect] = true
			}
		}
		return true
	})
	return info
}

type callWalker struct {
	src        string
	lineBase   int
	inherited  []string
	vars       map[string]string
	calls      *[]Call
	info       walkInfo
	parseErr   error
	nestedErrs []error
}

func (w *callWalker) visit(node syntax.Node) bool {
	if w.parseErr != nil {
		return false
	}
	if redirect, ok := node.(*syntax.Redirect); ok {
		w.visitRedirect(redirect)
		return false
	}
	if clause, ok := node.(*syntax.TimeClause); ok {
		w.addTime(clause)
		if w.parseErr != nil {
			return false
		}
	}
	if stmt, ok := node.(*syntax.Stmt); ok {
		w.visitStmt(stmt)
	}
	if call, ok := node.(*syntax.CallExpr); ok {
		w.visitCall(call)
		return w.parseErr == nil
	}
	return true
}

func (w *callWalker) visitRedirect(redirect *syntax.Redirect) {
	// Цель перенаправления выполняется, тело heredoc — данные, пока оно не передано оболочке.
	if redirect.Word != nil {
		syntax.Walk(redirect.Word, w.visit)
	}
	if redirect.Hdoc != nil && !w.info.shellHdocs[redirect] && !quotedHeredoc(redirect.Word) {
		syntax.Walk(redirect.Hdoc, w.visit)
	}
}

func (w *callWalker) addTime(clause *syntax.TimeClause) {
	start, end := int(clause.Pos().Offset()), int(clause.End().Offset())
	if !validSpan(w.src, start, end) {
		w.parseErr = fmt.Errorf("shellcmd: invalid source span for time clause")
		return
	}
	*w.calls = append(*w.calls, Call{Name: "time", Raw: w.src[start:end], Line: int(clause.Pos().Line()) + w.lineBase, Via: append([]string(nil), w.inherited...), Offset: start})
}

func (w *callWalker) visitStmt(stmt *syntax.Stmt) {
	if call, ok := stmt.Cmd.(*syntax.CallExpr); ok && len(stmt.Redirs) > 0 {
		w.info.redirs[call] = stmtRedirs(stmt.Redirs, w.src)
	}
}

func (w *callWalker) visitCall(call *syntax.CallExpr) {
	if len(call.Args) == 0 {
		collectAssignments(call, w.src, w.vars)
		return
	}
	args, split := words(call.Args, w.src, w.vars)
	start, end := int(call.Args[0].Pos().Offset()), int(call.Args[len(call.Args)-1].End().Offset())
	if !validSpan(w.src, start, end) {
		w.parseErr = fmt.Errorf("shellcmd: invalid source span for command")
		return
	}
	via := append([]string(nil), w.inherited...)
	if w.info.timedCalls[call] {
		via = append(via, "time")
	}
	ws := splitWords(cmdWords(call.Args, w.src), split)
	c := cmd{call: Call{Name: commandName(args[0]), Args: args[1:], Raw: w.src[start:end], Line: int(call.Pos().Line()) + w.lineBase, Via: via, Offset: ws[0].offset, Redirs: w.info.redirs[call], Loop: w.info.loopCalls[call], Background: w.info.backgroundCalls[call]}, words: ws}
	if err := emit(c, w.lineBase, w.vars, w.calls); err != nil {
		var nested *NestedError
		if errors.As(err, &nested) {
			w.nestedErrs = append(w.nestedErrs, err)
			return
		}
		w.parseErr = err
		return
	}
}

func backgroundCallSet(file *syntax.File) map[*syntax.CallExpr]bool {
	backgroundCalls := make(map[*syntax.CallExpr]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		stmt, ok := node.(*syntax.Stmt)
		if !ok || !stmt.Background {
			return true
		}
		syntax.Walk(stmt, func(child syntax.Node) bool {
			if call, ok := child.(*syntax.CallExpr); ok {
				backgroundCalls[call] = true
			}
			return true
		})
		return true
	})
	return backgroundCalls
}

func quotedHeredoc(word *syntax.Word) bool {
	quoted := false
	if word != nil {
		syntax.Walk(word, func(node syntax.Node) bool {
			switch node.(type) {
			case *syntax.SglQuoted, *syntax.DblQuoted:
				quoted = true
			}
			return !quoted
		})
	}
	return quoted
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
			value = unquotedText(assign.Value, src)
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

func parseHeredocStmts(stmts []*syntax.Stmt, src string, lineBase int, inherited []string, vars map[string]string, loopCalls, backgroundCalls map[*syntax.CallExpr]bool, calls *[]Call) error {
	var nestedErrs []error
	for _, stmt := range stmts {
		name, shellVia, ok := shellInputName(stmt, src, vars)
		if !ok {
			continue
		}
		for _, redir := range stmt.Redirs {
			if redir.Hdoc == nil {
				continue
			}
			var body mapped
			body.add(heredocText(redir, src), int(redir.Hdoc.Pos().Offset()))
			via := append(append(append([]string(nil), inherited...), shellVia...), name)
			call, _ := stmt.Cmd.(*syntax.CallExpr)
			if err := parseNested(body.String(), body.pos, int(redir.Hdoc.Pos().Line())+lineBase-1, via, loopCalls[call], backgroundCalls[call], vars, calls); err != nil {
				nestedErrs = append(nestedErrs, err)
			}
		}
	}
	return errors.Join(nestedErrs...)
}

func heredocText(redir *syntax.Redirect, src string) string {
	start, end := int(redir.Hdoc.Pos().Offset()), int(redir.Hdoc.End().Offset())
	if !validSpan(src, start, end) {
		return ""
	}
	delim, _ := wordValue(redir.Word)
	end = start + heredocBodyLen(src[start:end], delim)
	return src[start:end]
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
	args, _ := words(call.Args, src, vars)
	name, args := commandName(args[0]), args[1:]
	var via []string
	for {
		if isShell(name) {
			for _, redir := range stmt.Redirs {
				if redir.Hdoc != nil && shellReadsHeredoc(args) {
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

// shellReadsHeredoc — оболочка читает heredoc как скрипт, если её stdin не заменён -c или файлом.
func shellReadsHeredoc(args []string) bool {
	operand, command, stdin := shellArgs(args)
	return !command && (stdin || operand == len(args))
}

// shellArgs разбирает `bash [опции] [-c СКРИПТ | -s | ФАЙЛ] [аргументы]`: operand — индекс первого
// аргумента после опций (len(args) — его нет), command — есть -c, stdin — есть -s.
func shellArgs(args []string) (operand int, command, stdin bool) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" || args[i] == "-" {
			return i + 1, command, stdin
		}
		isOption, takesValue, readsStdin, readsCommand := shellArgumentOption(args[i])
		if !isOption {
			return i, command, stdin
		}
		stdin, command = stdin || readsStdin, command || readsCommand
		if takesValue {
			i++
		}
	}
	return len(args), command, stdin
}

func shellArgumentOption(arg string) (option, takesValue, stdin, command bool) {
	if arg == "--rcfile" || arg == "--init-file" {
		return true, true, false, false
	}
	if strings.HasPrefix(arg, "--") {
		return true, false, false, false
	}
	if len(arg) < 2 || (arg[0] != '-' && arg[0] != '+') {
		return false, false, false, false
	}
	for j := 1; j < len(arg); j++ {
		switch arg[j] {
		case 'c':
			command = true
		case 's':
			stdin = true
		case 'o', 'O':
			takesValue = j == len(arg)-1
			return true, takesValue, stdin, command
		}
	}
	return true, false, stdin, command
}

// IsShell сообщает, входит ли команда в список поддерживаемых оболочек.
func IsShell(name string) bool { return isShell(name) }

// NestedError описывает синтаксическую ошибку скрипта, переданного вложенной оболочке.
type NestedError struct {
	Script string
	Err    error
}

func (e *NestedError) Error() string { return e.Err.Error() }
func (e *NestedError) Unwrap() error { return e.Err }

// parseNested добавляет вызовы скрипта src, который выполнит вложенная оболочка; pos[i] — смещение байта i
// скрипта во внешнем тексте (-1 — такого байта нет).
func parseNested(src string, pos []int, lineBase int, via []string, inLoop, inBackground bool, vars map[string]string, calls *[]Call) error {
	nestedCalls := make([]Call, 0)
	nestedVars := make(map[string]string, len(vars))
	maps.Copy(nestedVars, vars)
	err := parse(src, lineBase, via, nestedVars, &nestedCalls)
	if err != nil {
		var nestedErr *NestedError
		if !errors.As(err, &nestedErr) {
			err = &NestedError{Script: src, Err: err}
		}
	}
	for i := range nestedCalls {
		nestedCalls[i].Offset = outerOffset(nestedCalls[i].Offset, pos)
		nestedCalls[i].Loop = nestedCalls[i].Loop || inLoop
		nestedCalls[i].Background = nestedCalls[i].Background || inBackground
		redirs := slices.Clone(nestedCalls[i].Redirs)
		for j := range redirs {
			redirs[j].BodyStart, redirs[j].BodyEnd = outerSpan(redirs[j].BodyStart, redirs[j].BodyEnd, pos)
		}
		nestedCalls[i].Redirs = redirs
	}
	*calls = append(*calls, nestedCalls...)
	maps.Copy(vars, nestedVars)
	return err
}

// outerOffset переводит смещение во вложенном скрипте во внешний текст по карте pos.
func outerOffset(offset int, pos []int) int {
	if offset < 0 || offset >= len(pos) {
		return -1
	}
	return pos[offset]
}

// outerSpan переводит непустой диапазон [start, end) вложенного скрипта во внешний текст по карте pos:
// от места первого байта до места последнего включительно; нет места у края — -1, -1.
func outerSpan(start, end int, pos []int) (int, int) {
	if start < 0 || end <= start {
		return -1, -1
	}
	first, last := outerOffset(start, pos), outerOffset(end-1, pos)
	if first < 0 || last < first {
		return -1, -1
	}
	return first, last + 1
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
		script, pos := wordScript(arg, src)
		ws[i] = word{offset: -1, script: script, pos: pos}
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
// Возвращает и карту байтов текста в исходную команду.
func wordScript(w *syntax.Word, src string) (string, []int) {
	var text mapped
	if _, literal := wordValue(w); literal {
		for _, part := range w.Parts {
			text.addLiteral(part, src)
		}
		return text.String(), text.pos
	}
	if len(w.Parts) != 1 {
		return "", nil
	}
	quoted, ok := w.Parts[0].(*syntax.DblQuoted)
	if !ok || !slices.ContainsFunc(quoted.Parts, func(p syntax.WordPart) bool { _, lit := p.(*syntax.Lit); return lit }) {
		return "", nil
	}
	start, end := int(quoted.Pos().Offset()), int(quoted.End().Offset())
	if !validSpan(src, start, end) || end-start < 2 {
		return "", nil
	}
	text.unescape(src[start+1:end-1], start+1, true)
	return text.String(), text.pos
}

// sourceBase — смещение value в src, если value — точный текст узла n без skip байтов по краям; иначе -1.
func sourceBase(n syntax.Node, value string, skip int, src string) int {
	start, end := int(n.Pos().Offset())+skip, int(n.End().Offset())-skip
	if validSpan(src, start, end) && src[start:end] == value {
		return start
	}
	return -1
}

// litSource — исходный текст литерала и его смещение: в Value разбор уже убрал продолжения строки
// (`\`+перевод строки), а карте нужны байты исходника; без верного места — Value и -1. Литерал перед
// продолжением строки кончается на `\` без перевода строки — срез берёт и его.
func litSource(lit *syntax.Lit, src string, double bool) (raw string, base int) {
	start, end := int(lit.Pos().Offset()), int(lit.End().Offset())
	if !validSpan(src, start, end) {
		return lit.Value, -1
	}
	if strings.HasSuffix(src[start:end], `\`) && end < len(src) && src[end] == '\n' {
		end++
	}
	var fromSource, fromValue mapped
	fromSource.unescape(src[start:end], start, double)
	fromValue.unescape(lit.Value, -1, double)
	if fromSource.String() != fromValue.String() {
		return lit.Value, -1
	}
	return src[start:end], start
}

// mapped — текст и смещение каждого его байта в исходной команде (-1 — такого байта нет).
type mapped struct {
	text []byte
	pos  []int
}

func (m *mapped) String() string { return string(m.text) }

// addLiteral добавляет значение части слова без подстановок (её пропускает wordValue).
func (m *mapped) addLiteral(part syntax.WordPart, src string) {
	switch p := part.(type) {
	case *syntax.Lit:
		raw, base := litSource(p, src, false)
		m.unescape(raw, base, false)
	case *syntax.SglQuoted:
		base := -1
		if !p.Dollar {
			base = sourceBase(p, p.Value, 1, src)
		}
		m.add(p.Value, base)
	case *syntax.DblQuoted:
		for _, inner := range p.Parts {
			if lit, ok := inner.(*syntax.Lit); ok {
				raw, base := litSource(lit, src, true)
				m.unescape(raw, base, true)
			}
		}
	}
}

// add добавляет raw как есть; base — смещение raw в исходной команде, -1 — его нет.
func (m *mapped) add(raw string, base int) {
	for i := 0; i < len(raw); i++ {
		m.push(raw[i], base, i)
	}
}

func (m *mapped) push(b byte, base, i int) {
	m.text = append(m.text, b)
	if base < 0 {
		m.pos = append(m.pos, -1)
	} else {
		m.pos = append(m.pos, base+i)
	}
}

// unescape добавляет raw без экранирования; base — смещение raw в исходной команде, -1 — его нет.
// В двойных кавычках обратная косая значима только перед " \ $ ` и переводом строки, вне кавычек — перед
// любым байтом; `\`+перевод строки — продолжение строки.
func (m *mapped) unescape(raw string, base int, double bool) {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) && (!double || strings.IndexByte("\"\\$`\n", raw[i+1]) >= 0) {
			i++
			if raw[i] == '\n' {
				continue
			}
		}
		m.push(raw[i], base, i)
	}
}

// unescapeDouble снимает экранирование в двойных кавычках.
func unescapeDouble(s string) string {
	var m mapped
	m.unescape(s, -1, true)
	return m.String()
}

// unescapeBare снимает экранирование вне кавычек: `\x` — x, `\`+перевод строки — продолжение строки.
func unescapeBare(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var m mapped
	m.unescape(s, -1, false)
	return m.String()
}

// words — значения слов команды; у слов с подстановками — их текст. argv0 из переменной, присвоенной
// раньше литералом, — её значение, без кавычек ($P) делённое на слова по пробелам, табам и переводам
// строк, как делит оболочка; другой argv0 с подстановками — текст без кавычек ("$HOME/bin/x" → $HOME/bin/x).
// Второй результат — сколько слов добавило деление argv0.
func words(ws []*syntax.Word, src string, vars map[string]string) ([]string, int) {
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
			if _, quoted := ws[0].Parts[0].(*syntax.DblQuoted); !quoted {
				if fields := strings.FieldsFunc(vars[name], isIFS); len(fields) > 1 {
					return append(fields, result[1:]...), len(fields) - 1
				}
			}
		} else if _, literal := wordValue(ws[0]); !literal {
			result[0] = unquotedText(ws[0], src)
		}
	}
	return result, 0
}

func isIFS(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n'
}

// splitWords вставляет после argv0 слова, добавленные делением его значения: их нет в исходном тексте.
func splitWords(ws []word, split int) []word {
	if split == 0 {
		return ws
	}
	result := append([]word{ws[0]}, make([]word, split)...)
	for i := 1; i <= split; i++ {
		result[i] = word{offset: -1}
	}
	return append(result, ws[1:]...)
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
					text.WriteString(unescapeDouble(lit.Value))
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
				value.WriteString(unescapeDouble(lit.Value))
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
		Via: via, Offset: c.words[i].offset, Redirs: c.call.Redirs, Loop: c.call.Loop, Background: c.call.Background}, words: c.words[i:]}
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
		operand, command, _ := shellArgs(args)
		if !command || operand >= len(args) || c.words[operand+1].script == "" {
			return nil // нет -c, скрипта нет или он виден только при запуске
		}
		script := c.words[operand+1]
		return parseNested(script.script, script.pos, c.call.Line-1, c.via(name, "-c"), c.call.Loop, c.call.Background, vars, calls)
	case name == "ssh":
		start, ok := sshCommand(args)
		if !ok {
			return nil
		}
		var remote mapped
		for i, w := range c.words[start+1:] {
			if w.script == "" {
				return nil
			}
			if i > 0 {
				remote.add(" ", -1) // ssh соединяет слова команды пробелом
			}
			remote.text = append(remote.text, w.script...)
			remote.pos = append(remote.pos, w.pos...)
		}
		return parseNested(remote.String(), remote.pos, c.call.Line-1, c.via("ssh"), c.call.Loop, c.call.Background, vars, calls)
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

// sshCommand — индекс в args первого слова удалённой команды ssh; false — команды нет. OpenSSH разбирает
// опции и после хоста, если до хоста не было `--`.
func sshCommand(args []string) (int, bool) {
	i, ended := sshOptions(args, 0)
	i++ // хост
	if !ended {
		i, _ = sshOptions(args, i)
	}
	return i, i < len(args)
}

// sshOptions пропускает опции ssh с позиции i; ended — их закончил `--`.
func sshOptions(args []string, i int) (int, bool) {
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if args[i] == "--" {
			return i + 1, true
		}
		if optionValue("ssh", args[i]) {
			i++
		}
		i++
	}
	return i, false
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
		if name == "command" && commandIsLookup(args) {
			return nil, via, false
		}
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

// commandIsLookup — `command -v X`/`-V X`: ключи до имени программы ищут её, а не запускают.
func commandIsLookup(args []string) bool {
	for _, arg := range args {
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			return false
		}
		if strings.ContainsAny(arg, "vV") {
			return true
		}
	}
	return false
}

// wrappers — программы, которые запускают программу из своих аргументов.
var wrappers = set("timeout", "env", "sudo", "nohup", "nice", "ionice", "stdbuf", "time", "command", "builtin", "exec", "xargs")

// valueOptions — опции обёрток и ssh, следующее слово после которых — их значение, а не программа.
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
	"ssh":     set("-B", "-b", "-c", "-D", "-E", "-e", "-F", "-I", "-i", "-J", "-L", "-l", "-m", "-O", "-o", "-P", "-p", "-Q", "-R", "-S", "-W", "-w"),
}

func set(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

// optionValue — следующее слово — значение опции: `-u`, и в сцепке `-iu` (у `-uroot` значение внутри).
func optionValue(wrapper, option string) bool {
	if valueOptions[wrapper][option] {
		return true
	}
	if len(option) < 3 || option[0] != '-' || option[1] == '-' {
		return false
	}
	for j := 1; j < len(option); j++ {
		if valueOptions[wrapper]["-"+option[j:j+1]] {
			return j == len(option)-1
		}
	}
	return false
}
