// Package shellcmd находит простые команды в исходном тексте оболочки.
package shellcmd

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"

	"strconv"
	"strings"
	"unicode/utf8"

	shExpand "mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

var errUnknownWrapperSignal = errors.New("shellcmd: unknown wrapper option may hide a signal")
var errUncertainWrapperBudget = errors.New("shellcmd: uncertain wrapper search exceeds budget")
var errCommandExpansionDepth = errors.New("shellcmd: command expansion depth exceeded")

// bashBuiltinAndKeywordNames is from GNU Bash 5.3.9:
// `bash --version`, `compgen -b`, and `compgen -k`.
const bashBuiltinAndKeywordNames = `
.
:
[
alias
bg
bind
break
builtin
caller
cd
command
compgen
complete
compopt
continue
declare
dirs
disown
echo
enable
eval
exec
exit
export
false
fc
fg
getopts
hash
help
history
jobs
kill
let
local
logout
mapfile
popd
printf
pushd
pwd
read
readarray
readonly
return
set
shift
shopt
source
suspend
test
times
trap
true
type
typeset
ulimit
umask
unalias
unset
wait
!
[[
]]
case
coproc
do
done
elif
else
esac
fi
for
function
if
in
select
then
time
until
while
{
}
`

var bashBuiltinAndKeywordSet = literalWordSet(bashBuiltinAndKeywordNames)

// test и [ с -v вычисляют индекс массива в имени переменной (подстановка, присваивание);
// wait -p присваивает переменную — они вне списка.
var literalBuiltinAllowSet = literalWordSet(`kill echo true false : pwd exit`)

func literalWordSet(words string) map[string]struct{} {
	set := make(map[string]struct{})
	for word := range strings.FieldsSeq(words) {
		set[word] = struct{}{}
	}
	return set
}

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
	Redirs      []Redir
	literalArgs uint32
	tildeArgs   []Tilde
	// Loop — вызов в теле цикла while, until, for или select (и во вложенном скрипте такого тела).
	Loop bool
	// Background — вызов в задании, чей оператор или объемлющий оператор заканчивается `&`.
	Background bool
	// ScriptInput сообщает, что оболочка исполняет неизвестный текст из stdin или process substitution.
	ScriptInput bool
}

// Tilde describes Bash tilde expansion in a word. Bash expands an unquoted
// tilde-prefix at the start of a word, or after an unquoted colon in assignment
// values, to $HOME (`~`, `~/path`) or another directory (`~user`, `~+`, `~-`).
// A colon ends a tilde-prefix. In Bash mode, an argument that is a valid
// `name=value` assignment word, including `name+=value` and indexed names,
// follows assignment-value rules; Bash POSIX mode disables this for argument
// words. TildeOther is a syntactic mark: Bash can leave `~user`, `~+N`, `~-N`,
// or `~N` unchanged when the user or directory-stack entry does not exist.
// TildeModeUnknown means braceexpand or POSIX mode may have changed in a way the
// analysis cannot resolve. The mode is followed only along commands that run in
// order; a change inside a loop, a trap action, a function called in more than
// one mode or a deferred eval text makes the words it may reach unknown, and so
// does an eval text nested more than eight levels deep. When that bit is set,
// Home and Other describe the shell's default mode.
type Tilde uint8

var declarationCommands = [...]string{"export", "readonly", "declare", "typeset", "local"}

const (
	// TildeHome marks expansion of `~`, `~/path`, or the prefix before `:` to $HOME.
	TildeHome Tilde = 1
	// TildeOther marks a `~user`, `~+`, `~-`, or `~N` prefix. It is syntactic:
	// Bash can leave it unchanged when the user is missing or the directory stack
	// has no such entry.
	TildeOther Tilde = 2
	// TildeModeUnknown marks a word whose result can change with runtime shell options.
	TildeModeUnknown Tilde = 4
)

// ArgTilde reports Bash tilde expansion in Args[i], including nested script words.
// Out-of-range indices return zero.
func (c Call) ArgTilde(i int) Tilde {
	if i < 0 || i >= len(c.Args) {
		return 0
	}
	if i >= len(c.tildeArgs) {
		return 0
	}
	return c.tildeArgs[i]
}

// ArgLiteral сообщает, что Args[i] — литеральное слово в простом скрипте: все его
// предложения — простые вызовы без присваиваний, перенаправлений, списков, составных команд
// и нелитеральных слов. Имя вызова должно быть внешней командой либо builtin из списка
// kill echo true false : pwd exit. Список builtin и ключевых слов взят из
// GNU Bash 5.3.9 (`bash --version`, `compgen -b`, `compgen -k`). Вложенный скрипт разрешён
// только как `bash/sh -c '…'` с литеральными дополнительными словами, один уровень.
// `!` разрешён только в одинарных кавычках или как `\!` вне кавычек; i должен быть [0, 32).
// Управляющий байт в тексте скрипта, кроме перевода строки и табуляции, даёт false для всего
// скрипта: парсер и bash по-разному читают `\r` (bash считает его литерой слова) и NUL. Так же
// действует обратная косая перед переводом строки: в конце комментария bash её не продолжает.
// Всё прочее даёт false. Не учитываются изменения извне: окружение (BASH_ENV, PATH,
// экспортированные функции), rc-файлы и содержимое source-файлов.
func (c Call) ArgLiteral(i int) bool {
	return i >= 0 && i < 32 && c.literalArgs&(uint32(1)<<uint(i)) != 0
}

// cmd — вызов вместе со словами его командной строки: words[0] — argv0, дальше — Args по порядку.
type cmd struct {
	call           Call
	posixEnv       bool
	mode           shellModes
	argv0          string
	literalContext bool
	words          []word
	budget         *nestedParseBudget
	source         string
	expansionDepth int

	stdinInput    *syntax.Redirect
	stdinConsumed bool
}

// word — место слова в исходной команде и его текст как скрипта.
type word struct {
	singleQuoted bool
	tilde        Tilde
	raw          string
	sourceText   string // исходный текст слова, включая кавычки
	name         string // unquotedText, используемый для имени команды
	ansiQuoted   bool
	offset       int    // смещение в исходной команде; -1 — слово не литерал
	script       string // текст, который выполнит оболочка, получив слово скриптом; "" — не виден до запуска
	pos          []int  // pos[i] — смещение байта i текста script в исходной команде; -1 — такого байта нет
	subst        [][2]int
}

// Redir — перенаправление: `> f`, `2>>log`, `<<EOF` с телом.
type Redir struct {
	Op     string // оператор: >, >>, <, <<, <<-, <<<, &>, >& …
	N      string // номер дескриптора перед оператором ("2" в 2>), пусто — по умолчанию
	Target string // цель: значение литерального слова, иначе его исходный текст; у heredoc — ограничитель
	// Tilde reports Bash tilde expansion in the redirection word. Here-document
	// delimiters are not expanded; here-strings also recognize prefixes after `:`.
	Tilde Tilde
	Body  string // тело heredoc как в исходном тексте
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
	calls := make([]Call, 0)
	for i := range all {
		if wanted[all[i].Name] {
			calls = append(calls, all[i])
		}
	}
	var nested *NestedError
	if err != nil && !errors.As(err, &nested) {
		return nil, err
	}
	return calls, err
}

type nestedParseBudget struct {
	used                int
	depth               int
	expansionUsed       int
	expansionLimit      int
	limit               int
	parsedBytes         int
	visitedNodes        int
	walkInfoNodes       int
	stdinSteps          int
	substSteps          int
	uncertainWork       int
	trackWork           bool
	literalShell        string
	initialPosix        bool
	initialBrace        bool
	initialPosixUnknown bool
	initialBraceUnknown bool
	parsedShells        map[string]int
	// modeTexts are the eval and trap texts the mode walks of all scripts read.
	modeTexts *modeTexts
}

func newNestedParseBudget(inputSize int) *nestedParseBudget {
	limit := max(inputSize*(maxNestedScripts+1), 4096)
	return &nestedParseBudget{limit: limit, expansionLimit: max(inputSize*2, 4096), initialBrace: true}
}

func parse(src string, lineBase int, inherited []string, vars map[string]string, calls *[]Call) error {
	return parseWithBudget(src, lineBase, inherited, vars, calls, newNestedParseBudget(len(src)))
}

func parseWithBudget(src string, lineBase int, inherited []string, vars map[string]string, calls *[]Call, budget *nestedParseBudget) error {
	if budget.trackWork {
		budget.parsedBytes += len(src)
	}
	file, full, ignored, err := budget.parseShell(src)
	if err != nil {
		return fmt.Errorf("shellcmd: parse shell: %w", err)
	}
	var optionErr error
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || optionErr != nil || len(call.Args) == 0 {
			return true
		}
		command, literal := wordValue(call.Args[0])
		name := baseName(command)
		if !literal || name != "flock" && name != "setsid" {
			return true
		}
		values, _ := words(call.Args, full, vars)
		if len(values) == 0 {
			return true
		}
		args := values[1:]
		for i := 0; wrappers[name] && i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" && args[i] != "--"; {
			key, _, _ := strings.Cut(args[i], "=")
			if wrapperLongOptionAmbiguous(name, key) {
				optionErr = fmt.Errorf("shellcmd: ambiguous %s option %q", name, key)
				return false
			}
			i = optionNext(name, args, i)
		}
		return true
	})
	if optionErr != nil {
		return optionErr
	}
	err = walkFileWithBudget(file, full, src, lineBase, inherited, vars, calls, ignored, nil, budget)
	if err != nil {
		return err
	}
	return checkUncertainTime(file, src, lineBase, inherited, vars, budget)
}

// A shell time clause can also be read as the external time wrapper. Keep the
// shell's calls, but report an ambiguous signal found by that interpretation.
func checkUncertainTime(file *syntax.File, src string, lineBase int, inherited []string, vars map[string]string, budget *nestedParseBudget) error {
	if !strings.Contains(src, "time") {
		return nil
	}
	var offsets []int
	syntax.Walk(file, func(node syntax.Node) bool {
		clause, ok := node.(*syntax.TimeClause)
		if !ok || clause.Stmt == nil {
			return true
		}
		call, ok := clause.Stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		args, _ := words(call.Args, src, vars)
		if !wrapperOptionsKnown("time", args) {
			offsets = append(offsets, int(clause.Pos().Offset()))
		}
		return true
	})
	if len(offsets) == 0 {
		return nil
	}
	var alternate strings.Builder
	last := 0
	for _, offset := range offsets {
		if offset < last || offset >= len(src) {
			continue
		}
		alternate.WriteString(src[last:offset])
		alternate.WriteByte('\\')
		last = offset
	}
	alternate.WriteString(src[last:])
	var candidates []Call
	err := parseWithBudget(alternate.String(), lineBase, inherited, maps.Clone(vars), &candidates, budget)
	var nested *NestedError
	if err != nil && !errors.As(err, &nested) {
		return nil
	}
	for i := range candidates {
		if call := &candidates[i]; signalCall(call) && slices.Contains(call.Via, "time") {
			return &NestedError{Script: src, Err: errors.New("shellcmd: ambiguous time option may hide a signal")}
		}
	}
	if errors.Is(err, errUnknownWrapperSignal) || errors.Is(err, errUncertainWrapperBudget) || errors.Is(err, errCommandExpansionDepth) {
		return err
	}
	return nil
}

func walkFileWithBudget(file *syntax.File, full, src string, lineBase int, inherited []string, vars map[string]string, calls *[]Call, ignored [][2]int, subst *mapped, budget *nestedParseBudget) error {
	if budget.trackWork {
		budget.parsedBytes += len(src)
	}
	first := len(*calls)
	walkText := src
	if len(full) > len(src) {
		walkText = full
	}
	err := walkCalls(file, walkText, len(src), lineBase, inherited, vars, calls, ignored, subst, budget)
	if len(full) > len(src) {
		*calls = append((*calls)[:first], trimAppended((*calls)[first:], src)...)
	}
	return err
}

// trimAppended убирает следы ограничителя, дописанного parseShell: вызовы из дописанного текста (после
// `\` в конце строки он склеивается с ней) выпадают, тело дочитанного heredoc кончается на конце src.
func trimAppended(calls []Call, src string) []Call {
	kept := calls[:0]
	for i := range calls {
		c := &calls[i]
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
		kept = append(kept, *c)
	}
	return kept
}

// parseShell разбирает текст как bash. Незакрытый heredoc bash дочитывает до конца текста с предупреждением,
// а разбор считает ошибкой — тогда ограничитель дописывается в конец; full — разобранный текст.
func (b *nestedParseBudget) parseShell(src string) (*syntax.File, string, [][2]int, error) {
	if b.trackWork {
		if b.parsedShells == nil {
			b.parsedShells = make(map[string]int)
		}
		b.parsedShells[src]++
	}
	return parseShellWithMetadata(src)
}

type commentNodes struct {
	comments   []*syntax.Comment
	backquotes [][2]int
	heredocs   [][2]int
}

func collectCommentNodes(file *syntax.File) commentNodes {
	var nodes commentNodes
	syntax.Walk(file, func(node syntax.Node) bool {
		switch node := node.(type) {
		case *syntax.Comment:
			nodes.comments = append(nodes.comments, node)
		case *syntax.CmdSubst:
			if node.Backquotes {
				nodes.backquotes = append(nodes.backquotes, [2]int{int(node.Pos().Offset()), int(node.End().Offset())})
			}
		case *syntax.Redirect:
			if node.Hdoc != nil && !quotedHeredoc(node.Word) {
				nodes.heredocs = append(nodes.heredocs, [2]int{int(node.Hdoc.Pos().Offset()), int(node.Hdoc.End().Offset())})
			}
		}
		return true
	})
	return nodes
}

func continuedCommentLine(src string, start, end int) (slash, continuedAt, commentEnd int) {
	slash, continuedAt = -1, -1
	end = min(end, len(src))
	commentEnd = end
	for newline := start + 1; newline < end; newline++ {
		if src[newline] != '\n' {
			continue
		}
		slashEnd := newline - 1
		if slashEnd >= start && src[slashEnd] == '\r' {
			slashEnd--
		}
		last := slashEnd
		for last >= start && src[last] == '\\' {
			last--
		}
		if (slashEnd-last)%2 == 1 {
			if slash < 0 {
				slash, continuedAt = slashEnd, newline+1
			}
			continue
		}
		commentEnd = newline + 1
		break
	}
	return slash, continuedAt, commentEnd
}

func commentContinuations(file *syntax.File, src string) []int {
	nodes := collectCommentNodes(file)
	var offsets []int
	for _, comment := range nodes.comments {
		start, end := int(comment.Pos().Offset()), int(comment.End().Offset())
		if end < len(src) && end > 0 && src[end] == '\n' && src[end-1] == '\r' {
			end++
		}
		if start < 0 || start >= len(src) {
			continue
		}
		inBackquote := false
		for _, span := range nodes.backquotes {
			if start >= span[0] && start < span[1] {
				inBackquote = true
				break
			}
		}
		if offsetInSpans(start, nodes.heredocs) {
			continue
		}
		if inBackquote {
			// End positions in backquotes omit removed escapes. Find the
			// newline from the original comment start instead.
			if !strings.HasSuffix(comment.Text, "\\\n") {
				continue
			}
			newline := strings.IndexByte(src[start:], '\n')
			if newline <= 0 {
				continue
			}
			last := start + newline - 1
			first := last
			for first > start && src[first-1] == '\\' {
				first--
			}
			if (last-first+1)%2 == 0 {
				offsets = append(offsets, last)
			}
			continue
		}
		if slash, _, _ := continuedCommentLine(src, start, end); slash >= 0 {
			offsets = append(offsets, slash)
		}
	}
	return offsets
}

func offsetInSpans(offset int, spans [][2]int) bool {
	for _, span := range spans {
		if offset >= span[0] && offset < span[1] {
			return true
		}
	}
	return false
}

func preprocessedCommentCommands(file *syntax.File, src string) [][2]int {
	nodes := collectCommentNodes(file)
	containers := make([][2]int, 0, len(nodes.backquotes)+len(nodes.heredocs))
	containers = append(containers, nodes.backquotes...)
	containers = append(containers, nodes.heredocs...)
	var ignored [][2]int
	for _, comment := range nodes.comments {
		start := int(comment.Pos().Offset())
		if start < 0 || start >= len(src) {
			continue
		}
		for _, container := range containers {
			if start < container[0] || start >= container[1] {
				continue
			}
			_, continuedAt, end := continuedCommentLine(src, start, container[1])
			if continuedAt >= 0 && continuedAt < end {
				ignored = append(ignored, [2]int{continuedAt, end})
			}
			break
		}
	}
	return ignored
}
func parseShell(src string) (*syntax.File, string, error) {
	file, full, _, err := parseShellWithMetadata(src)
	return file, full, err
}

func hasBackslashLineEnding(src string) bool {
	for start := 0; start < len(src); {
		next := strings.IndexByte(src[start:], '\\')
		if next < 0 {
			return false
		}
		index := start + next
		if index+1 < len(src) && (src[index+1] == '\n' || src[index+1] == '\r' && index+2 < len(src) && src[index+2] == '\n') {
			return true
		}
		start = index + 1
	}
	return false
}

func parseShellWithMetadata(src string) (*syntax.File, string, [][2]int, error) {
	if strings.ContainsRune(src, '\r') {
		return parseShellCarriageReturns(src)
	}
	return parseShellEncoded(src)
}

func parseShellCarriageReturns(src string) (*syntax.File, string, [][2]int, error) {
	// A CR escaped by an odd run of backslashes must not be parsed as
	// whitespace or as part of a CRLF line continuation. Preserve offsets.
	var positions []int
	for i := 0; i < len(src); {
		n := strings.Index(src[i:], "\\\r")
		if n < 0 {
			break
		}
		cr := i + n + 1
		start := cr - 1
		for start > 0 && src[start-1] == '\\' {
			start--
		}
		if (cr-start)%2 == 1 {
			positions = append(positions, cr)
		}
		i = cr + 1
	}
	if len(positions) == 0 {
		return parseShellEncoded(src)
	}
	for marker := byte(1); marker < 32; marker++ {
		if marker == '\n' || marker == '\r' || marker == '\t' || strings.IndexByte(src, marker) >= 0 {
			continue
		}
		encoded := []byte(src)
		for _, pos := range positions {
			encoded[pos] = marker
		}
		file, full, ignored, err := parseCarriageReturnEncoded(string(encoded), marker)
		if err == nil && len(full) > len(src) {
			_, originalFull, _, originalErr := parseShellEncoded(src)
			if originalErr == nil && len(originalFull) == len(src) {
				return nil, src, nil, fmt.Errorf("shellcmd: inconsistent here-document termination")
			}
		}
		var pe syntax.ParseError
		if errors.As(err, &pe) {
			err = restoreCarriageReturnParseError(pe, marker)
		}
		return file, strings.ReplaceAll(full, string(marker), "\r"), ignored, err
	}
	return nil, src, nil, fmt.Errorf("shellcmd: cannot represent carriage return in shell input")
}

func parseCarriageReturnEncoded(encoded string, marker byte) (*syntax.File, string, [][2]int, error) {
	file, full, ignored, err := parseShellEncoded(encoded)
	if file != nil {
		syntax.Walk(file, func(node syntax.Node) bool {
			switch node := node.(type) {
			case *syntax.Lit:
				node.Value = strings.ReplaceAll(node.Value, string(marker), "\r")
			case *syntax.SglQuoted:
				node.Value = strings.ReplaceAll(node.Value, string(marker), "\r")
			}
			return true
		})
	}
	return file, full, ignored, err
}

func restoreCarriageReturnParseError(pe syntax.ParseError, marker byte) syntax.ParseError {
	quoted := strconv.Quote(string(marker))
	pe.Text = strings.ReplaceAll(pe.Text, quoted[1:len(quoted)-1], `\r`)
	pe.Text = strings.ReplaceAll(pe.Text, string(marker), "\r")
	return pe
}

func parseShellEncoded(src string) (*syntax.File, string, [][2]int, error) {
	if !strings.Contains(src, "#") || !hasBackslashLineEnding(src) {
		file, full, err := parseShellVariant(src, false)
		return file, full, nil, err
	}
	originalFile, originalFull, originalErr := parseShellVariant(src, false)
	working := src
	retries := commentRetryLimit
	for range strings.Count(src, "\\\n") + strings.Count(src, "\\\r\n") + 1 {
		file, parsedFull, err := parseShellVariant(working, true)
		if err != nil {
			recovered, ok := retryCommentContinuation(working, &retries)
			if !ok {
				return originalFile, originalFull, nil, originalErr
			}
			working = recovered
			continue
		}
		offsets := commentContinuations(file, working)
		if len(offsets) == 0 {
			return file, parsedFull, preprocessedCommentCommands(file, working), nil
		}
		data := []byte(working)
		for _, offset := range offsets {
			if offset >= 0 && offset < len(data) {
				data[offset] = ' '
			}
		}
		working = string(data)
	}
	return originalFile, originalFull, nil, originalErr
}

// commentRetryLimit bounds the reparses spent on a script that fails to
// parse: each one blanks a single backslash, so a large script with a real
// syntax error and many line continuations costs a few parses, not one per
// continuation.
const commentRetryLimit = 16

// retryCommentContinuation blanks one backslash that ends a line after a #
// on the same line, when the reparse succeeds and puts the backslash inside a
// comment: bash ends a comment at the newline, the parser continues it.
func retryCommentContinuation(src string, budget *int) (string, bool) {
	for _, pos := range commentBackslashCandidates(src) {
		if *budget <= 0 {
			break
		}
		*budget--
		data := []byte(src)
		data[pos] = ' '
		candidate := string(data)
		// A failed reparse only means this backslash is not the cause.
		if file, _, err := parseShellVariant(candidate, true); err == nil && insideComment(file, pos) {
			return candidate, true
		}
	}
	return "", false
}

// commentBackslashCandidates lists the backslashes of runs that end a line
// after a # on the same line, each leaving an odd number of backslashes from
// it to the newline: one continues the line at the top level, and inside
// backquotes \\ becomes \, so an even run continues it there.
func commentBackslashCandidates(src string) []int {
	var candidates []int
	for start := 0; start < len(src); {
		slash := strings.IndexByte(src[start:], '\\')
		if slash < 0 {
			break
		}
		slash += start
		end := slash
		for end < len(src) && src[end] == '\\' {
			end++
		}
		start = end
		newline := end
		if newline < len(src) && src[newline] == '\r' {
			newline++
		}
		if newline >= len(src) || src[newline] != '\n' {
			continue
		}
		lineStart := strings.LastIndexByte(src[:slash], '\n') + 1
		if !strings.Contains(src[lineStart:slash], "#") {
			continue
		}
		for pos := slash; pos < end; pos++ {
			if (end-pos)%2 == 1 {
				candidates = append(candidates, pos)
			}
		}
	}
	return candidates
}

func insideComment(file *syntax.File, offset int) bool {
	for _, comment := range collectCommentNodes(file).comments {
		if int(comment.Pos().Offset()) <= offset && offset < int(comment.End().Offset()) {
			return true
		}
	}
	return false
}

func parseShellVariant(src string, keepComments bool) (file *syntax.File, full string, err error) {
	options := []syntax.ParserOption{syntax.Variant(syntax.LangBash)}
	if keepComments {
		options = append(options, syntax.KeepComments(true))
	}
	file, err = syntax.NewParser(options...).Parse(strings.NewReader(src), "")
	if err == nil {
		return file, src, nil
	}
	var pe syntax.ParseError
	if !errors.As(err, &pe) {
		return file, src, err
	}
	if !strings.HasPrefix(pe.Text, "unclosed here-document ") {
		return nil, src, err
	}
	return recoverUnclosedHeredocs(src, keepComments)
}

func recoverUnclosedHeredocs(src string, keepComments bool) (file *syntax.File, full string, err error) {
	full = src
	for range 8 {
		options := []syntax.ParserOption{syntax.Variant(syntax.LangBash)}
		if keepComments {
			options = append(options, syntax.KeepComments(true))
		}
		file, err = syntax.NewParser(options...).Parse(strings.NewReader(full), "")
		var pe syntax.ParseError
		if !errors.As(err, &pe) {
			return file, full, err
		}
		delim, ok := unclosedHeredocDelimiter(pe.Text)
		if !ok {
			return nil, full, err
		}
		if trailing := len(full) - len(strings.TrimRight(full, `\\`)); trailing%2 == 1 {
			full += "\n"
		}
		full += "\n" + delim + "\n"
	}
	return nil, full, err
}

func unclosedHeredocDelimiter(text string) (string, bool) {
	quoted, ok := strings.CutPrefix(text, "unclosed here-document ")
	if !ok {
		return "", false
	}
	delim, err := strconv.Unquote(quoted)
	return delim, err == nil
}

func walkCalls(file *syntax.File, src string, sourceLen, lineBase int, inherited []string, vars map[string]string, calls *[]Call, ignored [][2]int, subst *mapped, budget *nestedParseBudget) error {
	baseCalls := len(*calls)
	info := collectWalkInfo(file, src, vars, budget)

	walker := callWalker{src: src, sourceLen: sourceLen, lineBase: lineBase, inherited: inherited, vars: vars, calls: calls, seen: newCallSet(), budget: budget, info: &info, ignoredCalls: ignored, subst: subst}
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
	for i := range calls {
		call := calls[i]
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

type pendingProcessSubstitutionCall struct {
	call *syntax.CallExpr
}

type walkInfo struct {
	topLevelCalls map[*syntax.CallExpr]bool
	timedCalls    map[*syntax.CallExpr]bool
	posixCalls    map[*syntax.CallExpr]bool
	braceCalls    map[*syntax.CallExpr]bool
	unknownPosix  map[*syntax.CallExpr]bool
	unknownBrace  map[*syntax.CallExpr]bool
	redirs        map[*syntax.CallExpr][]Redir

	stdinInputs map[*syntax.CallExpr]*syntax.Redirect
	coprocCalls map[*syntax.CallExpr]bool

	loopCalls       map[*syntax.CallExpr]bool
	backgroundCalls map[*syntax.CallExpr]bool
	shellInputs     []heredocShellInput
	shellHdocs      map[*syntax.Redirect]bool
	localSubstHdocs map[*syntax.Redirect]bool

	errors               []error
	scriptInputs         map[*syntax.CallExpr]bool
	knownShellInputs     map[*syntax.CallExpr]*syntax.Redirect
	pipedHeredocs        []pipedHeredoc
	workNodes            int
	processSubstitutions *processSubstitutionIndex
	trackWork            bool
	stdinIndex           *stdinIndex
	// hasInputRedir says whether src holds any `<`; without one no call has stdin to resolve.
	hasInputRedir              bool
	hasProcessSubstitutionText bool
	shellScriptInputCandidates bool
	literalSimple              bool
}

func collectWalkInfo(file *syntax.File, src string, vars map[string]string, budget *nestedParseBudget) walkInfo {
	initial := shellModes{posix: budget.initialPosix, posixUnknown: budget.initialPosixUnknown, brace: budget.initialBrace, braceUnknown: budget.initialBraceUnknown}
	if budget.modeTexts == nil {
		budget.modeTexts = newModeTexts()
	}
	posixCalls, unknownPosix, braceCalls, unknownBrace := parsedShellModes(file, src, initial, budget.modeTexts)
	if !needsWalkInfo(src) {
		info := walkInfo{redirs: make(map[*syntax.CallExpr][]Redir), posixCalls: posixCalls, braceCalls: braceCalls, unknownPosix: unknownPosix, unknownBrace: unknownBrace, literalSimple: literalScriptSimple(file, src)}
		info.collectTopLevelCalls(file)
		return info
	}
	info := walkInfo{
		posixCalls:                 posixCalls,
		braceCalls:                 braceCalls,
		unknownPosix:               unknownPosix,
		unknownBrace:               unknownBrace,
		literalSimple:              literalScriptText(src),
		trackWork:                  budget.trackWork,
		hasInputRedir:              strings.Contains(src, "<"),
		hasProcessSubstitutionText: strings.Contains(src, "<(") || strings.Contains(src, ">("),
		shellScriptInputCandidates: hasShellScriptInputCandidate(src),
		timedCalls:                 make(map[*syntax.CallExpr]bool),
		redirs:                     make(map[*syntax.CallExpr][]Redir),

		stdinInputs: make(map[*syntax.CallExpr]*syntax.Redirect),

		loopCalls:       make(map[*syntax.CallExpr]bool),
		backgroundCalls: make(map[*syntax.CallExpr]bool),
		shellHdocs:      make(map[*syntax.Redirect]bool),
		localSubstHdocs: make(map[*syntax.Redirect]bool),

		scriptInputs:     make(map[*syntax.CallExpr]bool),
		knownShellInputs: make(map[*syntax.CallExpr]*syntax.Redirect),
	}
	if strings.Contains(src, "coproc") {
		info.coprocCalls = make(map[*syntax.CallExpr]bool)
	}
	knownPipes := make(map[*syntax.BinaryCmd]bool)
	visit := func(node syntax.Node) {
		collectWalkNode(node, file, src, vars, &info, knownPipes)
		if info.literalSimple && !literalScriptNodeSimple(node, src) {
			info.literalSimple = false
		}
		collectCallContextNode(node, &info)
	}
	if info.trackWork {
		syntax.Walk(file, func(node syntax.Node) bool {
			info.workNodes++
			visit(node)
			return true
		})
	} else {
		syntax.Walk(file, func(node syntax.Node) bool {
			visit(node)
			return true
		})
	}
	if info.processSubstitutions != nil {
		info.processSubstitutions.sort()
		for _, call := range info.processSubstitutions.calls {
			// collectCallWalkInfo acts only on a span inside the arguments; checking that first keeps
			// computed names beside an unrelated process substitution from being expanded.
			if !info.processSubstitutions.contains(call.call.Args) {
				continue
			}
			source, relevant := processSubstitutionCallKind(call.call, src, vars)
			if relevant {
				collectCallWalkInfo(call.call, source, src, vars, &info)
			}
		}
		for _, stmt := range info.processSubstitutions.stmts {
			collectStmtProcessSubstitutionWalkInfo(stmt, src, vars, &info)
		}
		for _, stmt := range info.processSubstitutions.pipelines {
			markPipeline(stmt, src, vars, &info)
		}
	}
	info.collectTopLevelCalls(file)
	if budget.trackWork {
		budget.walkInfoNodes += info.workNodes
		if info.stdinIndex != nil {
			budget.stdinSteps += info.stdinIndex.steps()
		}
	}
	return info
}

type shellModes struct {
	posix, posixUnknown bool
	brace, braceUnknown bool
}

func parsedShellModes(file *syntax.File, src string, initial shellModes, texts *modeTexts) (map[*syntax.CallExpr]bool, map[*syntax.CallExpr]bool, map[*syntax.CallExpr]bool, map[*syntax.CallExpr]bool) {
	w := newShellModeWalker(src, texts)
	w.indexFunctions(file, src)
	w.list(file.Stmts, initial, false, 0)
	anyChange := w.dynamicModeRisk || w.anyModeChange
	for _, call := range w.trapCalls {
		// A trap action runs at a time the parser does not know: with a mode
		// change anywhere in the script, subshells and trap actions included,
		// its words get an unknown mode.
		if !anyChange {
			continue
		}
		mode := shellModes{posix: initial.posix, brace: initial.brace, posixUnknown: true, braceUnknown: true}
		w.record(call, mode)
		w.markTrapFunctionsUnknown(call, mode)
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok {
			if _, exists := w.posix[call]; !exists {
				w.posix[call], w.brace[call] = initial.posix, initial.brace
				w.posixUnknown[call], w.braceUnknown[call] = initial.posixUnknown, initial.braceUnknown
			}
		}
		return true
	})
	return w.posix, w.posixUnknown, w.brace, w.braceUnknown
}

// markTrapFunctionsUnknown gives an unknown mode to the bodies of the functions a
// trap action names and of the functions they call.
func (w *shellModeWalker) markTrapFunctionsUnknown(call *syntax.CallExpr, mode shellModes) {
	action, ok := trapAction(call)
	if !ok || action == "" {
		return
	}
	if file, _, err := w.texts.parse(action); err == nil {
		w.markUnknown(file, mode)
	}
}

type shellModeWalker struct {
	src                 string
	posix, posixUnknown map[*syntax.CallExpr]bool
	brace, braceUnknown map[*syntax.CallExpr]bool
	assignmentModes     map[*syntax.Assign]shellModes
	// evalDepth is how many literal eval texts hold the walked text.
	evalDepth int
	*shellFunctions
}

// shellFunctions is the state a walker shares with the walkers of literal eval
// texts: a function defined in one is called from the other, and a trap set in
// either runs at the end.
type shellFunctions struct {
	trapCalls []*syntax.CallExpr
	functions map[string]*syntax.Stmt
	// functionSrc is the source the offsets of a body refer to.
	functionSrc map[*syntax.Stmt]string
	// functionCallers lists, by name, the functions whose bodies call it.
	functionCallers map[string][]string
	// functionEffects holds, by name and over all its definitions, the modes a
	// call may change through its body or the functions it reaches. A new
	// definition only adds to it, so it is kept up to date as bodies appear.
	functionEffects map[string]shellModes
	// functionSeen is the mode a body was walked or marked with.
	functionSeen map[*syntax.Stmt]shellModes
	// dynamicCallModes are the modes all functions were walked with for a
	// command with a computed name.
	dynamicCallModes map[shellModes]bool
	dynamicModeRisk  bool
	// anyModeChange reports a mode change anywhere in the file, subshells included.
	anyModeChange bool
	texts         *modeTexts
}

func newShellModeWalker(src string, texts *modeTexts) shellModeWalker {
	return shellModeWalker{
		src: src, posix: make(map[*syntax.CallExpr]bool), posixUnknown: make(map[*syntax.CallExpr]bool),
		brace: make(map[*syntax.CallExpr]bool), braceUnknown: make(map[*syntax.CallExpr]bool),
		assignmentModes: make(map[*syntax.Assign]shellModes),
		shellFunctions: &shellFunctions{
			functions: make(map[string]*syntax.Stmt), functionSrc: make(map[*syntax.Stmt]string),
			functionCallers: make(map[string][]string), functionEffects: make(map[string]shellModes),
			functionSeen: make(map[*syntax.Stmt]shellModes), dynamicCallModes: make(map[shellModes]bool),
			texts: texts,
		},
	}
}

// nested returns a walker for a literal eval text that records into the same maps.
func (w *shellModeWalker) nested(src string) *shellModeWalker {
	return &shellModeWalker{src: src, posix: w.posix, posixUnknown: w.posixUnknown, brace: w.brace, braceUnknown: w.braceUnknown, assignmentModes: w.assignmentModes, shellFunctions: w.shellFunctions}
}

// forFunction returns the walker for the source a function body comes from.
func (w *shellModeWalker) forFunction(body *syntax.Stmt) *shellModeWalker {
	if src, ok := w.functionSrc[body]; ok && src != w.src {
		return w.nested(src)
	}
	return w
}

// indexFunctions defines every function of the file before the walk, so a call
// placed before the definition in the text still sees its mode changes.
func (w *shellModeWalker) indexFunctions(file *syntax.File, src string) {
	syntax.Walk(file, func(node syntax.Node) bool {
		if fn, ok := node.(*syntax.FuncDecl); ok {
			w.defineFunction(fn, src)
		}
		return true
	})
	w.dynamicModeRisk = w.dynamicModeRisk || w.texts.shellModeChanges(file.Stmts, src).any()
	w.anyModeChange = w.dynamicModeRisk || w.texts.shellModeChangesAnywhere(file, src)
}

// definesDeferredCode reports a function definition or a trap command in a
// literal eval text: code that runs after the eval, in the mode of that time.
func definesDeferredCode(file *syntax.File) bool {
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.FuncDecl:
			found = true
		case *syntax.CallExpr:
			if len(n.Args) > 0 {
				if name, literal := wordValue(n.Args[0]); !literal || commandName(name) == "trap" {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// defineFunction makes a body the current one for its name. A body defined
// while walking, such as one from a literal eval, adds its mode changes to the
// name and to the functions that call it.
func (w *shellModeWalker) defineFunction(fn *syntax.FuncDecl, src string) {
	names := funcDeclNames(fn)
	if len(names) == 0 || fn.Body == nil {
		return
	}
	for _, name := range names {
		w.functions[name.Value] = fn.Body
	}
	if _, known := w.functionSrc[fn.Body]; known {
		return
	}
	w.functionSrc[fn.Body] = src
	mutation := w.texts.functionModeMutation(fn.Body, src)
	callees, err := functionCallees(fn.Body)
	mutation = mergeShellModes(mutation, unknownModesForParseError(err))
	w.dynamicModeRisk = w.dynamicModeRisk || mutation.any()
	gain := shellModes{posixUnknown: mutation.posixUnknown, braceUnknown: mutation.braceUnknown}
	for _, callee := range callees {
		for _, name := range names {
			w.functionCallers[callee] = append(w.functionCallers[callee], name.Value)
		}
		effect := w.functionEffects[callee]
		gain.posixUnknown = gain.posixUnknown || effect.posixUnknown
		gain.braceUnknown = gain.braceUnknown || effect.braceUnknown
	}
	for _, name := range names {
		w.spreadFunctionEffect(name.Value, gain)
	}
}

// funcDeclNames lists the names a declaration defines: one Name, several
// zsh Names, or none for an anonymous function.
func funcDeclNames(fn *syntax.FuncDecl) []*syntax.Lit {
	if fn.Name != nil {
		return []*syntax.Lit{fn.Name}
	}
	return fn.Names
}

// spreadFunctionEffect adds the modes to a function and to its callers, over
// any depth of calls. A function takes each mode once, so all definitions
// together cost time linear in the call graph.
func (f *shellFunctions) spreadFunctionEffect(name string, gain shellModes) {
	type pending struct {
		name string
		gain shellModes
	}
	queue := []pending{{name, gain}}
	for len(queue) > 0 {
		next := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		have := f.functionEffects[next.name]
		added := shellModes{
			posixUnknown: next.gain.posixUnknown && !have.posixUnknown,
			braceUnknown: next.gain.braceUnknown && !have.braceUnknown,
		}
		if !added.any() {
			continue
		}
		have.posixUnknown = have.posixUnknown || added.posixUnknown
		have.braceUnknown = have.braceUnknown || added.braceUnknown
		f.functionEffects[next.name] = have
		for _, caller := range f.functionCallers[next.name] {
			queue = append(queue, pending{caller, added})
		}
	}
}

// trapCommand reports a trap command, also behind builtin or command.
func trapCommand(call *syntax.CallExpr) bool {
	for _, arg := range call.Args {
		name, literal := wordValue(arg)
		if !literal {
			return false
		}
		switch commandName(name) {
		case "trap":
			return true
		case "builtin", "command", "-p", "--":
			continue
		}
		return false
	}
	return false
}

// functionCallees lists the literal command names a body runs in the calling
// shell, the functions of its trap actions included. The error is the first
// trap action that does not parse.
func functionCallees(body syntax.Node) ([]string, error) {
	var names []string
	var traps []*syntax.CallExpr
	syntax.Walk(body, func(node syntax.Node) bool {
		if !walkShellModeNode(node) {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		first, literal := wordValue(call.Args[0])
		if !literal {
			return true
		}
		names = append(names, commandName(first))
		if trapCommand(call) {
			traps = append(traps, call)
		}
		return true
	})
	var firstErr error
	for _, call := range traps {
		nested, err := trapActionCallees(call)
		names = append(names, nested...)
		firstErr = cmp.Or(firstErr, err)
	}
	return names, firstErr
}

func trapActionCallees(call *syntax.CallExpr) ([]string, error) {
	action, ok := trapAction(call)
	if !ok || action == "" {
		return nil, nil
	}
	file, _, err := parseShell(action)
	if err != nil {
		return nil, err
	}
	return functionCallees(file)
}

func (w *shellModeWalker) record(call *syntax.CallExpr, mode shellModes) {
	if _, seen := w.posix[call]; seen {
		previous := shellModes{
			posix:        w.posix[call],
			posixUnknown: w.posixUnknown[call],
			brace:        w.brace[call],
			braceUnknown: w.braceUnknown[call],
		}
		mode = mergeShellModes(previous, mode)
	}
	w.posix[call], w.posixUnknown[call] = mode.posix, mode.posixUnknown
	w.brace[call], w.braceUnknown[call] = mode.brace, mode.braceUnknown
}

func (w *shellModeWalker) list(body []*syntax.Stmt, mode shellModes, conditional bool, depth int) shellModes {
	if len(body) == 0 {
		return mode
	}
	if depth > 128 {
		mode.posixUnknown, mode.braceUnknown = true, true
		for _, item := range body {
			w.markUnknown(item, mode)
		}
		return mode
	}
	for _, item := range body {
		mode = w.stmt(item, mode, conditional, depth+1)
	}
	return mode
}

func (w *shellModeWalker) stmt(item *syntax.Stmt, mode shellModes, conditional bool, depth int) shellModes {
	if item == nil {
		return mode
	}
	if depth > 128 {
		mode.posixUnknown, mode.braceUnknown = true, true
		w.markUnknown(item, mode)
		return mode
	}
	for _, redir := range item.Redirs {
		w.visitNestedSubstitutions(redir, mode, conditional, depth+1)
	}
	if item.Background {
		w.stmt(&syntax.Stmt{Cmd: item.Cmd, Redirs: item.Redirs}, mode, conditional, depth+1)
		return mode
	}
	return w.compoundStmt(item, mode, conditional, depth)
}

// markUnknown records an unknown mode for every call and assignment under node
// and for the bodies of the functions those calls name.
func (w *shellModeWalker) markUnknown(node syntax.Node, mode shellModes) {
	syntax.Walk(node, func(child syntax.Node) bool {
		switch n := child.(type) {
		case *syntax.CallExpr:
			w.record(n, mode)
			if len(n.Args) > 0 {
				if name, literal := wordValue(n.Args[0]); literal {
					if body := w.functions[commandName(name)]; body != nil {
						w.markFunctionUnknown(body, mode)
					}
				}
			}
		case *syntax.Assign:
			w.assignmentModes[n] = mode
		}
		return true
	})
}

func (w *shellModeWalker) compoundStmt(item *syntax.Stmt, mode shellModes, conditional bool, depth int) shellModes {
	switch node := item.Cmd.(type) {
	case *syntax.CallExpr:
		return w.callStmt(node, mode, conditional, depth)
	case *syntax.DeclClause:
		w.visitNestedSubstitutions(node, mode, conditional, depth+1)
		for _, assign := range node.Args {
			w.assignmentModes[assign] = mode
		}
		return declarationMode(node, mode, conditional)
	case *syntax.FuncDecl:
		w.defineFunction(node, w.src)
	case *syntax.Block:
		return w.list(node.Stmts, mode, conditional, depth+1)
	case *syntax.Subshell:
		w.list(node.Stmts, mode, conditional, depth+1)
	case *syntax.IfClause:
		return w.ifMode(node, mode, conditional, depth)
	case *syntax.WhileClause:
		return w.walkLoop(node.Cond, node.Do, mode, conditional, depth)
	case *syntax.ForClause:
		return w.forMode(node, mode, conditional, depth)
	case *syntax.CaseClause:
		return w.caseMode(node, mode, conditional, depth)
	case *syntax.TimeClause:
		if node.Stmt != nil {
			return w.stmt(node.Stmt, mode, conditional, depth+1)
		}
	case *syntax.CoprocClause:
		if node.Stmt != nil {
			w.stmt(node.Stmt, mode, conditional, depth+1)
		}
	case *syntax.TestClause, *syntax.ArithmCmd, *syntax.LetClause:
		w.visitNestedSubstitutions(node, mode, conditional, depth+1)
	case *syntax.BinaryCmd:
		return w.binaryMode(node, mode, conditional, depth)
	}
	return mode
}

func (w *shellModeWalker) caseMode(node *syntax.CaseClause, mode shellModes, conditional bool, depth int) shellModes {
	w.visitNestedSubstitutions(node.Word, mode, conditional, depth+1)
	out := mode
	changed := false
	for _, candidate := range node.Items {
		for _, pattern := range candidate.Patterns {
			w.visitNestedSubstitutions(pattern, mode, conditional, depth+1)
		}
		armMode := w.list(candidate.Stmts, mode, conditional, depth+1)
		changed = changed || armMode != mode
		out = mergeShellModes(out, armMode)
	}
	if changed {
		all := make([]*syntax.Stmt, 0)
		for _, candidate := range node.Items {
			all = append(all, candidate.Stmts...)
		}
		changes := w.modeChanges(all)
		out.posixUnknown = out.posixUnknown || changes.posixUnknown
		out.braceUnknown = out.braceUnknown || changes.braceUnknown
	}
	return out
}

func (w *shellModeWalker) forMode(node *syntax.ForClause, mode shellModes, conditional bool, depth int) shellModes {
	if loop, ok := node.Loop.(*syntax.WordIter); ok {
		for _, word := range loop.Items {
			w.visitNestedSubstitutions(word, mode, conditional, depth+1)
		}
	}
	changes := w.loopChanges(node.Do, mode)
	bodyMode := mode
	bodyMode.posixUnknown = bodyMode.posixUnknown || changes.posixUnknown
	bodyMode.braceUnknown = bodyMode.braceUnknown || changes.braceUnknown
	w.list(node.Do, bodyMode, conditional, depth+1)
	mode.posixUnknown = mode.posixUnknown || changes.posixUnknown
	mode.braceUnknown = mode.braceUnknown || changes.braceUnknown
	return mode
}

func (w *shellModeWalker) callStmt(call *syntax.CallExpr, mode shellModes, conditional bool, depth int) shellModes {
	w.visitCallInputs(call, mode, conditional, depth)
	args, _, _ := wordValues(call.Args, w.src, nil, false)
	if len(call.Args) == 1 && commandWordIsModeOnlySubstitution(call.Args[0], w.src) {
		return mode
	}
	if len(args) == 0 {
		for _, assign := range call.Assigns {
			if assign.Name != nil && assign.Name.Value == "POSIXLY_CORRECT" {
				mode.posix, mode.posixUnknown = setShellMode(mode.posix, mode.posixUnknown, true, conditional)
			}
		}
		return mode
	}
	if len(args) == 1 && args[0] == "" {
		return mode
	}
	w.record(call, mode)
	return w.applyCallCommand(call, args, mode, conditional, depth)
}

func (w *shellModeWalker) visitCallInputs(call *syntax.CallExpr, mode shellModes, conditional bool, depth int) {
	for _, arg := range call.Args {
		w.visitNestedSubstitutions(arg, mode, conditional, depth+1)
	}
	for _, assign := range call.Assigns {
		w.visitNestedSubstitutions(assign, mode, conditional, depth+1)
		w.assignmentModes[assign] = mode
	}
}

func (w *shellModeWalker) applyCallCommand(call *syntax.CallExpr, args []string, mode shellModes, conditional bool, depth int) shellModes {
	name, rest := unwrapSetCommand(args)
	if name == "trap" {
		w.trapCalls = append(w.trapCalls, call)
	}
	if !literalCommandName(call.Args[0]) && w.dynamicModeRisk {
		if !w.dynamicCallModes[mode] {
			w.dynamicCallModes[mode] = true
			for _, body := range w.functions {
				w.callFunction(body, mode, conditional, depth)
			}
		}
		mode.posixUnknown, mode.braceUnknown = true, true
		return mode
	}
	functionName := commandName(args[0])
	if body := w.functions[functionName]; body != nil {
		w.callFunction(body, mode, conditional, depth)
		risk := w.functionMutationEffects(functionName)
		mode.posixUnknown = mode.posixUnknown || risk.posixUnknown
		mode.braceUnknown = mode.braceUnknown || risk.braceUnknown
		return mode
	}
	if hasPosixEnvironment(call) && posixPrefixAffectsShell(name) {
		if len(args) > 0 && args[0] == "command" {
			mode.posixUnknown = true
		} else {
			mode.posix, mode.posixUnknown = true, false
		}
	}
	mode = w.applyModeCommand(name, rest, call, mode, conditional, depth)
	mode = applyPosixAssignmentMode(name, rest, call, mode, conditional)
	if !literalCommandName(call.Args[0]) && w.dynamicModeRisk {
		mode.posixUnknown, mode.braceUnknown = true, true
	}
	return mode
}

func commandWordIsModeOnlySubstitution(word *syntax.Word, src string) bool {
	if word == nil || len(word.Parts) != 1 {
		return false
	}
	sub, ok := word.Parts[0].(*syntax.CmdSubst)
	if !ok || len(sub.Stmts) == 0 {
		return false
	}
	for _, stmt := range sub.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return false
		}
		args, _, _ := wordValues(call.Args, src, nil, false)
		name, _ := unwrapSetCommand(args)
		if name != "set" && name != "shopt" {
			return false
		}
	}
	return true
}

func posixPrefixAffectsShell(name string) bool {
	return slices.Contains([]string{":", "break", "continue", "eval", "exec", "exit", "export", "readonly", "return", "set", "shift", "times", "trap", "unset", ".", "source"}, name)
}

func literalCommandName(word *syntax.Word) bool {
	_, ok := wordValue(word)
	return ok
}

func (w *shellModeWalker) applyModeCommand(name string, rest []string, call *syntax.CallExpr, mode shellModes, conditional bool, depth int) shellModes {
	dynamic := dynamicModeArgs(name, call)
	mode.posixUnknown = mode.posixUnknown || dynamic.posixUnknown
	mode.braceUnknown = mode.braceUnknown || dynamic.braceUnknown
	switch name {
	case "set":
		mode = applySetModes(rest, mode, conditional)
	case "shopt":
		mode = applyShoptModes(rest, mode, conditional)
	case "unset":
		if unsetPosixVariable(rest) {
			mode.posix, mode.posixUnknown = setShellMode(mode.posix, mode.posixUnknown, false, conditional)
		}
	case "eval":
		mode = w.evalMode(call, mode, conditional, depth)
	}
	if name == "exec" && len(rest) > 0 || name == "source" || name == "." {
		mode.posixUnknown, mode.braceUnknown = true, true
	}
	if name == "trap" && trapMayChangeBeforeNextCommand(rest) {
		mode.posixUnknown, mode.braceUnknown = true, true
	}
	if name == "trap" {
		changes, err := trapActionMayChangeMode(call, w)
		if err != nil || changes {
			mode.posixUnknown, mode.braceUnknown = true, true
		}
	}
	return mode
}

// evalMode walks a literal eval text in the mode of the call; any other text,
// or one nested deeper than maxModeTextDepth, leaves the mode unknown.
func (w *shellModeWalker) evalMode(call *syntax.CallExpr, mode shellModes, conditional bool, depth int) shellModes {
	literal, ok := literalEvalText(call.Args)
	if !ok || w.evalDepth >= maxModeTextDepth {
		mode.posixUnknown, mode.braceUnknown = true, true
		return mode
	}
	file, full, err := w.texts.parse(literal)
	if err != nil {
		return mergeShellModes(mode, unknownModesForParseError(err))
	}
	before := mode
	inner := w.nested(full)
	inner.evalDepth = w.evalDepth + 1
	mode = inner.list(file.Stmts, mode, conditional, depth+1)
	if (w.dynamicModeRisk || w.anyModeChange) && definesDeferredCode(file) {
		// The words of the text are read from the mode of the eval call; a body
		// or trap action that runs later does not have it.
		before.posixUnknown, before.braceUnknown = true, true
		w.record(call, before)
	}
	return mode
}

func trapActionMayChangeMode(call *syntax.CallExpr, w *shellModeWalker) (bool, error) {
	action, ok := trapAction(call)
	if !ok {
		return true, nil
	}
	if action == "" {
		return false, nil
	}
	file, full, err := w.texts.parse(action)
	if err != nil {
		return false, err
	}
	if w.texts.shellModeChanges(file.Stmts, full).any() {
		return true, nil
	}
	mutates := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if c, ok := node.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			name, literal := wordValue(c.Args[0])
			if literal && w.functionMutationEffects(commandName(name)).any() {
				mutates = true
			}
		}
		return !mutates
	})
	return mutates, nil
}

func trapMayChangeBeforeNextCommand(args []string) bool {
	if len(args) == 0 || args[0] == "-p" || args[0] == "-l" {
		return false
	}
	for _, signal := range args[1:] {
		if signal != "EXIT" && signal != "0" {
			return true
		}
	}
	return false
}

func applyPosixAssignmentMode(name string, rest []string, call *syntax.CallExpr, mode shellModes, conditional bool) shellModes {
	if name == "set" {
		for _, assign := range call.Assigns {
			if assign.Name != nil && assign.Name.Value == "POSIXLY_CORRECT" {
				mode.posix, mode.posixUnknown = setShellMode(mode.posix, mode.posixUnknown, true, conditional)
			}
		}
		return mode
	}
	if name != "eval" && !slices.Contains(declarationCommands[:], name) {
		return mode
	}
	for _, assign := range call.Assigns {
		if assign.Name != nil && assign.Name.Value == "POSIXLY_CORRECT" {
			if name == "eval" {
				mode.posix, mode.posixUnknown = setShellMode(mode.posix, mode.posixUnknown, true, conditional)
			} else {
				mode.posix, mode.posixUnknown = false, true
			}
		}
	}
	for _, arg := range rest {
		if slices.Contains(declarationCommands[:], name) && (arg == "POSIXLY_CORRECT" || strings.HasPrefix(arg, "POSIXLY_CORRECT=")) {
			if strings.HasPrefix(arg, "POSIXLY_CORRECT=") {
				mode.posix, mode.posixUnknown = setShellMode(mode.posix, mode.posixUnknown, true, conditional)
			} else {
				mode.posix, mode.posixUnknown = false, true
			}
		}
	}
	return mode
}

func assignmentShellModes(file *syntax.File, src string) map[*syntax.Assign]shellModes {
	w := newShellModeWalker(src, newModeTexts())
	w.indexFunctions(file, src)
	w.list(file.Stmts, shellModes{brace: true}, false, 0)
	return w.assignmentModes
}

func literalEvalText(args []*syntax.Word) (string, bool) {
	if len(args) < 2 {
		return "", false
	}
	values := make([]string, len(args)-1)
	for i, arg := range args[1:] {
		value, ok := wordValue(arg)
		if !ok {
			return "", false
		}
		values[i] = value
	}
	return strings.Join(values, " "), true
}

func (w *shellModeWalker) visitNestedSubstitutions(node syntax.Node, mode shellModes, conditional bool, depth int) {
	syntax.Walk(node, func(n syntax.Node) bool {
		switch sub := n.(type) {
		case *syntax.CmdSubst:
			w.list(sub.Stmts, mode, conditional, depth+1)
			return false
		case *syntax.ProcSubst:
			w.list(sub.Stmts, mode, conditional, depth+1)
			return false
		}
		return true
	})
}

func declarationMode(node *syntax.DeclClause, mode shellModes, conditional bool) shellModes {
	if node.Variant == nil || !slices.Contains(declarationCommands[:], node.Variant.Value) {
		return mode
	}
	for _, assign := range node.Args {
		if assign.Name != nil && assign.Name.Value == "POSIXLY_CORRECT" {
			if assign.Value == nil {
				mode.posix, mode.posixUnknown = false, true
			} else {
				mode.posix, mode.posixUnknown = setShellMode(mode.posix, mode.posixUnknown, true, conditional)
			}
		}
	}
	return mode
}

// callFunction walks a body once; a call in another mode marks the body and the
// functions it calls unknown instead of walking it again.
func (w *shellModeWalker) callFunction(body *syntax.Stmt, mode shellModes, conditional bool, depth int) {
	if previous, seen := w.functionSeen[body]; seen {
		if previous != mode {
			w.markFunctionUnknown(body, mergeShellModes(previous, mode))
		}
		return
	}
	w.functionSeen[body] = mode
	w.forFunction(body).stmt(body, mode, conditional, depth+1)
}

// markFunctionUnknown gives an unknown mode to the calls of a body and of the
// functions it calls. A body already marked with that mode is skipped, so each
// body is marked a bounded number of times.
func (w *shellModeWalker) markFunctionUnknown(body *syntax.Stmt, mode shellModes) {
	previous, seen := w.functionSeen[body]
	if seen {
		mode = mergeShellModes(previous, mode)
	}
	mode.posixUnknown, mode.braceUnknown = true, true
	if seen && previous == mode {
		return
	}
	w.functionSeen[body] = mode
	w.forFunction(body).markUnknown(body, mode)
}

// functionMutationEffects reports the modes a call of the function may change,
// through its own body or the functions it calls, recursion included.
func (w *shellModeWalker) functionMutationEffects(name string) shellModes {
	return w.functionEffects[name]
}

func (w *shellModeWalker) ifMode(node *syntax.IfClause, mode shellModes, conditional bool, depth int) shellModes {
	condition := w.list(node.Cond, mode, conditional, depth+1)
	thenMode, elseMode := w.list(node.Then, condition, false, depth+1), condition
	if node.Else != nil {
		elseMode = w.list(node.Else.Then, condition, false, depth+1)
	}
	out := mergeShellModes(thenMode, elseMode)
	conditionChanges := w.modeChanges(node.Cond)
	out.posixUnknown = out.posixUnknown || conditionChanges.posixUnknown
	out.braceUnknown = out.braceUnknown || conditionChanges.braceUnknown
	return out
}

// walkLoop gives the condition and the body the mode before the loop when the
// loop changes no mode, and an unknown mode otherwise: the condition runs again
// after the body, and which pass a word sees depends on the run.
func (w *shellModeWalker) walkLoop(cond, body []*syntax.Stmt, mode shellModes, conditional bool, depth int) shellModes {
	changes := w.loopChanges(append(slices.Clone(cond), body...), mode)
	mode.posixUnknown = mode.posixUnknown || changes.posixUnknown
	mode.braceUnknown = mode.braceUnknown || changes.braceUnknown
	w.list(cond, mode, conditional, depth+1)
	w.list(body, mode, conditional, depth+1)
	return mode
}

func (w *shellModeWalker) loopChanges(stmts []*syntax.Stmt, initial shellModes) shellModes {
	mode := initial
	var changes shellModes
	for _, stmt := range stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok {
			other := w.modeChanges([]*syntax.Stmt{stmt})
			changes.posixUnknown = changes.posixUnknown || other.posixUnknown
			changes.braceUnknown = changes.braceUnknown || other.braceUnknown
			mode.posixUnknown = mode.posixUnknown || other.posixUnknown
			mode.braceUnknown = mode.braceUnknown || other.braceUnknown
			continue
		}
		args, _, _ := wordValues(call.Args, w.src, nil, false)
		if len(args) == 0 {
			changes.posixUnknown = changes.posixUnknown || hasPosixEnvironment(call)
			continue
		}
		name, rest := unwrapSetCommand(args)
		before := mode
		mode = w.applyModeCommand(name, rest, call, mode, false, 0)
		mode = applyPosixAssignmentMode(name, rest, call, mode, false)
		changes.posixUnknown = changes.posixUnknown || before.posix != mode.posix || before.posixUnknown != mode.posixUnknown
		changes.braceUnknown = changes.braceUnknown || before.brace != mode.brace || before.braceUnknown != mode.braceUnknown
		if body := w.functions[commandName(args[0])]; body != nil {
			effect := w.functionMutationEffects(commandName(args[0]))
			changes.posixUnknown = changes.posixUnknown || effect.posixUnknown
			changes.braceUnknown = changes.braceUnknown || effect.braceUnknown
		}
	}
	return changes
}

func setShellMode(current, currentUnknown, enable, conditional bool) (bool, bool) {
	if !conditional {
		return enable, false
	}
	return enable, currentUnknown || current != enable
}

func (w *shellModeWalker) binaryMode(node *syntax.BinaryCmd, mode shellModes, conditional bool, depth int) shellModes {
	if node.Op == syntax.Pipe || node.Op == syntax.PipeAll {
		w.stmt(node.Y, mode, conditional, depth+1)
		w.stmt(node.X, mode, conditional, depth+1)
		return mode
	}
	left := w.stmt(node.X, mode, conditional, depth+1)
	rightEntry := left
	leftChanges := w.modeChanges([]*syntax.Stmt{node.X})
	rightEntry.posixUnknown = rightEntry.posixUnknown || leftChanges.posixUnknown
	rightEntry.braceUnknown = rightEntry.braceUnknown || leftChanges.braceUnknown
	w.stmt(node.Y, rightEntry, false, depth+1)
	out := mode
	changes := w.modeChanges([]*syntax.Stmt{node.X, node.Y})
	out.posixUnknown = out.posixUnknown || changes.posixUnknown
	out.braceUnknown = out.braceUnknown || changes.braceUnknown
	return out
}

func mergeShellModes(a, b shellModes) shellModes {
	out := shellModes{posix: a.posix, brace: a.brace}
	if a.posixUnknown || b.posixUnknown || a.posix != b.posix {
		out.posix, out.posixUnknown = false, true
	} else {
		out.posix = a.posix
	}
	if a.braceUnknown || b.braceUnknown || a.brace != b.brace {
		out.brace, out.braceUnknown = true, true
	} else {
		out.brace = a.brace
	}
	return out
}

func walkShellModeNode(node syntax.Node) bool {
	switch n := node.(type) {
	case *syntax.FuncDecl, *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst, *syntax.CoprocClause:
		return false
	case *syntax.Stmt:
		return !n.Background
	case *syntax.BinaryCmd:
		return n.Op != syntax.Pipe && n.Op != syntax.PipeAll
	default:
		return true
	}
}

func (t *modeTexts) functionModeMutation(body *syntax.Stmt, src string) shellModes {
	var changes shellModes
	syntax.Walk(body, func(node syntax.Node) bool {
		if !walkShellModeNode(node) {
			return false
		}
		if decl, ok := node.(*syntax.DeclClause); ok {
			changes.posixUnknown = changes.posixUnknown || declarationSetsPosix(decl)
			if decl.Variant != nil && decl.Variant.Value == "local" && localRestoresOptions(decl, src) {
				changes.posixUnknown, changes.braceUnknown = true, true
			}
			return true
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		mutation := t.callModeMutation(call, src)
		changes.posixUnknown = changes.posixUnknown || mutation.posixUnknown
		changes.braceUnknown = changes.braceUnknown || mutation.braceUnknown
		return true
	})
	return changes
}

func localRestoresOptions(decl *syntax.DeclClause, src string) bool {
	for _, assign := range decl.Args {
		if assign.Naked && assign.Value != nil {
			if value, ok := wordValue(assign.Value); ok && value == "-" {
				return true
			}
			if unquotedText(assign.Value, src) == "-" {
				return true
			}
		}
	}
	return false
}

func (t *modeTexts) shellModeChanges(stmts []*syntax.Stmt, src string) shellModes {
	return t.shellModeChangesAt(stmts, src, 0)
}

// shellModeChangesAt is shellModeChanges for a text nested depth levels deep
// in eval and trap texts.
func (t *modeTexts) shellModeChangesAt(stmts []*syntax.Stmt, src string, depth int) shellModes {
	var changes shellModes
	for _, stmt := range stmts {
		syntax.Walk(stmt, func(node syntax.Node) bool {
			if !walkShellModeNode(node) {
				return false
			}
			switch n := node.(type) {
			case *syntax.CallExpr:
				m := t.callModeMutationAt(n, src, depth)
				changes.posixUnknown = changes.posixUnknown || m.posixUnknown
				changes.braceUnknown = changes.braceUnknown || m.braceUnknown
			case *syntax.DeclClause:
				if declarationSetsPosix(n) {
					changes.posixUnknown = true
				}
			}
			return true
		})
	}
	return changes
}

// shellModeChangesAnywhere reports a mode change in any part of the file,
// subshells, substitutions and function bodies included.
func (t *modeTexts) shellModeChangesAnywhere(file *syntax.File, src string) bool {
	changed := false
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			changed = changed || t.callModeMutation(n, src).any()
		case *syntax.DeclClause:
			changed = changed || declarationNamesPosix(n)
		}
		return !changed
	})
	return changed
}

func (w *shellModeWalker) modeChanges(stmts []*syntax.Stmt) shellModes {
	changes := w.texts.shellModeChanges(stmts, w.src)
	for _, stmt := range stmts {
		syntax.Walk(stmt, func(node syntax.Node) bool {
			if !walkShellModeNode(node) {
				return false
			}
			call, ok := node.(*syntax.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			args, _, _ := wordValues(call.Args, w.src, nil, false)
			if len(args) == 0 {
				return true
			}
			effect := w.functionMutationEffects(commandName(args[0]))
			changes.posixUnknown = changes.posixUnknown || effect.posixUnknown
			changes.braceUnknown = changes.braceUnknown || effect.braceUnknown
			return true
		})
	}
	return changes
}

func (m shellModes) any() bool { return m.posixUnknown || m.braceUnknown }

// declarationNamesPosix reports a declaration of POSIXLY_CORRECT by any
// declaration command, a local one included.
func declarationNamesPosix(decl *syntax.DeclClause) bool {
	for _, assign := range decl.Args {
		if assign.Name != nil && assign.Name.Value == "POSIXLY_CORRECT" {
			return true
		}
	}
	return false
}

func declarationSetsPosix(decl *syntax.DeclClause) bool {
	if decl.Variant == nil || !slices.Contains(declarationCommands[:], decl.Variant.Value) {
		return false
	}
	if decl.Variant.Value == "local" || decl.Variant.Value == "declare" || decl.Variant.Value == "typeset" {
		return false
	}
	for _, assign := range decl.Args {
		if assign.Name != nil && assign.Name.Value == "POSIXLY_CORRECT" {
			return true
		}
	}
	return false
}

func (t *modeTexts) callModeMutation(call *syntax.CallExpr, src string) shellModes {
	return t.callModeMutationAt(call, src, 0)
}

// callModeMutationAt is callModeMutation for a call nested depth levels deep in
// eval and trap texts.
func (t *modeTexts) callModeMutationAt(call *syntax.CallExpr, src string, depth int) shellModes {
	args, _, _ := wordValues(call.Args, src, nil, false)
	name, rest := unwrapSetCommand(args)
	changes := mergeShellModes(optionCommandMutation(name, rest), dynamicModeArgs(name, call))
	if name == "source" || name == "." {
		changes.posixUnknown, changes.braceUnknown = true, true
	}
	if name == "trap" {
		changes = t.mergeTrapActionMutation(changes, call, depth)
	}
	if name == "unset" && unsetPosixVariable(rest) {
		changes.posixUnknown = true
	}
	if name == "eval" {
		if literalText, literal := literalEvalText(call.Args); !literal {
			changes.posixUnknown, changes.braceUnknown = true, true
		} else {
			nested := t.nestedTextModeChanges(literalText, depth)
			changes.posixUnknown = changes.posixUnknown || nested.posixUnknown
			changes.braceUnknown = changes.braceUnknown || nested.braceUnknown
		}
	}
	if hasPosixEnvironment(call) && (len(call.Args) == 0 || posixPrefixAffectsShell(name)) {
		changes.posixUnknown = true
	}
	if name == "unset" && unsetPosixVariable(rest) {
		changes.posixUnknown = true
	}
	if len(call.Args) > 0 && !literalCommandName(call.Args[0]) {
		changes.posixUnknown, changes.braceUnknown = true, true
	}
	return changes
}

// mergeTrapActionMutation adds the modes a trap action may change by its own
// text; the functions it names count as calls of the trap command.
func (t *modeTexts) mergeTrapActionMutation(changes shellModes, call *syntax.CallExpr, depth int) shellModes {
	action, ok := trapAction(call)
	if !ok {
		changes.posixUnknown, changes.braceUnknown = true, true
		return changes
	}
	nested := t.nestedTextModeChanges(action, depth)
	changes.posixUnknown = changes.posixUnknown || nested.posixUnknown
	changes.braceUnknown = changes.braceUnknown || nested.braceUnknown
	return changes
}

// nestedTextModeChanges reports the modes an eval or trap text may change. A
// text nested deeper than maxModeTextDepth is not read: each level holds the
// next one, so reading them all costs time quadratic in the length. A text
// read once keeps its answer for the other places it appears.
func (t *modeTexts) nestedTextModeChanges(text string, depth int) shellModes {
	if changes, ok := t.changes[text]; ok {
		return changes
	}
	if depth >= maxModeTextDepth {
		return shellModes{posixUnknown: true, braceUnknown: true}
	}
	var changes shellModes
	if file, full, err := t.parse(text); err != nil {
		changes = unknownModesForParseError(err)
	} else {
		changes = t.shellModeChangesAt(file.Stmts, full, depth+1)
	}
	t.changes[text] = changes
	return changes
}

// maxModeTextDepth is how deep the mode walk follows eval texts inside eval
// and trap texts; a deeper text leaves the mode unknown.
const maxModeTextDepth = 8

// modeTexts holds the eval and trap texts the mode walk has read, shared by
// all scripts of one command line: a text nested in eval is parsed and scanned
// once, not again at each level that holds it.
type modeTexts struct {
	parsed  map[string]parsedModeText
	changes map[string]shellModes
}

type parsedModeText struct {
	file *syntax.File
	src  string
	err  error
}

func newModeTexts() *modeTexts {
	return &modeTexts{parsed: make(map[string]parsedModeText), changes: make(map[string]shellModes)}
}

func (t *modeTexts) parse(text string) (*syntax.File, string, error) {
	if parsed, ok := t.parsed[text]; ok {
		return parsed.file, parsed.src, parsed.err
	}
	file, full, err := parseShell(text)
	t.parsed[text] = parsedModeText{file: file, src: full, err: err}
	return file, full, err
}

// trapAction returns the literal action of a trap command; a query, a reset or
// a missing action is an empty action.
func trapAction(call *syntax.CallExpr) (string, bool) {
	args := call.Args
	for len(args) > 0 {
		name, literal := wordValue(args[0])
		if !literal {
			return "", false
		}
		args = args[1:]
		if commandName(name) == "trap" {
			break
		}
	}
	if len(args) > 0 {
		if first, literal := wordValue(args[0]); literal && first == "--" {
			args = args[1:]
		}
	}
	if len(args) < 2 {
		return "", true
	}
	action, literal := wordValue(args[0])
	if !literal {
		return "", false
	}
	if action == "-" || strings.HasPrefix(action, "-") {
		return "", true
	}
	return action, true
}

// dynamicModeArgs reports the modes a set, shopt or unset command may change
// through a word the parser cannot read: an option of set before its
// positional arguments, any word of shopt, a name of unset.
func dynamicModeArgs(name string, call *syntax.CallExpr) shellModes {
	var unknown shellModes
	if name != "set" && name != "shopt" && name != "unset" {
		return unknown
	}
	command := false
	for _, word := range call.Args {
		value, literal := wordValue(word)
		if !command {
			command = literal && commandName(value) == name
			continue
		}
		if literal {
			if name == "set" && setOptionStopsAt(value) {
				break
			}
			continue
		}
		unknown.posixUnknown = true
		unknown.braceUnknown = unknown.braceUnknown || name != "unset"
		break
	}
	return unknown
}

func optionCommandMutation(name string, rest []string) shellModes {
	if name != "set" && name != "shopt" {
		return shellModes{}
	}
	apply := applySetModes
	if name == "shopt" {
		apply = applyShoptModes
	}
	braceOn := apply(rest, shellModes{brace: true}, false)
	braceOff := apply(rest, shellModes{brace: false}, false)
	posixOn := apply(rest, shellModes{posix: true}, false)
	posixOff := apply(rest, shellModes{posix: false}, false)
	return shellModes{
		posixUnknown: !posixOn.posix || posixOff.posix,
		braceUnknown: !braceOn.brace || braceOff.brace,
	}
}

func unknownModesForParseError(err error) shellModes {
	if err == nil {
		return shellModes{}
	}
	return shellModes{posixUnknown: true, braceUnknown: true}
}

func unwrapSetCommand(args []string) (string, []string) {
	i := 0
	for i < len(args) {
		switch args[i] {
		case "builtin":
			i++
		case "command":
			i++
			if i < len(args) && args[i] == "-p" {
				i++
			}
		default:
			if i < len(args) && args[i] == "--" {
				i++
			}
			if i >= len(args) {
				return "", nil
			}
			return commandName(args[i]), args[i+1:]
		}
	}
	return "", nil
}

func unsetPosixVariable(args []string) bool {
	functionsOnly := false
	for i := range args {
		arg := args[i]
		if arg == "--" {
			return !functionsOnly && slices.Contains(args[i+1:], "POSIXLY_CORRECT")
		}
		if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			for _, flag := range arg[1:] {
				if flag == 'f' {
					functionsOnly = true
				} else if flag == 'v' {
					functionsOnly = false
				}
			}
			continue
		}
		if arg == "POSIXLY_CORRECT" {
			return !functionsOnly
		}
	}
	return false
}

func applySetModes(args []string, mode shellModes, conditional bool) shellModes {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if setOptionStopsAt(arg) {
			break
		}
		mode, i = applySetOptionToken(args, i, mode, conditional)
	}
	return mode
}

func setOptionStopsAt(arg string) bool {
	return arg == "--" || arg == "-" || strings.HasPrefix(arg, "--") || !strings.HasPrefix(arg, "+") && !strings.HasPrefix(arg, "-")
}

func applySetOptionToken(args []string, i int, mode shellModes, conditional bool) (shellModes, int) {
	arg := args[i]
	sign := arg[0] == '-'
	if arg == "-o" || arg == "+o" {
		return applyFollowingSetOption(args, i, sign, mode, conditional)
	}
	letters := arg[1:]
	if strings.ContainsRune(letters, 'B') {
		mode.brace, mode.braceUnknown = setShellMode(mode.brace, mode.braceUnknown, sign, conditional)
	}
	before, remainder, found := strings.Cut(letters, "o")
	if !found {
		return mode, i
	}
	if before != "" && (remainder == "posix" || remainder == "braceexpand") {
		return applyNamedSetOption(remainder, sign, mode, conditional), i
	}
	if remainder == "" || onlySetOptionLetters(remainder) {
		return applyFollowingSetOption(args, i, sign, mode, conditional)
	}
	return mode, i
}

func applyFollowingSetOption(args []string, i int, sign bool, mode shellModes, conditional bool) (shellModes, int) {
	if i+1 >= len(args) {
		return mode, i
	}
	return applyNamedSetOption(args[i+1], sign, mode, conditional), i + 1
}

func onlySetOptionLetters(s string) bool {
	return strings.Trim(s, "abefhkmnptuvxBCEHPT") == ""
}

func applyNamedSetOption(name string, enable bool, mode shellModes, conditional bool) shellModes {
	switch name {
	case "posix":
		mode.posix, mode.posixUnknown = setShellMode(mode.posix, mode.posixUnknown, enable, conditional)
	case "braceexpand":
		mode.brace, mode.braceUnknown = setShellMode(mode.brace, mode.braceUnknown, enable, conditional)
	}
	return mode
}

func applyShoptModes(args []string, mode shellModes, conditional bool) shellModes {
	enable, optionMode, operation := true, false, false
	for i := range args {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			for _, flag := range arg[1:] {
				switch flag {
				case 's':
					enable = true
					operation = true
				case 'u':
					enable = false
					operation = true
				case 'o':
					optionMode = true
				}
			}
			continue
		}
		if operation && optionMode && arg == "posix" {
			mode.posix, mode.posixUnknown = setShellMode(mode.posix, mode.posixUnknown, enable, conditional)
		}
		if operation && optionMode && arg == "braceexpand" {
			mode.brace, mode.braceUnknown = setShellMode(mode.brace, mode.braceUnknown, enable, conditional)
		}
	}
	return mode
}

func (info *walkInfo) collectTopLevelCalls(file *syntax.File) {
	// Этот индекс нужен только для ArgLiteral; на корпусе команд он давал около 1,4%
	// выделенной памяти Find.
	if !info.literalSimple {
		return
	}
	info.topLevelCalls = make(map[*syntax.CallExpr]bool)
	for _, stmt := range file.Stmts {
		if call, ok := stmt.Cmd.(*syntax.CallExpr); ok {
			info.topLevelCalls[call] = true
		}
	}
}

func collectWalkNode(node syntax.Node, file *syntax.File, src string, vars map[string]string, info *walkInfo, knownPipes map[*syntax.BinaryCmd]bool) {
	switch n := node.(type) {
	case *syntax.ProcSubst:
		if info.hasProcessSubstitutionText {
			info.ensureProcessSubstitutionIndex().add(n)
		}
	case *syntax.BinaryCmd:
		collectPipelineWalkInfo(n, src, vars, info, knownPipes)
	case *syntax.CallExpr:
		if info.hasProcessSubstitutionText {
			index := info.ensureProcessSubstitutionIndex()
			index.calls = append(index.calls, pendingProcessSubstitutionCall{call: n})
		}
	case *syntax.Stmt:
		collectStmtWalkInfo(n, file, src, vars, info)
	}
}

func collectPipelineWalkInfo(binary *syntax.BinaryCmd, src string, vars map[string]string, info *walkInfo, knownPipes map[*syntax.BinaryCmd]bool) {
	if binary.Op != syntax.Pipe && binary.Op != syntax.PipeAll {
		return
	}

	_, err := collectPipedLiteralWalkInfo(binary, src, vars, info)
	if err != nil {
		info.errors = append(info.errors, err)
	}
	if collectPipedHeredocWalkInfo(binary, src, vars, info) {
		knownPipes[binary] = true
		return
	}
	if info.hasProcessSubstitutionText {
		index := info.ensureProcessSubstitutionIndex()
		index.pipelines = append(index.pipelines, binary.Y)
	} else {
		markPipeline(binary.Y, src, vars, info)
	}
}

func (info *walkInfo) ensureProcessSubstitutionIndex() *processSubstitutionIndex {
	if info.processSubstitutions == nil {
		index := &processSubstitutionIndex{}
		if info.trackWork {
			index.work = &info.workNodes
		}
		info.processSubstitutions = index
	}
	return info.processSubstitutions
}

func collectPipedLiteralWalkInfo(binary *syntax.BinaryCmd, src string, vars map[string]string, info *walkInfo) (bool, error) {
	if binary.Y == nil || len(binary.Y.Redirs) != 0 {
		return false, nil
	}
	producer := literalPipeProducer(binary.X)
	if producer == nil || len(producer.Args) == 0 {
		return false, nil
	}
	name, literal := wordValue(producer.Args[0])
	if !literal || (commandName(name) != "echo" && commandName(name) != "printf") {
		return false, nil
	}
	shell, via, ok := pipedShellNameWithWork(binary.Y, src, vars, nil)
	if !ok {
		return false, nil
	}
	if call, ok := binary.Y.Cmd.(*syntax.CallExpr); ok {
		for _, arg := range call.Args[1:] {
			if value, literal := wordValue(arg); literal && value == "--noexec" {
				return false, nil
			}
		}
	}
	body, ok, err := literalPipeBody(commandName(name), producer.Args[1:], src, vars)
	if err != nil {
		return true, &NestedError{Script: nodeText(producer, src), Err: err}
	}
	if !ok {
		return false, nil
	}
	info.pipedHeredocs = append(info.pipedHeredocs, pipedHeredoc{body: &body, shell: binary.Y, name: shell, via: via})
	if body.alternate != nil {
		info.pipedHeredocs = append(info.pipedHeredocs, pipedHeredoc{body: body.alternate, shell: binary.Y, name: shell, via: via, alternate: true})
	}
	return true, nil
}

func literalPipeProducer(stmt *syntax.Stmt) *syntax.CallExpr {
	for stmt != nil && len(stmt.Redirs) == 0 {
		left, ok := stmt.Cmd.(*syntax.BinaryCmd)
		if !ok {
			call, _ := stmt.Cmd.(*syntax.CallExpr)
			return call
		}
		if left.Op != syntax.Pipe && left.Op != syntax.PipeAll {
			return nil
		}
		stmt = left.Y
	}
	return nil
}

func literalPipeBody(name string, args []*syntax.Word, src string, vars map[string]string) (mapped, bool, error) {
	values := make([]string, 0, len(args))
	parts := make([]mapped, 0, len(args))
	for _, arg := range args {
		value, literal := wordValue(arg)
		if !literal {
			return mapped{}, false, nil
		}
		var part mapped
		if err := part.expandParts(arg.Parts, src, vars, partUnquoted); err != nil {
			return mapped{}, false, err
		}
		values = append(values, value)
		parts = append(parts, part)
	}
	if name == "echo" {
		body, err := literalEchoBody(values, parts)
		return body, true, err
	}
	body, ok, err := literalPrintfBody(values)
	if !ok || !printfFormatAlternatives(values[0]) {
		return body, ok, err
	}
	// Bash и внешний printf по-разному обрабатывают \c и \% в формате.
	alternate, alternateOK, err := literalPrintfBodyMode(values, false)
	if err != nil {
		return mapped{}, false, err
	}
	if alternateOK && alternate.String() != body.String() {
		body.alternate = &alternate
	}
	return body, true, nil
}

func literalEchoBody(values []string, parts []mapped) (mapped, error) {
	var body mapped
	newline, expand := true, false
	for len(values) > 0 && len(values[0]) > 1 && values[0][0] == '-' && strings.Trim(values[0][1:], "neE") == "" {
		for _, option := range values[0][1:] {
			switch option {
			case 'n':
				newline = false
			case 'e':
				expand = true
			case 'E':
				expand = false
			}
		}
		values, parts = values[1:], parts[1:]
	}
	for i, part := range parts {
		if i > 0 {
			body.add(" ", -1)
		}
		if expand {
			value := values[i]
			stop := echoStop(value)
			if stop >= 0 {
				value = value[:stop]
			}
			text, _, err := shExpand.Format(nil, bashEscapeInput(value, true), nil)
			if err != nil {
				return mapped{}, fmt.Errorf("shellcmd: expand piped echo: %w", err)
			}
			body.add(text, -1)
			if stop >= 0 {
				return body, nil
			}
		} else {
			body.text = append(body.text, part.text...)
			body.pos = append(body.pos, part.pos...)
		}
	}
	if newline {
		body.add("\n", -1)
	}
	return body, nil
}

func echoStop(value string) int {
	for i := 0; i+1 < len(value); i++ {
		if value[i] == '\\' {
			if value[i+1] == 'c' {
				return i
			}
			i++
		}
	}
	return -1
}

func literalPrintfBody(values []string) (mapped, bool, error) {
	return literalPrintfBodyMode(values, true)
}

func literalPrintfBodyMode(values []string, stopFormat bool) (mapped, bool, error) {
	if len(values) == 0 || strings.HasPrefix(values[0], "-") {
		return mapped{}, false, nil
	}
	format, args := values[0], values[1:]
	if !stopFormat {
		format = bashPrintfFormat(format)
	}
	hasStop := slices.ContainsFunc(values, func(value string) bool { return strings.Contains(value, `\c`) })
	var body mapped
	for {
		current, currentArgs, stopped := format, args, false
		if hasStop {
			current, currentArgs, stopped = printfStop(format, args, stopFormat)
		}
		for i := 0; i < len(current); i++ {
			if current[i] == '%' {
				i++
				if i >= len(current) || !strings.ContainsRune("%sbdiuoxc", rune(current[i])) {
					return mapped{}, false, nil
				}
			}
		}
		text, consumed, err := shExpand.Format(nil, current, bashPrintfArgs(current, currentArgs))
		if err != nil {
			return mapped{}, false, fmt.Errorf("shellcmd: format piped printf: %w", err)
		}
		if len(text) > maxNestedScriptBytes-len(body.text) {
			return mapped{}, false, fmt.Errorf("shellcmd: piped printf exceeds %d bytes", maxNestedScriptBytes)
		}
		body.add(text, -1)
		if stopped || consumed == 0 || consumed >= len(args) {
			break
		}
		args = args[consumed:]
	}
	return body, true, nil
}

// bashEscapeInput приводит восьмеричные escape аргументов echo и %b к формату expand.Format.
func bashEscapeInput(value string, echo bool) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '\\' || i+1 >= len(value) {
			out.WriteByte(value[i])
			i++
			continue
		}
		next := value[i+1]
		if next == '0' || !echo && next >= '1' && next <= '7' {
			start := i + 1
			if next == '0' {
				start++
			}
			end, number := start, 0
			for end < len(value) && end < start+3 && value[end] >= '0' && value[end] <= '7' {
				number = number*8 + int(value[end]-'0')
				end++
			}
			const hex = "0123456789abcdef"
			out.WriteString(`\x`)
			out.WriteByte(hex[(number>>4)&15])
			out.WriteByte(hex[number&15])
			i = end
			continue
		}
		if echo && next >= '1' && next <= '7' {
			out.WriteByte('\\')
		}
		out.WriteByte('\\')
		out.WriteByte(next)
		i += 2
	}
	return out.String()
}

// printfFormatAlternatives определяет, может ли встроенный printf отличаться от внешнего.
func printfFormatAlternatives(format string) bool {
	for i := 0; i+1 < len(format); i++ {
		if format[i] != '\\' {
			continue
		}
		if format[i+1] == 'c' || format[i+1] == '%' {
			return true
		}
		i++
	}
	return false
}

func bashPrintfFormat(format string) string {
	if !strings.Contains(format, `\%`) {
		return format
	}
	var out strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] == '\\' && i+1 < len(format) {
			if format[i+1] == '%' {
				out.WriteByte('\\')
			}
			out.WriteByte(format[i])
			i++
		}
		out.WriteByte(format[i])
	}
	return out.String()
}

func bashPrintfArgs(format string, args []string) []string {
	var result []string
	argument := 0
	for i := 0; i < len(format); i++ {
		if format[i] == '\\' && i+1 < len(format) {
			i++
			continue
		}
		if format[i] != '%' || i+1 >= len(format) {
			continue
		}
		i++
		if format[i] == '%' {
			continue
		}
		if format[i] == 'b' && argument < len(args) {
			value := bashEscapeInput(args[argument], false)
			if value != args[argument] {
				if result == nil {
					result = slices.Clone(args)
				}
				result[argument] = value
			}
		}
		argument++
	}
	if result != nil {
		return result
	}
	return args
}

// printfStop находит первую остановку в формате или в аргументе %b.
func printfStop(format string, args []string, stopFormat bool) (string, []string, bool) {
	argument := 0
	for i := 0; i < len(format); i++ {
		if format[i] == '\\' && i+1 < len(format) {
			if stopFormat && format[i+1] == 'c' {
				return format[:i], args, true
			}
			i++
			continue
		}
		if format[i] != '%' || i+1 >= len(format) {
			continue
		}
		i++
		if format[i] == '%' {
			continue
		}
		if format[i] == 'b' && argument < len(args) {
			if stop := echoStop(args[argument]); stop >= 0 {
				short := slices.Clone(args)
				short[argument] = short[argument][:stop]
				return format[:i+1], short, true
			}
		}
		argument++
	}
	return format, args, false
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
	name, via, accepted := pipedShellNameWithWork(binary.Y, src, vars, nil)
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

func collectCallWalkInfo(call *syntax.CallExpr, source bool, src string, vars map[string]string, info *walkInfo) {
	if source {
		if info.hasProcessSubstitution(call.Args[1:]) {
			info.scriptInputs[call] = true
		}
		return
	}
	if info.hasProcessSubstitution(call.Args) && shellInputTargetWithIndex(call, src, vars, info.processSubstitutions) {
		info.scriptInputs[call] = true
	}
}

// collectScriptInput marks a shell call that runs a script shellcmd has not
// read: from stdin, a descriptor, or as a coprocess.
func collectScriptInput(stmt *syntax.Stmt, call *syntax.CallExpr, file *syntax.File, src string, vars map[string]string, info *walkInfo, stdin *syntax.Redirect, offset int) {
	coproc := info.coprocCalls[call]
	if !info.shellScriptInputCandidates && !coproc {
		return
	}
	redirs := stmt.Redirs
	if info.stdinIndex != nil {
		redirs = info.stdinIndex.redirsAt(offset)
	}
	if info.shellScriptInputCandidates && shellScriptInputForm(call, src, vars, redirs) && !knownPipedScriptInput(stmt, call, src, vars, info) {
		known, source := knownShellScriptInputState(file, stmt, call, src, vars, stdin, redirs)
		if known {
			if source != nil {
				info.knownShellInputs[call] = source
			}
		} else {
			info.scriptInputs[call] = true
		}
	}
	if coproc {
		readsCommand, err := coprocCommandReadsStdin(call, src, vars)
		if err != nil {
			info.errors = append(info.errors, err)
		} else if (readsCommand || shellInputTargetWithIndex(call, src, vars, info.processSubstitutions)) && !knownShellScriptInput(file, stmt, call, src, vars, stdin, redirs) {
			info.scriptInputs[call] = true
		}
	}
}

func knownPipedScriptInput(stmt *syntax.Stmt, call *syntax.CallExpr, src string, vars map[string]string, info *walkInfo) bool {
	args, _ := words(call.Args, src, vars)
	shell, targetArgs, ok := shellInputTargetArgs(args)
	if !ok {
		return false
	}
	operand, command, useStdin := shellArgs(shell, targetArgs)
	if command || !useStdin && operand < len(targetArgs) {
		return false
	}
	for _, redir := range stmt.Redirs {
		if fd, valid := inputRedirFD(redir); valid && fd == 0 {
			return false
		}
	}
	for _, piped := range info.pipedHeredocs {
		if piped.shell == stmt {
			return true
		}
	}
	return false
}

func collectStmtWalkInfo(stmt *syntax.Stmt, file *syntax.File, src string, vars map[string]string, info *walkInfo) {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok {
		return
	}
	collectStmtProcessSubstitutionCandidate(stmt, call, src, info)
	if len(call.Args) == 0 {
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
	collectScriptInput(stmt, call, file, src, vars, info, stdin.redir, offset)
	// The body the shell runs: the source of its script descriptor when the
	// descriptor state names one, else its stdin unless the script is read
	// from another descriptor.
	script := stdin.redir
	if redir := info.knownShellInputs[call]; redir != nil {
		script = redir
	} else if script != nil && shellScriptFromOtherFD(call, src, vars) {
		script = nil
	}
	name, shellVia, input, err := shellInputNameFrom(stmt, src, vars, script, false)
	if err != nil {
		info.errors = append(info.errors, err)
	} else if input {
		info.addShellInput(stmt, script, name, shellVia)
	} else if info.knownShellInputs[call] != nil {
		// The script is a known body that is not parsed as a nested script
		// here: its calls stay unseen.
		info.scriptInputs[call] = true
	}
	if stdin.redir != nil && script != stdin.redir {
		name, shellVia, input, err := shellInputNameFrom(stmt, src, vars, stdin.redir, true)
		if err != nil {
			info.errors = append(info.errors, err)
		} else if input {
			info.addShellInput(stmt, stdin.redir, name, shellVia)
		}
	}
}

// addShellInput records redir as the script of the shell that stmt runs.
func (info *walkInfo) addShellInput(stmt *syntax.Stmt, redir *syntax.Redirect, name string, via []string) {
	info.shellInputs = append(info.shellInputs, heredocShellInput{stmt: stmt, redir: redir, name: name, via: via})
	if !slices.Contains(via, "ssh") && redir != nil && redir.Hdoc != nil {
		info.shellHdocs[redir] = true
		info.localSubstHdocs[redir] = true
	}
}

// shellScriptFromOtherFD reports a shell whose script is a descriptor path
// other than stdin, such as /dev/fd/3: its stdin is data, not the script.
func shellScriptFromOtherFD(call *syntax.CallExpr, src string, vars map[string]string) bool {
	args, _ := words(call.Args, src, vars)
	shell, targetArgs, ok := shellInputTargetArgs(args)
	if !ok {
		return false
	}
	input, stdin := shellScriptInputArgs(targetArgs, shell)
	return input && !stdin
}

func collectStmtProcessSubstitutionCandidate(stmt *syntax.Stmt, call *syntax.CallExpr, src string, info *walkInfo) {
	if !info.hasProcessSubstitutionText {
		return
	}
	possibleShell := len(call.Args) == 0
	if len(call.Args) > 0 {
		first, literal := wordValue(call.Args[0])
		if !literal {
			possibleShell = true
		} else {
			name := commandName(first)
			possibleShell = isShell(name) || wrappers[name]
		}
	}
	if possibleShell {
		for _, redir := range stmt.Redirs {
			if redir.Op.String() != "<" {
				continue
			}
			start, end := int(redir.Word.Pos().Offset()), int(redir.Word.End().Offset())
			if !validSpan(src, start, end) || (!strings.Contains(src[start:end], "<(") && !strings.Contains(src[start:end], ">(")) {
				continue
			}
			index := info.ensureProcessSubstitutionIndex()
			index.stmts = append(index.stmts, stmt)
			break
		}
	}
}

func collectStmtProcessSubstitutionWalkInfo(stmt *syntax.Stmt, src string, vars map[string]string, info *walkInfo) {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok {
		return
	}
	for _, redir := range stmt.Redirs {
		if redir.Op.String() != "<" || !info.hasProcessSubstitution([]*syntax.Word{redir.Word}) {
			continue
		}
		if shellInputTargetWithIndex(call, src, vars, info.processSubstitutions) {
			info.scriptInputs[call] = true
			return
		}
	}
}

func hasShellScriptInputCandidate(src string) bool {
	if shellDashInputCandidate(src) {
		return true
	}
	if strings.Contains(src, "<&") {
		return true
	}
	if !strings.Contains(src, "/dev/") {
		return strings.Contains(src, "/proc/")
	}
	return strings.Contains(src, "/dev/stdin") || strings.Contains(src, "/dev/fd/") || strings.Contains(src, "/proc/")
}

func shellDashInputCandidate(src string) bool {
	if !strings.Contains(src, " -") && !strings.Contains(src, "\t-") && !strings.Contains(src, "\n-") {
		return false
	}
	for _, name := range shellNames {
		for from := 0; from < len(src); {
			rel := strings.Index(src[from:], name)
			if rel < 0 {
				break
			}
			end := from + rel + len(name)
			from = end
			for end < len(src) && (src[end] == ' ' || src[end] == '\t' || src[end] == '\n') {
				end++
			}
			if end < len(src) && src[end] == '-' {
				return true
			}
		}
	}
	return false
}

func needsWalkInfo(src string) bool {
	// A descriptor path is a script source even without a redirection:
	// . /dev/stdin reads the stdin of the enclosing script.
	if strings.ContainsAny(src, "<>&|") || strings.Contains(src, "/dev/") || strings.Contains(src, "/proc/") {
		return true
	}
	for _, name := range []string{"bash", "zsh", "sh", "dash", "timeout", "env", "sudo", "nohup", "nice", "ionice", "stdbuf", "unbuffer", "time", "command", "builtin", "exec", "xargs", "eval", "watch", "trap", "su", "runuser", "flock", "script", "tmux", "ssh", "find", "source", "for", "while", "until", "select"} {
		if strings.Contains(src, name) {
			return true
		}
	}
	return false
}

type pipedHeredoc struct {
	body      *mapped
	redir     *syntax.Redirect
	shell     *syntax.Stmt
	name      string
	via       []string
	alternate bool
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
		if wrapperReadsStdinShell(name, args) {
			return "sh", append(via, name), true
		}
		if isShell(name) {
			operand, command, stdin := shellArgs(name, args)
			return name, via, !shellNoExec(name, args) && !command && (stdin || operand == len(args))
		}
		if target, ok := scan.find(execScanArgs(name, args)); ok {
			via = append(via, name, "exec")
			name, args = commandName(target[0]), target[1:]
			continue
		}
		if shell, ok := implicitPipedShell(name, args); ok {
			return shell, append(via, name), true
		}
		if name == "env" {
			if target, ok := envSplitCommand(args); ok {
				via = append(via, "env")
				name, args = commandName(target[0]), target[1:]
				continue
			}
		}
		if start, ok := sudoDashDashAssignmentStart(name, args); ok {
			via = append(via, name)
			name, args = commandName(args[start]), args[start+1:]
			continue
		}
		if wrappers[name] {
			if !wrapperOptionsKnown(name, args) {
				if start, ok := uncertainWrapperTarget(name, args, true); ok {
					via = append(via, name)
					name, args = commandName(args[start]), args[start+1:]
					continue
				}
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

func implicitPipedShell(name string, args []string) (string, bool) {
	if name == "sudo" && sudoReadsStdin(args) {
		return "bash", true
	}
	if name == "su" && uncertainSuReadsStdin(args) {
		return "sh", true
	}
	return "", false
}

func markPipeline(stmt *syntax.Stmt, src string, vars map[string]string, info *walkInfo) {
	if stmt == nil {
		return
	}
	syntax.Walk(stmt, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if ok && possibleShellInput(call) {
			if shellInputTargetWithIndex(call, src, vars, info.processSubstitutions) {
				info.scriptInputs[call] = true
			}
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

func shellInputTargetWithIndex(call *syntax.CallExpr, src string, vars map[string]string, processSubs *processSubstitutionIndex) bool {
	return shellInputTargetWithIndexAndWork(call, src, vars, processSubs, nil)
}

func shellInputTargetWithIndexAndWork(call *syntax.CallExpr, src string, vars map[string]string, processSubs *processSubstitutionIndex, work *int) bool {
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
		if name == "su" && uncertainSuReadsStdin(args) {
			return true
		}
		if (name == "su" || name == "runuser") && suReadsShell(name, args) {
			return true
		}
		if isShell(name) {
			return shellInputForShellCall(call, name, args, argOffset, processSubs)
		}
		var ok bool
		name, args, argOffset, ok = shellInputNextTargetWithScan(name, args, argOffset, &scan)
		if !ok {
			return false
		}
	}
}

func shellInputForShellCall(call *syntax.CallExpr, shell string, args []string, argOffset int, processSubs *processSubstitutionIndex) bool {
	operand, command, stdin := shellArgs(shell, args)
	if shellNoExec(shell, args) || command {
		return false
	}
	if stdin || operand == len(args) {
		return true
	}
	index := 1 + argOffset + operand
	return operand < len(args) && index < len(call.Args) && processSubs.contains(call.Args[index:index+1])
}

// shellScriptInputFD классифицирует пути, являющиеся ссылкой на открытый дескриптор.
// Обычный путь (включая имя файла "-") возвращает -1.
func shellScriptInputFD(name string) int {
	if strings.HasPrefix(name, "/") {
		name = path.Clean(name)
	}
	if name == "/dev/stdin" {
		return 0
	}
	for _, prefix := range descriptorPathPrefixes {
		if after, ok := strings.CutPrefix(name, prefix); ok {
			n, err := strconv.Atoi(after)
			if err == nil && n >= 0 {
				return n
			}
		}
	}
	return -1
}

var descriptorPathPrefixes = []string{"/dev/fd/", "/proc/self/fd/", "/proc/$$/fd/", "/proc/${$}/fd/"}

// descriptorPathUnresolved reports a descriptor path whose number is a
// substitution, such as /dev/fd/$fd after exec {fd}<: a descriptor of unknown
// content.
func descriptorPathUnresolved(name string) bool {
	for _, prefix := range descriptorPathPrefixes {
		if after, ok := strings.CutPrefix(name, prefix); ok && strings.ContainsAny(after, "$`") {
			return true
		}
	}
	return false
}

// isSourceBuiltin reports source and ., which run a file in the current
// shell: the first operand is the script, stdin is not read for it.
func isSourceBuiltin(name string) bool {
	return name == "source" || name == "."
}

// sourceFileOperand returns the file source and . run: options end at --,
// and -p PATH (bash 5.3) names the search path, not the file. Another option
// is an error, so nothing runs.
func sourceFileOperand(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--":
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		case arg == "-p":
			i++
		case strings.HasPrefix(arg, "-p"):
		case len(arg) > 1 && arg[0] == '-':
			return "", false
		default:
			return arg, true
		}
	}
	return "", false
}

func shellDoubleDashHasFileSemantics(args []string, shell string, operand int) bool {
	if (shell != "sh" && shell != "dash") || operand == 0 || args[operand-1] != "--" {
		return false
	}
	for _, arg := range args[:operand-1] {
		if strings.HasPrefix(arg, "--") {
			return true
		}
	}
	return false
}

// shellScriptFD returns the descriptor a shell, source or . reads its script
// from: 0 for stdin, N for a /dev/fd/N operand. A regular file, a command
// string or no script at all is not a descriptor.
func shellScriptFD(args []string, shell string) (int, bool) {
	if isSourceBuiltin(shell) {
		file, ok := sourceFileOperand(args)
		if !ok {
			return 0, false
		}
		fd := shellScriptInputFD(file)
		return fd, fd >= 0
	}
	operand, command, useStdin := shellArgs(shell, args)
	if command {
		return 0, false
	}
	if useStdin || operand >= len(args) {
		return 0, true
	}
	fd := shellScriptInputFD(args[operand])
	return fd, fd >= 0
}

func shellScriptInputArgs(args []string, shell string) (input, stdin bool) {
	if isSourceBuiltin(shell) {
		file, ok := sourceFileOperand(args)
		if !ok {
			return false, false
		}
		fd := shellScriptInputFD(file)
		return fd >= 0 || descriptorPathUnresolved(file), fd == 0
	}
	operand, command, useStdin := shellArgs(shell, args)
	if command || shellNoExec(shell, args) {
		return false, false
	}
	if useStdin {
		if operand < len(args) {
			return false, true
		}
		if operand > 0 && (args[operand-1] == "-" || args[operand-1] == "--") {
			if shellDoubleDashHasFileSemantics(args, shell, operand) {
				return false, false
			}
			return false, true
		}
		return false, false
	}
	if operand < len(args) {
		fd := shellScriptInputFD(args[operand])
		return fd >= 0 || descriptorPathUnresolved(args[operand]), fd == 0
	}
	if len(args) == 0 {
		return false, true
	}
	return false, false
}

// shellScriptInputForm reports a shell that reads its script from stdin or a
// descriptor; redirs are the redirections that set up its input, those of the
// enclosing statements first.
func shellScriptInputForm(call *syntax.CallExpr, src string, vars map[string]string, redirs []*syntax.Redirect) bool {
	if len(call.Args) == 0 {
		return false
	}
	first, literal := wordValue(call.Args[0])
	if !literal {
		return false
	}
	name := commandName(first)
	if isScriptInputShell(name) {
		return shellScriptInputDirectForm(call, src, vars, name, redirs)
	}
	if !wrappers[name] || !wrapperScriptInputCandidate(call, src, redirs) {
		return false
	}
	return shellScriptInputWrappedForm(call, src, vars, redirs)
}

func wrapperScriptInputCandidate(call *syntax.CallExpr, src string, redirs []*syntax.Redirect) bool {
	start, end := int(call.Pos().Offset()), int(call.End().Offset())
	if !validSpan(src, start, end) {
		return false
	}
	raw := src[start:end]
	if hasUnknownStdinRedir(redirs) {
		return true
	}
	return strings.Contains(raw, "/dev/") || strings.Contains(raw, "/proc/") || shellDashInputCandidate(raw)
}

func shellScriptInputDirectForm(call *syntax.CallExpr, src string, vars map[string]string, shell string, redirs []*syntax.Redirect) bool {
	start, end := int(call.Pos().Offset()), int(call.End().Offset())
	if validSpan(src, start, end) {
		raw := src[start:end]
		if !strings.Contains(raw, "/dev/") && !strings.Contains(raw, "/proc/") && !strings.Contains(raw, " -") && !strings.Contains(raw, "\t-") && !strings.Contains(raw, "\n-") && !hasUnknownStdinRedir(redirs) {
			return false
		}
	}
	args, _ := words(call.Args[1:], src, vars)
	input, stdin := shellScriptInputArgs(args, shell)
	return input || stdin || shellReadsImplicitStdin(args, shell) && hasUnknownStdinRedir(redirs)
}

func coprocCommandReadsStdin(call *syntax.CallExpr, src string, vars map[string]string) (bool, error) {
	args, _ := words(call.Args, src, vars)
	if len(args) < 3 || !isScriptInputShell(commandName(args[0])) {
		return false, nil
	}
	operand, command, _ := shellArgs(commandName(args[0]), args[1:])
	if !command || operand >= len(args[1:]) {
		return false, nil
	}
	inner, err := Calls(args[1:][operand])
	if err != nil {
		return false, err
	}
	read, eval := false, false
	for i := range inner {
		nested := &inner[i]
		read = read || nested.Name == "read" || nested.Name == "mapfile" || nested.Name == "readarray"
		eval = eval || nested.Name == "eval"
	}
	return read && eval, nil
}

func shellScriptInputWrappedForm(call *syntax.CallExpr, src string, vars map[string]string, redirs []*syntax.Redirect) bool {
	args, _ := words(call.Args, src, vars)
	shell, args, ok := shellInputTargetArgs(args)
	if !ok {
		return false
	}
	input, stdin := shellScriptInputArgs(args, shell)
	return input || stdin || shellReadsImplicitStdin(args, shell) && hasUnknownStdinRedir(redirs)
}

// shellReadsImplicitStdin reports a shell given only options, -s among them:
// it reads its script from stdin without naming it, so only where stdin comes
// from decides.
func shellReadsImplicitStdin(args []string, shell string) bool {
	if isSourceBuiltin(shell) {
		return false
	}
	operand, command, _ := shellArgs(shell, args)
	return !command && operand >= len(args) && !shellNoExec(shell, args)
}

func shellInputTargetArgs(args []string) (string, []string, bool) {
	if len(args) == 0 {
		return "", nil, false
	}
	name, args := commandName(args[0]), args[1:]
	for name != "" && !isScriptInputShell(name) {
		var ok bool
		name, args, _, ok = shellInputNextTarget(name, args, 0)
		if !ok {
			return "", nil, false
		}
	}
	return name, args, isScriptInputShell(name)
}

// hasUnknownStdinRedir reports a stdin redirection from another descriptor
// (<&N, /dev/fd/N) or from a name known only at run time, which may be one;
// a literal file name is not unknown. Whether a later redirection or an
// earlier exec makes stdin known is decided by knownShellScriptInput.
func hasUnknownStdinRedir(redirs []*syntax.Redirect) bool {
	for _, redir := range redirs {
		if fd, valid := inputRedirFD(redir); !valid || fd != 0 {
			continue
		}
		target, literal := redirectTarget(redir)
		switch redir.Op.String() {
		case "<&":
			if !literal || target != "0" && target != "-" {
				return true
			}
		case "<", "<>":
			if !literal || shellScriptInputFD(target) > 0 {
				return true
			}
		}
	}
	return false
}

// redirectTarget returns a redirection word and whether it is literal: a
// substitution, a glob and a leading tilde are expanded at run time.
func redirectTarget(redir *syntax.Redirect) (string, bool) {
	target, literal := wordValue(redir.Word)
	return target, literal && !strings.ContainsAny(target, "*?[") && !strings.HasPrefix(target, "~")
}

func knownShellScriptInput(file *syntax.File, stmt *syntax.Stmt, call *syntax.CallExpr, src string, vars map[string]string, stdin *syntax.Redirect, redirs []*syntax.Redirect) bool {
	known, _ := knownShellScriptInputState(file, stmt, call, src, vars, stdin, redirs)
	return known
}

func knownShellScriptInputState(file *syntax.File, stmt *syntax.Stmt, call *syntax.CallExpr, src string, vars map[string]string, stdin *syntax.Redirect, redirs []*syntax.Redirect) (bool, *syntax.Redirect) {
	args, _ := words(call.Args, src, vars)
	shell, targetArgs, ok := shellInputTargetArgs(args)
	if !ok {
		return false, nil
	}
	fd, ok := shellScriptFD(targetArgs, shell)
	if !ok {
		return false, nil
	}
	known, set, sources := knownExecFDStates(file, int(stmt.Pos().Offset()))
	// Stdin from an enclosing command or pipe is open before the command's own
	// redirections; its own stdin redirection takes its place among them.
	if stdin != nil && !slices.Contains(redirs, stdin) {
		applyKnownInputRedirect(stdin, 0, known, set, sources)
	}
	// Redirections apply left to right: <&3 before 3<<E copies the earlier fd 3.
	for _, redir := range redirs {
		dest, valid := inputRedirFD(redir)
		if valid {
			applyKnownInputRedirect(redir, dest, known, set, sources)
		}
	}
	return set[fd] && known[fd], sources[fd]
}

func knownExecFDStates(file *syntax.File, before int) (map[int]bool, map[int]bool, map[int]*syntax.Redirect) {
	known, set, sources := make(map[int]bool), make(map[int]bool), make(map[int]*syntax.Redirect)
	for _, stmt := range file.Stmts {
		if int(stmt.Pos().Offset()) >= before {
			break
		}
		directExec := false
		if call, ok := stmt.Cmd.(*syntax.CallExpr); ok && len(call.Args) > 0 {
			name, literal := wordValue(call.Args[0])
			directExec = literal && commandName(name) == "exec"
		}
		if !directExec {
			nestedExec := false
			syntax.Walk(stmt, func(node syntax.Node) bool {
				call, ok := node.(*syntax.CallExpr)
				if ok && len(call.Args) > 0 {
					name, literal := wordValue(call.Args[0])
					nestedExec = nestedExec || literal && commandName(name) == "exec"
				}
				return !nestedExec
			})
			if nestedExec {
				clear(known)
				clear(set)
				clear(sources)
			}
			continue
		}
		for _, redir := range stmt.Redirs {
			fd, valid := inputRedirFD(redir)
			if valid {
				applyKnownInputRedirect(redir, fd, known, set, sources)
			}
		}
	}
	return known, set, sources
}

func applyKnownInputRedirect(redir *syntax.Redirect, dest int, known, set map[int]bool, sources map[int]*syntax.Redirect) {
	target, literal := redirectTarget(redir)
	switch redir.Op.String() {
	case "<<", "<<-", "<<<":
		known[dest] = true
		set[dest] = true
		sources[dest] = redir
	case "<", "<>":
		if literal {
			if source := shellScriptInputFD(target); source >= 0 {
				known[dest] = set[source] && known[source]
				set[dest] = true
				sources[dest] = sources[source]
				return
			}
		}
		// Only a literal path is known: a substituted name may expand to
		// /dev/stdin or a descriptor path at run time.
		known[dest] = literal && !strings.Contains(target, "<(") && !strings.Contains(target, ">(") && !strings.Contains(target, "/dev/fd/") && !strings.Contains(target, "/proc/")
		set[dest] = true
		sources[dest] = nil
	case "<&", ">&":
		source, ok := shellFDNumber(target)
		known[dest] = literal && ok && set[source] && known[source]
		set[dest] = true
		if literal && ok {
			sources[dest] = sources[source]
		} else {
			sources[dest] = nil
		}
	}
}

func inputRedirFD(redir *syntax.Redirect) (int, bool) {
	if redir.N == nil || redir.N.Value == "" {
		switch redir.Op.String() {
		case ">", ">&", ">>", ">|", "&>", "&>>":
			return 1, true
		}
		return 0, true
	}
	return shellFDNumber(redir.N.Value)
}

func shellFDNumber(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	number, maxInt := 0, int(^uint(0)>>1)
	for i := 0; i < len(value); i++ {
		digit := int(value[i] - '0')
		if digit < 0 || digit > 9 || number > (maxInt-digit)/10 {
			return 0, false
		}
		number = number*10 + digit
	}
	return number, true
}

func shellInputNextTarget(name string, args []string, argOffset int) (string, []string, int, bool) {
	return shellInputNextTargetWithScan(name, args, argOffset, &wrapperExecScan{})
}

func shellInputNextTargetWithScan(name string, args []string, argOffset int, scan *wrapperExecScan) (string, []string, int, bool) {
	if wrapperReadsStdinShell(name, args) {
		return "sh", nil, argOffset, true
	}
	if target, ok := scan.find(execScanArgs(name, args)); ok {
		argOffset += len(args) - len(target) + 1
		return commandName(target[0]), target[1:], argOffset, true
	}
	if start, ok := sudoDashDashAssignmentStart(name, args); ok {
		argOffset += start + 1
		return commandName(args[start]), args[start+1:], argOffset, true
	}
	if wrappers[name] {
		if !wrapperOptionsKnown(name, args) {
			if start, ok := uncertainWrapperTarget(name, args, true); ok {
				return commandName(args[start]), args[start+1:], argOffset + start + 1, true
			}
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
	src          string
	sourceLen    int
	lineBase     int
	inherited    []string
	vars         map[string]string
	calls        *[]Call
	info         *walkInfo
	seen         *callSet
	budget       *nestedParseBudget
	ignoredCalls [][2]int
	subst        *mapped
	parseErr     error
	nestedErrs   []error
}

func (w *callWalker) visit(node syntax.Node) bool {
	if w.budget.trackWork {
		w.budget.visitedNodes++
	}

	if w.parseErr != nil {
		return false
	}
	switch node.(type) {
	case *syntax.CmdSubst, *syntax.ProcSubst:
		if w.outerSubstitution(node) {
			return false
		}
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
		start := int(call.Pos().Offset())
		for _, span := range w.ignoredCalls {
			if start >= span[0] && start < span[1] {
				return false
			}
		}
		w.visitCall(call)
		if w.parseErr != nil {
			return false
		}
	}
	return true
}

// outerSubstitution reports whether this node was already run by the outer shell.
func (w *callWalker) outerSubstitution(node syntax.Node) bool {
	if w.subst == nil {
		return false
	}
	offset := outerOffset(int(node.Pos().Offset()), w.subst.pos)
	i := sort.Search(len(w.subst.subst), func(i int) bool {
		if w.budget.trackWork {
			w.budget.substSteps++
		}
		return w.subst.subst[i][1] > offset
	})
	return i < len(w.subst.subst) && offset >= w.subst.subst[i][0]
}

func (w *callWalker) visitRedirect(redirect *syntax.Redirect) {
	if redirect.Hdoc != nil && redirect.Word != nil {
		quoted := quotedHeredoc(redirect.Word) || strings.Contains(nodeText(redirect.Word, w.src), `\`)
		line, column, err := swallowedHeredocDelimiter(redirect, w.src[:w.sourceLen], quoted)
		if err != nil {
			w.parseErr = fmt.Errorf("%d:%d: heredoc delimiter: %w", line+w.lineBase, column, err)
			return
		}
		if line > 0 {
			w.parseErr = fmt.Errorf("%d:%d: heredoc delimiter occurs inside parsed body", line+w.lineBase, column)
			return
		}
	}
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
		braceMode, known := w.info.braceCalls[call]
		if !known {
			braceMode = true
		}
		w.info.redirs[call] = stmtRedirs(stmt.Redirs, w.src, braceMode, w.info.unknownBrace[call])

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
	words, err := cmdWords(call.Args, w.src, needsScripts, literals)
	if err != nil {
		w.parseErr = fmt.Errorf("shellcmd: expand command word: %w", err)
		return
	}
	if commandName(args[0]) == "eval" {
		for i := 1; i < len(call.Args); i++ {
			if !literals[i] {
				var text mapped
				if err := text.expandParts(call.Args[i].Parts, w.src, w.vars, partUnquoted); err != nil {
					w.parseErr = err
					return
				}
				words[i].script, words[i].pos, words[i].subst = text.String(), text.pos, text.subst
			}
		}
	}
	ws, tildeArgs := markedCallWords(call, words, split, w.src, w.info, w.budget.initialPosix)
	posixEnv := hasPosixEnvironment(call)
	mode := shellModes{posix: w.info.posixCalls[call], posixUnknown: w.info.unknownPosix[call], brace: w.info.braceCalls[call], braceUnknown: w.info.unknownBrace[call]}
	c := cmd{call: Call{Name: commandWordName(args[0], ws[0]), Args: args[1:], Raw: w.src[start:end], Line: int(call.Pos().Line()) + w.lineBase, Via: via, Offset: ws[0].offset, Redirs: w.info.redirs[call], tildeArgs: tildeArgs, Loop: w.info.loopCalls[call], Background: w.info.backgroundCalls[call], ScriptInput: w.info.scriptInputs[call]}, posixEnv: posixEnv, mode: mode, words: ws, budget: w.budget, source: w.src, stdinInput: w.info.stdinInputs[call]}
	for _, part := range call.Args[0].Parts {
		if proc, ok := part.(*syntax.ProcSubst); ok && w.outerSubstitution(proc) {
			c.call.Name = ""
			break
		}
	}
	setLiteralArgs(&c, call, w.src, w.info, w.budget)
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

func markedCallWords(call *syntax.CallExpr, words []word, split int, src string, info *walkInfo, initialPosix bool) ([]word, []Tilde) {
	posix, ok := info.posixCalls[call]
	if !ok {
		posix = initialPosix
	}
	braceExpand, ok := info.braceCalls[call]
	if !ok {
		braceExpand = true
	}
	for i := 1; i < len(call.Args); i++ {
		words[i].tilde = wordTildeWithMode(call.Args[i], src, false, true, posix, braceExpand, info.unknownPosix[call], info.unknownBrace[call])
	}
	ws := splitWords(words, split)
	marks := make([]Tilde, len(ws)-1)
	for i := 1; i < len(ws); i++ {
		marks[i-1] = ws[i].tilde
	}
	if !slices.ContainsFunc(marks, func(mark Tilde) bool { return mark != 0 }) {
		marks = nil
	}
	return ws, marks
}

func wordTildeWithMode(word *syntax.Word, src string, assignment, argumentAssignment, posix, braceExpand, posixUnknown, braceUnknown bool) Tilde {
	basePosix := posix && !posixUnknown
	baseBrace := braceExpand || braceUnknown
	base := wordTildeOptions(word, src, assignment, argumentAssignment, basePosix, baseBrace)
	if posixUnknown {
		alternate := wordTildeOptions(word, src, assignment, argumentAssignment, true, baseBrace)
		if alternate != base {
			base |= TildeModeUnknown
		}
	}
	if braceUnknown {
		alternate := wordTildeOptions(word, src, assignment, argumentAssignment, basePosix, false)
		if alternate != base&^TildeModeUnknown {
			base |= TildeModeUnknown
		}
	}
	return base
}

func hasPosixEnvironment(call *syntax.CallExpr) bool {
	for _, assign := range call.Assigns {
		if assign.Name != nil && assign.Name.Value == "POSIXLY_CORRECT" {
			return true
		}
	}
	return false
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
	case *syntax.CoprocClause:
		mark(n.Stmt, info.coprocCalls)
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

// swallowedHeredocDelimiter compares the parser's body with Bash delimiter lines.
func swallowedHeredocDelimiter(redirect *syntax.Redirect, src string, quoted bool) (int, int, error) {
	delim, err := heredocDelimiter(redirect.Word)
	if err != nil {
		return int(redirect.Pos().Line()), int(redirect.Pos().Col()), err
	}
	start, end := int(redirect.Hdoc.Pos().Offset()), min(int(redirect.Hdoc.End().Offset()), len(src))
	if !validSpan(src, start, end) {
		return 0, 0, nil
	}
	start = heredocScanStart(redirect, src, start, quoted)
	body := src[start:end]
	if !quoted && !strings.Contains(body, "\\\n") {
		return 0, 0, nil
	}
	line := int(redirect.Hdoc.Pos().Line())
	if start < int(redirect.Hdoc.Pos().Offset()) {
		line--
	}
	match := delimMatch{delim: delim, possible: true}
	dash := redirect.Op == syntax.DashHdoc
	joining := false
	for offset := start; offset < end; line++ {
		text, rest, found := strings.Cut(src[offset:end], "\n")
		lineEnd := offset + len(text)
		text, column := match.trimLine(text, dash, joining)
		continued := !quoted && found && oddTrailingBackslashes(text)
		if continued {
			match.add(text[:len(text)-1])
		} else {
			match.add(text)
			if delimiterLineDisagrees(match.complete(), text, delim, dash && joining, lineEnd == end, found && lineEnd+1 == len(src)) {
				return line, column, nil
			}
			match.reset()
		}
		joining = continued
		if !found {
			break
		}
		offset = end - len(rest)
	}
	return 0, 0, nil
}

// delimMatch follows Bash's comparison of a joined body line with the heredoc delimiter.
type delimMatch struct {
	delim    string
	matched  int
	possible bool
}

func (m *delimMatch) add(fragment string) {
	if m.possible && m.matched+len(fragment) <= len(m.delim) && m.delim[m.matched:m.matched+len(fragment)] == fragment {
		m.matched += len(fragment)
		return
	}
	m.possible = false
}

func (m *delimMatch) complete() bool { return m.possible && m.matched == len(m.delim) }

func (m *delimMatch) reset() { m.matched, m.possible = 0, true }

// trimLine drops leading tabs for <<- where Bash does: at the start of a joined line, and after a continuation that matched nothing yet.
func (m *delimMatch) trimLine(text string, dash, joining bool) (string, int) {
	if !dash || joining && !(m.possible && m.matched == 0) {
		return text, 1
	}
	trimmed := strings.TrimLeft(text, "\t")
	return trimmed, 1 + len(text) - len(trimmed)
}

func oddTrailingBackslashes(text string) bool {
	return (len(text)-len(strings.TrimRight(text, `\`)))%2 == 1
}

// delimiterLineDisagrees reports whether Bash and the parser disagree on whether a complete body line ends the heredoc.
func delimiterLineDisagrees(bashDelimiter bool, text, delim string, trimTabs, bodyEnd, lastLine bool) bool {
	if trimTabs {
		text = strings.TrimLeft(text, "\t")
	}
	parserDelimiter := bodyEnd && text == delim
	return bashDelimiter != parserDelimiter && !(bashDelimiter && lastLine)
}

func heredocScanStart(redirect *syntax.Redirect, src string, start int, quoted bool) int {
	if !quoted && start > 0 && src[start-1] == '\n' {
		previous := strings.LastIndexByte(src[:start-1], '\n') + 1
		text := src[previous : start-1]
		if redirect.Op == syntax.DashHdoc {
			text = strings.TrimLeft(text, "\t")
		}
		if text == `\` {
			start = previous
		}
	}
	return start
}

func heredocDelimiter(word *syntax.Word) (string, error) {
	var value strings.Builder
	for _, part := range word.Parts {
		if quoted, ok := part.(*syntax.SglQuoted); ok && quoted.Dollar {
			if strings.Contains(quoted.Value, `\u`) || strings.Contains(quoted.Value, `\U`) {
				return "", fmt.Errorf("locale-dependent ANSI-C heredoc delimiter")
			}
			var decoded mapped
			if err := decoded.unescapeANSI(quoted.Value, -1); err != nil {
				return "", err
			}
			if strings.ContainsAny(decoded.String(), "\x01\x7f") {
				return "", fmt.Errorf("ambiguous ANSI-C heredoc delimiter")
			}
			value.WriteString(decoded.String())
			continue
		}
		piece, ok := wordValue(&syntax.Word{Parts: []syntax.WordPart{part}})
		if !ok {
			return "", fmt.Errorf("unsupported heredoc delimiter")
		}
		value.WriteString(piece)
	}
	return value.String(), nil
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
		vars[assign.Name.Value] = assignValue(assign, src)
	}
}

// assignValue is the literal value of an assignment, or its source text without quotes
// when the value has an expansion.
func assignValue(assign *syntax.Assign, src string) string {
	value, literal := wordValue(assign.Value)
	if !literal {
		value = unquotedText(assign.Value, src)
	}
	return value
}

// tildeChar is one source byte together with the context that controls expansion.
type tildeChar struct {
	b        byte
	eligible bool
	dynamic  bool
	offset   int
}

func wordTilde(word *syntax.Word, src string, assignment, argumentAssignment, posix bool) Tilde {
	return wordTildeOptions(word, src, assignment, argumentAssignment, posix, true)
}

func wordTildeOptions(word *syntax.Word, src string, assignment, argumentAssignment, posix, braceExpand bool) Tilde {
	if word == nil {
		return 0
	}
	chars := make([]tildeChar, 0, len(word.Parts)*4)
	var nested Tilde
	for _, part := range word.Parts {
		partChars, partNested := tildePartCharsMode(part, src, assignment, posix)
		chars = append(chars, partChars...)
		nested |= partNested
	}
	argAssign := false
	if argumentAssignment {
		chars, argAssign = assignmentArgumentTilde(word, src, chars, posix)
		assignment = argAssign
	}
	if braceExpand && argAssign && assignmentTildeBeforeBrace(word, src, chars) {
		return nested
	}
	if braceExpand && argAssign && wordHasBraceExpansion(word) {
		return nested
	}
	braceExpand = braceExpand && !assignment && !argAssign
	return nested | scanTildeWord(word, src, chars, assignment, braceExpand)
}

func wordHasBraceExpansion(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	_, split := splitBraces(word)
	return split
}

// Bytes that stand for the braces of a group syntax.SplitBraces would return to
// literal text; splitBraces turns them back into braces after the split.
const (
	literalLeftBrace  = '\x01'
	literalRightBrace = '\x02'
)

// splitBraces is syntax.SplitBraces on a copy of word in linear time. SplitBraces
// copies the parts of a group without a separator, or of an unclosed one, into
// the enclosing element, so nested groups cost the square of their depth; their
// braces are hidden from it as literalLeftBrace and literalRightBrace first.
func splitBraces(word *syntax.Word) (syntax.Word, bool) {
	copyWord := *word
	copyWord.Parts = slices.Clone(word.Parts)
	literal := literalBraceGroups(word.Parts)
	if literal != nil {
		for i, offsets := range literal {
			part, ok := copyWord.Parts[i].(*syntax.Lit)
			if !ok {
				continue
			}
			lit := *part
			value := []byte(lit.Value)
			for _, j := range offsets {
				if value[j] == '{' {
					value[j] = literalLeftBrace
				} else {
					value[j] = literalRightBrace
				}
			}
			lit.Value = string(value)
			copyWord.Parts[i] = &lit
		}
	}
	split := syntax.SplitBraces(&copyWord)
	if literal != nil {
		restoreLiteralBraces(copyWord.Parts)
	}
	return copyWord, split
}

func restoreLiteralBraces(parts []syntax.WordPart) {
	restore := strings.NewReplacer(string(rune(literalLeftBrace)), "{", string(rune(literalRightBrace)), "}")
	var walk func(parts []syntax.WordPart)
	walk = func(parts []syntax.WordPart) {
		for i, part := range parts {
			switch p := part.(type) {
			case *syntax.Lit:
				if strings.ContainsAny(p.Value, string([]byte{literalLeftBrace, literalRightBrace})) {
					lit := *p
					lit.Value = restore.Replace(p.Value)
					parts[i] = &lit
				}
			case *syntax.BraceExp:
				for _, elem := range p.Elems {
					walk(elem.Parts)
				}
			}
		}
	}
	walk(parts)
}

// literalBraceGroups finds, as syntax.SplitBraces pairs them, the braces of the
// groups it would return to literal text: a group without a comma or a `..` at
// its own level and a group never closed. It returns the byte offsets by part
// index, or nil when there are none or a Lit already holds a stand-in byte.
func literalBraceGroups(parts []syntax.WordPart) map[int][]int {
	type position struct{ part, index int }
	type group struct {
		open      position
		separated bool
	}
	var open []group
	var literal []position
	for i, part := range parts {
		lit, ok := part.(*syntax.Lit)
		if !ok {
			continue
		}
		if strings.ContainsAny(lit.Value, string([]byte{literalLeftBrace, literalRightBrace})) {
			return nil
		}
		value := lit.Value
		for j := 0; j < len(value); j++ {
			switch value[j] {
			case '\\':
				j++
			case '{':
				open = append(open, group{open: position{i, j}})
			case ',':
				if len(open) > 0 {
					open[len(open)-1].separated = true
				}
			case '.':
				// A `..` separates a sequence; after a comma it is literal, but
				// the group is separated already.
				if len(open) > 0 && j+1 < len(value) && value[j+1] == '.' {
					open[len(open)-1].separated = true
				}
			case '}':
				if len(open) == 0 {
					continue
				}
				top := open[len(open)-1]
				open = open[:len(open)-1]
				if !top.separated {
					literal = append(literal, top.open, position{i, j})
				}
			}
		}
	}
	for _, unclosed := range open {
		literal = append(literal, unclosed.open)
	}
	if len(literal) == 0 {
		return nil
	}
	byPart := make(map[int][]int)
	for _, p := range literal {
		byPart[p.part] = append(byPart[p.part], p.index)
	}
	return byPart
}

func assignmentTildeBeforeBrace(word *syntax.Word, src string, chars []tildeChar) bool {
	if _, split := splitBraces(word); !split {
		return false
	}
	tilde := len(src) + 1
	for _, ch := range chars {
		if ch.b == '~' && ch.eligible {
			tilde = ch.offset
			break
		}
	}
	if tilde > len(src) {
		return false
	}
	for _, ch := range chars {
		if ch.b == '{' && ch.eligible && ch.offset > tilde {
			return true
		}
	}
	return false
}

func assignmentTildeInsideBrace(word *syntax.Word, src string) bool {
	copyWord, split := splitBraces(word)
	if !split {
		return false
	}
	var tildeOffsets []int
	for _, part := range word.Parts {
		chars, _ := tildePartCharsMode(part, src, true, false)
		for _, ch := range chars {
			if ch.b == '~' && ch.eligible {
				tildeOffsets = append(tildeOffsets, ch.offset)
			}
		}
	}
	return braceContainsAnyOffset(copyWord.Parts, tildeOffsets)
}

func braceContainsAnyOffset(parts []syntax.WordPart, offsets []int) bool {
	for _, part := range parts {
		brace, ok := part.(*syntax.BraceExp)
		if !ok {
			continue
		}
		lo, hi := int(brace.Pos().Offset()), int(brace.End().Offset())
		for _, offset := range offsets {
			if offset >= lo && offset < hi {
				return true
			}
		}
		for _, elem := range brace.Elems {
			if braceContainsAnyOffset(elem.Parts, offsets) {
				return true
			}
		}
	}
	return false
}

func tildePartCharsMode(part syntax.WordPart, src string, assignment, posix bool) ([]tildeChar, Tilde) {
	switch p := part.(type) {
	case *syntax.Lit:
		return tildeSourceSpan(p, src, true), 0
	case *syntax.SglQuoted:
		return tildeSourceSpan(part, src, false), 0
	case *syntax.DblQuoted:
		var nested Tilde
		for _, child := range p.Parts {
			if param, ok := child.(*syntax.ParamExp); ok && param.Repl != nil && param.Repl.With != nil {
				nested |= wordTilde(param.Repl.With, src, false, false, posix)
			}
		}
		return tildeSourceSpan(part, src, false), nested
	case *syntax.ParamExp:
		var nested Tilde
		if p.Exp != nil && p.Exp.Word != nil {
			nested = wordTilde(p.Exp.Word, src, assignment, false, posix)
		}
		if p.Repl != nil && p.Repl.With != nil {
			nested |= wordTilde(p.Repl.With, src, false, false, posix)
		}
		return []tildeChar{{b: '$', eligible: false, dynamic: true, offset: int(p.Pos().Offset())}}, nested
	case *syntax.ExtGlob:
		// A pattern is no user name: inside a tilde prefix it keeps the tilde.
		chars := tildeSourceSpan(part, src, true)
		for i := range chars {
			chars[i].dynamic = true
		}
		return chars, 0
	case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp:
		return []tildeChar{{b: '$', eligible: false, dynamic: true, offset: int(part.Pos().Offset())}}, 0
	default:
		return tildeSourceSpan(part, src, false), 0
	}
}

// scanTildeWord asks syntax.SplitBraces for Bash brace alternatives, then scans
// their paths with a bounded set of states instead of constructing their products.
func scanTildeWord(word *syntax.Word, src string, chars []tildeChar, assignment, braceExpand bool) Tilde {
	if !slices.ContainsFunc(chars, func(ch tildeChar) bool { return ch.b == '~' }) {
		return 0
	}
	if !braceExpand {
		return scanTildeCharsMode(chars, assignment)
	}
	if !hasBraceListExpansion(chars) {
		return scanTildeCharsMode(chars, assignment)
	}
	if !assignment && !braceExpansionCanAffectLeadingTilde(chars) {
		return scanTildeCharsMode(chars, assignment)
	}
	copyWord, split := splitBraces(word)
	if !split {
		return scanTildeCharsMode(chars, assignment)
	}
	states := scanTildePartsFrom(copyWord.Parts, src, assignment, map[tildeScanState]struct{}{{start: true}: {}})
	return tildeStatesMarks(states)
}

func hasBraceListExpansion(chars []tildeChar) bool {
	commaAtDepth := make([]bool, 0, 8)
	for i := 0; i < len(chars); i++ {
		ch, skip := consumeTildeChar(chars, &i)
		if skip || !ch.eligible {
			continue
		}
		switch ch.b {
		case '{':
			commaAtDepth = append(commaAtDepth, false)
		case ',':
			if len(commaAtDepth) > 0 {
				commaAtDepth[len(commaAtDepth)-1] = true
			}
		case '}':
			if len(commaAtDepth) > 0 {
				hasComma := commaAtDepth[len(commaAtDepth)-1]
				commaAtDepth = commaAtDepth[:len(commaAtDepth)-1]
				if hasComma {
					return true
				}
			}
		}
	}
	return false
}

// braceExpansionCanAffectLeadingTilde reports whether a brace list can expose
// a tilde at a word start. A literal prefix, or a terminated tilde prefix,
// makes later braces irrelevant to tilde expansion.
func braceExpansionCanAffectLeadingTilde(chars []tildeChar) bool {
	i := 0
	for i < len(chars) {
		ch, skip := consumeTildeChar(chars, &i)
		if skip {
			i++
			continue
		}
		if ch.b == '~' && ch.eligible {
			return braceFollowsTildePrefix(chars, i+1)
		}
		if ch.b == '{' && ch.eligible {
			return !leadingBraceIsLiteral(chars)
		}
		return false
	}
	return false
}

func braceFollowsTildePrefix(chars []tildeChar, i int) bool {
	for i < len(chars) {
		prefix, continued := consumeTildeChar(chars, &i)
		if continued {
			i++
			continue
		}
		if !prefix.eligible || strings.ContainsRune("\\$`'\"", rune(prefix.b)) || prefix.b == '/' || prefix.b == ':' {
			return false
		}
		if prefix.b == '{' {
			return true
		}
		i++
	}
	return false
}

// leadingBraceIsLiteral recognizes a leading singleton brace group without
// flattening its nested parts through syntax.SplitBraces.
func leadingBraceIsLiteral(chars []tildeChar) bool {
	first := 0
	for first < len(chars) {
		ch, skip := consumeTildeChar(chars, &first)
		if skip {
			first++
			continue
		}
		if ch.b != '{' || !ch.eligible {
			return false
		}
		break
	}
	if first >= len(chars) {
		return false
	}
	depth, rootComma := 0, false
	for i := first; i < len(chars); i++ {
		ch, skip := consumeTildeChar(chars, &i)
		if skip {
			continue
		}
		if !ch.eligible {
			continue
		}
		switch ch.b {
		case '{':
			depth++
		case ',':
			if depth == 1 {
				rootComma = true
			}
		case '}':
			depth--
			if depth == 0 {
				return !rootComma
			}
		}
	}
	return depth > 0
}

func tildeStatesMarks(states map[tildeScanState]struct{}) Tilde {
	var marks Tilde
	for state := range states {
		marks |= state.marks | finishTildePrefix(state)
	}
	return marks
}

type tildeScanState struct {
	start           bool
	colon           bool
	pending         uint8
	prefixKind      uint8
	prefixDelimited bool
	prefixInvalid   bool
	marks           Tilde
}

func scanTildePartsFrom(parts []syntax.WordPart, src string, assignment bool, states map[tildeScanState]struct{}) map[tildeScanState]struct{} {
	for _, part := range parts {
		if !assignment {
			if brace, ok := part.(*syntax.BraceExp); ok && tildeStatesPastStart(states) && bracePrefixSafe(brace) {
				continue
			}
		}
		switch p := part.(type) {
		case *syntax.BraceExp:
			states = scanTildeBraceStates(states, p, src, assignment)
		case *syntax.Lit:
			states = scanTildeText(states, tildeValueChars(p.Value), assignment)
		default:
			chars, nested := tildePartCharsMode(part, src, assignment, false)
			states = scanTildeText(states, chars, assignment)
			if nested != 0 {
				marked := make(map[tildeScanState]struct{}, len(states))
				for state := range states {
					state.marks |= nested
					marked[state] = struct{}{}
				}
				states = marked
			}
		}
	}
	return states
}

func tildeStatesPastStart(states map[tildeScanState]struct{}) bool {
	for state := range states {
		if state.start || state.pending != 0 && state.prefixKind != 2 {
			return false
		}
	}
	return true
}

func bracePrefixSafe(brace *syntax.BraceExp) bool {
	for _, elem := range brace.Elems {
		for _, part := range elem.Parts {
			lit, ok := part.(*syntax.Lit)
			if !ok {
				return false
			}
			for i := 0; i < len(lit.Value); i++ {
				if strings.ContainsRune("\\$`'\"", rune(lit.Value[i])) {
					return false
				}
				if lit.Value[i] == '\\' {
					i++
				}
			}
		}
	}
	return true
}

func tildeValueChars(value string) []tildeChar {
	chars := make([]tildeChar, len(value))
	for i := range value {
		chars[i] = tildeChar{b: value[i], eligible: true}
	}
	return chars
}

func scanTildeBraceStates(states map[tildeScanState]struct{}, brace *syntax.BraceExp, src string, assignment bool) map[tildeScanState]struct{} {
	next := make(map[tildeScanState]struct{})
	for state := range states {
		for _, elem := range brace.Elems {
			branch := scanTildePartsFrom(elem.Parts, src, assignment, map[tildeScanState]struct{}{state: {}})
			for result := range branch {
				next[result] = struct{}{}
			}
		}
	}
	return next
}

func scanTildeText(states map[tildeScanState]struct{}, chars []tildeChar, assignment bool) map[tildeScanState]struct{} {
	return scanTildeTextMode(states, chars, assignment)
}

func scanTildeTextMode(states map[tildeScanState]struct{}, chars []tildeChar, assignment bool) map[tildeScanState]struct{} {
	if len(states) == 1 {
		var state tildeScanState
		for candidate := range states {
			state = candidate
			break
		}
		state = scanTildeCharsState(state, chars, assignment)
		return map[tildeScanState]struct{}{state: {}}
	}
	next := make(map[tildeScanState]struct{}, len(states))
	for i := 0; i < len(chars); i++ {
		ch, skip := consumeTildeChar(chars, &i)
		if skip {
			continue
		}
		clear(next)
		for state := range states {
			next[advanceTildeScan(state, ch, assignment)] = struct{}{}
		}
		states, next = next, states
	}
	return states
}

func advanceTildeScan(state tildeScanState, ch tildeChar, assignment bool) tildeScanState {
	if state.pending != 0 {
		state = advanceTildePrefix(state, ch, assignment)
	}
	if state.pending == 0 && ch.b == '~' && ch.eligible && (state.start || assignment && state.colon) {
		state.pending = 1
		state.prefixKind = 0
		state.prefixDelimited = false
		state.prefixInvalid = false
	}
	state.start = false
	state.colon = assignment && ch.b == ':' && ch.eligible
	return state
}

func advanceTildePrefix(state tildeScanState, ch tildeChar, assignment bool) tildeScanState {
	if ch.b == '/' && ch.eligible {
		state.marks |= finishTildePrefix(state)
		return clearTildePrefix(state)
	}
	if ch.b == ':' && ch.eligible && !state.prefixDelimited {
		if state.prefixKind == 0 {
			state.prefixKind = 1
		}
		if assignment {
			state.marks |= finishTildePrefix(state)
			return clearTildePrefix(state)
		}
		state.prefixDelimited = true
		return state
	}
	if ch.dynamic {
		if !state.prefixDelimited {
			state.prefixInvalid = true
		}
		return state
	}
	if !ch.eligible || strings.ContainsRune("\\$`'\"", rune(ch.b)) || !state.prefixDelimited && strings.ContainsRune("{}", rune(ch.b)) {
		state.prefixInvalid = true
		return state
	}
	if !state.prefixDelimited && state.prefixKind == 0 {
		state.prefixKind = 2
	}
	return state
}

func finishTildePrefix(state tildeScanState) Tilde {
	if state.pending == 0 || state.prefixInvalid {
		return 0
	}
	if state.prefixKind == 2 {
		return TildeOther
	}
	return TildeHome
}

func clearTildePrefix(state tildeScanState) tildeScanState {
	state.pending = 0
	state.prefixKind = 0
	state.prefixDelimited = false
	state.prefixInvalid = false
	return state
}

func tildeSourceSpan(node syntax.Node, src string, eligible bool) []tildeChar {
	lo, hi := int(node.Pos().Offset()), int(node.End().Offset())
	if !validSpan(src, lo, hi) {
		return nil
	}
	chars := make([]tildeChar, hi-lo)
	for i := lo; i < hi; i++ {
		chars[i-lo] = tildeChar{b: src[i], eligible: eligible, offset: i}
	}
	return chars
}

func assignmentArgumentTilde(word *syntax.Word, src string, chars []tildeChar, posix bool) ([]tildeChar, bool) {
	lo, hi := int(word.Pos().Offset()), int(word.End().Offset())
	if !validSpan(src, lo, hi) {
		return chars, false
	}
	raw := src[lo:hi]
	eq := strings.IndexByte(raw, '=')
	if eq <= 0 {
		return chars, false
	}
	nameEnd := eq
	if raw[eq-1] == '+' {
		nameEnd--
	}
	if !validArgumentAssignName(raw[:nameEnd]) || posix {
		return chars, false
	}
	valueOffset := lo + eq + 1
	index := len(chars)
	for i, ch := range chars {
		if ch.offset >= valueOffset {
			index = i
			break
		}
	}
	if index == len(chars) && eq+1 < len(chars) {
		index = eq + 1
	}
	return chars[index:], true
}

func validArgumentAssignName(name string) bool {
	if i := strings.IndexByte(name, '['); i >= 0 {
		if i == 0 || !validShellName(name[:i]) || !balancedArrayName(name[i:]) {
			return false
		}
		return !strings.ContainsRune(name[i+1:len(name)-1], '=')
	}
	return validShellName(name)
}

func balancedArrayName(name string) bool {
	depth := 0
	for i, r := range name {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
			if depth < 0 || depth == 0 && i != len(name)-1 {
				return false
			}
		}
	}
	return depth == 0
}

func scanTildeCharsMode(chars []tildeChar, assignment bool) Tilde {
	state := scanTildeCharsState(tildeScanState{start: true}, chars, assignment)
	return state.marks | finishTildePrefix(state)
}

func scanTildeCharsState(state tildeScanState, chars []tildeChar, assignment bool) tildeScanState {
	for i := 0; i < len(chars); i++ {
		ch, skip := consumeTildeChar(chars, &i)
		if skip {
			continue
		}
		state = advanceTildeScan(state, ch, assignment)
	}
	return state
}

func consumeTildeChar(chars []tildeChar, index *int) (tildeChar, bool) {
	ch := chars[*index]
	if ch.b != '\\' || !ch.eligible || *index+1 >= len(chars) {
		return ch, false
	}
	*index++
	ch = chars[*index]
	if ch.b == '\n' {
		return ch, true
	}
	ch.eligible = false
	return ch, false
}

func validShellName(name string) bool {
	if name == "" || !(name[0] == '_' || name[0] >= 'A' && name[0] <= 'Z' || name[0] >= 'a' && name[0] <= 'z') {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func commandName(name string) string {
	if strings.Contains(name, "$(") || strings.Contains(name, "`") {
		return ""
	}
	return baseName(name)
}

// commandWordName uses the same unquoted word text as wordValues. An already
// expanded value takes precedence, except for ANSI-C quotes, which are decoded
// consistently regardless of the remaining arguments.
func commandWordName(value string, parsed word) string {
	if parsed.sourceText == value || parsed.ansiQuoted {
		value = parsed.name
	}
	return commandName(value)
}

func validSpan(src string, start, end int) bool {
	return start >= 0 && end >= start && end <= len(src)
}

func setLiteralArgs(c *cmd, expr *syntax.CallExpr, src string, info *walkInfo, budget *nestedParseBudget) {
	c.literalContext = info.literalSimple && literalCallContext(c, expr, info, budget)
	if !c.literalContext || len(expr.Args) == 0 || !literalWord(expr.Args[0], src) {
		return
	}
	c.call.literalArgs = literalArgsMask(expr.Args[1:], src)
}

func literalCallContext(c *cmd, expr *syntax.CallExpr, info *walkInfo, budget *nestedParseBudget) bool {
	if !info.topLevelCalls[expr] || len(c.call.Redirs) != 0 || c.call.Loop || c.call.Background || c.call.ScriptInput || info.timedCalls[expr] {
		return false
	}
	if len(c.call.Via) == 0 {
		return true
	}
	return budget.literalShell != "" && len(c.call.Via) == 2 && c.call.Via[0] == budget.literalShell && c.call.Via[1] == "-c"
}

func literalWord(word *syntax.Word, src string) bool {
	if word == nil || len(word.Parts) == 0 {
		return false
	}
	start, end := int(word.Pos().Offset()), int(word.End().Offset())
	if !validSpan(src, start, end) {
		return false
	}
	for _, part := range word.Parts {
		if !literalWordPart(part, src) {
			return false
		}
	}
	return !hasUnquotedExpansion(src[start:end])
}

func literalWordPart(part syntax.WordPart, src string) bool {
	switch p := part.(type) {
	case *syntax.Lit:
		return true
	case *syntax.SglQuoted:
		return !p.Dollar
	case *syntax.DblQuoted:
		if p.Dollar {
			return false
		}
		for _, inner := range p.Parts {
			lit, ok := inner.(*syntax.Lit)
			if !ok || !literalDoublePart(lit, src) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func literalDoublePart(lit *syntax.Lit, src string) bool {
	start, end := int(lit.Pos().Offset()), int(lit.End().Offset())
	return validSpan(src, start, end) && !strings.Contains(src[start:end], `\`)
}

func hasUnquotedExpansion(text string) bool {
	var quote byte
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if quote == 0 && (ch == '"' || ch == '\'') {
			quote = ch
			continue
		}
		if quote == ch {
			quote = 0
			continue
		}
		if quote != 0 {
			if quote == '"' && ch == '!' {
				return true
			}
			continue
		}
		if ch == '\\' {
			i++
			continue
		}
		if ch == '!' || strings.ContainsRune("{}~*?[]$`", rune(ch)) {
			return true
		}
	}
	return false
}

func singleQuotedWord(word *syntax.Word) bool {
	if word == nil || len(word.Parts) != 1 {
		return false
	}
	quoted, ok := word.Parts[0].(*syntax.SglQuoted)
	return ok && !quoted.Dollar
}

// literalScriptText отвергает управляющие байты, кроме перевода строки и табуляции, и обратную
// косую перед переводом строки: `\r` и NUL парсер читает не так, как bash, а `\` с переводом строки
// в конце комментария парсер считает продолжением строки, bash — нет.
func literalScriptText(src string) bool {
	return !strings.Contains(src, "\\\n") && !strings.ContainsFunc(src, func(r rune) bool { return r < ' ' && r != '\n' && r != '\t' })
}

func literalScriptSimple(file *syntax.File, src string) bool {
	if !literalScriptText(src) {
		return false
	}
	simple := true
	syntax.Walk(file, func(node syntax.Node) bool {
		if simple && !literalScriptNodeSimple(node, src) {
			simple = false
		}
		return true
	})
	return simple
}

func literalScriptNodeSimple(node syntax.Node, src string) bool {
	switch n := node.(type) {
	case *syntax.Stmt:
		if n.Negated || n.Background || n.Coprocess || n.Disown || len(n.Redirs) != 0 {
			return false
		}
		if _, ok := n.Cmd.(*syntax.CallExpr); !ok {
			return false
		}
	case *syntax.CallExpr:
		if len(n.Assigns) != 0 || len(n.Args) == 0 {
			return false
		}
		for _, word := range n.Args {
			if !literalWord(word, src) {
				return false
			}
		}
		name, ok := wordValue(n.Args[0])
		if !ok {
			return false
		}
		if _, builtin := bashBuiltinAndKeywordSet[name]; builtin {
			_, allowed := literalBuiltinAllowSet[name]
			return allowed
		}
	case syntax.Command:
		if _, ok := n.(*syntax.CallExpr); !ok {
			return false
		}
	}
	return true
}

func literalArgsMask(words []*syntax.Word, src string) uint32 {
	var mask uint32
	for i, word := range words {
		if i < 32 && literalWord(word, src) {
			mask |= uint32(1) << uint(i)
		}
	}
	return mask
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
		args := shellStmtArgs(call, src, vars)
		restoreMode := setNestedShellMode(budget, name, args)
		if err := parseNestedMapped(body, line, via, loopCalls[call], backgroundCalls[call], vars, calls, budget); err != nil {
			nestedErrs = append(nestedErrs, err)
		}
		restoreMode()
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
	previousFirst := 0
	for _, input := range piped {
		redir := input.redir
		var body mapped
		if input.body != nil {
			body = *input.body
		} else {
			body = heredocScript(redir, src)
		}
		via := append(slices.Clone(inherited), "pipe")
		via = append(via, input.via...)
		via = append(via, input.name)
		call, _ := input.shell.Cmd.(*syntax.CallExpr)
		first := len(*calls)
		args := shellStmtArgs(call, src, vars)
		restoreMode := setNestedShellMode(budget, input.name, args)
		err := parseNestedWithBudget(body.String(), body.pos, lineBase, via, loopCalls[call], backgroundCalls[call], vars, calls, budget)
		restoreMode()
		for i := first; i < len(*calls); i++ {
			offset := (*calls)[i].Offset
			if validSpan(src, offset, offset) {
				(*calls)[i].Line = lineBase + 1 + strings.Count(src[:offset], "\n")
			}
		}
		if input.alternate {
			dedupePipedAlternatives(calls, previousFirst, first)
		} else {
			previousFirst = first
		}
		if err != nil {
			nestedErrs = append(nestedErrs, err)
		}
	}
	return errors.Join(nestedErrs...)
}

func shellStmtArgs(call *syntax.CallExpr, src string, vars map[string]string) []string {
	if call == nil {
		return nil
	}
	values, _, _ := wordValues(call.Args, src, vars, false)
	if len(values) > 1 {
		return values[1:]
	}
	return nil
}

func setNestedShellMode(budget *nestedParseBudget, name string, args []string) func() {
	previousPosix, previousBrace := budget.initialPosix, budget.initialBrace
	previousPosixUnknown, previousBraceUnknown := budget.initialPosixUnknown, budget.initialBraceUnknown
	end := envCommandStart(args, 0)
	budget.initialPosix = name == "sh" || shellArgsEnablePosix(args) || envHasPosixCorrect(args[:end])
	budget.initialBrace = shellArgsEnableBrace(args)
	budget.initialPosixUnknown, budget.initialBraceUnknown = false, false
	return func() {
		budget.initialPosix, budget.initialBrace = previousPosix, previousBrace
		budget.initialPosixUnknown, budget.initialBraceUnknown = previousPosixUnknown, previousBraceUnknown
	}
}

// dedupePipedAlternatives объединяет одинаковые вызовы двух вариантов stdout,
// сохраняя кратность и различия в перенаправлениях и контексте.
func dedupePipedAlternatives(calls *[]Call, first, second int) {
	key := func(call Call) callIdentity {
		hash := hashCallParts(callHashOffsetBasis, call.Args)
		return callIdentity{name: call.Name, offset: call.Offset, hash: hashCallParts(hash, call.Via)}
	}
	previous := make(map[callIdentity][]Call)
	for i := first; i < second; i++ {
		call := &(*calls)[i]
		id := key(*call)
		previous[id] = append(previous[id], *call)
	}
	kept := (*calls)[:second]
	for i := second; i < len(*calls); i++ {
		call := &(*calls)[i]
		id := key(*call)
		matches := previous[id]
		duplicate := false
		for j := range matches {
			match := &matches[j]
			if sameCall(*match, *call) && match.Line == call.Line && match.Loop == call.Loop && match.Background == call.Background && match.ScriptInput == call.ScriptInput && slices.Equal(match.Redirs, call.Redirs) {
				matches[j] = matches[len(matches)-1]
				previous[id] = matches[:len(matches)-1]
				duplicate = true
				break
			}
		}
		if !duplicate {
			kept = append(kept, *call)
		}
	}
	*calls = kept
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
	return shellInputNameFrom(stmt, src, vars, stdinInput, false)
}

func shellInputNameFrom(stmt *syntax.Stmt, src string, vars map[string]string, stdinInput *syntax.Redirect, sourceArgs bool) (string, []string, bool, error) {
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
		if !isScriptInputShell(name) && !wrappers[name] {
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
	target, shellVia, err := resolveShellInputWithWork(name, args[1:], hasHeredoc, nil, sourceArgs)
	return target, shellVia, target != "", err
}

func resolveShellInputWithWork(name string, args []string, hasHeredoc bool, work *int, sourceArgs bool) (string, []string, error) {
	var via []string
	scan := wrapperExecScan{work: work}
	for {
		if hasHeredoc && wrapperReadsStdinShell(name, args) {
			return "sh", append(via, name), nil
		}
		if name == "sudo" && hasHeredoc && sudoReadsStdin(args) {
			return "bash", append(via, "sudo"), nil
		}
		if (name == "su" || name == "runuser") && hasHeredoc && (suReadsShell(name, args) || name == "su" && uncertainSuReadsStdin(args)) {
			return "sh", append(via, name), nil
		}
		if isScriptInputShell(name) {
			if sourceArgs {
				// Only the arguments of the first source or . are searched.
				if !isSourceBuiltin(name) {
					return "", nil, nil
				}
				sourceArgs = false
			} else if target, shellVia, ok := shellInputForShell(name, args, hasHeredoc, via); ok {
				return target, shellVia, nil
			}
			// source FILE can run its arguments, as in source x exec -- bash:
			// the exec scan below still applies.
			if !isSourceBuiltin(name) {
				return "", nil, nil
			}
		}
		var ok bool
		name, args, via, ok = nextShellInputTarget(name, args, via, &scan)
		if !ok {
			return "", nil, nil
		}
	}
}

func wrapperReadsStdinShell(name string, args []string) bool {
	switch name {
	case "chroot", "setarch", "linux32", "linux64", "i386", "x86_64", "uname26", "nsenter", "run0", "pkexec":
		if wrapperExitsWithoutCommand(name, args) || name == "chroot" && len(args) == 0 {
			return false
		}
		return wrapperStart(name, args) >= len(args)
	default:
		return false
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
	input, _ := shellScriptInputArgs(args, name)
	if input || !isSourceBuiltin(name) && (shellReadsHeredoc(name, args) || mainCompatibleNoExecHeredoc(name, args)) {
		return name, via, true
	}
	return "", nil, false
}

func nextShellInputTarget(name string, args []string, via []string, scan *wrapperExecScan) (string, []string, []string, bool) {
	if wrapperReadsStdinShell(name, args) {
		return "sh", nil, append(via, name), true
	}
	if target, ok := scan.find(execScanArgs(name, args)); ok {
		return commandName(target[0]), target[1:], append(via, name, "exec"), true
	}
	if target, routeVia, _, ok := sudoDashDashRoute(name, args); ok && len(target) > 0 {
		return commandName(target[0]), target[1:], append(via, routeVia...), true
	}
	if start, ok := uncertainWrapperTarget(name, args, true); ok {
		return commandName(args[start]), args[start+1:], append(via, name), true
	}
	if wrappers[name] {
		if wrapped, ok := unwrapOnce(name, args); ok && len(wrapped) > 0 {
			if !isShell(commandName(wrapped[0])) {
				if target, fallbackVia, fallbackOK := unwrapMain(name, args); fallbackOK && len(target) > 0 && isShell(baseName(target[0])) && shellReadsHeredoc(baseName(target[0]), target[1:]) {
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
	if target, fallbackVia, fallbackOK := unwrapMain(name, args); fallbackOK && len(target) > 0 && isShell(baseName(target[0])) && shellReadsHeredoc(baseName(target[0]), target[1:]) {
		return baseName(target[0]), target[1:], append(via, fallbackVia...), true
	}
	return "", nil, nil, false
}

var shellNames = []string{"bash", "sh", "dash", "zsh", "ksh", "mksh", "ksh93", "lksh", "rksh", "rksh93", "pdksh", "fish"}

func isShell(name string) bool {
	return slices.Contains(shellNames, name)
}

func isScriptInputShell(name string) bool {
	return isShell(name) || isSourceBuiltin(name)
}

// shellReadsHeredoc — оболочка читает heredoc как скрипт, если её stdin не заменён -c или файлом.
func shellReadsHeredoc(shell string, args []string) bool {
	if shellNoExec(shell, args) {
		return false
	}
	operand, command, stdin := shellArgs(shell, args)
	return !command && (stdin || operand == len(args))
}

func mainCompatibleNoExecHeredoc(shell string, args []string) bool {
	if !shellNoExec(shell, args) || !hasOption("bash", args, "-E") {
		return false
	}
	operand, command, stdin := shellArgs(shell, args)
	return !command && (stdin || operand == len(args))
}

func shellNoExec(shell string, args []string) bool {
	if shell == "fish" || isKornShell(shell) {
		return false
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" || args[i] == "-" {
			return false
		}
		option, values, _, _, noExec := shellArgumentOption(shell, args[i])
		if !option {
			return false
		}
		if noExec {
			return true
		}
		i += values
	}
	return false
}

// shellArgs разбирает `bash [опции] [-c СКРИПТ | -s | ФАЙЛ] [аргументы]`: operand — индекс первого
// аргумента после опций (len(args) — его нет), command — есть -c, stdin — оболочка читает скрипт со stdin.
func shellArgs(shell string, args []string) (operand int, command, stdin bool) {
	if shell == "fish" {
		return fishShellArgs(args)
	}
	for i := 0; i < len(args); i++ {
		if shell == "fish" && strings.HasPrefix(args[i], "--command=") {
			return i, true, stdin
		}
		if args[i] == "--" || args[i] == "-" {
			return i + 1, command, stdin || i+1 == len(args)
		}
		isOption, values, readsStdin, readsCommand, _ := shellArgumentOption(shell, args[i])
		if !isOption {
			return i, command, stdin
		}
		stdin, command = stdin || readsStdin, command || readsCommand
		values = shellOptionValueCount(shell, args, i, values)
		i += values
	}
	return len(args), command, stdin
}

func fishShellArgs(args []string) (operand int, command, stdin bool) {
	scripts, _, file, valid := fishInvocationOptions(args)
	if !valid {
		return len(args), false, false
	}
	for _, script := range scripts {
		if script.command {
			return script.index, true, false
		}
	}
	return file, false, file == len(args) || file < len(args) && args[file] == "-"
}

func shellOptionValueCount(shell string, args []string, index, values int) int {
	if !isKornShell(shell) {
		return values
	}
	arg := args[index]
	if (arg == "-o" || arg == "+o") && index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") && !strings.HasPrefix(args[index+1], "+") {
		return 1
	}
	if shell == "mksh" && arg == "-T" {
		return 1
	}
	return values
}

func shellArgumentOption(shell, arg string) (option bool, values int, stdin, command, noExec bool) {
	if shell == "fish" {
		if fishOpt, fishValues, fishCommand, fishNoExec, ok := fishArgumentOption(arg); ok {
			return fishOpt, fishValues, false, fishCommand, fishNoExec
		}
	}
	if isKornShell(shell) {
		return kornArgumentOption(shell, arg)
	}
	if arg == "--rcfile" || arg == "--init-file" {
		return true, 1, false, false, false
	}
	if strings.HasPrefix(arg, "--") {
		return true, 0, false, false, false
	}
	if len(arg) < 2 || (arg[0] != '-' && arg[0] != '+') {
		return false, 0, false, false, false
	}
	for j := 1; j < len(arg); j++ {
		switch arg[j] {
		case 'c':
			command = true
		case 's':
			stdin = shell != "fish"
		case 'o':
			if shell == "zsh" {
				if j+1 < len(arg) {
					return true, values, stdin, command, noExec
				}
				values++
			} else {
				values++
			}
		case 'O':
			if shell != "zsh" {
				values++
			}
		case 'n':
			noExec = arg[0] == '-'
		}
	}
	return true, values, stdin, command, noExec
}

type fishScriptArg struct {
	index   int
	offset  int
	command bool
	option  string
}

type fishLongOptionSpec struct {
	name    string
	value   bool
	command bool
}

var fishLongOptions = []fishLongOptionSpec{
	{name: "--command", value: true, command: true}, {name: "--init-command", value: true, command: true},
	{name: "--debug", value: true}, {name: "--debug-output", value: true}, {name: "--profile", value: true},
	{name: "--profile-startup", value: true}, {name: "--features", value: true},
	{name: "--interactive"}, {name: "--login"}, {name: "--no-config"},
	{name: "--no-execute"}, {name: "--private"}, {name: "--print-rusage-self"},
	{name: "--print-debug-categories"}, {name: "--version"}, {name: "--help"},
}

func fishLongOption(prefix string) (fishLongOptionSpec, bool) {
	if prefix == "--init-cmd" {
		prefix = "--init-command"
	}
	if prefix == "--co" || prefix == "--c" {
		prefix = "--command"
	} // fish accepts these command abbreviations.
	match := fishLongOptionSpec{}
	for _, option := range fishLongOptions {
		if option.name == prefix {
			return option, true
		}
		if strings.HasPrefix(option.name, prefix) {
			if match.name != "" {
				return fishLongOptionSpec{}, false
			}
			match = option
		}
	}
	return match, match.name != ""
}

func fishArgumentOption(arg string) (option bool, values int, command, noExec, ok bool) {
	if strings.HasPrefix(arg, "--") {
		key, _, attached := strings.Cut(arg, "=")
		long, found := fishLongOption(key)
		if !found || attached && !long.value {
			return false, 0, false, false, false
		}
		return true, boolInt(long.value && !attached), long.name == "--command", false, true
	}
	if arg == "-" || len(arg) < 2 || arg[0] != '-' {
		return false, 0, false, false, false
	}
	for i := 1; i < len(arg); i++ {
		switch arg[i] {
		case 'c', 'C', 'd', 'o', 'p', 'f':
			return true, boolInt(i == len(arg)-1), arg[i] == 'c', false, true
		case 'i', 'l', 'N', 'P', 'n', 'v', 'h':
		default:
			return false, 0, false, false, false
		}
	}
	return true, 0, false, false, true
}

func fishInvocationOptions(args []string) (scripts []fishScriptArg, noExec bool, file int, valid bool) {
	file, valid = len(args), true
	sawFile := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if !sawFile && !fishHasCommandScript(scripts) && i+1 < len(args) {
				file, sawFile = i+1, true
			}
			break
		}
		if arg == "" || !strings.HasPrefix(arg, "-") || arg == "-" {
			if !sawFile && !fishHasCommandScript(scripts) {
				file, sawFile = i, true
			}
			continue
		}
		parsed, consumed, ok := fishInvocationOption(args, i)
		if !ok {
			return nil, false, file, false
		}
		scripts = append(scripts, parsed.scripts...)
		i += consumed
	}
	return scripts, noExec, file, valid
}

func fishHasCommandScript(scripts []fishScriptArg) bool {
	for _, script := range scripts {
		if script.command {
			return true
		}
	}
	return false
}

type fishOptionResult struct {
	scripts []fishScriptArg
}

func fishInvocationOption(args []string, i int) (fishOptionResult, int, bool) {
	if strings.HasPrefix(args[i], "--") {
		return fishLongInvocationOption(args, i)
	}
	return fishShortInvocationOption(args, i)
}

func fishLongInvocationOption(args []string, i int) (fishOptionResult, int, bool) {
	key, _, attached := strings.Cut(args[i], "=")
	option, ok := fishLongOption(key)
	if !ok || attached && !option.value {
		return fishOptionResult{}, 0, false
	}
	if option.value {
		index, offset, consumed, ok := fishOptionValue(args, i, len(key)+1, attached)
		if !ok {
			return fishOptionResult{}, 0, false
		}
		result := fishOptionResult{}
		if option.command {
			result.scripts = append(result.scripts, fishScriptArg{index: index, offset: offset, command: option.name == "--command", option: option.name})
		}
		return result, consumed, true
	}
	return fishOptionResult{}, 0, true
}

func fishShortInvocationOption(args []string, i int) (fishOptionResult, int, bool) {
	arg := args[i]
	result := fishOptionResult{}
	for j := 1; j < len(arg); j++ {
		ch := arg[j]
		switch ch {
		case 'c', 'C', 'd', 'o', 'p', 'f':
			index, offset, consumed, ok := fishOptionValue(args, i, j+1, j+1 < len(arg))
			if !ok {
				return fishOptionResult{}, 0, false
			}
			if ch == 'c' || ch == 'C' {
				result.scripts = append(result.scripts, fishScriptArg{index: index, offset: offset, command: ch == 'c', option: "-" + string(ch)})
			}
			return result, consumed, true
		case 'i':
		case 'n', 'v', 'h':
		case 'l', 'N', 'P':
		default:
			return fishOptionResult{}, 0, false
		}
	}
	return result, 0, true
}

func fishOptionValue(args []string, i, offset int, attached bool) (index, valueOffset, consumed int, ok bool) {
	if attached {
		return i, offset, 0, true
	}
	if i+1 >= len(args) {
		return 0, 0, 0, false
	}
	return i + 1, 0, 1, true
}

func isKornShell(shell string) bool {
	switch shell {
	case "ksh", "mksh", "ksh93", "lksh", "rksh", "rksh93", "pdksh":
		return true
	default:
		return false
	}
}

func kornArgumentOption(shell, arg string) (option bool, values int, stdin, command, noExec bool) {
	if shell == "mksh" && arg == "-T" {
		return true, 1, false, false, false
	}
	if len(arg) < 2 || (arg[0] != '-' && arg[0] != '+') {
		return false, 0, false, false, false
	}
	for j := 1; j < len(arg); j++ {
		if arg[j] == 'c' {
			command = true
		}
		if arg[j] == 's' {
			stdin = true
		}
	}
	return true, 0, stdin, command, false
}

func kornInvocation(args []string, shell string) (operand int, command, stdin, valid bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || arg == "-" {
			return i + 1, command, stdin || i+1 == len(args), false
		}
		if !isKornOption(arg) {
			return i, command, stdin, true
		}
		next, readsCommand, readsStdin, ok := kornOption(args, i, shell)
		if !ok {
			return i, command, stdin, false
		}
		command, stdin = command || readsCommand, stdin || readsStdin
		i = next
	}
	return len(args), command, stdin, true
}

func isKornOption(arg string) bool { return len(arg) > 1 && (arg[0] == '-' || arg[0] == '+') }

func kornOption(args []string, index int, shell string) (next int, command, stdin, valid bool) {
	arg := args[index]
	if next, handled, valid := kornValueOption(args, index, shell); handled {
		return next, false, false, valid
	}
	return kornFlagCluster(args, arg, index, shell)
}

func kornValueOption(args []string, index int, shell string) (next int, handled, valid bool) {
	arg := args[index]
	if strings.HasPrefix(arg, "--") {
		return index, true, false
	}
	if arg == "-o" || arg == "+o" {
		if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") || strings.HasPrefix(args[index+1], "+") {
			return index, true, false
		}
		return index + 1, true, true
	}
	if strings.ContainsRune(arg[1:], 'o') {
		return index, true, false
	}
	if shell == "mksh" {
		if strings.ContainsRune(arg[1:], 'T') {
			if arg != "-T" || index+1 >= len(args) {
				return index, true, false
			}
			return index + 1, true, true
		}
	}
	return index, false, false
}

func kornFlagCluster(args []string, arg string, index int, shell string) (next int, command, stdin, valid bool) {
	allowed := "abcefhiklmnprstuvxBCDEGH"
	if shell == "mksh" {
		allowed = "abCefhiklmnprUuvXxcs"
	}
	for j := 1; j < len(arg); j++ {
		ch := arg[j]
		if !strings.ContainsRune(allowed, rune(ch)) {
			return index, command, stdin, false
		}
		if ch == 's' {
			stdin = true
		}
		if ch == 'c' {
			if j != len(arg)-1 || index+1 >= len(args) {
				return index, command, stdin, false
			}
			return index + 1, true, stdin, true
		}
	}
	return index, false, stdin, true
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
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
	return parseNestedFileWithBudget(nil, "", src, pos, lineBase, via, inLoop, inBackground, vars, calls, nil, nil, budget)
}

var fishSafeCommands = set("echo", "printf", "cd", "pwd", "true", "false", "test", "count", "contains", "string", "math", "path", "realpath", "type", "ls", "grep", "kill")

func fishScriptUnsupported(src string) bool {
	if !fishScriptCharactersSupported(src) || fishScriptUsesUnsupportedCommand(src) {
		return true
	}
	return false
}

func fishScriptCharactersSupported(src string) bool {
	quote := byte(0)
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if quote == '\'' {
			if ch == '\\' {
				return false
			}
			if ch == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			if ch == '\\' || ch == '$' || ch == '(' {
				return false
			}
			if ch == '"' {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == ';' {
			continue
		}
		if fishSupportedScriptByte(ch) {
			continue
		}
		return false
	}
	return quote == 0
}

func fishSupportedScriptByte(ch byte) bool {
	return ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || strings.ContainsRune("_./=:,+@%-", rune(ch))
}

func fishScriptUsesUnsupportedCommand(src string) bool {
	builtins := literalWordSet(fishBuiltinNames)
	functions := literalWordSet(fishFunctionNames)
	command := true
	var word strings.Builder
	quotedWord := false
	check := func() bool {
		value := word.String()
		word.Reset()
		wasQuoted := quotedWord
		quotedWord = false
		if value == "" {
			return false
		}
		if !command {
			return false
		}
		if fishAssignmentWord(value) && !wasQuoted {
			return false
		}
		if strings.Contains(value, "=") {
			return true
		}
		command = false
		if fishSafeCommands[value] {
			return false
		}
		_, builtin := builtins[value]
		_, function := functions[value]
		return builtin || function
	}
	quote := byte(0)
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				word.WriteByte(ch)
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quotedWord = true
			quote = ch
			continue
		}
		if ch == ';' || ch == '\n' {
			if check() {
				return true
			}
			command = true
			continue
		}
		if ch == ' ' || ch == '\t' {
			if check() {
				return true
			}
			continue
		}
		word.WriteByte(ch)
	}
	return check()
}

func fishAssignmentWord(word string) bool {
	key, _, ok := strings.Cut(word, "=")
	if !ok || key == "" {
		return false
	}
	for i, r := range key {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func viaUsesKornShell(via []string) bool {
	for i := len(via) - 1; i >= 0; i-- {
		if isShell(via[i]) {
			return isKornShell(via[i])
		}
	}
	return false
}

func viaUsesFishShell(via []string) bool {
	for i := len(via) - 1; i >= 0; i-- {
		if isShell(via[i]) {
			return via[i] == "fish"
		}
	}
	return false
}

func kornScriptUsesAlias(src string) bool {
	for start := 0; start < len(src); {
		rel := strings.Index(src[start:], "alias")
		if rel < 0 {
			return false
		}
		i := start + rel
		left := i == 0 || !isShellWordByte(src[i-1])
		end := i + len("alias")
		right := end == len(src) || !isShellWordByte(src[end])
		if left && right {
			return true
		}
		start = end
	}
	return false
}

func isShellWordByte(ch byte) bool {
	return ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9'
}

func kornCommandNameHasSubstitution(src string) bool {
	for offset := 0; offset < len(src); {
		rel := strings.IndexAny(src[offset:], "$`")
		if rel < 0 {
			return false
		}
		index := offset + rel
		start := index
		for start > 0 && !strings.ContainsRune(";|&\n", rune(src[start-1])) {
			start--
		}
		if kornSubstitutionAtCommand(src, start, index) {
			return true
		}
		offset = index + 2
	}
	return false
}

func kornSubstitutionAtCommand(src string, start, index int) bool {
	wordStart := index
	for wordStart > start && src[wordStart-1] != ' ' && src[wordStart-1] != '\t' && src[wordStart-1] != '\n' {
		wordStart--
	}
	if fishAssignmentWord(src[wordStart:index]) {
		return false
	}
	fields := strings.Fields(src[start:index])
	for len(fields) > 0 && fishAssignmentWord(fields[0]) {
		fields = fields[1:]
	}
	inline := index > 0 && src[index-1] != ' ' && src[index-1] != '\t' && src[index-1] != '\n'
	if inline {
		return len(fields) <= 1
	}
	if len(fields) == 0 {
		return true
	}
	return len(fields) == 1 && slices.Contains([]string{"time", "command", "builtin", "exec", "nice", "env"}, fields[0])
}

func parseNestedFileWithBudget(file *syntax.File, full, src string, pos []int, lineBase int, via []string, inLoop, inBackground bool, vars map[string]string, calls *[]Call, ignored [][2]int, subst *mapped, budget *nestedParseBudget) error {
	if viaUsesFishShell(via) {
		unsupported := fishScriptUnsupported(src)
		if unsupported {
			return &NestedError{Script: src, Err: fmt.Errorf("shellcmd: fish script outside the supported subset")}
		}
	}
	if viaUsesKornShell(via) {
		unsupported := kornScriptUsesAlias(src)
		if unsupported {
			return &NestedError{Script: src, Err: fmt.Errorf("shellcmd: ksh alias expansion is not modeled")}
		}
		if kornCommandNameHasSubstitution(src) {
			return &NestedError{Script: src, Err: fmt.Errorf("shellcmd: dynamic ksh command name is not modeled")}
		}
	}
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
	if file == nil && subst == nil {
		err = parseWithBudget(src, lineBase, via, nestedVars, &nestedCalls, budget)
	} else {
		if file == nil {
			file, full, ignored, err = budget.parseShell(src)
		}
		if err == nil {
			err = walkFileWithBudget(file, full, src, lineBase, via, nestedVars, &nestedCalls, ignored, subst, budget)
		}
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
func stmtRedirs(rs []*syntax.Redirect, src string, braceMode, braceUnknown bool) []Redir {
	out := make([]Redir, 0, len(rs))
	for _, r := range rs {
		red := Redir{Op: r.Op.String(), BodyStart: -1, BodyEnd: -1}
		if r.N != nil {
			red.N = r.N.Value
		}
		if r.Word != nil {
			if red.Op != "<<" && red.Op != "<<-" {
				red.Tilde = redirectionWordTilde(r.Word, src, red.Op, braceMode, braceUnknown)
			}
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

func redirectionWordTilde(word *syntax.Word, src, op string, braceMode, braceUnknown bool) Tilde {
	if op != "<<<" && braceUnknown {
		base := redirectionWordTildeMode(word, src, op, braceMode)
		alternate := redirectionWordTildeMode(word, src, op, !braceMode)
		if alternate != base {
			base |= TildeModeUnknown
		}
		return base
	}
	return redirectionWordTildeMode(word, src, op, braceMode)
}

func redirectionWordTildeMode(word *syntax.Word, src, op string, braceMode bool) Tilde {
	if op != "<<<" && braceMode {
		if copyWord, split := splitBraces(word); split && braceWordCount(copyWord.Parts) != 1 {
			return 0
		}
	}
	return wordTildeWithMode(word, src, op == "<<<", false, false, braceMode && op != "<<<", false, false)
}

func braceWordCount(parts []syntax.WordPart) int {
	return braceWordCountWithContext(parts, false)
}

func braceWordCountWithContext(parts []syntax.WordPart, inherited bool) int {
	present := make([]bool, len(parts))
	for i, part := range parts {
		present[i] = wordHasPresence([]syntax.WordPart{part})
	}
	prefix, suffix := make([]bool, len(parts)+1), make([]bool, len(parts)+1)
	for i := range parts {
		prefix[i+1] = prefix[i] || present[i]
	}
	for i := len(parts) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] || present[i]
	}
	count := 1
	for index, part := range parts {
		factor := 1
		if brace, ok := part.(*syntax.BraceExp); ok {
			factor = 0
			around := inherited
			around = around || prefix[index] || suffix[index+1]
			for _, elem := range brace.Elems {
				if around || wordHasPresence(elem.Parts) {
					factor = min(2, factor+braceWordCountWithContext(elem.Parts, around))
				}
			}
		}
		count = min(2, count*factor)
	}
	return count
}

func wordHasPresence(parts []syntax.WordPart) bool {
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if p.Value != "" {
				return true
			}
		case *syntax.SglQuoted, *syntax.DblQuoted:
			return true
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp:
			return true
		case *syntax.BraceExp:
			for _, elem := range p.Elems {
				if wordHasPresence(elem.Parts) {
					return true
				}
			}
		default:
			return true
		}
	}
	return false
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
func cmdWords(args []*syntax.Word, src string, scripts bool, literals []bool) ([]word, error) {
	ws := make([]word, len(args))
	for i, arg := range args {
		ws[i].offset = -1
		ws[i].singleQuoted = singleQuotedWord(arg)
		ws[i].sourceText = nodeText(arg, src)
		ws[i].name = unquotedText(arg, src)
		if len(arg.Parts) == 1 && isANSICQuoted(arg.Parts[0]) {
			ws[i].ansiQuoted = true
			var err error
			ws[i].name, _, err = wordScript(arg, src, false)
			if err != nil {
				return nil, err
			}
		}
		if i == 0 {
			start, end := int(arg.Pos().Offset()), int(arg.End().Offset())
			if validSpan(src, start, end) {
				ws[i].raw = src[start:end]
			}
		}
		if scripts {
			var err error
			ws[i].script, ws[i].pos, err = wordScript(arg, src, literals[i])
			if err != nil {
				return nil, err
			}
		}
		if literals[i] {
			ws[i].offset = int(arg.Pos().Offset())
		}
	}
	return ws, nil
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
	alternate *mapped
	text      []byte
	pos       []int
	rawStart  []int
	rawEnd    []int
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
	argv0, posixEnv := c.argv0, c.posixEnv
	if c.call.Name == "exec" {
		for j := 0; j+1 < i-1; j++ {
			if c.call.Args[j] == "-a" || c.call.Args[j] == "--argv0" {
				argv0 = c.call.Args[j+1]
			}
		}
	}
	if c.call.Name == "env" && envHasPosixCorrect(c.call.Args[:max(0, i-1)]) {
		posixEnv = true
	}
	return cmd{call: Call{Name: commandWordName(c.call.Args[i-1], c.words[i]), Args: c.call.Args[i:], Raw: c.call.Raw, Line: c.call.Line,
		Via: via, Offset: c.words[i].offset, Redirs: c.call.Redirs, tildeArgs: tildeArgTail(c.call.tildeArgs, i), Loop: c.call.Loop, Background: c.call.Background, ScriptInput: c.call.ScriptInput}, mode: c.mode, words: c.words[i:], budget: c.budget, source: c.source,
		stdinInput: c.stdinInput, stdinConsumed: c.stdinConsumed || c.call.Name == "xargs", expansionDepth: c.expansionDepth + 1, posixEnv: posixEnv, argv0: argv0}
}

func tildeArgTail(args []Tilde, start int) []Tilde {
	if start < 0 || start >= len(args) {
		return nil
	}
	return args[start:]
}

func (c cmd) via(more ...string) []string {
	return append(slices.Clone(c.call.Via), more...)
}

// emit записывает вызовы точного разбора и совместимого с прежней версией обхода.
func emit(c cmd, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	if c.expansionDepth > maxNestedScripts {
		return &NestedError{Script: c.call.Raw, Err: errCommandExpansionDepth}
	}
	if c.call.Name == "setsid" && setsidDynamicTarget(c.call.Args) {
		return &NestedError{Script: c.call.Raw, Err: fmt.Errorf("shellcmd: dynamic setsid command is not modeled")}
	}
	appendCall(seen, calls, c.call)

	if err := emitExpanded(c, lineBase, vars, calls, seen); err != nil {
		return err
	}
	return emitCompatibleRoutes(c, lineBase, vars, calls, seen)
}

func setsidDynamicTarget(args []string) bool {
	index := wrapperStart("setsid", args)
	return index < len(args) && strings.ContainsAny(args[index], "$`")
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
	if name == "flock" && flockVerboseOption(args) {
		lock := wrapperOptionEnd(name, args, 0)
		if lock < len(args) {
			if err := emit(c.sub(lock+1, c.via("flock")), lineBase, vars, calls, seen); err != nil {
				return err
			}
		}
	}
	if err := emitSudoDashDashRoute(c, name, args, lineBase, vars, calls, seen); err != nil {
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
	if err := emitMainTarget(c, main, mainVia, mainOK, lineBase, vars, calls, seen); err != nil {
		return err
	}
	if err := emitUncertainWrapper(c, lineBase, vars); err != nil {
		return err
	}
	if !exactOK {
		if start, via, ok := uncertainNestedWrapper(name, args); ok {
			return emit(c.sub(start, c.via(via...)), lineBase, vars, calls, seen)
		}
		if uncertainNestedBeyondLimit(name, args) {
			return &NestedError{Script: c.call.Raw, Err: errCommandExpansionDepth}
		}
	}
	return nil
}

// Explore only actual wrapper targets, never arbitrary argument suffixes.
// Speculative calls stay private: an ambiguous signal or search limit is reported as an error.
func emitUncertainWrapper(c cmd, lineBase int, vars map[string]string) error {
	if c.call.Name == "su" || c.call.Name == "runuser" || c.call.Name == "flock" {
		if err := uncertainCommandScripts(c, lineBase, vars); err != nil {
			return err
		}
	}
	if c.call.Name == "su" {
		if c.call.ScriptInput && uncertainSuReadsStdin(c.call.Args) {
			return uncertainProcessInput(c, lineBase, vars)
		}
		return nil
	}
	for _, start := range uncertainWrapperTargets(c.call.Name, c.call.Args) {
		if c.budget != nil {
			c.budget.uncertainWork++
			if c.budget.uncertainWork > c.budget.limit {
				return &NestedError{Script: c.call.Raw, Err: errUncertainWrapperBudget}
			}
		}
		candidate := c.sub(start+1, c.via(c.call.Name))
		remaining := maxNestedScripts * 2
		if c.call.ScriptInput && uncertainShellCandidate(candidate.call.Name, candidate.call.Args, &remaining) {
			if err := uncertainProcessInput(c, lineBase, vars); err != nil {
				return err
			}
		}
		var calls []Call
		err := emit(candidate, lineBase, maps.Clone(vars), &calls, newCallSet())
		signal, signalErr := uncertainCallsSignal(c, calls, vars)
		if signalErr != nil {
			return &NestedError{Script: c.call.Raw, Err: signalErr}
		}
		if signal {
			return &NestedError{Script: c.call.Raw, Err: errUnknownWrapperSignal}
		}
		if errors.Is(err, errUnknownWrapperSignal) || errors.Is(err, errUncertainWrapperBudget) || errors.Is(err, errCommandExpansionDepth) {
			return err
		}
	}
	return nil
}

func uncertainProcessInput(c cmd, lineBase int, vars map[string]string) error {
	signal, err := processSubstitutionSignal(c, lineBase, vars)
	if err != nil {
		return &NestedError{Script: c.call.Raw, Err: err}
	}
	if signal {
		return &NestedError{Script: c.call.Raw, Err: errUnknownWrapperSignal}
	}
	return nil
}

func uncertainCommandScripts(c cmd, lineBase int, vars map[string]string) error {
	for _, start := range uncertainWrapperTargets(c.call.Name, c.call.Args) {
		index := start
		if c.call.Name == "su" || c.call.Name == "runuser" {
			index++ // the first positional word is a user
		}
		arg, ok := commandOptionAt(c.call.Name, c.call.Args, index)
		if !ok {
			continue
		}
		script, ok := mappedScriptWords(c, []scriptArg{arg})
		if !ok {
			continue
		}
		var calls []Call
		err := parseNestedMapped(script, lineBase, c.via(c.call.Name, "-c"), c.call.Loop, c.call.Background, maps.Clone(vars), &calls, c.budget)
		if errors.Is(err, errCommandExpansionDepth) || errors.Is(err, errUncertainWrapperBudget) {
			return err
		}
		if anySignalCall(calls) || errors.Is(err, errUnknownWrapperSignal) {
			return &NestedError{Script: c.call.Raw, Err: errUnknownWrapperSignal}
		}
	}
	return nil
}

// A literal echo or printf in an input process substitution supplies a known
// shell script. Other producers remain represented as unknown script input.
func processSubstitutionSignal(c cmd, lineBase int, vars map[string]string) (bool, error) {
	for _, redir := range c.call.Redirs {
		if redir.Op != "<" || !strings.HasPrefix(redir.Target, "<(") || !strings.HasSuffix(redir.Target, ")") {
			continue
		}
		src := redir.Target[2 : len(redir.Target)-1]
		file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
		if err != nil {
			return false, fmt.Errorf("shellcmd: parse process substitution: %w", err)
		}
		if len(file.Stmts) != 1 {
			continue
		}
		body, ok, err := literalProducerBody(file.Stmts[0], src, vars)
		if err != nil {
			return false, fmt.Errorf("shellcmd: read process substitution: %w", err)
		}
		if !ok {
			continue
		}
		var calls []Call
		budget := c.budget
		if budget == nil {
			budget = newNestedParseBudget(len(body.String()))
		}
		err = parseWithBudget(body.String(), lineBase, nil, maps.Clone(vars), &calls, budget)
		if errors.Is(err, errUncertainWrapperBudget) || errors.Is(err, errCommandExpansionDepth) {
			return false, err
		}
		var nested *NestedError
		if err != nil && !errors.As(err, &nested) {
			continue
		}
		if anySignalCall(calls) || errors.Is(err, errUnknownWrapperSignal) {
			return true, nil
		}
	}
	return false, nil
}

func uncertainCallsSignal(c cmd, calls []Call, vars map[string]string) (bool, error) {
	for i := range calls {
		call := &calls[i]
		if call.Name == "kill" && (slices.Contains(call.Via, "xargs") || slices.Contains(call.Via, "parallel")) && !signalCall(call) {
			possible, err := xargsMayReceiveSignalTarget(c, vars)
			if err != nil {
				return false, err
			}
			if possible {
				return true, nil
			}
		}
		if !signalCall(call) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// A literal pipe into xargs supplies arguments to an otherwise bare kill.
// Unknown input must remain conservative; known non-target data cannot signal.
func xargsMayReceiveSignalTarget(c cmd, vars map[string]string) (bool, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(c.source), "")
	if err != nil {
		return false, fmt.Errorf("shellcmd: parse xargs pipe: %w", err)
	}
	known, possible := false, false
	var inputErr error
	syntax.Walk(file, func(node syntax.Node) bool {
		pipe, ok := node.(*syntax.BinaryCmd)
		if !ok || pipe.Op != syntax.Pipe && pipe.Op != syntax.PipeAll || pipe.Y == nil || c.call.Offset < int(pipe.Y.Pos().Offset()) || c.call.Offset >= int(pipe.Y.End().Offset()) {
			return true
		}
		var literal, target bool
		literal, target, inputErr = xargsLiteralPipeTarget(pipe.X, c.source, vars)
		known = known || literal
		possible = possible || target
		return inputErr == nil
	})
	if inputErr != nil {
		return false, inputErr
	}
	return !known || possible, nil
}

func xargsLiteralPipeTarget(stmt *syntax.Stmt, src string, vars map[string]string) (bool, bool, error) {
	body, ok, err := literalProducerBody(stmt, src, vars)
	if err != nil {
		return false, false, fmt.Errorf("shellcmd: read xargs pipe: %w", err)
	}
	if !ok {
		return false, false, nil
	}
	for target := range strings.FieldsSeq(body.String()) {
		if signalTargetWord(target) {
			return true, true, nil
		}
	}
	return true, false, nil
}

func literalProducerBody(stmt *syntax.Stmt, src string, vars map[string]string) (mapped, bool, error) {
	producer := literalPipeProducer(stmt)
	if producer == nil || len(producer.Args) == 0 {
		return mapped{}, false, nil
	}
	name, literal := wordValue(producer.Args[0])
	if !literal || commandName(name) != "echo" && commandName(name) != "printf" {
		return mapped{}, false, nil
	}
	return literalPipeBody(commandName(name), producer.Args[1:], src, vars)
}

// Descend through known wrappers up to the first wrapper with an unknown
// option, so a permissive compatibility route cannot skip that wrapper.
func uncertainNestedWrapper(name string, args []string) (int, []string, bool) {
	start := 1
	var via []string
	for len(via) < maxNestedScripts && wrappers[name] && wrapperOptionsKnown(name, args) {
		if wrapperRunsScript(name, args) {
			break
		}
		target, ok := unwrapOnce(name, args)
		if !ok || len(target) == 0 {
			break
		}
		start += len(args) - len(target)
		via = append(via, name)
		name, args = commandName(target[0]), target[1:]
		if wrappers[name] && !wrapperOptionsKnown(name, args) {
			return start, via, true
		}
		start++
	}
	return 0, nil, false
}

func uncertainNestedBeyondLimit(name string, args []string) bool {
	depth := 0
	for wrappers[name] {
		if !wrapperOptionsKnown(name, args) {
			return depth >= maxNestedScripts
		}
		if wrapperRunsScript(name, args) {
			return false
		}
		target, ok := unwrapOnce(name, args)
		if !ok || len(target) == 0 {
			return false
		}
		name, args = commandName(target[0]), target[1:]
		depth++
	}
	return false
}

func flockVerboseOption(args []string) bool {
	for i := 0; i < len(args) && wrapperOptionWord("flock", args[i]); {
		if args[i] == "--" {
			return false
		}
		key, _, _ := strings.Cut(args[i], "=")
		if wrapperLongOption("flock", key) == "--verbose" {
			return true
		}
		i = optionNext("flock", args, i)
	}
	return false
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

func emitSudoDashDashRoute(c cmd, name string, args []string, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	if assignment, via, start, ok := sudoDashDashAssignmentRoute(name, args); ok {
		candidate := c.sub(start, c.via(via...))
		candidate.call.Name = commandName(assignment[0])
		candidate.call.Args = slices.Clone(assignment[1:])
		candidate.words = c.words[start:]
		if err := emit(candidate, lineBase, vars, calls, seen); err != nil {
			return err
		}
	}
	_, via, start, ok := sudoDashDashRoute(name, args)
	if !ok {
		return nil
	}
	candidate := c.sub(start, c.via(via...))
	if seen.hasBase(candidate.call) {
		return nil
	}
	return emit(candidate, lineBase, vars, calls, seen)
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
	if !seen.hasBase(candidate.call) {
		if err := emit(candidate, lineBase, vars, calls, seen); err != nil {
			return err
		}
	}
	return nil
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
	for i := range calls {
		markCall(seen, calls[i])
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
	return unknownWrapperOption(name, args) < 0
}

func unknownWrapperOption(name string, args []string) int {
	if name == "command" || name == "exec" || name == "builtin" {
		return -1
	}
	for i := 0; i < len(args) && wrapperOptionWord(name, args[i]); {
		if sudoEnvWord(name, args[i]) {
			i++
			continue
		}
		next, unknown := wrapperOptionStep(name, args, i)
		if unknown {
			if name == "taskset" && tasksetAttachedCPUCluster(args[i]) {
				i = optionNext(name, args, i)
				continue
			}
			return i
		}
		if next <= i {
			i++
		} else {
			i = next
		}
	}
	return -1
}

// An unknown option can be a flag or consume the next word. The search is
// bounded by argument positions, and keeps known options for wrapper parsing.
type uncertainOptionState struct {
	i     int
	known []string
}

func uncertainWrapperTargets(name string, args []string) []int {
	return uncertainWrapperTargetsWithWork(name, args, nil)
}

func uncertainWrapperTargetsWithWork(name string, args []string, work *int) []int {
	if !wrappers[name] {
		return nil
	}
	first := unknownWrapperOption(name, args)
	if first < 0 || wrapperHelpBeforeUnknown(name, args[:first+1]) {
		return nil
	}
	var starts []int
	walkUncertainOptions(name, args, work, func(state uncertainOptionState) bool {
		i := state.i
		if i >= len(args) {
			return false
		}
		if args[i] == "--" {
			i++
		}
		if start, ok := uncertainTargetStart(name, args, i, state.known); ok && !slices.Contains(starts, start) {
			starts = append(starts, start)
		}
		return false
	})
	return starts
}

// walkUncertainOptions explores both meanings of unknown options once per position.
func walkUncertainOptions(name string, args []string, work *int, visit func(uncertainOptionState) bool) bool {
	queue := []uncertainOptionState{{}}
	visited := make(map[int]bool)
	for len(queue) > 0 {
		if work != nil {
			*work++
		}
		state := queue[0]
		queue = queue[1:]
		i := state.i
		if visited[i] {
			continue
		}
		visited[i] = true
		if i >= len(args) || !wrapperOptionWord(name, args[i]) || args[i] == "--" {
			if visit(state) {
				return true
			}
			continue
		}
		if sudoEnvWord(name, args[i]) {
			queue = append(queue, uncertainOptionState{i + 1, append(slices.Clone(state.known), args[i])})
			continue
		}
		next, unknown := wrapperOptionStep(name, args, i)
		if unknown && name == "taskset" && tasksetAttachedCPUCluster(args[i]) {
			next, unknown = optionNext(name, args, i), false
		}
		if unknown {
			queue = uncertainOptionBranches(queue, state, args)
			continue
		}
		if next <= i {
			next = i + 1
		}
		known := append(slices.Clone(state.known), args[i:next]...)
		queue = append(queue, uncertainOptionState{next, known})
	}
	return false
}

// su runs the user's shell, not a program named by its first positional word.
// With no additional shell arguments, that shell can read a script from stdin.
func uncertainSuReadsStdin(args []string) bool {
	if unknownWrapperOption("su", args) < 0 {
		return false
	}
	return walkUncertainOptions("su", args, nil, func(state uncertainOptionState) bool {
		clean := append(slices.Clone(state.known), args[state.i:]...)
		return suReadsShell("su", clean)
	})
}

func uncertainOptionBranches(queue []uncertainOptionState, state uncertainOptionState, args []string) []uncertainOptionState {
	i := state.i
	queue = append(queue, uncertainOptionState{i + 1, state.known})
	if i+1 < len(args) && !strings.Contains(args[i], "=") {
		queue = append(queue, uncertainOptionState{i + 2, state.known})
	}
	return queue
}

func wrapperHelpBeforeUnknown(name string, args []string) bool {
	switch name {
	case "taskset", "strace", "flock", "setsid", "chrt":
		return wrapperExitsWithoutCommand(name, args)
	}
	return false
}

func uncertainTargetStart(name string, args []string, i int, known []string) (int, bool) {
	if i >= len(args) {
		return 0, false
	}
	clean := append(slices.Clone(known), args[i:]...)
	if name == "parallel" {
		return i, true
	}
	if (name == "timeout" || wrapperStart(name, clean) > len(known)) && !uncertainPositionalValid(name, args[i]) {
		return 0, false
	}
	target, ok := unwrapOnce(name, clean)
	if !ok || len(target) == 0 {
		return 0, false
	}
	start := len(args) - len(target)
	return start, start >= i && start < len(args)
}

// These wrappers consume a duration, CPU mask or scheduling priority before
// the command. A command name in that position cannot start a valid route.
func uncertainPositionalValid(name, value string) bool {
	if name != "timeout" && name != "taskset" && name != "chrt" {
		return true
	}
	if strings.ContainsAny(value, "$`") {
		return true
	}
	if name == "timeout" {
		value = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(value, "s"), "m"), "h"), "d")
		_, err := strconv.ParseFloat(value, 64)
		return err == nil || validHexDuration(value)
	}
	if name == "taskset" {
		for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '-' || r == ':' }) {
			if !validCPUPart(part) {
				return false
			}
		}
		return value != ""
	}
	_, err := strconv.Atoi(value)
	return err == nil
}

func validHexDuration(value string) bool {
	if !strings.HasPrefix(value, "0x") && !strings.HasPrefix(value, "0X") {
		return false
	}
	_, err := strconv.ParseUint(value[2:], 16, 64)
	return err == nil
}

func validCPUPart(value string) bool {
	if _, err := strconv.ParseUint(value, 0, 64); err == nil {
		return true
	}
	_, err := strconv.ParseUint(value, 16, 64)
	return err == nil
}

func uncertainWrapperTarget(name string, args []string, shell bool) (int, bool) {
	remaining := maxNestedScripts * 2
	for _, start := range uncertainWrapperTargets(name, args) {
		candidate := commandName(args[start])
		if shell && uncertainShellCandidate(candidate, args[start+1:], &remaining) || !shell && candidate == "kill" && (start == unknownWrapperOption(name, args)+1 || signalCall(&Call{Name: candidate, Args: args[start+1:]})) {
			return start, true
		}
	}
	return 0, false
}

func uncertainShellCandidate(name string, args []string, remaining *int) bool {
	if isShell(name) {
		return true
	}
	if !wrappers[name] || *remaining <= 0 {
		return false
	}
	*remaining--
	if name == "su" {
		return uncertainSuReadsStdin(args)
	}
	if start, ok := sudoDashDashAssignmentStart(name, args); ok {
		return uncertainShellCandidate(commandName(args[start]), args[start+1:], remaining)
	}
	if wrapperOptionsKnown(name, args) {
		target, ok := unwrapOnce(name, args)
		return ok && len(target) > 0 && uncertainShellCandidate(commandName(target[0]), target[1:], remaining)
	}
	for _, start := range uncertainWrapperTargets(name, args) {
		if uncertainShellCandidate(commandName(args[start]), args[start+1:], remaining) {
			return true
		}
	}
	return false
}

// anySignalCall reports whether a call in calls sends a signal.
func anySignalCall(calls []Call) bool {
	for i := range calls {
		if signalCall(&calls[i]) {
			return true
		}
	}
	return false
}

// signalCall recognises numeric and dynamic kill targets, excluding listing
// modes; pkill and killall are always considered signal calls.
func signalCall(call *Call) bool {
	if call.Name == "pkill" || call.Name == "killall" {
		return true
	}
	if call.Name != "kill" {
		return false
	}
	if len(call.Args) > 0 && (call.Args[0] == "-l" || call.Args[0] == "-L" || call.Args[0] == "--list" || call.Args[0] == "--table") {
		return false
	}
	var state signalScan
	for i := 0; i < len(call.Args); i++ {
		i = state.step(call.Args, i)
	}
	return state.target
}

type signalScan struct {
	target, signalOptionSeen, otherOption, afterDash bool
}

func (s *signalScan) step(args []string, i int) int {
	arg := args[i]
	if arg == "--" {
		s.afterDash = true
		return i
	}
	if s.afterDash {
		s.target = s.target || signalTargetWord(arg)
		return i
	}
	switch arg {
	case "--verbose", "-v", "-a", "--all":
		return i
	case "-q", "--queue":
		s.otherOption = true
		return i + 1
	case "--timeout":
		s.otherOption = true
		return i + 2
	}
	if next, option, valid := signalOption(args, i); option {
		if valid {
			if strings.HasPrefix(arg, "-") && integerWord(arg) && (s.signalOptionSeen || s.otherOption) {
				s.target = true
			} else {
				s.signalOptionSeen = true
			}
		}
		return next - 1
	}
	s.target = s.target || signalTargetWord(arg)
	return i
}

func signalTargetWord(arg string) bool {
	if arg == "" {
		return false
	}
	if strings.ContainsAny(arg, "$`%") {
		return true
	}
	if integerWord(arg) {
		return true
	}
	_, err := strconv.ParseInt(arg, 0, 64)
	return err == nil
}

func signalOption(args []string, i int) (int, bool, bool) {
	arg := args[i]
	if arg == "--" {
		return i + 1, true, true
	}
	if arg == "-s" || arg == "--signal" || arg == "-n" {
		return i + 2, true, i+1 < len(args) && signalName(args[i+1])
	}
	if value, ok := strings.CutPrefix(arg, "--signal="); ok {
		return i + 1, true, signalName(value)
	}
	if len(arg) > 2 && (strings.HasPrefix(arg, "-s") || strings.HasPrefix(arg, "-n")) {
		return i + 1, true, signalName(arg[2:])
	}
	if strings.HasPrefix(arg, "-") {
		return i + 1, true, signalName(arg[1:])
	}
	return i, false, true
}

func integerWord(value string) bool {
	_, err := strconv.Atoi(value)
	return err == nil
}

func signalName(name string) bool {
	if strings.ContainsAny(name, "$`") {
		return true
	}
	if _, err := strconv.Atoi(name); err == nil {
		return true
	}
	name = strings.TrimPrefix(strings.ToUpper(name), "SIG")
	if name == "RTMIN" || name == "RTMAX" {
		return true
	}
	if offset, ok := strings.CutPrefix(name, "RTMIN+"); ok {
		return validCPUPart(offset)
	}
	if offset, ok := strings.CutPrefix(name, "RTMAX-"); ok {
		return validCPUPart(offset)
	}
	return slices.Contains([]string{"HUP", "INT", "QUIT", "ILL", "TRAP", "ABRT", "BUS", "FPE", "KILL", "USR1", "SEGV", "USR2", "PIPE", "ALRM", "TERM", "CHLD", "CONT", "STOP", "TSTP", "TTIN", "TTOU", "URG", "XCPU", "XFSZ", "VTALRM", "PROF", "WINCH", "IO", "PWR", "SYS"}, name)
}

// taskset's older Calls behavior passes malformed attached CPU-list groups
// through, while other unknown taskset options stop wrapper descent.
func tasksetAttachedCPUCluster(arg string) bool {
	if len(arg) < 3 || arg[0] != '-' || arg[1] == '-' {
		return false
	}
	hasCPUList := false
	for _, r := range arg[1:] {
		key := "-" + string(r)
		if flagOptions["taskset"][key] {
			if key == "-c" {
				hasCPUList = true
			}
			continue
		}
		if !hasCPUList || !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return hasCPUList
}

func wrapperOptionStep(name string, args []string, i int) (int, bool) {
	arg := args[i]
	// GNU nice retains the historical `nice -N COMMAND` spelling alongside
	// `-n N`; a negative integer is its adjustment, not an unknown option.
	if name == "nice" && niceOldAdjustment(arg) {
		return i + 1, false
	}
	// xargs optional arguments are accepted only when attached to the option.
	if name == "xargs" && xargsOptionalOption(arg) {
		return i + 1, false
	}
	if arg == "--" {
		return len(args), false
	}
	if next, ok := dashWordOption(name, args, i); ok {
		return next, false
	}
	if strings.HasPrefix(arg, "--") {
		return longWrapperOptionStep(name, args, i)
	}
	return shortWrapperOptionStep(name, args, i)
}

func niceOldAdjustment(arg string) bool {
	if len(arg) < 2 || arg[0] != '-' {
		return false
	}
	value := arg[1:]
	if len(value) > 1 && (value[0] == '-' || value[0] == '+') {
		value = value[1:]
	}
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	_, err := strconv.Atoi(arg[1:])
	return err == nil
}

func xargsOptionalOption(arg string) bool {
	if len(arg) < 2 || arg[0] != '-' || arg[1] == '-' {
		return false
	}
	for j := 1; j < len(arg); j++ {
		key := "-" + arg[j:j+1]
		if key == "-e" || key == "-i" || key == "-l" {
			return flagOptions["xargs"][key]
		}
		if valueOptions["xargs"][key] {
			return false
		}
	}
	return false
}

func tasksetHasSeparateCPUList(args []string) bool {
	for _, arg := range args {
		if arg == "-c" || arg == "--cpu-list" {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
			hasCPUList := false
			validCluster := true
			for _, r := range arg[1:] {
				key := "-" + string(r)
				if !flagOptions["taskset"][key] {
					validCluster = false
					break
				}
				if key == "-c" {
					hasCPUList = true
				}
			}
			if validCluster && hasCPUList {
				return true
			}
		}
	}
	return false
}

func longWrapperOptionStep(name string, args []string, i int) (int, bool) {
	key, _, attached := strings.Cut(args[i], "=")
	key = wrapperLongOption(name, key)
	if !valueOptions[name][key] && !flagOptions[name][key] && !optionalValueOptions[name][key] {
		return i, true
	}
	if valueOptions[name][key] && !attached && i+1 < len(args) {
		return i + 2, false
	}
	return i + 1, false
}

func wrapperLongOption(name, key string) string {
	if key == "--" || !strings.HasPrefix(key, "--") {
		return key
	}
	if valueOptions[name][key] || flagOptions[name][key] {
		return key
	}
	match := ""
	for _, options := range []map[string]bool{valueOptions[name], flagOptions[name], optionalValueOptions[name]} {
		for option := range options {
			if strings.HasPrefix(option, key) {
				if match != "" {
					return key
				}
				match = option
			}
		}
	}
	if match == "" {
		return key
	}
	return match
}

func wrapperLongOptionAmbiguous(name, key string) bool {
	if key == "--" || !strings.HasPrefix(key, "--") || valueOptions[name][key] || flagOptions[name][key] {
		return false
	}
	matches := 0
	for _, options := range []map[string]bool{valueOptions[name], flagOptions[name], optionalValueOptions[name]} {
		for option := range options {
			if strings.HasPrefix(option, key) {
				matches++
			}
		}
	}
	return matches > 1
}

func shortWrapperOptionStep(name string, args []string, i int) (int, bool) {
	for j := 1; j < len(args[i]); j++ {
		key, takesValue := shortOptionAt(name, args[i], j)
		if optionalValueOptions[name][key] {
			return i + 1, false
		}
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
	for i := range s.more[key] {
		if sameCallArgs(s.more[key][i], call) {
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
	for i := range s.more[key] {
		if sameCall(s.more[key][i], call) {
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
	for i := range s.more[key] {
		if sameCall(s.more[key][i], call) {
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

func sudoDashDashAssignmentStart(name string, args []string) (int, bool) {
	_, end, ok := sudoDashDashAssignmentBounds(name, args)
	return end, ok
}

func sudoDashDashAssignmentWord(name string, args []string) (int, bool) {
	start, _, ok := sudoDashDashAssignmentBounds(name, args)
	return start, ok
}

func sudoDashDashAssignmentBounds(name string, args []string) (int, int, bool) {
	if name != "sudo" {
		return 0, 0, false
	}
	for i := 0; i < len(args); {
		if args[i] == "--" {
			start := i + 1
			end := start
			for end < len(args) && sudoEnvWord(name, args[end]) {
				end++
			}
			return start, end, end > start && end < len(args)
		}
		if sudoEnvWord(name, args[i]) {
			i++
		} else if strings.HasPrefix(args[i], "-") {
			next := optionNext(name, args, i)
			if next <= i {
				i++
			} else {
				i = next
			}
		} else {
			return 0, 0, false
		}
	}
	return 0, 0, false
}

// sudoDashDashRoute finds the command behind VAR=value words after sudo --,
// following the same wrapper chain used by regular unwrapping.
func sudoDashDashRoute(name string, args []string) ([]string, []string, int, bool) {
	var via []string
	wordStart := 1
	for range maxNestedScripts {
		if start, ok := sudoDashDashAssignmentStart(name, args); ok {
			return args[start:], append(via, name), wordStart + start, true
		}
		start, nextName, ok := sudoRouteNext(name, args)
		if !ok {
			return nil, nil, 0, false
		}
		wordStart += start + 1
		via = append(via, name)
		name, args = nextName, args[start+1:]
	}
	return nil, nil, 0, false
}

func sudoDashDashAssignmentRoute(name string, args []string) ([]string, []string, int, bool) {
	var via []string
	wordStart := 1
	for range maxNestedScripts {
		if start, ok := sudoDashDashAssignmentWord(name, args); ok {
			return args[start:], append(via, name), wordStart + start, true
		}
		start, nextName, ok := sudoRouteNext(name, args)
		if !ok {
			return nil, nil, 0, false
		}
		wordStart += start + 1
		via = append(via, name)
		name, args = nextName, args[start+1:]
	}
	return nil, nil, 0, false
}

func sudoRouteNext(name string, args []string) (int, string, bool) {
	if wrappers[name] && wrapperOptionsKnown(name, args) {
		wrapped, ok := unwrapOnce(name, args)
		if ok && len(wrapped) > 0 {
			return len(args) - len(wrapped), commandName(wrapped[0]), true
		}
		return 0, "", false
	}
	if !baselineWrapper(name) {
		return 0, "", false
	}
	start, ok := mainUnwrapStart(name, args)
	if !ok {
		return 0, "", false
	}
	return start, baseName(args[start]), true
}

// mainUnwrapStart mirrors the permissive option scan used by unwrapMain and
// returns the wrapped command's index in args.
func mainUnwrapStart(name string, args []string) (int, bool) {
	if !baselineWrapper(name) || name == "command" && commandIsLookup(args) {
		return 0, false
	}
	i := 0
	for i < len(args) && (strings.HasPrefix(args[i], "-") || sudoEnvWord(name, args[i])) {
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
	if name == "env" {
		i = envCommandStart(args, i)
	}
	return i, i < len(args)
}

func unwrapMain(name string, args []string) ([]string, []string, bool) {
	// На вложенных подстановках построение неиспользуемых результатов без обёртки
	// давало 13% всех выделенных объектов в профиле Find.
	if addedWrapper(name) {
		if !wrapperOptionsKnown(name, args) {
			return nil, nil, false
		}
		target, ok := unwrapOnce(name, args)
		if !ok || len(target) == 0 {
			return nil, nil, false
		}
		return target, []string{name}, true
	}
	if !baselineWrapper(name) {
		return nil, nil, false
	}
	var via [maxNestedScripts]string
	count := 0
	for baselineWrapper(name) {
		if count >= maxNestedScripts {
			return nil, nil, false
		}
		via[count] = name
		count++
		i, ok := mainUnwrapStart(name, args)
		if !ok {
			return nil, nil, false
		}
		name, args = baseName(args[i]), args[i+1:]
	}
	if name == "" {
		return nil, nil, false
	}
	return append([]string{name}, args...), slices.Clone(via[:count]), count > 0
}

func addedWrapper(name string) bool {
	switch name {
	case "prlimit", "systemd-inhibit", "dbus-run-session", "run0", "pkexec", "chroot", "setarch", "nsenter", "ssh-agent", "linux32", "linux64", "i386", "x86_64", "uname26":
		return true
	default:
		return false
	}
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
	childTilts := make([]Tilde, 0, len(parallelArgs)-start-1)
	for i := start + 1; i < len(parallelArgs); i++ {
		if parallelArgs[i] == ":::" || parallelArgs[i] == "::::" {
			continue
		}
		args = append(args, parallelArgs[i])
		argWord := c.words[parallelOffset+i+1]
		words = append(words, argWord)
		childTilts = append(childTilts, argWord.tilde)
	}
	if !slices.ContainsFunc(childTilts, func(mark Tilde) bool { return mark != 0 }) {
		childTilts = nil
	}
	child := cmd{call: Call{Name: commandWordName(parallelArgs[start], commandWord), Args: args, Raw: c.call.Raw, Line: c.call.Line,
		Via: c.via(wrapperVia...), Offset: commandWord.offset, Redirs: c.call.Redirs, tildeArgs: childTilts, Loop: c.call.Loop,
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

func literalShellScript(c cmd, name string, operand int) bool {
	if (name != "bash" && name != "sh") || c.call.Name != name || !c.literalContext || len(c.call.Via) != 0 || len(c.call.Redirs) != 0 || c.call.Loop || c.call.Background || c.call.ScriptInput || operand != 1 || len(c.words) < 3 || c.words[0].raw != name || c.call.Args[0] != "-c" {
		return false
	}
	return c.words[2].singleQuoted
}

// expand раскрывает то, что вызов запускает, кроме программы под обёрткой (её раскрывает emit).
func expand(c cmd, lineBase int, vars map[string]string, calls *[]Call, seen *callSet) error {
	name, args := c.call.Name, c.call.Args
	if target, ok := execTarget(execScanArgs(name, args)); ok {
		return emit(c.sub(len(c.words)-len(target), c.via(name, "exec")), lineBase, vars, calls, seen)
	}
	switch {
	case isShell(name):
		return expandShell(c, name, args, vars, calls)
	case name == "ssh":
		return expandSSH(c, lineBase, vars, calls)
	case name == "find":
		return expandFind(c, lineBase, vars, calls, seen)
	case scriptInvocation(name, args):
		return expandScriptInvocation(c, vars, calls)

	}
	return nil
}

func expandShell(c cmd, name string, args []string, vars map[string]string, calls *[]Call) error {
	if name == "fish" {
		return expandFishShell(c, args, vars, calls)
	}
	if isKornShell(name) {
		_, _, _, valid := kornInvocation(args, name)
		if !valid {
			return &NestedError{Script: strings.Join(args, " "), Err: fmt.Errorf("shellcmd: unsupported or ambiguous %s option", name)}
		}
	}
	operand, command, _ := shellArgs(name, args)
	if isKornShell(name) && command && operand < len(args) && kornCommandNameHasSubstitution(args[operand]) {
		return &NestedError{Script: args[operand], Err: fmt.Errorf("shellcmd: dynamic ksh command name is not modeled")}
	}
	if !command || operand >= len(args) || operand+1 >= len(c.words) || c.words[operand+1].script == "" {
		return nil
	}
	return expandShellCommand(c, name, operand, vars, calls)
}

func expandFishShell(c cmd, args []string, vars map[string]string, calls *[]Call) error {
	scripts, _, file, valid := fishInvocationOptions(args)
	if !valid {
		return &NestedError{Script: strings.Join(args, " "), Err: fmt.Errorf("shellcmd: unsupported or ambiguous fish option")}
	}
	for _, invocation := range scripts {
		text := args[invocation.index]
		if invocation.offset > 0 && invocation.offset <= len(text) {
			text = text[invocation.offset:]
		}
		unsupported := fishScriptUnsupported(text)
		if unsupported {
			return &NestedError{Script: text, Err: fmt.Errorf("shellcmd: fish script outside the supported subset")}
		}
	}
	var nestedErrs []error
	for _, invocation := range scripts {
		script, ok := mappedScriptWords(c, []scriptArg{{index: invocation.index, offset: invocation.offset}})
		if !ok || len(script.text) == 0 {
			continue
		}
		first := len(*calls)
		viaFlag := invocation.option
		if viaFlag == "" {
			viaFlag = "-c"
		}
		if err := parseNestedWithBudget(script.String(), script.pos, c.call.Line-1, c.via("fish", viaFlag), c.call.Loop, c.call.Background, vars, calls, c.budget); err != nil {
			nestedErrs = append(nestedErrs, err)
		}
		relocateNestedLines(c, first, calls)
	}
	if c.call.ScriptInput && c.stdinInput == nil {
		if body, ok, err := fishEchoPipeInput(c); err != nil {
			nestedErrs = append(nestedErrs, &NestedError{Script: c.call.Raw, Err: fmt.Errorf("shellcmd: parse fish pipe input: %w", err)})
		} else if ok {
			first := len(*calls)
			if err := parseNestedWithBudget(body, nil, c.call.Line-1, c.via("pipe", "fish"), c.call.Loop, c.call.Background, vars, calls, c.budget); err != nil {
				nestedErrs = append(nestedErrs, err)
			}
			relocateNestedLines(c, first, calls)
		} else {
			nestedErrs = append(nestedErrs, &NestedError{Script: c.call.Raw, Err: fmt.Errorf("shellcmd: fish script input is not available for parsing")})
		}
	}
	_ = file // File scripts are covered when their contents enter the shared nested-input path.
	return errors.Join(nestedErrs...)
}

func fishEchoPipeInput(c cmd) (string, bool, error) {
	end := c.call.Offset
	if end < 0 || end > len(c.source) {
		return "", false, nil
	}
	pipe := strings.LastIndex(c.source[:end], "|")
	if pipe < 0 {
		return "", false, nil
	}
	start := strings.LastIndexAny(c.source[:pipe], "\n;") + 1
	left := strings.TrimSpace(c.source[start:pipe])
	file, _, err := parseShell(left)
	if err != nil {
		return "", false, err
	}
	if len(file.Stmts) != 1 || len(file.Stmts[0].Redirs) != 0 || file.Stmts[0].Background {
		return "", false, nil
	}
	call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false, nil
	}
	name, ok := wordValue(call.Args[0])
	if !ok || name != "echo" {
		return "", false, nil
	}
	values := make([]string, len(call.Args)-1)
	for i, word := range call.Args[1:] {
		value, literal := wordValue(word)
		if !literal {
			return "", false, nil
		}
		values[i] = value
	}
	return strings.Join(values, " ") + "\n", true, nil
}

func expandShellCommand(c cmd, name string, operand int, vars map[string]string, calls *[]Call) error {
	script := c.words[operand+1]
	body, positions := script.script, script.pos
	first := len(*calls)
	previousLiteralShell := c.budget.literalShell
	previousPosix := c.budget.initialPosix
	previousBrace := c.budget.initialBrace
	previousPosixUnknown := c.budget.initialPosixUnknown
	previousBraceUnknown := c.budget.initialBraceUnknown
	c.budget.initialPosix = nestedShellPosix(c, name)
	c.budget.initialBrace = nestedShellBrace(c)
	c.budget.initialPosixUnknown, c.budget.initialBraceUnknown = false, false
	if literalShellScript(c, name, operand) {
		c.budget.literalShell = name
	}
	err := parseNestedWithBudget(body, positions, c.call.Line-1, c.via(name, "-c"), c.call.Loop, c.call.Background, vars, calls, c.budget)
	c.budget.literalShell = previousLiteralShell
	c.budget.initialPosix = previousPosix
	c.budget.initialBrace = previousBrace
	c.budget.initialPosixUnknown = previousPosixUnknown
	c.budget.initialBraceUnknown = previousBraceUnknown
	relocateNestedLines(c, first, calls)
	return err
}

func nestedShellPosix(c cmd, name string) bool {
	argv0 := name
	if c.argv0 != "" {
		argv0 = baseName(c.argv0)
	}
	if argv0 == "sh" || c.posixEnv {
		return true
	}
	return shellArgsEnablePosix(shellInvocationArgs(c))
}

func shellArgsEnablePosix(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" || arg == "-lc" || arg == "-ic" || arg == "--command" {
			continue
		}
		if arg == "--posix" {
			return true
		}
		if (arg == "-o" || arg == "+o") && i+1 < len(args) {
			if args[i+1] == "posix" {
				return arg == "-o"
			}
			i++
			continue
		}
		if strings.HasPrefix(arg, "-o") && len(arg) > 2 && arg[2:] == "posix" {
			return true
		}
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "+") {
			break
		}
	}
	return false
}

func nestedShellBrace(c cmd) bool {
	return shellArgsEnableBrace(shellInvocationArgs(c))
}

func shellArgsEnableBrace(args []string) bool {
	mode := true
	for i := range args {
		arg := args[i]
		if arg == "-c" || arg == "-lc" || arg == "-ic" || arg == "--command" {
			continue
		}
		if arg == "+B" || arg == "-B" {
			mode = arg[0] == '-'
			continue
		}
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "+") {
			break
		}
	}
	return mode
}

func shellInvocationArgs(c cmd) []string {
	args := c.call.Args
	if c.call.Name == "env" {
		start := envCommandStart(args, 0)
		if start < len(args) {
			args = args[start+1:]
		}
	}
	return args
}

func envHasPosixCorrect(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "POSIXLY_CORRECT=") {
			return true
		}
	}
	return false
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
	for i := start + 1; i < len(c.words); i++ {
		w := &c.words[i]
		if w.script == "" {
			return nil
		}
		if i > start+1 {
			remote.add(" ", -1)
		}
		remote.text = append(remote.text, w.script...)
		remote.pos = append(remote.pos, w.pos...)
	}
	remoteText := remote.String()
	file, full, ignored, parseErr := c.budget.parseShell(remoteText)
	if parseErr != nil {
		return &NestedError{Script: remoteText, Err: fmt.Errorf("shellcmd: parse shell: %w", parseErr)}
	}
	first := len(*calls)
	if err := parseNestedFileWithBudget(file, full, remoteText, remote.pos, c.call.Line-1, c.via("ssh"), c.call.Loop, c.call.Background, vars, calls, ignored, nil, c.budget); err != nil {
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
	for i := range remoteCalls {
		call := &remoteCalls[i]
		if !isShell(call.Name) || !shellReadsHeredoc(call.Name, call.Args) {
			continue
		}
		if reader == nil {
			reader = newRemoteStdin(file, src, pos)
		}
		if reader.reads(call.Offset) {
			return *call, true
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

// redirsAt returns the redirections of every statement holding offset,
// outermost first, so later ones override earlier ones for the same descriptor.
func (index *stdinIndex) redirsAt(offset int) []*syntax.Redirect {
	ids := index.stmts.containing(offset)
	if len(ids) == 1 {
		return index.redirs[ids[0]]
	}
	var redirs []*syntax.Redirect
	for _, id := range ids {
		redirs = append(redirs, index.redirs[id]...)
	}
	return redirs
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
	if err := parseNestedMapped(body, bodyLine, via, c.call.Loop, c.call.Background, vars, calls, c.budget); err != nil {
		return err
	}
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

// parseNestedMapped skips substitutions already evaluated by the outer shell.
// The node position is mapped to the outer source before checking the spans;
// Call.Offset can be unknown even when the node has a source position.
func parseNestedMapped(body mapped, line int, via []string, inLoop, inBackground bool, vars map[string]string, calls *[]Call, budget *nestedParseBudget) error {
	if len(body.subst) == 0 {
		return parseNestedWithBudget(body.String(), body.pos, line, via, inLoop, inBackground, vars, calls, budget)
	}
	return parseNestedFileWithBudget(nil, "", body.String(), body.pos, line, via, inLoop, inBackground, vars, calls, nil, &body, budget)
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
	previousPosix, previousBrace := c.budget.initialPosix, c.budget.initialBrace
	previousPosixUnknown, previousBraceUnknown := c.budget.initialPosixUnknown, c.budget.initialBraceUnknown
	c.budget.initialPosix = c.mode.posix || c.mode.posixUnknown
	c.budget.initialBrace = c.mode.brace || c.mode.braceUnknown
	c.budget.initialPosixUnknown = c.mode.posixUnknown
	c.budget.initialBraceUnknown = c.mode.braceUnknown
	if c.call.Name == "eval" && c.posixEnv {
		c.budget.initialPosix, c.budget.initialPosixUnknown = true, false
	}
	if c.call.Name == "env" {
		end := envCommandStart(c.call.Args, 0)
		if envHasPosixCorrect(c.call.Args[:end]) {
			c.budget.initialPosix = true
			c.budget.initialPosixUnknown = false
		}
	}
	if c.call.Name == "flock" {
		c.budget.initialPosixUnknown, c.budget.initialBraceUnknown = true, true
	}
	err := parseNestedMapped(script, c.call.Line-1, c.via(viaName), c.call.Loop, c.call.Background, vars, calls, c.budget)
	c.budget.initialPosix, c.budget.initialBrace = previousPosix, previousBrace
	c.budget.initialPosixUnknown, c.budget.initialBraceUnknown = previousPosixUnknown, previousBraceUnknown
	relocateNestedLines(c, first, calls)
	if c.call.Name == "env" {
		for i := first; i < len(*calls); i++ {
			call := &(*calls)[i]
			if len(call.Via) == len(c.call.Via)+1 && call.Via[len(call.Via)-1] == "env" {
				for j := range call.tildeArgs {
					call.tildeArgs[j] = 0
				}
				for j := range call.Redirs {
					call.Redirs[j].Tilde = 0
				}
			}
		}
	}
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
		if w.script == "" && (c.call.Name != "eval" || w.offset < 0) || arg.offset > len(w.script) {
			return mapped{}, false
		}
		if i > 0 {
			script.add(" ", -1)
		}
		script.text = append(script.text, w.script[arg.offset:]...)
		script.pos = append(script.pos, w.pos[arg.offset:]...)
		script.subst = append(script.subst, w.subst...)
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
		if isShell(name) {
			return shellCommandArgument(args, i+1)
		}
		if i+1 < len(args) {
			return scriptArg{index: i + 1}, true
		}
		return scriptArg{}, false
	}
	if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") {
		return shortCommandOption(name, args, i, arg)
	}
	return scriptArg{}, false
}

func shellCommandArgument(args []string, start int) (scriptArg, bool) {
	for j := start; j < len(args); {
		option := args[j]
		if option == "-o" || option == "+o" || option == "-O" || option == "+O" {
			j += 2
			continue
		}
		if strings.Contains(" --posix --norc --noprofile --restricted -B +B -i -l -r -s ", " "+option+" ") {
			j++
			continue
		}
		if strings.HasPrefix(option, "-") && !strings.HasPrefix(option, "--") && len(option) > 1 && onlySetOptionLetters(option[1:]) {
			j++
			continue
		}
		return scriptArg{index: j}, true
	}
	return scriptArg{}, false
}

func shortCommandOption(name string, args []string, i int, arg string) (scriptArg, bool) {
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
	return scriptArg{}, false
}

func envSplitScriptArgs(args []string) []scriptArg {
	arg, ok := envSplitArg(args)
	if !ok {
		return nil
	}
	if arg.index < len(args) {
		text := args[arg.index]
		if arg.offset > 0 && arg.offset <= len(text) {
			text = text[arg.offset:]
		}
		fields := strings.Fields(text)
		start := wrapperOptionEnd("env", fields, 0)
		start = envCommandStart(fields, start)
		if start > 0 && start < len(fields) {
			if offset := strings.Index(text, fields[start]); offset >= 0 {
				arg.offset += offset
			}
		}
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
		if strings.HasPrefix(arg, "--") {
			key, value, attached := strings.Cut(arg, "=")
			if wrapperLongOption("env", key) == "--split-string" && valueOptions["env"]["--split-string"] {
				if attached {
					return scriptArg{index: i, offset: len(arg) - len(value)}, true
				}
				if i+1 < len(args) {
					return scriptArg{index: i + 1}, true
				}
				return scriptArg{}, false
			}
		}
		if valueOptions["env"]["-S"] && arg == "-S" || valueOptions["env"]["--split-string"] && arg == "--split-string" {
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

func envSplitCommand(args []string) ([]string, bool) {
	arg, ok := envSplitArg(args)
	if !ok || arg.index >= len(args) {
		return nil, false
	}
	text := args[arg.index]
	if arg.offset > 0 && arg.offset <= len(text) {
		text = text[arg.offset:]
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil, false
	}
	start := wrapperOptionEnd("env", words, 0)
	start = envCommandStart(words, start)
	if start >= len(words) {
		return nil, false
	}
	target := append([]string(nil), words[start:]...)
	target = append(target, args[arg.index+1:]...)
	return target, len(target) > 0
}

func envShortSplitArg(arg string, index int, args []string) (scriptArg, bool) {
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return scriptArg{}, false
	}
	for j := 1; j < len(arg); j++ {
		option, takesValue := shortOptionAt("env", arg, j)
		if option == "-S" && takesValue && valueOptions["env"]["-S"] {
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
	for i < len(args) && wrapperOptionWord(name, args[i]) {
		if args[i] == "--" {
			return i + 1
		}
		i = optionNext(name, args, i)
	}
	return i
}

// wrapperOptionWord — слово, которое обёртка разбирает до своей команды: ключ или,
// у sudo, присваивание окружения между ключами.
func wrapperOptionWord(name, arg string) bool {
	return strings.HasPrefix(arg, "-") && arg != "-" || sudoEnvWord(name, arg)
}

// sudoEnvWord — VAR=value среди ключей sudo: слово с '=' не первым символом sudo кладёт
// в окружение команды и продолжает разбор ключей; слово с '/' первым sudo запускает как путь
// к команде, а после `--` любое такое слово уже команда.
func sudoEnvWord(name, arg string) bool {
	return name == "sudo" && strings.IndexByte(arg, '=') > 0 && arg[0] != '-' && arg[0] != '/'
}

func optionNext(name string, args []string, i int) int {
	if i >= len(args) {
		return i
	}
	arg := args[i]
	if arg == "--" {
		return i + 1
	}
	if sudoEnvWord(name, arg) {
		return i + 1
	}
	if !strings.HasPrefix(arg, "-") || arg == "-" {
		return i
	}
	if name == "xargs" && xargsOptionalOption(arg) {
		return i + 1
	}
	if strings.HasPrefix(arg, "--") {
		key, _, hasValue := strings.Cut(arg, "=")
		key = wrapperLongOption(name, key)
		if !hasValue && valueOptions[name][key] && i+1 < len(args) {
			return i + 2
		}
		return i + 1
	}
	if next, ok := dashWordOption(name, args, i); ok {
		return next
	}
	for j := 1; j < len(arg); j++ {
		key, takesValue := shortOptionAt(name, arg, j)
		if optionalValueOptions[name][key] {
			return i + 1
		}
		if takesValue {
			if j == len(arg)-1 && i+1 < len(args) {
				return i + 2
			}
			return i + 1
		}
	}
	return i + 1
}

// dashWordOption reads a whole word that names a multi-letter option after
// one dash (unbuffer -noecho, -ignore SIG): it is that option, not a cluster
// of one-letter options. It returns the index after the option and its value.
func dashWordOption(name string, args []string, i int) (int, bool) {
	arg := args[i]
	if len(arg) <= 2 || arg[1] == '-' {
		return i, false
	}
	if valueOptions[name][arg] {
		return min(i+2, len(args)), true
	}
	if flagOptions[name][arg] {
		return i + 1, true
	}
	return i, false
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

func execScanArgs(name string, args []string) []string {
	if name == "command" && commandIsLookup(args) {
		return nil
	}
	if wrappers[name] {
		return args[wrapperOptionEnd(name, args, 0):]
	}
	return args
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
	// Без обёртки результат игнорируется; его построение давало 8% объектов
	// в профиле Find на вложенных подстановках.
	if !wrappers[name] {
		return nil, nil, false
	}
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
	if name == "command" && commandIsLookup(args) {
		return nil, false
	}
	if (name == "setsid" || name == "flock" || name == "chrt" || name == "strace" || addedWrapper(name)) && wrapperExitsWithoutCommand(name, args) {
		return nil, false
	}
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
	i := wrapperStart(name, args)
	if name == "timeout" && i < len(args) && args[i] != "--" {
		i++
	}
	if name == "env" {
		i = envCommandStart(args, i)
	}
	if i >= len(args) {
		return nil, false
	}
	return args[i:], true
}

// envCommandStart пропускает присваивания env и -i или - перед ними: GNU env заканчивает
// ключи на первом присваивании, и -i или - после него — уже команда.
func envCommandStart(args []string, i int) int {
	for assigned := false; i < len(args); i++ {
		if strings.Contains(args[i], "=") {
			assigned = true
			continue
		}
		if assigned || args[i] != "-i" && (args[i] != "-" || i+1 == len(args)) {
			break
		}
	}
	return i
}

func wrapperExitsWithoutCommand(name string, args []string) bool {
	for i := 0; i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-"; {
		if shortGroupExitsWithoutCommand(name, args[i]) {
			return true
		}
		arg, _, _ := strings.Cut(args[i], "=")
		arg = wrapperLongOption(name, arg)
		if name == "systemd-inhibit" && arg == "--list" || name == "setarch" && (arg == "--list" || arg == "--show") {
			return true
		}
		if name == "chrt" && (arg == "-m" || arg == "--max") {
			return true
		}
		if arg == "-h" || arg == "--help" || arg == "-V" || arg == "--version" {
			return true
		}
		i = optionNext(name, args, i)
	}
	return false
}

func shortGroupExitsWithoutCommand(name, arg string) bool {
	if len(arg) < 2 || arg[0] != '-' || arg[1] == '-' {
		return false
	}
	for j := 1; j < len(arg); j++ {
		key, takesValue := shortOptionAt(name, arg, j)
		if key == "-h" || key == "-V" || name == "chrt" && key == "-m" {
			return true
		}
		if name == "ssh-agent" && slices.Contains([]string{"-c", "-s", "-D", "-d", "-k"}, key) {
			return true
		}
		if takesValue || optionalValueOptions[name][key] {
			return false
		}
	}
	return false
}

func wrapperStart(name string, args []string) int {
	switch name {
	case "command":
		return commandTargetStart(args)
	case "setarch":
		return setarchStart(args)
	case "prlimit", "taskset":
		if hasOption(name, args, "-p", "--pid") {
			return len(args)
		}
	}
	return wrapperOperandEnd(name, args, wrapperOptionEnd(name, args, 0))
}

// setarchStart skips the optional architecture operand, then the options.
func setarchStart(args []string) int {
	i := 0
	if i < len(args) && args[i] != "--" && !strings.HasPrefix(args[i], "-") {
		i++ // optional architecture
	}
	return wrapperOptionEnd("setarch", args, i)
}

// wrapperOperandEnd skips the operands a wrapper takes before its command.
func wrapperOperandEnd(name string, args []string, i int) int {
	switch name {
	case "chroot", "flock":
		if i < len(args) {
			i++ // NEWROOT, or the lock file
		}
	case "taskset":
		if i < len(args) && (!hasOption(name, args, "-c", "--cpu-list") || tasksetHasSeparateCPUList(args[:i])) {
			i++
		}
	case "chrt":
		if i < len(args) && args[i] != "--" {
			i++
		}
	}
	return i
}

// commandTargetStart treats expanded words as uncertain options while looking
// for the command operand. Their runtime value may be an option, so a later
// literal word can still be the command being wrapped.
func commandTargetStart(args []string) int {
	for i, arg := range args {
		if arg == "--" {
			return i + 1
		}
		if strings.ContainsAny(arg, "$`") {
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			continue
		}
		return i
	}
	return len(args)
}

func hasOption(name string, args []string, options ...string) bool {
	for i := 0; i < len(args) && wrapperOptionWord(name, args[i]); {
		if args[i] == "--" {
			return false
		}
		if sudoEnvWord(name, args[i]) {
			i++
			continue
		}
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
	if name != "sudo" && strings.HasPrefix(arg, "--") {
		return false
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

// commandIsLookup identifies short option groups that look up command names.
func commandIsLookup(args []string) bool {
	for _, arg := range args {
		if arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-") {
			return false
		}
		// An option assembled by expansion is not a literal command lookup.
		if strings.ContainsAny(arg, "$`") {
			return false
		}
		if !strings.HasPrefix(arg, "--") && strings.ContainsAny(arg[1:], "vV") {
			return true
		}
	}
	return false
}

// wrappers — программы, которые запускают программу из своих аргументов.
var wrappers = set("timeout", "env", "sudo", "nohup", "nice", "ionice", "stdbuf", "unbuffer", "time", "command", "builtin", "exec", "xargs", "taskset", "chrt", "strace", "setpriv", "unshare", "flock", "busybox", "systemd-run", "parallel", "runuser", "doas", "su", "setsid", "prlimit", "systemd-inhibit", "dbus-run-session", "run0", "pkexec", "chroot", "setarch", "nsenter", "ssh-agent", "linux32", "linux64", "i386", "x86_64", "uname26")

// valueOptions — опции обёрток и ssh, следующее слово после которых — их значение, а не программа.
var valueOptions = map[string]map[string]bool{
	"timeout":          set("-s", "--signal", "-k", "--kill-after"),
	"env":              set("-u", "--unset", "-C", "--chdir", "-S", "--split-string", "-a", "--argv0"),
	"sudo":             set("-u", "--user", "-g", "--group", "-C", "--close-from", "-D", "--chdir", "-h", "--host", "-p", "--prompt", "-r", "--role", "-t", "--type", "-T", "--command-timeout", "-U", "--other-user", "-R", "--chroot"),
	"nice":             set("-n", "--adjustment"),
	"unbuffer":         set("-ignore"),
	"ionice":           set("-c", "--class", "-n", "--classdata", "-p", "--pid", "-P", "--pgid", "-u", "--uid"),
	"stdbuf":           set("-i", "--input", "-o", "--output", "-e", "--error"),
	"time":             set("-f", "--format", "-o", "--output"),
	"exec":             set("-a"),
	"xargs":            set("-I", "-n", "--max-args", "-P", "--max-procs", "-L", "-d", "--delimiter", "-E", "-s", "--max-chars", "-a", "--arg-file", "--process-slot-var"),
	"ssh":              set("-B", "-b", "-c", "-D", "-E", "-e", "-F", "-I", "-i", "-J", "-L", "-l", "-m", "-O", "-o", "-P", "-p", "-Q", "-R", "-S", "-W", "-w"),
	"taskset":          set("-p", "--pid"),
	"setsid":           set(),
	"chrt":             set("-p", "--pid", "-P", "-T", "-D", "--sched-period", "--sched-runtime", "--sched-deadline"),
	"strace":           set("-o", "--output", "-e", "-p", "--attach", "-u", "-s", "-O", "--summary-syscall-overhead", "-I", "-E", "-P", "-a", "-S", "-X", "-b", "--detach-on", "-U", "--summary-columns", "--argv0", "--trace", "--trace-fds", "--trace-path", "--abbrev", "--verbose", "--signal", "--status", "--inject", "--fault", "--syscall-limit", "--read", "--write", "--user", "--string-limit", "--env", "--columns", "--const-print-style", "--stack-trace-frame-limit", "--summary-sort-by", "--interruptible", "--decode-pids", "--raw", "--kvm", "--color"),
	"setpriv":          set("--reuid", "--ruid", "--euid", "--regid", "--rgid", "--egid", "--groups", "--inh-caps", "--ambient-caps", "--bounding-set", "--securebits", "--apparmor-profile", "--selinux-label", "--pdeathsig", "--ptracer", "--landlock-access", "--landlock-rule", "--landlock-restrict", "--seccomp-filter"),
	"unshare":          set("-S", "-G", "-R", "-w", "-l", "--setuid", "--setgid", "--setgroups", "--root", "--wd", "--load-interp", "--map-user", "--map-group", "--map-users", "--map-groups", "--monotonic", "--boottime", "--propagation"),
	"flock":            set("-E", "--conflict-exit-code", "-w", "--wait", "--timeout"),
	"systemd-run":      set("--unit", "--description", "--slice", "--property", "-p", "--uid", "--gid", "--working-directory", "--service-type", "-E", "--setenv", "-u", "-M", "-H", "--host", "--machine", "--on-active", "--on-boot", "--on-startup", "--on-unit-active", "--on-unit-inactive", "--on-calendar", "--timer-property", "--nice", "--root-directory", "--json", "--job-mode", "--path-property", "--socket-property", "--background", "--expand-environment", "-C", "--capsule"),
	"prlimit":          set("-o", "--output", "-p", "--pid"),
	"systemd-inhibit":  set("--what", "--who", "--why", "--mode"),
	"dbus-run-session": set("--config-file", "--dbus-daemon"),
	"run0":             set("-u", "--user", "-g", "--group", "--machine", "--unit", "--property", "--description", "--slice", "--nice", "-D", "--chdir", "--setenv", "--background", "--shell-prompt-prefix", "--lightweight", "--area"),
	"pkexec":           set("--user"),
	"chroot":           set("--userspec", "--groups"),
	"nsenter":          set("-t", "--target", "-N", "--net-socket", "-S", "--setuid", "-G", "--setgid"),
	"ssh-agent":        set("-E", "-a", "-O", "-P", "-t"),
	"runuser":          set("-u", "--user", "-g", "--group", "-G", "--supp-group", "-c", "--command", "--session-command", "-s", "--shell", "-w", "--whitelist-environment"),
	"su":               set("-c", "--command", "--session-command", "-s", "--shell", "-g", "--group", "-G", "--supp-group", "-w", "--whitelist-environment"),
	"doas":             set("-u", "--user", "-a", "--auth-type", "-C", "--config"),
	"watch":            set("-n", "--interval", "-q", "--equexit"),
	"script":           set("-c", "--command", "-E", "--echo", "-I", "--log-in", "-O", "--log-out", "-T", "--log-timing", "-B", "--log-io", "-m", "--logging-format", "-o", "--output-limit"),
	"parallel":         set("-j", "--jobs", "-n", "--max-args", "-P", "--max-procs", "-L", "--max-lines", "-d", "--delimiter", "-S", "--sshlogin", "-a", "--arg-file", "--sshloginfile", "--joblog", "--results", "--workdir", "--env", "--block", "--recstart", "--recend", "--regexp", "--tagstring", "--halt", "--timeout", "--delay", "--memfree", "--load", "--nice", "--filter", "--return", "--transferfile", "--cleanup", "--trc", "--basefile", "--record-env", "-C", "--colsep", "-I", "-E", "--retries", "--tmpdir", "--header", "--termseq", "--sshdelay", "--group-by", "--compress-program", "--limit", "--memsuspend"),
	"tmux":             set("-L", "-S", "-f", "-c", "-t", "-s", "-n", "-x", "-y", "-e", "-F", "-B", "-l", "-m", "-R", "-T", "-p"),
}

var flagOptions = map[string]map[string]bool{
	"time":             set("-p", "--portability", "-v", "--verbose", "-a", "--append", "-q", "--quiet"),
	"env":              set("-i", "--ignore-environment", "-0", "--null", "-v", "--debug", "--block-signal", "--default-signal", "--ignore-signal", "--list-signal-handling"),
	"exec":             set("-c", "-l"),
	"sudo":             set("-A", "--askpass", "-B", "--bell", "-P", "--preserve-groups", "-n", "--non-interactive", "-E", "--preserve-env", "-H", "--set-home", "-S", "--stdin", "-b", "--background", "-k", "--reset-timestamp", "-K", "--remove-timestamp", "-l", "--list", "-V", "--version", "-s", "--shell", "-i", "--login", "-e", "--edit", "-q", "--quiet", "-N", "--no-update"),
	"timeout":          set("-f", "--foreground", "-p", "--preserve-status", "-v", "--verbose"),
	"nohup":            set("--help", "--version"),
	"unbuffer":         set("-p", "-console", "-noecho", "-nottycopy", "-nottyinit"),
	"taskset":          set("-a", "--all-tasks", "-c", "--cpu-list"),
	"setsid":           set("-c", "--ctty", "-f", "--fork", "-w", "--wait", "-h", "--help", "-V", "--version"),
	"chrt":             set("-o", "--other", "-f", "--fifo", "-r", "--rr", "-b", "--batch", "-i", "--idle", "-d", "--deadline", "-R", "--reset-on-fork", "-a", "--all-tasks", "-m", "--max", "-v", "--verbose"),
	"strace":           set("-A", "-C", "-f", "-ff", "-c", "-q", "-qq", "-T", "-tt", "-y", "-yy", "-D", "-d", "--debug", "-h", "-i", "-k", "-n", "-N", "-r", "-t", "-v", "-w", "-x", "-xx", "-z", "--successful-only", "-Z", "--failed-only", "-Y", "-V", "--absolute-timestamps", "--always-show-pid", "--follow-forks", "--daemonize", "--summary", "--summary-wall-clock", "--summary-only", "--quiet", "--instruction-pointer", "--syscall-number", "--arg-names", "--relative-timestamps", "--syscall-times", "--no-abbrev", "--strings-in-hex", "--stack-trace", "--decode-fds", "--namespace", "--secontext", "--kill-on-exit", "--output-append-mode", "--output-separately", "--seccomp-bpf", "--tips", "--help", "--version"),
	"unshare":          set("-r", "--map-root-user", "-c", "--map-current-user", "--map-subids", "-U", "--user", "-f", "--fork", "-m", "--mount", "-p", "--pid", "-n", "--net", "-i", "--ipc", "-u", "--uts", "-C", "--cgroup", "-T", "--time", "--mount-proc", "--mount-binfmt", "--kill-child", "--map-auto", "--keep-caps"),
	"setpriv":          set("--init-groups", "--clear-groups", "--no-new-privs", "--keep-caps", "--dump", "--list-caps"),
	"watch":            set("-d", "--differences", "-t", "--no-title", "-c", "--color", "-g", "--chgexit", "-e", "--errexit", "-b", "--beep", "-p", "--precise", "-x", "--exec"),
	"parallel":         set("-k", "--keep-order", "--tag", "-q", "--quote", "--ungroup", "--group", "--line-buffer", "--tagstring"),
	"flock":            set("-x", "--exclusive", "-e", "-s", "--shared", "-u", "--unlock", "-n", "--nonblocking", "--nb", "-o", "--close", "-F", "--no-fork", "--fcntl", "--verbose", "-h", "--help", "-V", "--version"),
	"systemd-run":      set("-r", "--remain-after-exit", "--slice-inherit", "--tty", "-q", "--quiet", "-t", "--pty", "-T", "--pty-late", "-P", "--pipe", "-G", "--collect", "-d", "--user", "--scope", "--wait", "--system", "--same-dir", "-R", "--same-root-dir", "--send-sighup", "--no-ask-password", "--no-block", "--no-pager", "--ignore-failure", "-S", "--shell", "-v", "--verbose", "--on-clock-change", "--on-timezone-change"),
	"doas":             set("-n", "--non-interactive", "-s", "-L", "--"),
	"ionice":           set("-t", "--ignore"),
	"xargs":            set("-e", "-i", "-l", "--eof", "--replace", "--max-lines", "-r", "--no-run-if-empty", "-t", "--verbose", "-x", "--exit", "-0", "--null", "-p", "--interactive", "-o", "--open-tty", "--show-limits"),
	"script":           set("-q", "--quiet", "-f", "--flush", "-V", "--version"),
	"command":          set("-p", "-x"),
	"tmux":             set("-2", "-8", "-C", "-D", "-N", "-V", "-v", "-q", "-u"),
	"su":               set("-l", "--login", "-m", "--preserve-environment", "-p", "--preserve-environment", "-f", "--fast"),
	"runuser":          set("-l", "--login", "-m", "--preserve-environment", "-p", "--preserve-environment", "-f", "--fast", "-P", "--pty", "-T", "--no-pty", "--close-session", "--reset-environment"),
	"prlimit":          set("--noheadings", "--raw", "--verbose", "-h", "--help", "-V", "--version"),
	"systemd-inhibit":  set("--no-ask-password", "--no-pager", "--no-legend", "--list", "-h", "--help", "--version"),
	"dbus-run-session": set("--", "--help", "--version"),
	"run0":             set("--no-ask-password", "--slice-inherit", "--via-shell", "-i", "--pty", "--pty-late", "--pipe", "--empower", "-h", "--help", "-V", "--version"),
	"pkexec":           set("--disable-internal-agent", "--help", "--version", "--keep-cwd"),
	"chroot":           set("--skip-chdir"),
	"setarch":          set("-B", "--32bit", "-F", "--fdpic-funcptrs", "-I", "--short-inode", "-L", "--addr-compat-layout", "-R", "--addr-no-randomize", "-S", "--whole-seconds", "-T", "--sticky-timeouts", "-X", "--read-implies-exec", "-Z", "--mmap-page-zero", "-3", "--3gb", "--4gb", "--uname-2.6", "-v", "--verbose", "--list", "-h", "--help", "-V", "--version"),
	"nsenter":          set("-a", "--all", "--user-parent", "--preserve-credentials", "--keep-caps", "-e", "--env", "-F", "--no-fork", "-c", "--join-cgroup", "-Z", "--follow-context", "-h", "--help", "-V", "--version"),
	"ssh-agent":        set("-c", "-D", "-d", "-k", "-s", "-T", "-U", "--"),
}

// optionalValueOptions are accepted either without a value or with an attached value.
// util-linux prlimit resource limits require attached values; nsenter namespace options
// accept an optional attached path or ID.
var optionalValueOptions = map[string]map[string]bool{
	"prlimit": set("-c", "--core", "-d", "--data", "-e", "--nice", "-f", "--fsize", "-i", "--sigpending", "-l", "--memlock", "-m", "--rss", "-n", "--nofile", "-q", "--msgqueue", "-r", "--rtprio", "-s", "--stack", "-t", "--cpu", "-u", "--nproc", "-v", "--as", "-x", "--locks", "-y", "--rttime"),
	"nsenter": set("-m", "--mount", "-u", "--uts", "-i", "--ipc", "-n", "--net", "-p", "--pid", "-C", "--cgroup", "-U", "--user", "-T", "--time", "-r", "--root", "-w", "--wd", "-W", "--wdns"),
	"setarch": set("--show"),
}

func init() {
	for _, alias := range []string{"linux32", "linux64", "i386", "x86_64", "uname26"} {
		valueOptions[alias] = valueOptions["setarch"]
		flagOptions[alias] = flagOptions["setarch"]
		optionalValueOptions[alias] = optionalValueOptions["setarch"]
	}
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
	file, _, err := parseShell(cmd)
	if err != nil {
		return false, fmt.Errorf("parse shell functions: %w", err)
	}
	shadowed := false
	syntax.Walk(file, func(n syntax.Node) bool {
		if fn, ok := n.(*syntax.FuncDecl); ok && int(fn.Pos().Offset()) < c.Offset {
			for _, name := range funcDeclNames(fn) {
				shadowed = shadowed || unescapeBare(name.Value) == c.Name
			}
		}
		return !shadowed
	})
	return shadowed, nil
}
