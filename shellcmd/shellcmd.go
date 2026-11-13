// Package shellcmd находит простые команды в исходном тексте оболочки.
package shellcmd

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"strconv"
	"strings"
	"unicode/utf8"

	shExpand "mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// Call описывает простой вызов программы в тексте оболочки.
type Call struct {
	// Name — имя программы без каталога; "" — в слове имени есть `$(` или обратная кавычка.
	Name string
	Args []string
	// Raw — исходный текст команды; во вложенных скриптах он может отличаться от текста, который исполняет внутренняя оболочка.
	Raw  string
	Line int
	Via  []string
	// Offset — байтовое смещение имени программы (после снятых обёрток) в исходной команде, в том числе
	// внутри вложенного скрипта (`bash -c '…'`, `ssh host …`, heredoc на вход оболочке); -1 — его нет
	// в исходном тексте как слова: argv0 из переменной или с подстановкой, байт скрипта из $'…'.
	Offset int
	// Redirs — перенаправления простой команды по порядку (у снятой обёртки — те же, что у неё).
	Redirs []Redir
	// Loop — вызов в теле цикла while, until, for или select (и во вложенном скрипте такого тела).
	Loop bool
	// Background — вызов в задании, чей оператор или объемлющий оператор заканчивается `&`.
	Background bool
	// ScriptInput сообщает, что оболочка исполняет неизвестный текст из stdin или process substitution.
	ScriptInput bool
	literalMask uint32
}

// ArgLiteral сообщает, что оболочка не раскрывает Args[i] (Lit, '…' или экранирование без подстановок).
// Для i вне Args и i ≥ 32 возвращает false: раскрытие аргумента не исключено.
func (c Call) ArgLiteral(i int) bool {
	return i >= 0 && i < len(c.Args) && i < 32 && c.literalMask&(uint32(1)<<uint(i)) != 0
}

// cmd — вызов вместе со словами его командной строки: words[0] — argv0, дальше — Args по порядку.
type cmd struct {
	call           Call
	words          []word
	budget         *nestedParseBudget
	source         string
	expansionDepth int

	stdinInput    *syntax.Redirect
	stdinConsumed bool
}

// word — место слова в исходной команде и его текст как скрипта.
type word struct {
	offset  int    // смещение в исходной команде; -1 — источник слова не найден
	literal bool   // true — значение слова не содержит раскрытия оболочки
	script  string // текст, который выполнит оболочка, получив слово скриптом; "" — не виден до запуска
	pos     []int  // pos[i] — смещение байта i текста script в исходной команде; -1 — такого байта нет
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
	err := parse(cmd, 0, nil, make(map[string]string), &calls)

	if err != nil {
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
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	all, err := Calls(cmd)
	calls := make([]Call, 0, len(all))
	for _, call := range all {
		if wanted[call.Name] {
			calls = append(calls, call)
		}
	}
	var nested *NestedError
	if err != nil && !errors.As(err, &nested) {
		return nil, err
	}
	return calls, err
}

type nestedParseBudget struct {
	used           int
	depth          int
	expansionUsed  int
	expansionLimit int
	limit          int
	parsedBytes    int
	visitedNodes   int
	walkInfoNodes  int
	stdinSteps     int
	substSteps     int
	trackWork      bool
	parsedShells   map[string]int
}

func newNestedParseBudget(inputSize int) *nestedParseBudget {
	limit := max(inputSize*(maxNestedScripts+1), 4096)
	return &nestedParseBudget{limit: limit, expansionLimit: max(inputSize*2, 4096)}
}

func parse(src string, lineBase int, inherited []string, vars map[string]string, calls *[]Call) error {
	return parseWithBudget(src, lineBase, inherited, vars, calls, newNestedParseBudget(len(src)))
}

func parseWithBudget(src string, lineBase int, inherited []string, vars map[string]string, calls *[]Call, budget *nestedParseBudget) error {
	return parseWithSourceMap(src, nil, lineBase, inherited, vars, calls, budget)
}

func parseWithSourceMap(src string, sourceMap []int, lineBase int, inherited []string, vars map[string]string, calls *[]Call, budget *nestedParseBudget) error {
	if budget.trackWork {
		budget.parsedBytes += len(src)
	}
	file, full, err := budget.parseShell(src)
	if err != nil {
		return fmt.Errorf("shellcmd: parse shell: %w", err)
	}
	return walkFileWithSourceMap(file, full, src, sourceMap, lineBase, inherited, vars, calls, budget)
}

func walkFileWithSourceMap(file *syntax.File, full, src string, sourceMap []int, lineBase int, inherited []string, vars map[string]string, calls *[]Call, budget *nestedParseBudget) error {
	if budget.trackWork {
		budget.parsedBytes += len(src)
	}
	first := len(*calls)
	err := walkCalls(file, full, sourceMap, lineBase, inherited, vars, calls, budget)
	if len(full) > len(src) {
		*calls = append((*calls)[:first], trimAppended((*calls)[first:], src)...)
	}
	return err
}

// trimAppended убирает следы ограничителя, дописанного parseShell: вызовы из дописанного текста (после
// `\` в конце строки он склеивается с ней) выпадают, тело дочитанного heredoc кончается на конце src.
func trimAppended(calls []Call, src string) []Call {
	kept := calls[:0]
	for _, c := range calls {
		if c.Offset >= len(src) {
			continue
		}
		for i := range c.Redirs {
			r := &c.Redirs[i]
			switch {
			case r.BodyEnd <= len(src):
			case r.BodyStart >= len(src):
				r.BodyStart, r.BodyEnd, r.Body = -1, -1, ""
			default:
				r.BodyEnd, r.Body = len(src), src[r.BodyStart:]
			}
		}
		kept = append(kept, c)
	}
	return kept
}

// parseShell разбирает текст как bash. Незакрытый heredoc bash дочитывает до конца текста с предупреждением,
// а разбор считает ошибкой — тогда ограничитель дописывается в конец; full — разобранный текст.
func (b *nestedParseBudget) parseShell(src string) (*syntax.File, string, error) {
	if b.trackWork {
		if b.parsedShells == nil {
			b.parsedShells = make(map[string]int)
		}
		b.parsedShells[src]++
	}
	return parseShell(src)
}

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
		if trailing := len(full) - len(strings.TrimRight(full, `\`)); trailing%2 == 1 {
			// `\` в конце продолжил бы строку дописанным ограничителем.
			full += "\n"
		}
		full += "\n" + strings.TrimSuffix(delim, "`") + "\n"
	}
	return nil, full, err
}

func walkCalls(file *syntax.File, src string, sourceMap []int, lineBase int, inherited []string, vars map[string]string, calls *[]Call, budget *nestedParseBudget) error {
	baseCalls := len(*calls)
	info := collectWalkInfo(file, src, vars, budget)
	walker := callWalker{src: src, sourceMap: sourceMap, lineBase: lineBase, inherited: inherited, vars: vars, calls: calls, seen: newCallSet(), budget: budget, info: info}
	syntax.Walk(file, walker.visit)
	if walker.parseErr != nil {
		return fmt.Errorf("shellcmd: walk syntax tree: %w", walker.parseErr)
	}
	nestedErrs := append(walker.nestedErrs, info.errors...)
	if err := parseHeredocStmts(info.shellInputs, src, lineBase, inherited, vars, info.loopCalls, info.backgroundCalls, calls, budget); err != nil {
		nestedErrs = append(nestedErrs, err)
	}
	if err := parsePipedHeredocs(info.pipedHeredocs, src, lineBase, inherited, vars, info.loopCalls, info.backgroundCalls, calls, budget); err != nil {
		nestedErrs = append(nestedErrs, err)
	}
	*calls = append((*calls)[:baseCalls], dedupeCalls((*calls)[baseCalls:], walker.seen)...)
	return errors.Join(nestedErrs...)
}

func dedupeCalls(calls []Call, seen *callSet) []Call {
	seen.clear()
	write := 0
	for _, call := range calls {
		if seen.hasExact(call) {
			continue
		}
		seen.add(call)
		calls[write] = call
		write++
	}
	return calls[:write]
}

type heredocShellInput struct {
	stmt  *syntax.Stmt
	redir *syntax.Redirect
	name  string
	via   []string
}

type walkInfo struct {
	timedCalls map[*syntax.CallExpr]bool
	redirs     map[*syntax.CallExpr][]Redir

	stdinInputs     map[*syntax.CallExpr]*syntax.Redirect
	loopCalls       map[*syntax.CallExpr]bool
	backgroundCalls map[*syntax.CallExpr]bool
	shellInputs     []heredocShellInput
	shellHdocs      map[*syntax.Redirect]bool
	localSubstHdocs map[*syntax.Redirect]bool

	errors        []error
	scriptInputs  map[*syntax.CallExpr]bool
	pipedHeredocs []pipedHeredoc
	workNodes     int
	trackWork     bool
	stdinIndex    *stdinIndex
	// hasInputRedir says whether src holds any `<`; without one no call has stdin to resolve.
	hasInputRedir bool
}

func collectWalkInfo(file *syntax.File, src string, vars map[string]string, budget *nestedParseBudget) walkInfo {
	if !needsWalkInfo(src) {
		return walkInfo{redirs: make(map[*syntax.CallExpr][]Redir)}
	}
	info := walkInfo{
		trackWork:     budget.trackWork,
		hasInputRedir: strings.Contains(src, "<"),
		timedCalls:    make(map[*syntax.CallExpr]bool),
		redirs:        make(map[*syntax.CallExpr][]Redir),

		stdinInputs:     make(map[*syntax.CallExpr]*syntax.Redirect),
		loopCalls:       make(map[*syntax.CallExpr]bool),
		backgroundCalls: make(map[*syntax.CallExpr]bool),
		shellHdocs:      make(map[*syntax.Redirect]bool),
		localSubstHdocs: make(map[*syntax.Redirect]bool),

		scriptInputs: make(map[*syntax.CallExpr]bool),
	}
	knownPipes := make(map[*syntax.BinaryCmd]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		if info.trackWork {
			info.workNodes++
		}
		collectWalkNode(node, file, src, vars, &info, knownPipes)
		collectCallContextNode(node, &info)
		return true
	})
	if budget.trackWork {
		budget.walkInfoNodes += info.workNodes
		if info.stdinIndex != nil {
			budget.stdinSteps += info.stdinIndex.steps()
		}
	}
	return info
}

func collectWalkNode(node syntax.Node, file *syntax.File, src string, vars map[string]string, info *walkInfo, knownPipes map[*syntax.BinaryCmd]bool) {
	switch n := node.(type) {
	case *syntax.BinaryCmd:
		collectPipelineWalkInfo(n, src, vars, info, knownPipes)
	case *syntax.CallExpr:
		collectCallWalkInfo(n, src, vars, info)
	case *syntax.Stmt:
		collectStmtWalkInfo(n, file, src, vars, info)
	}
}

func collectPipelineWalkInfo(binary *syntax.BinaryCmd, src string, vars map[string]string, info *walkInfo, knownPipes map[*syntax.BinaryCmd]bool) {
	if binary.Op != syntax.Pipe && binary.Op != syntax.PipeAll {
		return
	}
	if collectPipedHeredocWalkInfo(binary, src, vars, info) {
		knownPipes[binary] = true
		return
	}
	markPipeline(binary.Y, src, vars, info.scriptInputs)
}

func hasHeredocRedir(redirs []*syntax.Redirect) bool {
	for _, redir := range redirs {
		if redir.Hdoc != nil {
			return true
		}
	}
	return false
}

func collectPipedHeredocWalkInfo(binary *syntax.BinaryCmd, src string, vars map[string]string, info *walkInfo) bool {
	producerStmt := binary.X
	if producerStmt == nil || !hasHeredocRedir(producerStmt.Redirs) {
		return false
	}
	producer, ok := producerStmt.Cmd.(*syntax.CallExpr)
	if !ok || !stdinEchoingCall(producer, src, vars) {
		return false
	}
	name, via, accepted := pipedShellName(binary.Y, src, vars)
	if !accepted {
		return false
	}
	found := false
	for _, redir := range producerStmt.Redirs {
		if redir.Hdoc == nil {
			continue
		}
		info.pipedHeredocs = append(info.pipedHeredocs, pipedHeredoc{redir: redir, shell: binary.Y, name: name, via: via})
		info.shellHdocs[redir] = true
		delete(info.localSubstHdocs, redir)
		found = true
	}
	return found
}

func collectCallWalkInfo(call *syntax.CallExpr, src string, vars map[string]string, info *walkInfo) {
	if len(call.Args) == 0 {
		return
	}
	name, _ := wordValue(call.Args[0])
	name = baseName(name)
	if name == "source" || name == "." {
		if hasProcessSubstitution(call.Args[1:], src) {
			info.scriptInputs[call] = true
		}
		return
	}
	if hasProcessSubstitution(call.Args, src) && shellInputTarget(call, src, vars) {
		info.scriptInputs[call] = true
	}
}

func collectStmtWalkInfo(stmt *syntax.Stmt, file *syntax.File, src string, vars map[string]string, info *walkInfo) {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return
	}
	offset := int(call.Args[0].Pos().Offset())
	var stdin stdinValue
	if info.hasInputRedir {
		if info.stdinIndex == nil {
			info.stdinIndex = newStdinIndex(file)
		}
		stdin = resolveStdinAt(info.stdinIndex, offset, stdinValue{})
	}
	if stdin.redir != nil {
		info.stdinInputs[call] = stdin.redir
	}
	name, shellVia, input, err := shellInputName(stmt, src, vars, stdin.redir)
	if err != nil {
		info.errors = append(info.errors, err)
	} else if input {
		info.shellInputs = append(info.shellInputs, heredocShellInput{stmt: stmt, redir: stdin.redir, name: name, via: shellVia})
		if !slices.Contains(shellVia, "ssh") && stdin.redir != nil && stdin.redir.Hdoc != nil {
			info.shellHdocs[stdin.redir] = true
			info.localSubstHdocs[stdin.redir] = true
		}
	}
	for _, redir := range stmt.Redirs {
		if redir.Op.String() != "<" || !hasProcessSubstitution([]*syntax.Word{redir.Word}, src) {
			continue
		}
		if shellInputTarget(call, src, vars) {
			info.scriptInputs[call] = true
			return
		}
	}
}

func needsWalkInfo(src string) bool {
	if strings.ContainsAny(src, "<>&|") {
		return true
	}
	for _, name := range []string{"bash", "zsh", "sh", "timeout", "env", "sudo", "nohup", "nice", "ionice", "stdbuf", "time", "command", "builtin", "exec", "xargs", "eval", "watch", "trap", "su", "runuser", "flock", "script", "tmux", "ssh", "find", "source", "for", "while", "until", "select"} {
		if strings.Contains(src, name) {
			return true
		}
	}
	return false
}

type pipedHeredoc struct {
	redir *syntax.Redirect
	shell *syntax.Stmt
	name  string
	via   []string
}

func stdinEchoingCall(call *syntax.CallExpr, src string, vars map[string]string) bool {
	if len(call.Args) == 0 {
		return false
	}
	values, _ := words(call.Args, src, vars)
	name := baseName(commandName(values[0]))
	switch name {
	case "tee":
		return true
	case "cat":
		afterOptions := false
		for _, arg := range values[1:] {
			if arg == "--" {
				afterOptions = true
				continue
			}
			if arg == "-" {
				if afterOptions {
					continue
				}
				continue
			}
			if afterOptions || !strings.HasPrefix(arg, "-") {
				return false
			}
			if arg == "-u" || arg == "-v" || arg == "--show-nonprinting" {
				continue
			}
			if strings.HasPrefix(arg, "-") && strings.Trim(arg[1:], "uv") == "" {
				continue
			}
			return false
		}
		return true
	default:
		return false
	}
}

func pipedShellName(stmt *syntax.Stmt, src string, vars map[string]string) (string, []string, bool) {
	return pipedShellNameWithWork(stmt, src, vars, nil)
}

func pipedShellNameWithWork(stmt *syntax.Stmt, src string, vars map[string]string, work *int) (string, []string, bool) {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", nil, false
	}
	values, _ := words(call.Args, src, vars)
	name, args := commandName(values[0]), values[1:]
	var via []string
	scan := wrapperExecScan{work: work}
	for {
		if isShell(name) {
			operand, command, stdin := shellArgs(args)
			return name, via, !shellNoExec(args) && !command && (stdin || operand == len(args))
		}
		if target, ok := scan.find(args); ok {
			via = append(via, name, "exec")
			name, args = commandName(target[0]), target[1:]
			continue
		}
		if name == "sudo" && sudoReadsStdin(args) {
			return "bash", append(via, "sudo"), true
		}
		if wrappers[name] {
			if !wrapperOptionsKnown(name, args) {
				return "", nil, false
			}
			if wrapped, ok := unwrapOnce(name, args); ok && len(wrapped) > 0 {
				via = append(via, name)
				name, args = commandName(wrapped[0]), wrapped[1:]
				continue
			}
		}
		wrapped, wrapperVia, ok := unwrap(name, args)
		if !ok {
			return "", nil, false
		}
		via = append(via, wrapperVia...)
		name, args = commandName(wrapped[0]), wrapped[1:]
	}
}

func markPipeline(stmt *syntax.Stmt, src string, vars map[string]string, inputs map[*syntax.CallExpr]bool) {
	if stmt == nil {
		return
	}
	syntax.Walk(stmt, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if ok && possibleShellInput(call) && shellInputTarget(call, src, vars) {
			inputs[call] = true
		}
		return true
	})
}

func possibleShellInput(call *syntax.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	first, literal := wordValue(call.Args[0])
	if !literal {
		return false
	}
	name := commandName(first)
	return isShell(name) || wrappers[name]
}

func hasProcessSubstitution(words []*syntax.Word, src string) bool {
	for _, word := range words {
		start, end := int(word.Pos().Offset()), int(word.End().Offset())
		if !validSpan(src, start, end) || (!strings.Contains(src[start:end], "<(") && !strings.Contains(src[start:end], ">(")) {
			continue
		}
		found := false
		syntax.Walk(word, func(node syntax.Node) bool {
			if _, ok := node.(*syntax.ProcSubst); ok {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

func shellInputTarget(call *syntax.CallExpr, src string, vars map[string]string) bool {
	return shellInputTargetWithWork(call, src, vars, nil)
}

func shellInputTargetWithWork(call *syntax.CallExpr, src string, vars map[string]string, work *int) bool {
	if len(call.Args) == 0 {
		return false
	}
	values, _ := words(call.Args, src, vars)
	if len(values) == 0 {
		return false
	}
	name := commandName(values[0])
	if !isShell(name) && !wrappers[name] {
		return false
	}
	args := values[1:]
	argOffset := 0
	scan := wrapperExecScan{work: work}
	for {
		if name == "sudo" && sudoReadsStdin(args) {
			return true
		}
		if (name == "su" || name == "runuser") && suReadsShell(name, args) {
			return true
		}
		if isShell(name) {
			return shellInputForShellCall(call, args, argOffset, src)
		}
		var ok bool
		name, args, argOffset, ok = shellInputNextTargetWithScan(name, args, argOffset, &scan)
		if !ok {
			return false
		}
	}
}

func shellInputForShellCall(call *syntax.CallExpr, args []string, argOffset int, src string) bool {
	operand, command, stdin := shellArgs(args)
	if shellNoExec(args) || command {
		return false
	}
	if stdin || operand == len(args) {
		return true
	}
	index := 1 + argOffset + operand
	return operand < len(args) && index < len(call.Args) && hasProcessSubstitution(call.Args[index:index+1], src)
}

func shellInputNextTarget(name string, args []string, argOffset int) (string, []string, int, bool) {
	return shellInputNextTargetWithScan(name, args, argOffset, &wrapperExecScan{})
}

func shellInputNextTargetWithScan(name string, args []string, argOffset int, scan *wrapperExecScan) (string, []string, int, bool) {
	if target, ok := scan.find(args); ok {
		argOffset += len(args) - len(target) + 1
		return commandName(target[0]), target[1:], argOffset, true
	}
	if wrappers[name] {
		if !wrapperOptionsKnown(name, args) {
			return "", nil, argOffset, false
		}
		if wrapped, ok := unwrapOnce(name, args); ok && len(wrapped) > 0 {
			argOffset += len(args) - len(wrapped) + 1
			return commandName(wrapped[0]), wrapped[1:], argOffset, true
		}
	}
	wrapped, _, ok := unwrap(name, args)
	if !ok {
		return "", nil, argOffset, false
	}
	argOffset += len(args) - len(wrapped) + 1
	return commandName(wrapped[0]), wrapped[1:], argOffset, true
}

type callWalker struct {
	src        string
	sourceMap  []int
	lineBase   int
	inherited  []string
	vars       map[string]string
	calls      *[]Call
	info       walkInfo
	seen       *callSet
	budget     *nestedParseBudget
	parseErr   error
	nestedErrs []error
}

func (w *callWalker) visit(node syntax.Node) bool {
	if w.budget.trackWork {
		w.budget.visitedNodes++
	}

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
		if w.parseErr != nil {
			return false
		}
	}
	return true
}

func (w *callWalker) visitRedirect(redirect *syntax.Redirect) {
	// Цель перенаправления выполняется, тело heredoc — данные, пока оно не передано оболочке.
	if redirect.Word != nil {
		syntax.Walk(redirect.Word, w.visit)
	}
	if redirect.Hdoc == nil || quotedHeredoc(redirect.Word) {
		return
	}
	if !w.info.shellHdocs[redirect] {
		syntax.Walk(redirect.Hdoc, w.visit)
		return
	}
	if !w.info.localSubstHdocs[redirect] {
		return
	}
	syntax.Walk(redirect.Hdoc, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			syntax.Walk(node, w.visit)
			return false
		}
		return true
	})
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

	args, split, literals := wordValues(call.Args, w.src, w.vars, true)

	start, end := int(call.Args[0].Pos().Offset()), int(call.Args[len(call.Args)-1].End().Offset())
	if !validSpan(w.src, start, end) {
		w.parseErr = fmt.Errorf("shellcmd: invalid source span for command")
		return
	}
	via := append([]string(nil), w.inherited...)
	if w.info.timedCalls[call] {
		via = append(via, "time")
	}
	needsScripts := wrappers[commandName(args[0])] || isShell(commandName(args[0])) || commandName(args[0]) == "ssh" || commandName(args[0]) == "find" || scriptInvocation(commandName(args[0]), args[1:])
	if !needsScripts {
		_, needsScripts = execTarget(args[1:])
	}
	if !needsScripts {
		needsScripts = mainShellArgIndex(args[1:]) >= 0
	}
	words, err := cmdWords(call.Args, w.src, needsScripts, literals, w.sourceMap)
	if err != nil {
		w.parseErr = fmt.Errorf("shellcmd: expand command word: %w", err)
		return
	}
	ws := splitWords(words, split)
	c := cmd{call: Call{Name: commandName(args[0]), Args: args[1:], Raw: w.src[start:end], Line: int(call.Pos().Line()) + w.lineBase, Via: via, Offset: ws[0].offset, Redirs: w.info.redirs[call], Loop: w.info.loopCalls[call], Background: w.info.backgroundCalls[call], ScriptInput: w.info.scriptInputs[call]}, words: ws, budget: w.budget, source: w.src, stdinInput: w.info.stdinInputs[call]}
	if err := emit(c, w.lineBase, w.vars, w.calls, w.seen); err != nil {
		var nested *NestedError
		if errors.As(err, &nested) {
			w.nestedErrs = append(w.nestedErrs, err)
			return
		}
		w.parseErr = err
		return
	}
}

func collectCallContextNode(node syntax.Node, info *walkInfo) {
	mark := func(node syntax.Node, target map[*syntax.CallExpr]bool) {
		syntax.Walk(node, func(child syntax.Node) bool {
			if info.trackWork {
				info.workNodes++
			}
			if call, ok := child.(*syntax.CallExpr); ok {
				target[call] = true
			}
			return true
		})
	}
	switch n := node.(type) {
	case *syntax.Stmt:
		if n.Background {
			mark(n, info.backgroundCalls)
		}
	case *syntax.TimeClause:
		// `time` без команды разбор даёт TimeClause без Stmt.
		if n.Stmt != nil {
			mark(n.Stmt, info.timedCalls)
		}
	case *syntax.WhileClause:
		for _, stmt := range n.Do {
			mark(stmt, info.loopCalls)
		}
	case *syntax.ForClause:
		for _, stmt := range n.Do {
			mark(stmt, info.loopCalls)
		}
	}
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

func parseHeredocStmts(inputs []heredocShellInput, src string, lineBase int, inherited []string, vars map[string]string, loopCalls, backgroundCalls map[*syntax.CallExpr]bool, calls *[]Call, budget *nestedParseBudget) error {
	var nestedErrs []error
	for _, input := range inputs {
		stmt, name, shellVia, redir := input.stmt, input.name, input.via, input.redir
		body, err := stdinRedirectBody(redir, src, vars)
		if err != nil {
			nestedErrs = append(nestedErrs, err)
			continue
		}
		via := append(append(append([]string(nil), inherited...), shellVia...), name)
		call, _ := stmt.Cmd.(*syntax.CallExpr)
		line := int(stmt.Pos().Line()) + lineBase - 1
		if redir != nil && redir.Hdoc != nil {
			line = int(redir.Hdoc.Pos().Line()) + lineBase - 1
		}
		first := len(*calls)
		if err := parseNestedWithBudget(body.String(), body.pos, line, via, loopCalls[call], backgroundCalls[call], vars, calls, budget); err != nil {
			nestedErrs = append(nestedErrs, err)
		}
		dropSubstCalls(calls, first, body, budget)
		preserveMappedRaw(*calls, first, body, src)
	}
	return errors.Join(nestedErrs...)
}

func preserveMappedRaw(calls []Call, first int, body mapped, src string) {
	if len(body.rawStart) != len(body.text) || len(body.rawEnd) != len(body.text) {
		return
	}
	text := body.String()
	searchFrom := 0
	for i := first; i < len(calls); i++ {
		start := strings.Index(text[searchFrom:], calls[i].Raw)
		if start < 0 {
			continue
		}
		start += searchFrom
		end := start + len(calls[i].Raw)
		searchFrom = end
		if start >= end || end > len(body.rawEnd) {
			continue
		}
		sourceStart, sourceEnd := body.rawStart[start], body.rawEnd[end-1]
		if !validSpan(src, sourceStart, sourceEnd) || sourceEnd <= sourceStart {
			continue
		}
		calls[i].Raw = src[sourceStart:sourceEnd]
		if dynamicFirstWord(calls[i].Raw) {
			calls[i].Offset = -1
		}
	}
}

func dynamicFirstWord(raw string) bool {
	word := strings.Fields(raw)
	return len(word) > 0 && (strings.ContainsAny(word[0], "$") || strings.Contains(word[0], "\x60"))
}

func parsePipedHeredocs(piped []pipedHeredoc, src string, lineBase int, inherited []string, vars map[string]string, loopCalls, backgroundCalls map[*syntax.CallExpr]bool, calls *[]Call, budget *nestedParseBudget) error {
	var nestedErrs []error
	for _, input := range piped {
		redir := input.redir
		body := heredocScript(redir, src)
		via := append(slices.Clone(inherited), "pipe")
		via = append(via, input.via...)
		via = append(via, input.name)
		call, _ := input.shell.Cmd.(*syntax.CallExpr)
		first := len(*calls)
		err := parseNestedWithBudget(body.String(), body.pos, lineBase, via, loopCalls[call], backgroundCalls[call], vars, calls, budget)
		for i := first; i < len(*calls); i++ {
			offset := (*calls)[i].Offset
			if validSpan(src, offset, offset) {
				(*calls)[i].Line = lineBase + 1 + strings.Count(src[:offset], "\n")
			}
		}
		if err != nil {
			nestedErrs = append(nestedErrs, err)
		}
	}
	return errors.Join(nestedErrs...)
}

func heredocScript(redir *syntax.Redirect, src string) mapped {
	var body mapped
	raw := heredocText(redir, src)
	base := int(redir.Hdoc.Pos().Offset())
	if redir.Op.String() != "<<-" {
		body.add(raw, base)
		return body
	}
	for start := 0; start < len(raw); {
		end := strings.IndexByte(raw[start:], '\n')
		if end < 0 {
			end = len(raw)
		} else {
			end += start + 1
		}
		prefix := 0
		for start+prefix < end && raw[start+prefix] == '\t' {
			prefix++
		}
		body.add(raw[start+prefix:end], base+start+prefix)
		start = end
	}
	return body
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

func shellInputName(stmt *syntax.Stmt, src string, vars map[string]string, stdinInput *syntax.Redirect) (string, []string, bool, error) {
	if stdinInput == nil {
		return "", nil, false, nil
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", nil, false, nil
	}
	hasHeredoc := stdinInput != nil
	if first, literal := wordValue(call.Args[0]); literal {
		name := commandName(first)
		if !isShell(name) && !wrappers[name] {
			execTarget := false
			execSeen := false
			for _, arg := range call.Args[1:] {
				value, ok := wordValue(arg)
				if !ok {
					execTarget = true
					break
				}
				if value == "exec" {
					execSeen = true
				}
				if value == "--" && execSeen {
					execTarget = true
					break
				}
			}
			if !execTarget {
				return "", nil, false, nil
			}
		}
	}
	args, _ := words(call.Args, src, vars)
	name := commandName(args[0])
	if name == "ssh" {
		return "", nil, false, nil
	}
	target, shellVia, err := resolveShellInput(name, args[1:], hasHeredoc)
	return target, shellVia, target != "", err
}

func resolveShellInput(name string, args []string, hasHeredoc bool) (string, []string, error) {
	return resolveShellInputWithWork(name, args, hasHeredoc, nil)
}

func resolveShellInputWithWork(name string, args []string, hasHeredoc bool, work *int) (string, []string, error) {
	var via []string
	scan := wrapperExecScan{work: work}
	for {
		if name == "sudo" && hasHeredoc && sudoReadsStdin(args) {
			return "bash", append(via, "sudo"), nil
		}
		if (name == "su" || name == "runuser") && hasHeredoc && suReadsShell(name, args) {
			return "sh", append(via, name), nil
		}
		if isShell(name) {
			target, shellVia, ok := shellInputForShell(name, args, hasHeredoc, via)
			if !ok {
				return "", nil, nil
			}
			return target, shellVia, nil
		}
		var ok bool
		name, args, via, ok = nextShellInputTarget(name, args, via, &scan)
		if !ok {
			return "", nil, nil
		}
	}
}

func sshExecutionDisablesStdin(args []string, host, command int) bool {
	end := min(command, len(args))
	for _, span := range [][2]int{{0, host}, {host + 1, end}} {
		for i := span[0]; i < span[1]; i++ {
			arg := args[i]
			if arg == "-o" {
				if i+1 < span[1] && sshConfigDisablesStdin(args[i+1]) {
					return true
				}
				i++
				continue
			}
			if strings.HasPrefix(arg, "-o") && len(arg) > 2 {
				if sshConfigDisablesStdin(arg[2:]) {
					return true
				}
				continue
			}
			if arg == "-N" || arg == "-n" || arg == "-f" || arg == "-s" || arg == "-O" || strings.HasPrefix(arg, "-O") || strings.HasPrefix(arg, "-W") {
				return true
			}
			if optionValue("ssh", arg) {
				i++
				continue
			}
			if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsAny(arg[1:], "Nnfs") {
				return true
			}
		}
	}
	return false
}

func sshConfigDisablesStdin(option string) bool {
	fields := strings.Fields(option)
	if len(fields) == 0 {
		return false
	}
	key, value, hasValue := strings.Cut(fields[0], "=")
	if hasValue {
		fields = append([]string{key, value}, fields[1:]...)
	} else if len(fields) > 1 {
		key, value = fields[0], fields[1]
	} else {
		return false
	}
	key, value = strings.ToLower(key), strings.ToLower(value)
	switch key {
	case "stdinnull":
		return value == "yes"
	case "forkafterauthentication":
		return value == "yes"
	case "sessiontype":
		return value == "none" || value == "subsystem"
	}
	return false
}

func shellInputForShell(name string, args []string, hasHeredoc bool, via []string) (string, []string, bool) {
	if !hasHeredoc {
		return "", nil, false
	}
	if shellReadsHeredoc(args) || mainCompatibleNoExecHeredoc(args) {
		return name, via, true
	}
	return "", nil, false
}

func nextShellInputTarget(name string, args []string, via []string, scan *wrapperExecScan) (string, []string, []string, bool) {
	if target, ok := scan.find(args); ok {
		return commandName(target[0]), target[1:], append(via, name, "exec"), true
	}
	if wrappers[name] {
		if wrapped, ok := unwrapOnce(name, args); ok && len(wrapped) > 0 {
			if !isShell(commandName(wrapped[0])) {
				if target, fallbackVia, fallbackOK := unwrapMain(name, args); fallbackOK && len(target) > 0 && isShell(baseName(target[0])) && shellReadsHeredoc(target[1:]) {
					return baseName(target[0]), target[1:], append(via, fallbackVia...), true
				}
			}
			return commandName(wrapped[0]), wrapped[1:], append(via, name), true
		}
	}
	wrapped, wrapperVia, ok := unwrap(name, args)
	if ok {
		return commandName(wrapped[0]), wrapped[1:], append(via, wrapperVia...), true
	}
	if target, fallbackVia, fallbackOK := unwrapMain(name, args); fallbackOK && len(target) > 0 && isShell(baseName(target[0])) && shellReadsHeredoc(target[1:]) {
		return baseName(target[0]), target[1:], append(via, fallbackVia...), true
	}
	return "", nil, nil, false
}

func isShell(name string) bool {
	return name == "bash" || name == "sh" || name == "zsh"
}

// shellReadsHeredoc — оболочка читает heredoc как скрипт, если её stdin не заменён -c или файлом.
func shellReadsHeredoc(args []string) bool {
	if shellNoExec(args) {
		return false
	}
	operand, command, stdin := shellArgs(args)
	return !command && (stdin || operand == len(args))
}

func mainCompatibleNoExecHeredoc(args []string) bool {
	if !shellNoExec(args) || !hasOption("bash", args, "-E") {
		return false
	}
	operand, command, stdin := shellArgs(args)
	return !command && (stdin || operand == len(args))
}

func shellNoExec(args []string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" || args[i] == "-" {
			return false
		}
		option, takesValue, _, _ := shellArgumentOption(args[i])
		if !option {
			return false
		}
		if strings.HasPrefix(args[i], "-") && !strings.HasPrefix(args[i], "--") && strings.Contains(args[i][1:], "n") {
			return true
		}
		if takesValue {
			i++
		}
	}
	return false
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

const maxNestedScripts = 32
const maxNestedScriptBytes = 1 << 20

// parseNested добавляет вызовы скрипта src, который выполнит вложенная оболочка; pos[i] — смещение байта i
// скрипта во внешнем тексте (-1 — такого байта нет).
func parseNested(src string, pos []int, lineBase int, via []string, inLoop, inBackground bool, vars map[string]string, calls *[]Call) error {
	return parseNestedWithBudget(src, pos, lineBase, via, inLoop, inBackground, vars, calls, newNestedParseBudget(len(src)))
}

func parseNestedWithBudget(src string, pos []int, lineBase int, via []string, inLoop, inBackground bool, vars map[string]string, calls *[]Call, budget *nestedParseBudget) error {
	return parseNestedFileWithBudget(nil, "", src, pos, lineBase, via, inLoop, inBackground, vars, calls, budget)
}

func parseNestedFileWithBudget(file *syntax.File, full, src string, pos []int, lineBase int, via []string, inLoop, inBackground bool, vars map[string]string, calls *[]Call, budget *nestedParseBudget) error {
	if budget.depth >= maxNestedScripts {
		return &NestedError{Script: src, Err: fmt.Errorf("shellcmd: nested script depth exceeds %d", maxNestedScripts)}
	}
	budget.depth++
	defer func() { budget.depth-- }()
	if len(src) > maxNestedScriptBytes {
		return &NestedError{Script: src, Err: fmt.Errorf("shellcmd: nested script exceeds %d bytes", maxNestedScriptBytes)}
	}
	if len(src) > budget.limit-budget.used {
		return &NestedError{Script: src, Err: fmt.Errorf("shellcmd: nested parse budget exceeds %d bytes", budget.limit)}
	}
	budget.used += len(src)
	nestedCalls := make([]Call, 0)
	nestedVars := make(map[string]string, len(vars))
	maps.Copy(nestedVars, vars)
	var err error
	if file == nil {
		err = parseWithSourceMap(src, pos, lineBase, via, nestedVars, &nestedCalls, budget)
	} else {
		err = walkFileWithSourceMap(file, full, src, pos, lineBase, via, nestedVars, &nestedCalls, budget)
	}
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
func cmdWords(args []*syntax.Word, src string, scripts bool, literals []bool, sourceMap []int) ([]word, error) {
	ws := make([]word, len(args))
	for i, arg := range args {
		ws[i].offset = -1
		ws[i].literal = literals[i]
		if scripts {
			var err error
			ws[i].script, ws[i].pos, err = wordScript(arg, src, literals[i])
			if err != nil {
				return nil, err
			}
		}
		if literals[i] {
			ws[i].offset = int(arg.Pos().Offset())
			if !sourceSpanMapped(int(arg.Pos().Offset()), int(arg.End().Offset()), sourceMap) {
				ws[i].literal = false
			}
		}
	}
	return ws, nil
}

func sourceSpanMapped(start, end int, sourceMap []int) bool {
	if len(sourceMap) == 0 {
		return true
	}
	if start < 0 || end > len(sourceMap) || start >= end {
		return false
	}
	for _, offset := range sourceMap[start:end] {
		if offset < 0 {
			return false
		}
	}
	return true
}

// wordScript returns the shell text of a word and its source offsets.
func wordScript(w *syntax.Word, src string, literal bool) (string, []int, error) {
	var text mapped
	if literal || len(w.Parts) == 1 && isANSICQuoted(w.Parts[0]) {
		for _, part := range w.Parts {
			if err := text.addLiteral(part, src); err != nil {
				return "", nil, err
			}
		}
		return text.String(), text.pos, nil
	}
	if len(w.Parts) != 1 {
		return "", nil, nil
	}
	quoted, ok := w.Parts[0].(*syntax.DblQuoted)
	if !ok || !slices.ContainsFunc(quoted.Parts, func(p syntax.WordPart) bool { _, lit := p.(*syntax.Lit); return lit }) {
		return "", nil, nil
	}
	start, end := int(quoted.Pos().Offset()), int(quoted.End().Offset())
	if !validSpan(src, start, end) || end-start < 2 {
		return "", nil, nil
	}
	text.unescape(src[start+1:end-1], start+1, true)
	return text.String(), text.pos, nil
}

func ansiSourceBase(n syntax.Node, value, src string) int {
	start, end := int(n.Pos().Offset())+2, int(n.End().Offset())-1
	if validSpan(src, start, end) && src[start:end] == value {
		return start
	}
	return -1
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
	text     []byte
	pos      []int
	rawStart []int
	rawEnd   []int
	// subst are source spans of substitutions the outer shell runs while
	// expanding a heredoc body or here-string.
	subst [][2]int
}

func (m *mapped) String() string { return string(m.text) }

// addLiteral добавляет значение части слова без подстановок (её пропускает wordValue).
func (m *mapped) addLiteral(part syntax.WordPart, src string) error {
	switch p := part.(type) {
	case *syntax.Lit:
		raw, base := litSource(p, src, false)
		m.unescape(raw, base, false)
	case *syntax.SglQuoted:
		base := -1
		if p.Dollar {
			base = ansiSourceBase(p, p.Value, src)
			return m.unescapeANSI(p.Value, base)
		}
		base = sourceBase(p, p.Value, 1, src)
		m.add(p.Value, base)
	case *syntax.DblQuoted:
		for _, inner := range p.Parts {
			if lit, ok := inner.(*syntax.Lit); ok {
				raw, base := litSource(lit, src, true)
				m.unescape(raw, base, true)
			}
		}
	}
	return nil
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

// unescapeANSI декодирует экранирование в ANSI-C-слове Bash.
func (m *mapped) unescapeANSI(raw string, base int) error {
	for i := 0; i < len(raw); {
		start := i
		if raw[i] != '\\' {
			_, size := utf8.DecodeRuneInString(raw[i:])
			if size < 1 {
				size = 1
			}
			for j := 0; j < size; j++ {
				m.push(raw[i+j], base, i+j)
			}
			i += size
			continue
		}
		end := ansiEscapeEnd(raw, i)
		escape := raw[i:end]
		var decoded string
		if len(escape) == 3 && escape[1] == 'c' {
			if escape[2] == '?' {
				decoded = string([]byte{0x7f})
			} else {
				decoded = string([]byte{escape[2] & 0x1f})
			}
		} else {
			var err error
			decoded, _, err = shExpand.Format(nil, escape, nil)
			if err != nil {
				return fmt.Errorf("shellcmd: decode ANSI-C escape: %w", err)
			}
		}
		if iNul := strings.IndexByte(decoded, 0); iNul >= 0 {
			decoded = decoded[:iNul]
			for j := 0; j < len(decoded); j++ {
				m.push(decoded[j], base, start)
			}
			return nil
		}
		for j := 0; j < len(decoded); j++ {
			m.push(decoded[j], base, start)
		}
		i = end
	}
	return nil
}

// ansiEscapeEnd finds one ANSI-C escape token; expand.Format remains the sole decoder.
func ansiEscapeEnd(raw string, start int) int {
	i := start + 1
	if i >= len(raw) {
		return len(raw)
	}
	switch raw[i] {
	case 'c':
		if i+1 < len(raw) {
			return i + 2
		}
	case 'x':
		i++
		for n := 0; i < len(raw) && n < 2 && isHexByte(raw[i]); n++ {
			i++
		}
		return i
	case 'u':
		return min(len(raw), i+5)
	case 'U':
		return min(len(raw), i+9)
	default:
		if raw[i] >= '0' && raw[i] <= '7' {
			i++
			for n := 1; i < len(raw) && n < 3 && raw[i] >= '0' && raw[i] <= '7'; n++ {
				i++
			}
			return i
		}
	}
	return min(len(raw), start+2)
}

func isHexByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

func isANSICQuoted(part syntax.WordPart) bool {
	quoted, ok := part.(*syntax.SglQuoted)
	return ok && quoted.Dollar
}

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
	result, split, _ := wordValues(ws, src, vars, false)
	return result, split
}

func wordValues(ws []*syntax.Word, src string, vars map[string]string, needLiterals bool) ([]string, int, []bool) {
	result := make([]string, len(ws))
	var literals []bool
	if needLiterals {
		literals = make([]bool, len(ws))
	}
	for i, w := range ws {
		if value, ok := wordValue(w); ok {
			result[i] = value
			if needLiterals {
				literals[i] = true
			}
			continue
		}
		result[i] = nodeText(w, src)
	}
	if len(ws) > 0 {
		name, ok := paramName(ws[0])
		value, set := vars[name]
		_, quoted := ws[0].Parts[0].(*syntax.DblQuoted)
		switch {
		case ok && set && !quoted:
			fields := splitFields(value)
			if len(fields) == 0 {
				if len(ws) > 1 {
					return result[1:], -1, literals
				}
				result[0] = value
				break
			}
			return append(fields, result[1:]...), len(fields) - 1, literals
		case ok && value != "":
			result[0] = value
		default:
			if !needLiterals || !literals[0] {
				result[0] = unquotedText(ws[0], src)
			}
		}
	}
	return result, 0, literals
}

// splitFields делит значение переменной на слова по пробелам, табам и переводам строк вне подстановок
// ($(…), ${…}, `…`): их текст остаётся одним словом, как останется результат подстановки в имени.
func splitFields(s string) []string {
	var fields []string
	var closers []byte
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case len(closers) > 0 && c == closers[len(closers)-1]:
			closers = closers[:len(closers)-1]
		case c == '`':
			closers = append(closers, '`')
		case c == '$' && i+1 < len(s) && (s[i+1] == '(' || s[i+1] == '{'):
			closers = append(closers, map[byte]byte{'(': ')', '{': '}'}[s[i+1]])
			if start < 0 {
				start = i
			}
			i++
			continue
		case len(closers) == 0 && (c == ' ' || c == '\t' || c == '\n'):
			if start >= 0 {
				fields = append(fields, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		fields = append(fields, s[start:])
	}
	return fields
}

// splitWords согласует слова команды с делением argv0: split > 0 — после argv0 вставлены слова,
// которых нет в исходном тексте; -1 — argv0 из пустого значения выпал, программа — следующее слово.
func splitWords(ws []word, split int) []word {
	if split == 0 {
		return ws
	}
	if split < 0 {
		return ws[1:]
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
		Via: via, Offset: c.words[i].offset, Redirs: c.call.Redirs, Loop: c.call.Loop, Background: c.call.Background, ScriptInput: c.call.ScriptInput}, words: c.words[i:], budget: c.budget, source: c.source,
		stdinInput: c.stdinInput, stdinConsumed: c.stdinConsumed || c.call.Name == "xargs", expansionDepth: c.expansionDepth + 1}
}

func (c cmd) via(more ...string) []string {
	return append(slices.Clone(c.call.Via), more...)
}

// emit записывает вызовы точного разбора и совместимого с прежней версией обхода.
func emit(c cmd, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	if c.expansionDepth > maxNestedScripts {
		return &NestedError{Script: c.call.Raw, Err: fmt.Errorf("shellcmd: command expansion depth exceeds %d", maxNestedScripts)}
	}
	c.call.literalMask = 0
	for i := 0; i < len(c.call.Args) && i < 32; i++ {
		if i+1 < len(c.words) && c.words[i+1].literal {
			c.call.literalMask |= uint32(1) << uint(i)
		}
	}
	appendCall(seen, calls, c.call)

	if err := emitExpanded(c, lineBase, vars, calls, seen); err != nil {
		return err
	}
	return emitCompatibleRoutes(c, lineBase, vars, calls, seen)
}

func emitExpanded(c cmd, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	start := len(*calls)
	err := expand(c, lineBase, vars, calls, seen)
	markCalls(seen, (*calls)[start:])
	return err
}

func emitCompatibleRoutes(c cmd, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	name, args := c.call.Name, c.call.Args
	if err := emitShellInputTarget(c, lineBase, vars, calls, seen); err != nil {
		return err
	}

	exact, exactVia, exactOK := unwrap(name, args)
	exactStart := -1
	if exactOK {
		exactStart = wrapperTargetStart(c, exact, exactVia)
	}
	main, mainVia, mainOK := unwrapMain(name, args)
	if mainOK && exactOK && len(c.words)-len(main) == exactStart {
		mainOK = false
	}
	if err := emitGenericShellTarget(c, name, args, exactStart, lineBase, vars, calls, seen); err != nil {
		return err
	}
	if err := emitExactWrapperTarget(c, exactOK, exactStart, exactVia, lineBase, vars, calls, seen); err != nil {
		return err
	}
	return emitMainTarget(c, main, mainVia, mainOK, lineBase, vars, calls, seen)
}

func emitShellInputTarget(c cmd, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	if !c.call.ScriptInput {
		return nil
	}
	target, ok := sudoShellTarget(c)
	if !ok {
		return nil
	}
	return emit(target, lineBase, vars, calls, seen)
}

func emitGenericShellTarget(c cmd, name string, args []string, exactStart, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	route, ok := genericShellTarget(c, name, args, exactStart)
	if !ok {
		return nil
	}
	return emit(route, lineBase, vars, calls, seen)
}

func emitExactWrapperTarget(c cmd, exactOK bool, start int, via []string, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	if !exactOK {
		return nil
	}
	child, ok := exactWrapperTarget(c, start, via)
	if !ok {
		return nil
	}
	return emit(child, lineBase, vars, calls, seen)
}

func emitMainTarget(c cmd, main []string, via []string, ok bool, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	if !ok {
		return nil
	}
	start := len(c.words) - len(main)
	candidate := c.sub(start, c.via(via...))
	if seen.hasBase(candidate.call) {
		return nil
	}
	return emit(candidate, lineBase, vars, calls, seen)
}

func appendCall(seen *callSet, calls *[]Call, call Call) {
	if seen.hasExact(call) {
		return
	}
	seen.add(call)
	*calls = append(*calls, call)
}

func markCall(seen *callSet, call Call) {
	seen.add(call)
}

func markCalls(seen *callSet, calls []Call) {
	for _, call := range calls {
		markCall(seen, call)
	}
}

func genericShellTarget(c cmd, name string, args []string, exactStart int) (cmd, bool) {
	if isShell(name) || name == "ssh" || dataArgs[name] || baselineWrapper(name) {
		return cmd{}, false
	}
	if _, ok := execTarget(args); ok {
		return cmd{}, false
	}
	index := mainShellArgIndex(args)
	if index < 0 {
		return cmd{}, false
	}
	if exactStart >= 0 && index+1 == exactStart {
		return cmd{}, false
	}
	return c.sub(index+1, c.via(name)), true
}

func exactWrapperTarget(c cmd, start int, via []string) (cmd, bool) {
	if slices.Contains(via, "parallel") {
		return parallelChild(c, via)
	}
	return c.sub(start, c.via(via...)), true
}

func sudoReadsStdin(args []string) bool {
	if !hasOption("sudo", args, "-s", "--shell", "-i", "--login") {
		return false
	}
	_, hasCommand := unwrapOnce("sudo", args)
	return !hasCommand
}

func sudoShellTarget(c cmd) (cmd, bool) {
	name, args := c.call.Name, c.call.Args
	argStart := 0
	peeled := false
	via := slices.Clone(c.call.Via)
	for {
		if name == "sudo" && peeled && sudoReadsStdin(args) {
			return c.sub(argStart+1, append(via, "sudo")), true
		}
		if !wrappers[name] || !wrapperOptionsKnown(name, args) {
			return cmd{}, false
		}
		wrapped, ok := unwrapOnce(name, args)
		if !ok || len(wrapped) == 0 {
			return cmd{}, false
		}
		argStart += len(args) - len(wrapped)
		peeled = true
		via = append(via, name)
		name, args = commandName(wrapped[0]), wrapped[1:]
	}
}

func wrapperOptionsKnown(name string, args []string) bool {
	if name == "command" && commandIsLookup(args) {
		return true
	}
	for i := 0; i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-"; {
		next, unknown := wrapperOptionStep(name, args, i)
		if unknown {
			return false
		}
		if next <= i {
			i++
		} else {
			i = next
		}
	}
	return true
}

func wrapperOptionStep(name string, args []string, i int) (int, bool) {
	arg := args[i]
	if arg == "--" {
		return len(args), false
	}
	if strings.HasPrefix(arg, "--") {
		return longWrapperOptionStep(name, args, i)
	}
	return shortWrapperOptionStep(name, args, i)
}

func longWrapperOptionStep(name string, args []string, i int) (int, bool) {
	key, _, attached := strings.Cut(args[i], "=")
	if !valueOptions[name][key] && !flagOptions[name][key] {
		return i, true
	}
	if valueOptions[name][key] && !attached && i+1 < len(args) {
		return i + 2, false
	}
	return i + 1, false
}

func shortWrapperOptionStep(name string, args []string, i int) (int, bool) {
	for j := 1; j < len(args[i]); j++ {
		key, takesValue := shortOptionAt(name, args[i], j)
		if !valueOptions[name][key] && !flagOptions[name][key] {
			return i, true
		}
		if takesValue {
			if j == len(args[i])-1 && i+1 < len(args) {
				return i + 2, false
			}
			return i + 1, false
		}
	}
	return i + 1, false
}

type callIdentity struct {
	name   string
	offset int
	hash   uint64
}

type callSet struct {
	calls map[callIdentity]Call
	more  map[callIdentity][]Call
}

func newCallSet() *callSet {
	return &callSet{calls: make(map[callIdentity]Call), more: make(map[callIdentity][]Call)}
}

func (s *callSet) clear() {
	clear(s.calls)
	clear(s.more)
}

const (
	callHashOffsetBasis uint64 = 14695981039346656037
	callHashPrime       uint64 = 1099511628211
)

func hashCallParts(hash uint64, parts []string) uint64 {
	for _, part := range parts {
		for i := 0; i < len(part); i++ {
			hash = (hash ^ uint64(part[i])) * callHashPrime
		}
		hash = (hash ^ 0xff) * callHashPrime
	}
	return hash
}

func callBaseIdentity(call Call) (callIdentity, bool) {
	if call.Offset < 0 {
		return callIdentity{}, false
	}
	return callIdentity{name: call.Name, offset: call.Offset, hash: hashCallParts(callHashOffsetBasis, call.Args)}, true
}

func sameCallArgs(a, b Call) bool {
	return slices.Equal(a.Args, b.Args)
}

func sameCall(a, b Call) bool {
	return sameCallArgs(a, b) && slices.Equal(a.Via, b.Via)
}

func (s *callSet) hasBase(call Call) bool {
	key, ok := callBaseIdentity(call)
	if !ok {
		return false
	}
	if first, exists := s.calls[key]; exists && sameCallArgs(first, call) {
		return true
	}
	for _, previous := range s.more[key] {
		if sameCallArgs(previous, call) {
			return true
		}
	}
	return false
}

func (s *callSet) hasExact(call Call) bool {
	key, ok := callBaseIdentity(call)
	if !ok {
		return false
	}
	if first, exists := s.calls[key]; exists && sameCall(first, call) {
		return true
	}
	for _, previous := range s.more[key] {
		if sameCall(previous, call) {
			return true
		}
	}
	return false
}

func (s *callSet) add(call Call) {
	key, ok := callBaseIdentity(call)
	if !ok {
		return
	}
	previous, exists := s.calls[key]
	if !exists {
		s.calls[key] = call
		return
	}
	if sameCall(previous, call) {
		return
	}
	for _, previous := range s.more[key] {
		if sameCall(previous, call) {
			return
		}
	}
	s.more[key] = append(s.more[key], call)
}

func callKeyBase(call Call) (string, bool) {
	if call.Offset < 0 {
		return "", false
	}
	return call.Name + "\x00" + strconv.Itoa(call.Offset) + "\x00" + strings.Join(call.Args, "\x00"), true
}

func wrapperTargetStart(c cmd, target, via []string) int {
	if slices.Contains(via, "parallel") {
		parallelOffset := 0
		parallelArgs := c.call.Args
		if c.call.Name != "parallel" {
			index := slices.Index(c.call.Args, "parallel")
			if index >= 0 {
				parallelOffset = index + 1
				parallelArgs = c.call.Args[parallelOffset:]
			}
		}
		if start, ok := parallelCommandStart(parallelArgs); ok {
			return parallelOffset + start + 1
		}
	}
	return len(c.words) - len(target)
}

func mainShellArgIndex(args []string) int {
	for i, arg := range args {
		if isShell(baseName(arg)) && i+1 < len(args) && shellCommandOption(args[i+1]) {
			return i
		}
	}
	return -1
}

// baselineWrapper и baselineOptionValue сохраняют совместимый с прежней версией разбор обёрток.
func baselineWrapper(name string) bool {
	switch name {
	case "timeout", "env", "sudo", "nohup", "nice", "ionice", "stdbuf", "time", "command", "builtin", "exec", "xargs":
		return true
	default:
		return false
	}
}

func baselineOptionValue(name, option string) bool {
	if baselineValueOptions[name][option] {
		return true
	}
	if len(option) < 3 || option[0] != '-' || option[1] == '-' {
		return false
	}
	for i := 1; i < len(option); i++ {
		if baselineValueOptions[name]["-"+option[i:i+1]] {
			return i == len(option)-1
		}
	}
	return false
}

var baselineValueOptions = map[string]map[string]bool{
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

func unwrapMain(name string, args []string) ([]string, []string, bool) {
	var via [maxNestedScripts]string
	count := 0
	for baselineWrapper(name) {
		if count >= maxNestedScripts {
			return nil, nil, false
		}
		via[count] = name
		count++
		if name == "command" && commandIsLookup(args) {
			return nil, nil, false
		}
		i := 0
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			if args[i] == "--" {
				i++
				break
			}
			if baselineOptionValue(name, args[i]) && i+1 < len(args) {
				i += 2
			} else {
				i++
			}
		}
		if name == "timeout" && i < len(args) && args[i] != "--" {
			i++
		}
		for name == "env" && i < len(args) && (strings.Contains(args[i], "=") || args[i] == "-i") {
			i++
		}
		if i >= len(args) {
			return nil, nil, false
		}
		name, args = baseName(args[i]), args[i+1:]
	}
	if name == "" {
		return nil, nil, false
	}
	return append([]string{name}, args...), slices.Clone(via[:count]), count > 0
}

func parallelChild(c cmd, wrapperVia []string) (cmd, bool) {
	parallelOffset := 0
	parallelArgs := c.call.Args
	if c.call.Name != "parallel" {
		index := slices.Index(c.call.Args, "parallel")
		if index < 0 {
			return cmd{}, false
		}
		parallelOffset = index + 1
		parallelArgs = c.call.Args[parallelOffset:]
	}
	start, ok := parallelCommandStart(parallelArgs)
	if !ok {
		return cmd{}, false
	}
	globalStart := parallelOffset + start
	commandWord := c.words[globalStart+1]
	args := make([]string, 0, len(parallelArgs)-start-1)
	words := []word{commandWord}
	for i := start + 1; i < len(parallelArgs); i++ {
		if parallelArgs[i] == ":::" || parallelArgs[i] == "::::" {
			continue
		}
		args = append(args, parallelArgs[i])
		words = append(words, c.words[parallelOffset+i+1])
	}
	child := cmd{call: Call{Name: commandName(parallelArgs[start]), Args: args, Raw: c.call.Raw, Line: c.call.Line,
		Via: c.via(wrapperVia...), Offset: commandWord.offset, Redirs: c.call.Redirs, Loop: c.call.Loop,
		Background: c.call.Background, ScriptInput: c.call.ScriptInput}, words: words, budget: c.budget, source: c.source,
		stdinInput: c.stdinInput, stdinConsumed: c.stdinConsumed}
	return child, true
}

func parallelCommandStart(args []string) (int, bool) {
	start := wrapperOptionEnd("parallel", args, 0)
	for i := start + 1; i < len(args); i++ {
		if args[i] == ":::" || args[i] == "::::" {
			return start, true
		}
	}
	return start, false
}

// expand раскрывает то, что вызов запускает, кроме программы под обёрткой (её раскрывает emit).
func expand(c cmd, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	name, args := c.call.Name, c.call.Args
	if target, ok := execTarget(args); ok {
		return emit(c.sub(len(c.words)-len(target), c.via(name, "exec")), lineBase, vars, calls, seen)
	}
	switch {
	case isShell(name):
		operand, command, _ := shellArgs(args)
		if command && operand < len(args) && operand+1 < len(c.words) && c.words[operand+1].script != "" {
			script := c.words[operand+1]
			first := len(*calls)
			err := parseNestedWithBudget(script.script, script.pos, c.call.Line-1, c.via(name, "-c"), c.call.Loop, c.call.Background, vars, calls, c.budget)
			relocateNestedLines(c, first, calls)
			return err
		}
		return nil
	case name == "ssh":
		return expandSSH(c, lineBase, vars, calls)
	case name == "find":
		return expandFind(c, lineBase, vars, calls, seen)
	case scriptInvocation(name, args):
		return expandScriptInvocation(c, vars, calls)

	}
	return nil
}

func expandSSH(c cmd, lineBase int, vars map[string]string, calls *[]Call) error {
	args := c.call.Args
	start, ok := sshCommand(args)
	host, _ := sshOptions(args, 0)
	if host >= len(args) {
		return nil
	}
	stdinDisabled := sshExecutionDisablesStdin(args, host, start)
	if !ok {
		if !c.stdinConsumed && !stdinDisabled {
			if input := c.stdinInput; input != nil {
				return expandSSHStdin(c, nil, input, lineBase, vars, calls)
			}
		}
		return nil
	}
	var remote mapped
	for i, w := range c.words[start+1:] {
		if w.script == "" {
			return nil
		}
		if i > 0 {
			remote.add(" ", -1)
		}
		remote.text = append(remote.text, w.script...)
		remote.pos = append(remote.pos, w.pos...)
	}
	remoteText := remote.String()
	file, full, parseErr := c.budget.parseShell(remoteText)
	if parseErr != nil {
		return &NestedError{Script: remoteText, Err: fmt.Errorf("shellcmd: parse shell: %w", parseErr)}
	}
	first := len(*calls)
	if err := parseNestedFileWithBudget(file, full, remoteText, remote.pos, c.call.Line-1, c.via("ssh"), c.call.Loop, c.call.Background, vars, calls, c.budget); err != nil {
		return err
	}
	relocateNestedLines(c, first, calls)
	if c.stdinConsumed || stdinDisabled {
		return nil
	}
	if input := c.stdinInput; input != nil {
		if candidate, ok := remoteStdinReader(c, file, remoteText, remote.pos, (*calls)[first:]); ok {
			return expandSSHStdin(c, &candidate, input, lineBase, vars, calls)
		}
	}
	return nil
}

// remoteStdinReader returns the first shell call of a remote script that reads
// the ssh command's stdin.
func remoteStdinReader(c cmd, file *syntax.File, src string, pos []int, remoteCalls []Call) (Call, bool) {
	var reader *remoteStdin
	if c.budget.trackWork {
		defer func() {
			if reader != nil {
				c.budget.stdinSteps += reader.steps()
			}
		}()
	}
	for _, call := range remoteCalls {
		if !isShell(call.Name) || !shellReadsHeredoc(call.Args) {
			continue
		}
		if reader == nil {
			reader = newRemoteStdin(file, src, pos)
		}
		if reader.reads(call.Offset) {
			return call, true
		}
	}
	return Call{}, false
}

type stdinValue struct {
	inherited bool
	redir     *syntax.Redirect
}

// remoteStdin answers, for calls of one remote ssh script, whether a shell
// call reads the ssh command's stdin. It walks the script once.
type remoteStdin struct {
	file  *syntax.File
	src   string
	plain bool // no `<` or `|`: every shell call inherits the ssh stdin
	// outers and inners pair each byte of src that has an outer offset with
	// that offset; when the outer offsets ascend, a lookup is a binary search.
	// Otherwise inner maps an outer offset to its first byte in src.
	outers, inners []int
	inner          map[int]int
	// unmapped is the first byte of src without an outer offset, -1 for none.
	unmapped int
	// pipes are the right-hand sides of pipelines, in outer offsets.
	pipes rangeIndex
	// xargs are xargs statements, in src offsets.
	xargs rangeIndex
	index *stdinIndex
}

func newRemoteStdin(file *syntax.File, src string, pos []int) *remoteStdin {
	remote := &remoteStdin{file: file, src: src, plain: !strings.ContainsAny(src, "<|"), unmapped: -1}
	remote.indexOffsets(pos)
	hasXargs := strings.Contains(src, "xargs")
	syntax.Walk(file, func(node syntax.Node) bool {
		remote.pipes.steps++
		switch node := node.(type) {
		case *syntax.BinaryCmd:
			remote.addPipe(node, pos)
		case *syntax.Stmt:
			if hasXargs {
				remote.addXargs(node)
			}
		}
		return true
	})
	if !remote.plain {
		remote.index = newStdinIndex(file)
	}
	return remote
}

func (remote *remoteStdin) addPipe(binary *syntax.BinaryCmd, pos []int) {
	op := binary.Op.String()
	if (op != "|" && op != "|&") || binary.Y == nil {
		return
	}
	start, end := int(binary.Y.Pos().Offset()), int(binary.Y.End().Offset())
	if start < 0 || end <= start || start >= len(remote.src) {
		return
	}
	if mappedStart, mappedEnd := outerSpan(start, min(end, len(remote.src)), pos); mappedStart >= 0 {
		remote.pipes.add(mappedStart, mappedEnd, 0)
	}
}

func (remote *remoteStdin) addXargs(stmt *syntax.Stmt) {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return
	}
	if name, literal := wordValue(call.Args[0]); literal && commandName(name) == "xargs" {
		remote.xargs.add(int(stmt.Cmd.Pos().Offset()), int(stmt.Cmd.End().Offset()), 0)
	}
}

// reads reports whether the shell call at outer offset reads the ssh stdin:
// it is not the reader of a pipeline or an xargs child, and no redirection
// gives its fd 0 another source.
func (remote *remoteStdin) reads(offset int) bool {
	inner, ok := remote.innerOffset(offset)
	if !ok || len(remote.pipes.containing(offset)) > 0 || len(remote.xargs.containing(inner)) > 0 {
		return false
	}
	if remote.plain {
		return true
	}
	return resolveStdinAt(remote.index, inner, stdinValue{inherited: true}).inherited
}

// indexOffsets fills the lookup from outer offsets to bytes of src: sorted
// pairs while the outer offsets ascend, a map of first bytes otherwise.
func (remote *remoteStdin) indexOffsets(pos []int) {
	for i, outer := range pos {
		if outer < 0 {
			if remote.unmapped < 0 {
				remote.unmapped = i
			}
			continue
		}
		if n := len(remote.outers); n > 0 && outer <= remote.outers[n-1] {
			remote.outers, remote.inners = nil, nil
			remote.inner = firstInnerOffsets(pos)
			return
		}
		remote.outers = append(remote.outers, outer)
		remote.inners = append(remote.inners, i)
	}
}

// firstInnerOffsets maps each outer offset in pos to its first index.
func firstInnerOffsets(pos []int) map[int]int {
	first := make(map[int]int, len(pos))
	for i, outer := range pos {
		if _, ok := first[outer]; !ok {
			first[outer] = i
		}
	}
	return first
}

// innerOffset returns the first byte of src at outer offset.
func (remote *remoteStdin) innerOffset(offset int) (int, bool) {
	if remote.inner != nil {
		inner, ok := remote.inner[offset]
		return inner, ok
	}
	if offset < 0 {
		return remote.unmapped, offset == -1 && remote.unmapped >= 0
	}
	i, found := slices.BinarySearch(remote.outers, offset)
	if !found {
		return 0, false
	}
	return remote.inners[i], true
}

func (remote *remoteStdin) steps() int {
	steps := remote.pipes.steps + remote.xargs.steps
	if remote.index != nil {
		steps += remote.index.steps()
	}
	return steps
}

// rangeIndex finds the ranges that hold an offset. Ranges taken from one
// syntax tree nest or are disjoint; then the ranges holding an offset are the
// last range starting at or before it and those of its ancestors that still
// hold it, since an earlier range holding the offset cannot end before that
// range starts. Ranges that overlap without nesting fall back to a full scan.
type rangeIndex struct {
	items  []rangeItem
	built  bool
	nested bool
	// steps counts ranges visited, for work-growth tests.
	steps int
}

type rangeItem struct {
	start, end int
	id         int
	// parent is the index of the nearest enclosing item, -1 for none.
	parent int
}

func (ranges *rangeIndex) add(start, end, id int) {
	ranges.items = append(ranges.items, rangeItem{start: start, end: end, id: id, parent: -1})
	ranges.built = false
}

func (ranges *rangeIndex) build() {
	ranges.built, ranges.nested = true, true
	slices.SortStableFunc(ranges.items, func(a, b rangeItem) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return b.end - a.end
	})
	var open []int
	for i := range ranges.items {
		item := &ranges.items[i]
		for len(open) > 0 && ranges.items[open[len(open)-1]].end <= item.start {
			open = open[:len(open)-1]
		}
		item.parent = -1
		if len(open) > 0 {
			item.parent = open[len(open)-1]
			if item.end > ranges.items[item.parent].end {
				ranges.nested = false
			}
		}
		open = append(open, i)
	}
	ranges.steps += len(ranges.items)
}

// containing returns the ids of the ranges holding offset, outermost first.
func (ranges *rangeIndex) containing(offset int) []int {
	if !ranges.built {
		ranges.build()
	}
	var found []rangeItem
	if !ranges.nested {
		for _, item := range ranges.items {
			ranges.steps++
			if item.start <= offset && offset < item.end {
				found = append(found, item)
			}
		}
		slices.SortStableFunc(found, func(a, b rangeItem) int { return (b.end - b.start) - (a.end - a.start) })
	} else {
		i, _ := slices.BinarySearchFunc(ranges.items, offset+1, func(item rangeItem, target int) int { return item.start - target })
		for i--; i >= 0; i = ranges.items[i].parent {
			ranges.steps++
			if offset < ranges.items[i].end {
				found = append(found, ranges.items[i])
			}
		}
		slices.Reverse(found)
	}
	ids := make([]int, len(found))
	for i, item := range found {
		ids[i] = item.id
	}
	return ids
}

// stdinIndex holds what resolving a call's stdin needs from one parsed file,
// gathered in a single walk so that each call is resolved without walking the
// file again.
type stdinIndex struct {
	funcs  []stdinFunc
	fnSpan rangeIndex
	redirs [][]*syntax.Redirect
	stmts  rangeIndex
	// calls maps a literal command name to the offsets of its calls, in walk order.
	calls map[string][]int
	// entry caches the stdin a function body starts with, by definition and initial value.
	entry     map[stdinEntryKey]stdinEntry
	walk      int
	callSteps int
}

type stdinFunc struct {
	name       string
	start, end int
}

// stdinEntryKey names one function definition: two definitions may share a name.
type stdinEntryKey struct {
	start     int
	inherited bool
}

type stdinEntry struct {
	invoked bool
	input   stdinValue
}

func newStdinIndex(file *syntax.File) *stdinIndex {
	index := &stdinIndex{calls: make(map[string][]int), entry: make(map[stdinEntryKey]stdinEntry)}
	syntax.Walk(file, func(node syntax.Node) bool {
		index.walk++
		switch node := node.(type) {
		case *syntax.FuncDecl:
			if node.Name != nil && node.Body != nil {
				start, end := int(node.Body.Pos().Offset()), int(node.Body.End().Offset())
				index.fnSpan.add(start, end, len(index.funcs))
				index.funcs = append(index.funcs, stdinFunc{node.Name.Value, start, end})
			}
		case *syntax.Stmt:
			index.addStmt(node)
		}
		return true
	})
	return index
}

func (index *stdinIndex) addStmt(stmt *syntax.Stmt) {
	if stmt.Cmd == nil {
		return
	}
	if len(stmt.Redirs) > 0 {
		index.stmts.add(int(stmt.Cmd.Pos().Offset()), int(stmt.Cmd.End().Offset()), len(index.redirs))
		index.redirs = append(index.redirs, stmt.Redirs)
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return
	}
	if name, literal := wordValue(call.Args[0]); literal {
		name = commandName(name)
		index.calls[name] = append(index.calls[name], int(call.Args[0].Pos().Offset()))
	}
}

func (index *stdinIndex) steps() int {
	return index.walk + index.fnSpan.steps + index.stmts.steps + index.callSteps
}

func resolveStdinAt(index *stdinIndex, offset int, initial stdinValue) stdinValue {
	if fn, ok := index.enclosingFunction(offset); ok {
		entry := index.functionEntry(fn, initial)
		if !entry.invoked {
			return stdinValue{}
		}
		initial = entry.input
	}
	return index.resolveContexts(offset, initial)
}

// functionEntry is the stdin a function body starts with: that of its last
// call outside its own body. Every call in the body shares it, so it is
// resolved once per function.
func (index *stdinIndex) functionEntry(fn stdinFunc, initial stdinValue) stdinEntry {
	key := stdinEntryKey{fn.start, initial.inherited}
	if entry, ok := index.entry[key]; ok && initial.redir == nil {
		return entry
	}
	// The last call in walk order outside the body wins; scanning back from
	// the end stops at it, so disjoint definitions sharing a name each look
	// only past the calls inside their own body.
	var entry stdinEntry
	calls := index.calls[fn.name]
	for i := len(calls) - 1; i >= 0; i-- {
		index.callSteps++
		if start := calls[i]; start < fn.start || start >= fn.end {
			entry = stdinEntry{invoked: true, input: index.resolveContexts(start, initial)}
			break
		}
	}
	if initial.redir == nil {
		index.entry[key] = entry
	}
	return entry
}

// enclosingFunction returns the innermost function whose body holds offset.
func (index *stdinIndex) enclosingFunction(offset int) (stdinFunc, bool) {
	ids := index.fnSpan.containing(offset)
	if len(ids) == 0 {
		return stdinFunc{}, false
	}
	return index.funcs[ids[len(ids)-1]], true
}

func (index *stdinIndex) resolveContexts(offset int, initial stdinValue) stdinValue {
	fds := map[string]stdinValue{"0": initial}
	for _, id := range index.stmts.containing(offset) {
		for _, redir := range index.redirs[id] {
			fd := "0"
			if redir.N != nil && redir.N.Value != "" {
				fd = redir.N.Value
			}
			switch redir.Op.String() {
			case "<<", "<<-", "<<<":
				fds[fd] = stdinValue{redir: redir}
			case "<", "<>":
				fds[fd] = stdinValue{}
			case "<&":
				target, ok := wordValue(redir.Word)
				if !ok || target == "-" {
					fds[fd] = stdinValue{}
				} else {
					fds[fd] = fds[target]
				}
			}
		}
	}
	return fds["0"]
}

func expandSSHStdin(c cmd, candidate *Call, input *syntax.Redirect, lineBase int, vars map[string]string, calls *[]Call) error {
	via := c.via("ssh", "bash")
	if candidate != nil {
		via = append(slices.Clone(candidate.Via), candidate.Name)
	}
	body, err := stdinRedirectBody(input, c.source, vars)
	if err != nil {
		return &NestedError{Script: nodeText(input.Hdoc, c.source), Err: err}
	}
	bodyLine := lineBase
	if input.Hdoc != nil {
		bodyLine = int(input.Hdoc.Pos().Line()) - 1
	} else if input.Word != nil {
		bodyLine = int(input.Word.Pos().Line()) - 1
	}
	first := len(*calls)
	if err := parseNestedWithBudget(body.String(), body.pos, bodyLine, via, c.call.Loop, c.call.Background, vars, calls, c.budget); err != nil {
		return err
	}
	dropSubstCalls(calls, first, body, c.budget)
	preserveMappedRaw(*calls, first, body, c.source)
	return nil
}

func stdinRedirectBody(input *syntax.Redirect, src string, vars map[string]string) (mapped, error) {
	switch {
	case input.Hdoc != nil && quotedHeredoc(input.Word):
		return heredocScript(input, src), nil
	case input.Hdoc != nil:
		// The parts of a heredoc word can reach past its body into the
		// delimiter line; the body ends where heredocText ends it.
		var body mapped
		limit := src[:int(input.Hdoc.Pos().Offset())+len(heredocText(input, src))]
		if err := body.expandParts(input.Hdoc.Parts, limit, vars, partHeredoc); err != nil {
			return mapped{}, err
		}
		if input.Op.String() == "<<-" {
			body = body.stripLeadingTabs()
		}
		return body, nil
	case input.Word != nil:
		var body mapped
		if err := body.expandParts(input.Word.Parts, src, vars, partUnquoted); err != nil {
			return mapped{}, err
		}
		body.pushSpan('\n', -1, -1, -1)
		return body, nil
	}
	return mapped{}, nil
}

// partContext is where a word part stands, which decides what a backslash escapes.
type partContext int

const (
	partUnquoted partContext = iota
	partDouble
	partHeredoc
)

// expandParts appends the text the outer shell passes on for parts of a
// heredoc body or here-string. Literal text is unescaped, and a plain $NAME or
// ${NAME} with a known literal value takes that value. Any other expansion —
// an unknown or special parameter, a command, process or arithmetic
// substitution — is kept as its source text: its value is not known here, and
// dropping it would hide an argument such as the target of kill. Substitutions
// the outer shell runs are recorded in subst, so that calls found again inside
// their kept text can be dropped.
func (m *mapped) expandParts(parts []syntax.WordPart, src string, vars map[string]string, context partContext) error {
	for _, part := range parts {
		start, end := int(part.Pos().Offset()), min(int(part.End().Offset()), len(src))
		if start >= end {
			continue
		}
		switch p := part.(type) {
		case *syntax.Lit:
			if context == partHeredoc {
				m.unescapeHeredoc(p, src)
			} else {
				raw, base := litSource(p, src, context == partDouble)
				m.unescapeSpans(raw, base, context == partDouble)
			}
		case *syntax.SglQuoted:
			if context != partUnquoted {
				m.keepSource(part, start, end, src)
				continue
			}
			if p.Dollar {
				// An unquoted $'…' in a here-string word: the outer shell
				// decodes its escapes before the inner shell reads it.
				if err := m.unescapeANSI(p.Value, ansiSourceBase(p, p.Value, src)); err != nil {
					return err
				}
				continue
			}
			m.addSpans(p.Value, start+1)
		case *syntax.DblQuoted:
			if err := m.expandParts(p.Parts, src, vars, partDouble); err != nil {
				return err
			}
		case *syntax.ParamExp:
			if value, ok := knownParam(p, vars); ok {
				for i := 0; i < len(value); i++ {
					m.pushSpan(value[i], -1, start, end)
				}
				continue
			}
			m.keepExpansion(part, start, end, src)
		default:
			m.keepExpansion(part, start, end, src)
		}
	}
	return nil
}

// keepExpansion keeps the source text of an expansion and records every
// command or process substitution the outer shell runs inside it, at any
// depth of ${…} or $((…)), but not those nested in another substitution: the
// outermost one is reported by the outer walk with all it contains. In a
// heredoc body and in double quotes the parser leaves <(…) as literal text,
// so only an unquoted here-string word holds process substitutions.
func (m *mapped) keepExpansion(part syntax.WordPart, start, end int, src string) {
	syntax.Walk(part, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			m.subst = append(m.subst, [2]int{int(node.Pos().Offset()), int(node.End().Offset())})
			return false
		}
		return true
	})
	m.keepSource(part, start, end, src)
}

// knownParam returns the value of a plain $NAME or ${NAME} whose value the
// script set to a literal.
func knownParam(p *syntax.ParamExp, vars map[string]string) (string, bool) {
	if p.Param == nil || p.Excl || p.Length || p.Width || p.Index != nil || p.Slice != nil || p.Repl != nil || p.Names != 0 || p.Exp != nil {
		return "", false
	}
	value, ok := vars[p.Param.Value]
	return value, ok
}

func (m *mapped) keepSource(part syntax.Node, start, end int, src string) {
	if !validSpan(src, start, end) {
		// No source span: keep the printed part, so the word is not lost.
		var printed strings.Builder
		if err := syntax.NewPrinter().Print(&printed, part); err == nil {
			for i := 0; i < printed.Len(); i++ {
				m.pushSpan(printed.String()[i], -1, -1, -1)
			}
		}
		return
	}
	for i := start; i < end; i++ {
		m.pushSpan(src[i], i, i, i+1)
	}
}

func (m *mapped) addSpans(text string, base int) {
	for i := 0; i < len(text); i++ {
		m.pushSpan(text[i], base+i, base+i, base+i+1)
	}
}

// unescapeSpans is unescape that also records the source span of each byte.
func (m *mapped) unescapeSpans(raw string, base int, double bool) {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) && (!double || strings.IndexByte("\"\\$`\n", raw[i+1]) >= 0) {
			i++
			if raw[i] == '\n' {
				continue
			}
		}
		m.pushOffset(raw[i], base, i)
	}
}

// unescapeHeredoc appends a literal of an unquoted heredoc body: a backslash
// escapes only $, `, \ and a newline. A literal can end just before the
// newline of a line continuation, its backslash being the last source byte.
func (m *mapped) unescapeHeredoc(lit *syntax.Lit, src string) {
	start, end := int(lit.Pos().Offset()), min(int(lit.End().Offset()), len(src))
	raw, base := lit.Value, -1
	if validSpan(src, start, end) {
		raw, base = src[start:end], start
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 == len(raw) && base >= 0 && end < len(src) && src[end] == '\n' {
			break
		}
		if raw[i] == '\\' && i+1 < len(raw) && strings.IndexByte("\\$`\n", raw[i+1]) >= 0 {
			i++
			if raw[i] == '\n' {
				continue
			}
		}
		m.pushOffset(raw[i], base, i)
	}
}

func (m *mapped) pushOffset(b byte, base, i int) {
	if base < 0 {
		m.pushSpan(b, -1, -1, -1)
		return
	}
	m.pushSpan(b, base+i, base+i, base+i+1)
}

func (m *mapped) pushSpan(b byte, pos, rawStart, rawEnd int) {
	m.text = append(m.text, b)
	m.pos = append(m.pos, pos)
	m.rawStart = append(m.rawStart, rawStart)
	m.rawEnd = append(m.rawEnd, rawEnd)
}

// stripLeadingTabs drops the tabs <<- removes: those at the start of a body
// line in the source, not ones a parameter value brings.
func (m mapped) stripLeadingTabs() mapped {
	var out mapped
	out.subst = m.subst
	lineStart := true
	for i, b := range m.text {
		if lineStart && b == '\t' && m.pos[i] >= 0 {
			continue
		}
		lineStart = b == '\n' && m.pos[i] >= 0
		out.pushSpan(b, m.pos[i], m.rawStart[i], m.rawEnd[i])
	}
	return out
}

// dropSubstCalls removes calls found inside the kept text of a substitution
// the outer shell runs: the outer walk reports them already. The spans come
// in source order and do not overlap, so each call is looked up by binary
// search.
func dropSubstCalls(calls *[]Call, first int, body mapped, budget *nestedParseBudget) {
	if len(body.subst) == 0 {
		return
	}
	kept := (*calls)[:first]
	for _, call := range (*calls)[first:] {
		i := sort.Search(len(body.subst), func(i int) bool {
			if budget != nil && budget.trackWork {
				budget.substSteps++
			}
			return body.subst[i][1] > call.Offset
		})
		if i == len(body.subst) || call.Offset < body.subst[i][0] {
			kept = append(kept, call)
		}
	}
	*calls = kept
}

func relocateNestedLines(parent cmd, first int, calls *[]Call) {
	base := parent.call.Offset
	var newlines []int
	for i := first; i < len(*calls); i++ {
		offset := (*calls)[i].Offset
		if offset < 0 {
			(*calls)[i].Line = parent.call.Line
			continue
		}
		if base < 0 {
			continue
		}
		start := offset - base
		if start < 0 || start >= len(parent.call.Raw) {
			continue
		}
		if newlines == nil {
			newlines = newlineOffsets(parent.call.Raw)
		}
		before, _ := slices.BinarySearch(newlines, start)
		(*calls)[i].Line = parent.call.Line + before
	}
}

// newlineOffsets returns the offsets of the newlines in text, in order.
func newlineOffsets(text string) []int {
	offsets := []int{}
	for i := range len(text) {
		if text[i] == '\n' {
			offsets = append(offsets, i)
		}
	}
	return offsets
}

func scriptInvocation(name string, args []string) bool {
	switch name {
	case "eval", "watch", "trap", "su", "runuser", "flock", "script", "tmux":
		return true
	}
	_, ok := envSplitArg(args)
	return name == "env" && ok
}

func expandScriptInvocation(c cmd, vars map[string]string, calls *[]Call) error {
	script, ok := invocationScript(c)
	if !ok {
		return nil
	}
	if len(script.text) > c.budget.expansionLimit-c.budget.expansionUsed {
		return &NestedError{Script: c.call.Raw, Err: fmt.Errorf("shellcmd: script expansion budget exceeds %d bytes", c.budget.expansionLimit)}
	}
	c.budget.expansionUsed += len(script.text)
	viaName := c.call.Name
	first := len(*calls)
	err := parseNestedWithBudget(script.String(), script.pos, c.call.Line-1, c.via(viaName), c.call.Loop, c.call.Background, vars, calls, c.budget)
	relocateNestedLines(c, first, calls)
	return err
}

type scriptArg struct {
	index  int
	offset int
}

func invocationScript(c cmd) (mapped, bool) {
	name, args := c.call.Name, c.call.Args
	switch name {
	case "eval":
		return mappedScriptWords(c, evalScriptArgs(args))
	case "watch":
		return mappedScriptWords(c, watchScriptArgs(args))
	case "trap":
		return mappedScriptWords(c, trapScriptArgs(args))
	case "su", "runuser", "flock", "script":
		arg, ok := commandStringArg(name, args)
		if ok {
			return mappedScriptWords(c, []scriptArg{arg})
		}
	case "env":
		return mappedScriptWords(c, envSplitScriptArgs(args))
	case "tmux":
		return mappedShellCommandWords(c, tmuxScriptArgs(args))
	}
	return mapped{}, false
}

func evalScriptArgs(args []string) []scriptArg {
	start := 0
	if len(args) > 0 && args[0] == "--" {
		start++
	}
	words := make([]scriptArg, 0, len(args)-start)
	for i := start; i < len(args); i++ {
		words = append(words, scriptArg{index: i})
	}
	return words
}

func watchScriptArgs(args []string) []scriptArg {
	start := wrapperStart("watch", args)
	if start >= len(args) {
		return nil
	}
	end := len(args)
	words := make([]scriptArg, 0, end-start)
	for i := start; i < end; i++ {
		words = append(words, scriptArg{index: i})
	}
	return words
}

func trapScriptArgs(args []string) []scriptArg {
	start := 0
	if len(args) > 0 && args[0] == "--" {
		start++
	}
	if start >= len(args) || args[start] == "-p" || args[start] == "-" {
		return nil
	}
	return []scriptArg{{index: start}}
}

func mappedScriptWords(c cmd, words []scriptArg) (mapped, bool) {
	if len(words) == 0 {
		return mapped{}, false
	}
	var script mapped
	for i, arg := range words {
		if arg.index < 0 || arg.index+1 >= len(c.words) {
			return mapped{}, false
		}
		w := c.words[arg.index+1]
		if w.script == "" || arg.offset > len(w.script) {
			return mapped{}, false
		}
		if i > 0 {
			script.add(" ", -1)
		}
		script.text = append(script.text, w.script[arg.offset:]...)
		script.pos = append(script.pos, w.pos[arg.offset:]...)
	}
	return script, true
}

func mappedShellCommandWords(c cmd, words []scriptArg) (mapped, bool) {
	if len(words) == 0 {
		return mapped{}, false
	}
	quoteScript := -1
	for i := 0; i+2 < len(words); i++ {
		command := c.words[words[i].index+1].script
		option := c.words[words[i+1].index+1].script
		if isShell(baseName(command)) && shellCommandOption(option) {
			quoteScript = i + 2
			break
		}
	}
	var script mapped
	for i, arg := range words {
		if arg.index < 0 || arg.index+1 >= len(c.words) {
			return mapped{}, false
		}
		w := c.words[arg.index+1]
		if w.script == "" || arg.offset > len(w.script) {
			return mapped{}, false
		}
		if i > 0 {
			script.add(" ", -1)
		}
		if i != quoteScript {
			script.add(w.script[arg.offset:], -1)
			continue
		}
		script.add("'", -1)
		for j := arg.offset; j < len(w.script); j++ {
			pos := -1
			if j < len(w.pos) {
				pos = w.pos[j]
			}
			if w.script[j] == '\'' {
				script.add("'", -1)
				script.add(`\'`, pos)
				script.add("'", -1)
			} else {
				script.add(w.script[j:j+1], pos)
			}
		}
		script.add("'", -1)
	}
	return script, true
}

func commandStringArg(name string, args []string) (scriptArg, bool) {
	if name == "script" {
		return scriptCommandArg(args)
	}
	if name == "flock" {
		lock := wrapperOptionEnd(name, args, 0)
		if lock >= len(args) {
			return scriptArg{}, false
		}
		return commandOptionAt(name, args, lock+1)
	}
	if name == "su" || name == "runuser" {
		userSeen := false
		for i := 0; i < len(args); {
			if arg, ok := commandOptionAt(name, args, i); ok {
				return arg, true
			}
			if args[i] == "-" {
				i++
				continue
			}
			if args[i] == "--" {
				return scriptArg{}, false
			}
			if strings.HasPrefix(args[i], "-") {
				next := optionNext(name, args, i)
				if next == i {
					i++
				} else {
					i = next
				}
				continue
			}
			if userSeen {
				return scriptArg{}, false
			}
			userSeen = true
			i++ // su допускает имя пользователя перед ключами оболочки.
		}
		return scriptArg{}, false
	}
	for i := 0; i < len(args); {
		if arg, ok := commandOptionAt(name, args, i); ok {
			return arg, true
		}
		if args[i] == "--" {
			return scriptArg{}, false
		}
		if !strings.HasPrefix(args[i], "-") {
			return scriptArg{}, false
		}
		next := optionNext(name, args, i)
		if next == i {
			i++
		} else {
			i = next
		}
	}
	return scriptArg{}, false
}

func scriptCommandArg(args []string) (scriptArg, bool) {
	fileSeen := false
	for i := 0; i < len(args); {
		if arg, ok := commandOptionAt("script", args, i); ok {
			return arg, true
		}
		if args[i] == "--" {
			return scriptArg{}, false
		}
		if strings.HasPrefix(args[i], "-") {
			next := optionNext("script", args, i)
			if next == i {
				i++
			} else {
				i = next
			}
			continue
		}
		if fileSeen {
			return scriptArg{}, false
		}
		fileSeen = true
		i++
	}
	return scriptArg{}, false
}

func commandOptionAt(name string, args []string, i int) (scriptArg, bool) {
	if i >= len(args) {
		return scriptArg{}, false
	}
	arg := args[i]
	if strings.HasPrefix(arg, "--command=") {
		return scriptArg{index: i, offset: len("--command=")}, true
	}
	if strings.HasPrefix(arg, "--session-command=") {
		return scriptArg{index: i, offset: len("--session-command=")}, true
	}
	if arg == "-c" || arg == "--command" || arg == "--session-command" {
		if i+1 < len(args) {
			return scriptArg{index: i + 1}, true
		}
		return scriptArg{}, false
	}
	if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") {
		for j := 1; j < len(arg); j++ {
			option, takesValue := shortOptionAt(name, arg, j)
			if option == "-c" {
				if j+1 < len(arg) {
					return scriptArg{index: i, offset: j + 1}, true
				}
				if i+1 < len(args) {
					return scriptArg{index: i + 1}, true
				}
				return scriptArg{}, false
			}
			if takesValue {
				return scriptArg{}, false
			}
		}
	}
	return scriptArg{}, false
}

func envSplitScriptArgs(args []string) []scriptArg {
	arg, ok := envSplitArg(args)
	if !ok {
		return nil
	}
	words := []scriptArg{arg}
	for i := arg.index + 1; i < len(args); i++ {
		words = append(words, scriptArg{index: i})
	}
	return words
}

func envSplitArg(args []string) (scriptArg, bool) {
	for i := 0; i < len(args); {
		arg := args[i]
		if after, ok := strings.CutPrefix(arg, "--split-string="); ok {
			return scriptArg{index: i, offset: len(arg) - len(after)}, true
		}
		if arg == "-S" || arg == "--split-string" {
			if i+1 < len(args) {
				return scriptArg{index: i + 1}, true
			}
			return scriptArg{}, false
		}
		if script, ok := envShortSplitArg(arg, i, args); ok {
			return script, true
		}
		if arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-") {
			return scriptArg{}, false
		}
		i = optionNext("env", args, i)
	}
	return scriptArg{}, false
}

func envShortSplitArg(arg string, index int, args []string) (scriptArg, bool) {
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return scriptArg{}, false
	}
	for j := 1; j < len(arg); j++ {
		option, takesValue := shortOptionAt("env", arg, j)
		if option == "-S" {
			if j+1 < len(arg) {
				return scriptArg{index: index, offset: j + 1}, true
			}
			if index+1 < len(args) {
				return scriptArg{index: index + 1}, true
			}
			return scriptArg{}, false
		}
		if takesValue {
			break
		}
	}
	return scriptArg{}, false
}

func tmuxCommandStart(args []string) int {
	for i := 0; i < len(args); {
		arg := args[i]
		if arg == "new-session" || arg == "new" || arg == "new-window" || arg == "neww" || arg == "split-window" || arg == "splitw" {
			return wrapperOptionEnd("tmux", args, i+1)
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			next := wrapperOptionEnd("tmux", args, i)
			if next == i {
				i++
			} else {
				i = next
			}
			continue
		}
		i++
	}
	return len(args)
}

func tmuxScriptArgs(args []string) []scriptArg {
	start := tmuxCommandStart(args)
	if start >= len(args) {
		return nil
	}
	words := make([]scriptArg, 0, len(args)-start)
	for i := start; i < len(args); i++ {
		words = append(words, scriptArg{index: i})
	}
	return words
}

func wrapperOptionEnd(name string, args []string, start int) int {
	i := start
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" {
		i = optionNext(name, args, i)
	}
	return i
}

func optionNext(name string, args []string, i int) int {
	if i >= len(args) {
		return i
	}
	arg := args[i]
	if arg == "--" {
		return i + 1
	}
	if !strings.HasPrefix(arg, "-") || arg == "-" {
		return i
	}
	if strings.HasPrefix(arg, "--") {
		key, _, hasValue := strings.Cut(arg, "=")
		if !hasValue && valueOptions[name][key] && i+1 < len(args) {
			return i + 2
		}
		return i + 1
	}
	for j := 1; j < len(arg); j++ {
		_, takesValue := shortOptionAt(name, arg, j)
		if takesValue {
			if j == len(arg)-1 && i+1 < len(args) {
				return i + 2
			}
			return i + 1
		}
	}
	return i + 1
}

func shortOptionAt(name, arg string, offset int) (string, bool) {
	if offset <= 0 || offset >= len(arg) {
		return "", false
	}
	option := "-" + arg[offset:offset+1]
	return option, valueOptions[name][option]
}

func expandFind(c cmd, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	args := c.call.Args
	var errs []error
	for i := 0; i < len(args); i++ {
		if args[i] != "-exec" && args[i] != "-execdir" && args[i] != "-ok" && args[i] != "-okdir" {
			continue
		}
		start := i + 1
		end := start
		for end < len(args) && args[end] != ";" && args[end] != "+" {
			end++
		}
		if end == start {
			continue
		}
		cmd := c.sub(start+1, c.via("find", args[i]))
		cmd.call.Args = slices.Clone(args[start+1 : end])
		cmd.words = cmd.words[:len(cmd.call.Args)+1]
		if err := emit(cmd, lineBase, vars, calls, seen); err != nil {
			errs = append(errs, err)
		}
		i = end
	}
	return errors.Join(errs...)
}

// dataArgs — программы, чьи аргументы — данные: `echo bash -c x` ничего не запускает.
var dataArgs = set("echo", "printf")

// sshCommand — индекс в args первого слова удалённой команды ssh; false — команды нет. OpenSSH разбирает
// опции и после хоста, если до хоста не было `--`.
func sshCommand(args []string) (int, bool) {
	i, ended := sshOptions(args, 0)
	i++
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

// wrapperExecScan indexes exec targets for successive suffixes of one argument list.
// work counts indexed arguments and lookups for growth tests.
type wrapperExecScan struct {
	args    []string
	targets []int
	work    *int
}

func (s *wrapperExecScan) find(args []string) ([]string, bool) {
	if len(args) == 0 {
		return nil, false
	}
	offset := len(s.args) - len(args)
	if offset < 0 || len(s.args) == 0 || &args[0] != &s.args[offset] {
		s.args = args
		s.targets = make([]int, len(args))
		nextDash, target := -1, -1
		for i := len(args) - 1; i >= 0; i-- {
			if s.work != nil {
				*s.work++
			}
			if args[i] == "--" && i+1 < len(args) {
				nextDash = i
			}
			if args[i] == "exec" && nextDash >= 0 {
				target = nextDash + 1
			}
			s.targets[i] = target
		}
		offset = 0
	}
	if s.work != nil {
		*s.work++
	}
	target := s.targets[offset]
	if target < 0 {
		return nil, false
	}
	return s.args[target:], true
}

func execTarget(args []string) ([]string, bool) {
	return execTargetWithWork(args, nil)
}

func execTargetWithWork(args []string, work *int) ([]string, bool) {
	seen := false
	for i, arg := range args {
		if work != nil {
			*work++
		}
		if arg == "exec" {
			seen = true
		}
		if arg == "--" && seen && i+1 < len(args) {
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
		if !wrapperOptionsKnown(name, args) {
			return nil, via, false
		}
		if wrapperRunsScript(name, args) {
			if len(via) > 0 {
				return append([]string{name}, args...), via, true
			}
			return nil, via, false
		}
		via = append(via, name)
		target, ok := unwrapOnce(name, args)
		if !ok {
			return nil, via, false
		}
		name, args = baseName(target[0]), target[1:]
	}
	if name == "" {
		return nil, via, false
	}
	return append([]string{name}, args...), via, len(via) > 0
}

func suReadsShell(name string, args []string) bool {
	if _, ok := commandStringArg(name, args); ok {
		return false
	}
	if name == "runuser" && hasOption(name, args, "-u", "--user") {
		return false
	}
	userSeen := false
	for i := 0; i < len(args); {
		if args[i] == "-" {
			i++
			continue
		}
		if args[i] == "--" {
			return false
		}
		if strings.HasPrefix(args[i], "-") {
			next := optionNext(name, args, i)
			if next == i {
				i++
			} else {
				i = next
			}
			continue
		}
		if userSeen {
			return false
		}
		userSeen = true
		i++
	}
	return true
}

func wrapperRunsScript(name string, args []string) bool {
	switch name {
	case "env":
		_, ok := envSplitArg(args)
		return ok
	case "su", "runuser":
		if _, ok := commandStringArg(name, args); ok {
			return true
		}
		return suReadsShell(name, args)
	case "flock":
		_, ok := commandStringArg(name, args)
		return ok
	}
	return false
}

func unwrapOnce(name string, args []string) ([]string, bool) {
	if name == "parallel" {
		start, ok := parallelCommandStart(args)
		if !ok {
			return nil, false
		}
		target := make([]string, 0, len(args)-start)
		for _, arg := range args[start:] {
			if arg != ":::" && arg != "::::" {
				target = append(target, arg)
			}
		}
		return target, true
	}
	if name == "command" && commandIsLookup(args) {
		return nil, false
	}
	i := wrapperStart(name, args)
	if name == "timeout" && i < len(args) && args[i] != "--" {
		i++
	}
	for name == "env" && i < len(args) && (strings.Contains(args[i], "=") || args[i] == "-i" || args[i] == "-" && i+1 < len(args)) {
		i++
	}
	if i >= len(args) {
		return nil, false
	}
	return args[i:], true
}

func wrapperStart(name string, args []string) int {
	i := wrapperOptionEnd(name, args, 0)
	switch name {
	case "taskset":
		if hasOption(name, args, "-p", "--pid") {
			return len(args)
		}
		if !hasOption(name, args, "-c", "--cpu-list") && i < len(args) {
			i++
		}
	case "chrt", "flock":
		if i < len(args) && args[i] != "--" {
			i++
		}
	}
	return i
}

func hasOption(name string, args []string, options ...string) bool {
	for i := 0; i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-"; {
		for _, option := range options {
			if optionMatches(name, args[i], option) {
				return true
			}
		}
		next := optionNext(name, args, i)
		if next <= i {
			i++
		} else {
			i = next
		}
	}
	return false
}

func optionMatches(name, arg, option string) bool {
	if arg == option || strings.HasPrefix(option, "--") && strings.HasPrefix(arg, option+"=") {
		return true
	}
	if len(option) != 2 || option[0] != '-' || option[1] == '-' {
		return false
	}
	for j := 1; j < len(arg); j++ {
		key, takesValue := shortOptionAt(name, arg, j)
		if key == option {
			return true
		}
		if takesValue {
			return false
		}
	}
	return false
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
var wrappers = set("timeout", "env", "sudo", "nohup", "nice", "ionice", "stdbuf", "time", "command", "builtin", "exec", "xargs", "taskset", "chrt", "strace", "setpriv", "unshare", "flock", "busybox", "systemd-run", "parallel", "runuser", "doas", "su")

// valueOptions — опции обёрток и ssh, следующее слово после которых — их значение, а не программа.
var valueOptions = map[string]map[string]bool{
	"timeout":     set("-s", "--signal", "-k", "--kill-after"),
	"env":         set("-u", "--unset", "-C", "--chdir", "-S", "--split-string", "-a", "--argv0"),
	"sudo":        set("-u", "--user", "-g", "--group", "-C", "--close-from", "-D", "--chdir", "-h", "--host", "-p", "--prompt", "-r", "--role", "-t", "--type", "-T", "--command-timeout", "-U", "--other-user", "-R", "--chroot"),
	"nice":        set("-n", "--adjustment"),
	"ionice":      set("-c", "--class", "-n", "--classdata", "-p", "--pid", "-P", "--pgid", "-u", "--uid"),
	"stdbuf":      set("-i", "--input", "-o", "--output", "-e", "--error"),
	"time":        set("-f", "--format", "-o", "--output"),
	"exec":        set("-a"),
	"xargs":       set("-I", "-n", "--max-args", "-P", "--max-procs", "-L", "--max-lines", "-d", "--delimiter", "-E", "-s", "--max-chars", "-a", "--arg-file"),
	"ssh":         set("-B", "-b", "-c", "-D", "-E", "-e", "-F", "-I", "-i", "-J", "-L", "-l", "-m", "-O", "-o", "-P", "-p", "-Q", "-R", "-S", "-W", "-w"),
	"taskset":     set("-c", "--cpu-list", "-p", "--pid"),
	"chrt":        set("-p", "--pid", "-P", "-T", "-D", "--sched-period", "--sched-runtime", "--sched-deadline"),
	"strace":      set("-o", "--output", "-e", "-p", "-u", "-s", "-O", "-I", "-E", "-P", "-a", "-S", "-X", "-b", "-U", "--argv0", "--trace", "--trace-fds", "--trace-path", "--abbrev", "--verbose", "--qualify", "--signal", "--status", "--inject", "--fault", "--syscall-limit", "--read", "--write", "--user", "--string-limit", "--env", "--columns", "--const-print-style"),
	"setpriv":     set("--reuid", "--ruid", "--euid", "--regid", "--rgid", "--egid", "--groups", "--inh-caps", "--ambient-caps", "--bounding-set", "--securebits", "--apparmor-profile", "--selinux-label", "--pdeathsig", "--ptracer", "--landlock-access", "--landlock-rule", "--landlock-restrict", "--seccomp-filter"),
	"unshare":     set("-S", "-G", "-R", "-w", "-l", "--setuid", "--setgid", "--root", "--wd", "--map-user", "--map-group", "--map-users", "--map-groups", "--setgroups", "--monotonic", "--boottime", "--propagation"),
	"flock":       set("-E", "--conflict-exit-code", "-w", "--wait", "--timeout"),
	"systemd-run": set("--unit", "--description", "--slice", "--property", "-p", "--uid", "--gid", "--working-directory", "--service-type", "-E", "--setenv", "-u", "-M", "-H", "--machine", "--on-active", "--on-boot", "--on-startup", "--on-unit-active", "--on-unit-inactive", "--on-calendar", "--timer-property", "--nice", "--root-directory", "--json", "--job-mode", "--path-property", "--socket-property"),
	"runuser":     set("-u", "--user", "-g", "--group", "-G", "--supp-group", "-c", "--command", "-s", "--shell", "-w", "--whitelist-environment", "--groups"),
	"su":          set("-c", "--command", "--session-command", "-s", "--shell", "-g", "--group", "-G", "--supp-group", "-w", "--whitelist-environment"),
	"doas":        set("-u", "--user", "-a", "--auth-type", "-C", "--config"),
	"watch":       set("-n", "--interval", "-q", "--equexit"),
	"script":      set("-c", "--command", "-E", "--echo", "-I", "--log-in", "-O", "--log-out", "-T", "--log-timing", "-B", "--log-io", "-m", "--logging-format", "-o", "--output-limit"),
	"parallel":    set("-j", "--jobs", "-n", "--max-args", "-P", "--max-procs", "-L", "--max-lines", "-d", "--delimiter", "-S", "--sshlogin", "-a", "--arg-file", "--sshloginfile", "--joblog", "--results", "--workdir", "--env", "--block", "--recstart", "--recend", "--regexp", "--tagstring", "--halt", "--timeout", "--delay", "--memfree", "--load", "--nice", "--filter", "--return", "--transferfile", "--cleanup", "--trc", "--basefile", "--record-env", "-C", "--colsep", "-I", "-E", "--retries", "--tmpdir", "--header", "--termseq", "--sshdelay", "--group-by", "--compress-program", "--limit", "--memsuspend"),
	"tmux":        set("-L", "-S", "-f", "-c", "-t", "-s", "-n", "-x", "-y", "-e", "-F", "-B", "-l", "-m", "-R", "-T", "-p"),
}

var flagOptions = map[string]map[string]bool{
	"env":         set("-i", "--ignore-environment", "-0", "--null", "-v", "--debug"),
	"sudo":        set("-A", "--askpass", "-B", "--bell", "-P", "--preserve-groups", "-n", "--non-interactive", "-E", "--preserve-env", "-H", "--set-home", "-S", "--stdin", "-b", "--background", "-k", "--reset-timestamp", "-K", "--remove-timestamp", "-l", "--list", "-V", "--version", "-s", "--shell", "-i", "--login", "-e", "--edit", "-q", "--quiet"),
	"timeout":     set("--foreground", "--preserve-status", "-v", "--verbose"),
	"taskset":     set("-a", "--all-tasks"),
	"chrt":        set("-o", "--other", "-f", "--fifo", "-r", "--rr", "-b", "--batch", "-i", "--idle", "-d", "--deadline", "-R", "--reset-on-fork", "-a", "--all-tasks"),
	"strace":      set("-f", "-ff", "-c", "-q", "-qq", "-T", "-tt", "-y", "-yy", "-D", "-d", "-k", "-r", "-t", "-v", "-x", "-xx", "-z", "-Z", "--follow-forks", "--daemonize", "--summary-only", "--quiet", "--absolute-timestamps", "--decode-fds", "--secontext", "--kill-on-exit", "--output-separately", "--seccomp-bpf"),
	"unshare":     set("-r", "--map-root-user", "-U", "--user", "-f", "--fork", "-m", "--mount", "-p", "--pid", "-n", "--net", "-i", "--ipc", "-u", "--uts", "-C", "--cgroup", "--mount-proc", "--kill-child", "--time", "--cgroup-namespace"),
	"setpriv":     set("--init-groups", "--clear-groups", "--no-new-privs", "--keep-caps", "--dump", "--list-caps"),
	"watch":       set("-d", "--differences", "-t", "--no-title", "-c", "--color", "-g", "--chgexit", "-e", "--errexit", "-b", "--beep", "-p", "--precise", "-x", "--exec"),
	"parallel":    set("-k", "--keep-order", "--tag", "-q", "--quote", "--ungroup", "--group", "--line-buffer", "--tagstring"),
	"flock":       set("-x", "--exclusive", "-s", "--shared", "-u", "--unlock", "-n", "--nonblock", "-o", "--close", "-F", "--no-fork"),
	"systemd-run": set("-q", "--quiet", "-t", "--pty", "-P", "--pipe", "-G", "--collect", "-d", "--remain-after-exit", "--user", "--scope", "--wait", "--system", "--same-dir", "--send-sighup", "--background", "--expand-environment", "--on-clock-change", "--on-timezone-change"),
	"doas":        set("-n", "--non-interactive", "-s", "-L", "--"),
	"ionice":      set("-t", "--ignore"),
	"xargs":       set("-i", "-r", "--no-run-if-empty", "-t", "--verbose", "-x", "--exit", "-0", "--null", "-p", "--interactive", "-o", "--open-tty"),
	"script":      set("-q", "--quiet", "-f", "--flush", "-V", "--version"),
	"command":     set("-p"),
	"tmux":        set("-2", "-8", "-C", "-D", "-N", "-V", "-v", "-q", "-u"),
	"su":          set("-l", "--login", "-m", "--preserve-environment", "-p", "--preserve-environment", "-f", "--fast"),
	"runuser":     set("-l", "--login", "-m", "--preserve-environment", "-p", "--preserve-environment", "--pty", "--close-session", "--reset-environment"),
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
		_, takesValue := shortOptionAt(wrapper, option, j)
		if takesValue {
			return j == len(option)-1
		}
	}
	return false
}

// ShadowedByFunction сообщает, объявлена ли в cmd до вызова функция оболочки с именем программы
// вызова: тогда вызов идёт в функцию, а не в программу. Ошибка — cmd не разбирается.
func (c Call) ShadowedByFunction(cmd string) (bool, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return false, fmt.Errorf("parse shell functions: %w", err)
	}
	shadowed := false
	syntax.Walk(file, func(n syntax.Node) bool {
		if fn, ok := n.(*syntax.FuncDecl); ok && fn.Name.Value == c.Name && int(fn.Pos().Offset()) < c.Offset {
			shadowed = true
		}
		return !shadowed
	})
	return shadowed, nil
}
