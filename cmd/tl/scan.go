package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/waitrec"
)

// hit — событие с его файлом.
type hit struct {
	src source
	ev  transcript.Event
	// cut, when set, is the text of the listing line already cut to its width: the database path gets only the
	// start of a long text.
	cut *string
}

type result struct {
	hits        []hit
	raw         rawSummary      // --raw: what matched beyond the listed events
	waits       []time.Duration // --wait: ожидания вывода, сведённые по ходу чтения файлов
	waitRows    []waitSample
	names       map[string]string // path → имя сессии
	linkIDs     map[string]string // path → уникальный id для ссылок в этой выборке
	searchPaths []string          // all transcript names reachable by find
	liveNames   map[string]bool   // names that take precedence in find
	liveErr     error             // registry records not read: links may coincide with their names
	files       int
	errs        []error // файлы, которые не прочитались: их события не вошли
	bad         int     // строк, которые не разобрать, по всем файлам
	// firstBad — первая из них с id файла, чтобы было куда смотреть.
	firstBad error
}

// filter — отбор событий одного файла: роль, время, поиск и окрестность найденного.
type filter struct {
	opts    options
	re      *regexp.Regexp
	rawRE   *regexp.Regexp
	needles [][]string // обязательные условия: внутри условие — варианты, условия выполняются все
}

func newFilter(opts options) (*filter, error) {
	f := &filter{opts: opts}
	if opts.raw != "" {
		re, err := regexp.Compile("(?i)" + opts.raw)
		if err != nil {
			return nil, fmt.Errorf("--raw: %w", err)
		}
		f.rawRE = re
	}
	if opts.grep == "" {
		return f, nil
	}
	re, err := regexp.Compile("(?i)" + opts.grep)
	if err != nil {
		return nil, fmt.Errorf("--grep: %w", err)
	}
	f.re = re
	// Отсев возможен только по фрагменту, который после сборки события остаётся в исходном JSON подряд.
	if parsed, err := syntax.Parse(opts.grep, syntax.Perl); err == nil && opts.context == 0 {
		for _, group := range requiredLiterals(parsed) {
			if !syntheticCondition(group) {
				f.needles = append(f.needles, group)
			}
		}
	}
	return f, nil
}

// Такие слова могут появиться в событии без соответствующего фрагмента исходной строки.
func syntheticCondition(group []string) bool {
	synthetic := transcript.SyntheticText()
	for _, needle := range group {
		if hasFolded([]byte(synthetic), needle) || strings.IndexFunc(needle, unicode.IsNumber) >= 0 {
			return true
		}
	}
	return false
}

// Пропущенный вызов мог дать Who результата: сохраняем как возможные все условия, не опровергнутые строкой.
// keepsBad tells whether a read with this filter parses a line that is not JSON: the prefilter of --grep and
// --raw passes it.
func (f *filter) keepsBad(raw []byte) bool {
	if f.rawRE != nil && !f.rawRE.Match(raw) {
		return false
	}
	for _, alternatives := range f.needles {
		if !slices.ContainsFunc(alternatives, func(needle string) bool { return hasFolded(raw, needle) }) {
			return false
		}
	}
	return true
}

func (f *filter) keepWithState(raw []byte, tools transcript.ToolNames) bool {
	keep, absent := f.keepRawState(raw, tools)
	if !keep && absent >= 0 {
		tools.PlainType()
	}
	if keep || absent >= 0 && len(f.needles) == 1 ||
		!bytes.Contains(raw, []byte(`"tool_use"`)) && !bytes.Contains(raw, []byte(`\u`)) {
		return keep
	}
	for i, alternatives := range f.needles {
		if i == absent {
			continue
		}
		for _, needle := range alternatives {
			if absent < 0 && bytes.Contains(raw, []byte(`\u`)) || hasFolded(raw, needle) {
				tools.AddPossible(needle)
			}
		}
	}

	return false
}

func (f *filter) keepRawState(raw []byte, tools transcript.ToolNames) (bool, int) {
	for index, alternatives := range f.needles {
		found := false
		for _, needle := range alternatives {
			if hasFolded(raw, needle) {
				found = true
				break
			}
		}

		if found && bytes.Contains(raw, []byte(transcript.ReminderOpen)) && !unsafeEscapes(raw) &&
			!outsideReminders(raw, alternatives) && !reminderMayJoin(raw, alternatives) && transcript.RemovesReminders(raw) {
			return false, -1
		}
		if !found {
			if f.possibleMissing(raw, tools, alternatives) {
				return f.rawRE == nil || f.rawRE.Match(raw), -1
			}
			return false, index
		}
	}
	return f.rawRE == nil || f.rawRE.Match(raw), -1
}

// possibleMissing сохраняет строки, в которых фрагмент может появиться при сборке события.
func (f *filter) possibleMissing(raw []byte, tools transcript.ToolNames, alternatives []string) bool {
	unsafe, hasUnicode := inspectEscapesFor(raw, alternatives)
	if !hasUnicode {
		tools.NoUnicodeEscapes()
	}
	if unsafe || hasUnicode && transcript.MayHideType(raw) || reminderMayJoin(raw, alternatives) {
		return true
	}
	return tools.Any(func(name string) bool {
		for _, needle := range alternatives {
			if hasFolded([]byte(name), needle) {
				return true
			}
		}
		return false
	}) && (bytes.Contains(raw, []byte(`"tool_result"`)) || bytes.Contains(raw, []byte(`"function_call_output"`)) ||
		bytes.Contains(raw, []byte(`"custom_tool_call_output"`)))
}

// Экранированная буква может входить в искомый фрагмент, а суррогат — быть частью такой буквы.
// Остальная пунктуация лишь разделяет фрагменты; исключение — удаляемое целиком напоминание.
func unsafeEscapes(raw []byte) bool {
	unsafe, _ := inspectEscapes(raw)
	return unsafe
}

func inspectEscapes(raw []byte) (bool, bool) {
	return inspectEscapesFor(raw, nil)
}

func inspectEscapesFor(raw []byte, alternatives []string) (bool, bool) {
	i := bytes.Index(raw, []byte(`\u`))
	if i < 0 {
		return false, false
	}
	if bytes.Contains(raw, []byte("reminder")) {
		return true, true
	}
	for i >= 0 {
		if i+6 > len(raw) {
			return true, true
		}
		var r rune
		for _, b := range raw[i+2 : i+6] {
			r <<= 4
			switch {
			case b >= '0' && b <= '9':
				r += rune(b - '0')
			case b >= 'a' && b <= 'f':
				r += rune(b - 'a' + 10)
			case b >= 'A' && b <= 'F':
				r += rune(b - 'A' + 10)
			default:
				return true, true
			}
		}
		if escapedMayMatter(raw, r, alternatives) {
			return true, true
		}
		i += 6
		next := bytes.Index(raw[i:], []byte(`\u`))
		if next < 0 {
			break
		}
		i += next
	}
	return false, true
}

func escapedMayMatter(raw []byte, r rune, alternatives []string) bool {
	if r >= 0xd800 && r <= 0xdfff {
		return true
	}
	return (safeLiteralRune(r) || unicode.SimpleFold(r) != r) &&
		(alternatives == nil || escapedMatches(r, alternatives) || bytes.Contains(raw, []byte("<")) ||
			bytes.Contains(raw, []byte(`\u003c`)) || bytes.Contains(raw, []byte(`\u003C`)))
}

// Экранированная руна опасна, если может входить в обязательный фрагмент с учётом свёртки.
func escapedMatches(r rune, alternatives []string) bool {
	for _, needle := range alternatives {
		for _, want := range needle {
			if simpleFoldEqual(r, want) {
				return true
			}
		}
	}
	return false
}

// outsideReminders ищет условие вне пар тегов, целиком находящихся в одной строке JSON.
func outsideReminders(raw []byte, alternatives []string) bool {
	const open, close = transcript.ReminderOpen, transcript.ReminderClose
	start, search := 0, 0
	contains := func(p []byte) bool {
		for _, needle := range alternatives {
			if hasFolded(p, needle) {
				return true
			}
		}
		return false
	}
	for search < len(raw) {
		i := bytes.Index(raw[search:], []byte(open))
		if i < 0 {
			break
		}
		i += search
		end := bytes.Index(raw[i+len(open):], []byte(close))
		if end < 0 {
			break
		}
		end += i + len(open) + len(close)
		sameString := true
		for j := i; j < end; j++ {
			if raw[j] == '\\' {
				j++
			} else if raw[j] == '"' {
				sameString = false
				break
			}
		}
		if !sameString {
			search = i + len(open)
			continue
		}
		if contains(raw[start:i]) {
			return true
		}
		start, search = end, end
	}
	return contains(raw[start:])
}

// reminderMayJoin проверяет совпадение, которое могло возникнуть при вырезании одного или нескольких напоминаний.
func reminderMayJoin(raw []byte, alternatives []string) bool {
	const open = transcript.ReminderOpen
	for offset := 0; offset < len(raw); {
		i := bytes.Index(raw[offset:], []byte(open))
		if i < 0 {
			return false
		}
		i += offset
		if bytes.Contains(raw[i+len(open):], []byte(`<\/`+transcript.ReminderClose[2:])) {
			return true
		}
		for _, needle := range alternatives {
			want := []rune(needle)
			for split := 1; split < len(want); split++ {
				if foldedBefore(raw[:i], want[:split]) && foldedAfterReminders(raw[i:], want[split:]) {
					return true
				}
			}
		}
		offset = i + len(open)
	}
	return false
}

func foldedBefore(raw []byte, want []rune) bool {
	for j := len(want) - 1; j >= 0; j-- {
		if len(raw) == 0 {
			return false
		}
		r, size := utf8.DecodeLastRune(raw)
		if !simpleFoldEqual(r, want[j]) {
			return false
		}
		raw = raw[:len(raw)-size]
	}
	return true
}

func foldedAfterReminders(raw []byte, want []rune) bool {
	const open, close = transcript.ReminderOpen, transcript.ReminderClose
	for _, r := range want {
		for bytes.HasPrefix(raw, []byte(open)) {
			end := bytes.Index(raw[len(open):], []byte(close))
			if end < 0 {
				return false
			}
			raw = raw[len(open)+end+len(close):]
		}
		if len(raw) == 0 {
			return false
		}
		got, size := utf8.DecodeRune(raw)
		if !simpleFoldEqual(got, r) {
			return false
		}
		raw = raw[size:]
	}
	return true
}

// requiredLiterals — обязательные условия совпадения: выполняются все, внутри условия — варианты.
func requiredLiterals(re *syntax.Regexp) [][]string {
	switch re.Op {
	case syntax.OpLiteral:
		return literalConditions(re.Rune)
	case syntax.OpCharClass:
		return classConditions(re.Rune)
	case syntax.OpCapture, syntax.OpPlus:
		return requiredLiterals(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min > 0 {
			return requiredLiterals(re.Sub[0])
		}
		return nil
	case syntax.OpConcat:
		var out [][]string
		for _, sub := range re.Sub {
			out = append(out, requiredLiterals(sub)...)
		}
		return out
	case syntax.OpAlternate:
		return alternateConditions(re.Sub)
	}
	return nil
}

func literalConditions(runes []rune) [][]string {
	var out [][]string
	var run []rune
	flush := func() {
		if len(run) > 0 {
			out = append(out, []string{string(run)})
			run = nil
		}
	}
	for _, r := range runes {
		if !safeLiteralRune(r) {
			flush()
		} else {
			run = append(run, r)
		}
	}
	flush()
	return out
}

// Буквы и цифры не пересекают границы полей и вставленной обвязки; JSON с Unicode-экранированием
// и строки с вырезанными напоминаниями проверяются отдельно до отсева.
func safeLiteralRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func classConditions(runes []rune) [][]string {
	var choices []string
	for i := 0; i+1 < len(runes); i += 2 {
		lo, hi := runes[i], runes[i+1]
		// Большие классы оставляем без отсева: проверка каждого варианта
		// дороже, чем разбор небольшого числа лишних строк.
		if hi-lo >= 4 || len(choices)+int(hi-lo)+1 > 4 {
			return nil
		}
		for r := lo; r <= hi; r++ {
			if !safeLiteralRune(r) {
				return nil
			}
			choices = append(choices, string(r))
		}
	}
	if len(choices) == 0 {
		return nil
	}
	return [][]string{choices}
}

func alternateConditions(subs []*syntax.Regexp) [][]string {
	var choices []string
	for _, sub := range subs {
		groups := requiredLiterals(sub)
		if len(groups) == 0 {
			return nil
		}
		best := groups[0]
		for _, group := range groups[1:] {
			if len(group[0]) > len(best[0]) {
				best = group
			}
		}
		choices = append(choices, best...)
	}
	return [][]string{choices}
}

// hasFolded ищет pattern в raw с простой свёрткой Unicode, как (?i) в regexp, без свёрнутой копии строки.
func hasFolded(raw []byte, pattern string) bool {
	want := []rune(pattern)
	if len(want) == 0 {
		return true
	}
	if isASCII(want) {
		if hasFoldedASCII(raw, want) {
			return true
		}
		// ASCII-шаблон совпадает и с не-ASCII вариантами свёртки (ſ для s, знак Кельвина для k) — только они
		// требуют общего пути.
		if !hasNonASCIIFoldVariant(raw, want) {
			return false
		}
	}
	starts := newFoldStarts(want[0])
	for offset := 0; offset < len(raw); {
		pos, size := starts.next(raw, offset)
		if pos == len(raw) {
			return false
		}
		if matchFoldedAt(raw[pos:], want) {
			return true
		}
		offset = pos + size
	}
	return false
}

func isASCII(runes []rune) bool {
	for _, r := range runes {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// foldStart — один вариант свёртки первой руны шаблона и его ближайшая позиция в строке.
type foldStart struct {
	bytes [utf8.UTFMax]byte
	size  int
	pos   int
}

// foldStarts — варианты свёртки первой руны; позиция варианта пересчитывается, только когда поиск ушёл за неё,
// поэтому редкий вариант не пробегает строку на каждом кандидате.
type foldStarts struct {
	list [8]foldStart
	n    int
}

func newFoldStarts(first rune) *foldStarts {
	s := &foldStarts{}
	s.add(first)
	for r := unicode.SimpleFold(first); r != first; r = unicode.SimpleFold(r) {
		s.add(r)
	}
	return s
}

func (s *foldStarts) add(r rune) {
	var buf [utf8.UTFMax]byte
	n := utf8.EncodeRune(buf[:], r)
	for i := 0; i < s.n; i++ {
		if s.list[i].size == n && bytes.Equal(s.list[i].bytes[:n], buf[:n]) {
			return
		}
	}
	if s.n < len(s.list) {
		s.list[s.n] = foldStart{bytes: buf, size: n, pos: -1}
		s.n++
	}
}

// next — ближайшая с offset позиция любого варианта и его длина в байтах; len(raw), если вариантов дальше нет.
func (s *foldStarts) next(raw []byte, offset int) (int, int) {
	pos, size := len(raw), 0
	for i := 0; i < s.n; i++ {
		c := &s.list[i]
		if c.pos < offset {
			c.pos = indexFrom(raw, offset, c.bytes[:c.size])
		}
		if c.pos < pos {
			pos, size = c.pos, c.size
		}
	}
	return pos, size
}

func indexFrom(raw []byte, offset int, needle []byte) int {
	var at int
	if len(needle) == 1 {
		at = bytes.IndexByte(raw[offset:], needle[0])
	} else {
		at = bytes.Index(raw[offset:], needle)
	}
	if at < 0 {
		return len(raw)
	}
	return offset + at
}

func matchFoldedAt(raw []byte, want []rune) bool {
	j := 0
	for _, w := range want {
		if j >= len(raw) {
			return false
		}
		got, n := utf8.DecodeRune(raw[j:])
		if !simpleFoldEqual(got, w) {
			return false
		}
		j += n
	}
	return true
}

func hasFoldedASCII(raw []byte, pattern []rune) bool {
	if len(pattern) == 1 {
		b := foldASCII(byte(pattern[0]))
		return bytes.IndexByte(raw, b) >= 0 || b >= 'a' && b <= 'z' && bytes.IndexByte(raw, b-('a'-'A')) >= 0
	}
	first := foldASCII(byte(pattern[0]))
	starts := [2]byte{first, first}
	if first >= 'a' && first <= 'z' {
		starts[1] = first - ('a' - 'A')
	}
	for _, b := range starts {
		for offset := 0; offset+len(pattern) <= len(raw); {
			at := bytes.IndexByte(raw[offset:], b)
			if at < 0 {
				break
			}
			at += offset
			if at+len(pattern) > len(raw) {
				break
			}
			i := 1
			for i < len(pattern) && foldASCII(raw[at+i]) == foldASCII(byte(pattern[i])) {
				i++
			}
			if i == len(pattern) {
				return true
			}
			offset = at + 1
		}
	}

	return false
}

func foldASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func hasNonASCIIFoldVariant(raw []byte, pattern []rune) bool {
	for _, r := range pattern {
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			if folded <= unicode.MaxASCII {
				continue
			}
			var encoded [utf8.UTFMax]byte
			n := utf8.EncodeRune(encoded[:], folded)
			if bytes.Contains(raw, encoded[:n]) {
				return true
			}
		}
	}
	return false
}

func simpleFoldEqual(a, b rune) bool {
	if a == b {
		return true
	}
	if unicode.ToLower(a) == unicode.ToLower(b) {
		return true
	}
	for r := unicode.SimpleFold(a); r != a; r = unicode.SimpleFold(r) {
		if r == b {
			return true
		}
	}
	return false
}

func (f *filter) inWindow(ev transcript.Event) bool {
	return f.inTimeWindow(ev) && f.opts.roles[ev.Role]
}

func (f *filter) inTimeWindow(ev transcript.Event) bool {
	if !f.opts.since.IsZero() && ev.Time.Before(f.opts.since) {
		return false
	}
	return f.opts.until.IsZero() || !ev.Time.After(f.opts.until)
}

// rawSummary is what --raw matched beyond the listed events: the lines whose events in the window and --grep are
// all of roles --role hides (hidden, of the roles); err is why the database could not count it.
type rawSummary struct {
	hidden int
	roles  []transcript.Role
	err    error
}

// add is the summary of two sets of files.
func (s rawSummary) add(o rawSummary) rawSummary {
	s.hidden += o.hidden
	s.roles = mergeRoles(s.roles, o.roles)
	return s
}

// notes is the part of the summary line about it.
func (s rawSummary) notes() string {
	if s.hidden == 0 {
		return ""
	}
	roles := make([]string, len(s.roles))
	for i, role := range s.roles {
		roles[i] = string(role)
	}
	return fmt.Sprintf("; %d line(s) matched --raw only in hidden roles (%s): --role all", s.hidden, strings.Join(roles, ", "))
}

// mergeRoles is the roles of a and b once each, in the order of transcript.Roles.
func mergeRoles(a, b []transcript.Role) []transcript.Role {
	set := make(map[transcript.Role]bool, len(a)+len(b))
	for _, role := range a {
		set[role] = true
	}
	for _, role := range b {
		set[role] = true
	}
	out := make([]transcript.Role, 0, len(set))
	for _, role := range transcript.Roles {
		if set[role] {
			out = append(out, role)
		}
	}
	return out
}

// rawLineRoles is what the events of one line --raw kept show: whether one passes all filters, and the hidden roles
// of those that pass all but --role.
type rawLineRoles struct {
	visible bool
	hidden  map[transcript.Role]bool
}

func (f *filter) recordRawLineRole(lines map[int]*rawLineRoles, ev transcript.Event) {
	if f.rawRE == nil || !f.inTimeWindow(ev) || !f.match(ev) {
		return
	}
	line := lines[ev.Line]
	if line == nil {
		line = &rawLineRoles{hidden: map[transcript.Role]bool{}}
		lines[ev.Line] = line
	}
	if f.opts.roles[ev.Role] {
		line.visible = true
		return
	}
	line.hidden[ev.Role] = true
}

// rawHiddenSummary counts the lines of which no event is shown and some is hidden only by --role.
func rawHiddenSummary(lines map[int]*rawLineRoles) rawSummary {
	var s rawSummary
	roles := map[transcript.Role]bool{}
	for _, line := range lines {
		if line.visible || len(line.hidden) == 0 {
			continue
		}
		s.hidden++
		for role := range line.hidden {
			roles[role] = true
		}
	}
	for _, role := range transcript.Roles {
		if roles[role] {
			s.roles = append(s.roles, role)
		}
	}
	return s
}

func (f *filter) match(ev transcript.Event) bool {
	return f.re == nil || f.re.MatchString(ev.Who+" "+ev.Shown())
}

// file читает один файл и отдаёт отобранные события с окрестностью -C; в режиме --wait вместо событий — ожидания
// вывода: события в памяти не копятся.
func (f *filter) file(src source) ([]hit, []time.Duration, []waitSample, transcript.Meta, error) {
	hits, waits, rows, meta, _, err := f.fileWithRawSummary(src)
	return hits, waits, rows, meta, err
}

// fileWithRawSummary is file with the rawSummary of the file under --raw.
func (f *filter) fileWithRawSummary(src source) ([]hit, []time.Duration, []waitSample, transcript.Meta, rawSummary, error) {
	keep := f.eventKeep()
	wt := f.newWaitTracker()
	if wt != nil && (f.opts.waitCache != nil || waitrec.IsRollout(src.Path)) {
		hits, waits, rows, meta, err := f.cachedWaitFile(src, wt)
		return hits, waits, rows, meta, rawSummary{}, err
	}
	return f.readFileWithContext(src, keep, wt, f.readEvents)
}

func (f *filter) eventKeep() func([]byte, transcript.ToolNames) bool {
	if len(f.needles) > 0 || f.rawRE != nil {
		return f.keepWithState
	}
	return nil
}

func (f *filter) newWaitTracker() *waitTracker {
	if f.opts.waitProg != "" || f.opts.waitAll {
		return newWaitTracker(f.opts)
	}
	return nil
}

func (f *filter) readFileWithContext(src source, keep func([]byte, transcript.ToolNames) bool, wt *waitTracker, read func(string, func([]byte, transcript.ToolNames) bool, func(transcript.Event)) (transcript.Meta, error)) ([]hit, []time.Duration, []waitSample, transcript.Meta, rawSummary, error) {
	var out []hit
	window := contextStream{src: src, radius: f.opts.context}
	lineRoles := map[int]*rawLineRoles{}
	consume := func(ev transcript.Event) {
		f.recordRawLineRole(lineRoles, ev)
		if wt != nil {
			if !f.opts.waitAll && !f.inWindow(ev) {
				return
			}
			wt.event(ev)
			return
		}
		if !f.inWindow(ev) {
			return
		}
		if f.opts.context > 0 {
			window.add(ev, f.match(ev))
			return
		}
		if f.match(ev) {
			out = append(out, hit{src: src, ev: ev})
		}
	}
	meta, err := read(src.Path, keep, consume)
	var waits []time.Duration
	var waitRows []waitSample
	if wt != nil {
		waits = wt.waits
		waitRows = f.waitRowsInWindow(wt.waitRows)
	}
	if err != nil {
		if f.opts.context > 0 {
			out = window.hits()
		}
		return out, waits, waitRows, meta, rawSummary{}, fmt.Errorf("read %s: %w", src.Path, err)
	}
	if f.opts.context > 0 {
		out = window.hits()
	}
	return out, waits, waitRows, meta, rawHiddenSummary(lineRoles), nil
}

// contextStream holds only the last radius events and the selected result while scanning a file.
type contextStream struct {
	src    source
	radius int
	before []transcript.Event
	head   int
	after  int
	out    []hit
}

func (s *contextStream) add(ev transcript.Event, matched bool) {
	if matched {
		for i := range s.before {
			pos := s.head + i
			if pos >= len(s.before) {
				pos -= len(s.before)
			}
			s.out = append(s.out, hit{src: s.src, ev: s.before[pos]})
		}
		s.before = s.before[:0]
		s.head = 0
		s.out = append(s.out, hit{src: s.src, ev: ev})
		s.after = s.radius
		return
	}
	if s.after > 0 {
		s.out = append(s.out, hit{src: s.src, ev: ev})
		s.after--
		return
	}
	if s.radius == 0 {
		return
	}
	if len(s.before) < s.radius {
		s.before = append(s.before, ev)
		return
	}
	s.before[s.head] = ev
	s.head++
	if s.head == s.radius {
		s.head = 0
	}
}

func (s *contextStream) hits() []hit { return s.out }

// contextWindow marks events within n positions of any match in the already-filtered file order.
func contextWindow(matches []bool, n int) []bool {
	keep := make([]bool, len(matches))
	if n >= len(matches) {
		for _, matched := range matches {
			if matched {
				clear(keep)
				for i := range keep {
					keep[i] = true
				}
				return keep
			}
		}
		return keep
	}
	last := -1
	for pos, matched := range matches {
		if !matched {
			continue
		}
		lo, hi := contextWindowBounds(pos, n, len(matches))
		for i := max(lo, last+1); i <= hi; i++ {
			keep[i] = true
		}
		last = max(last, hi)
	}
	return keep
}

// contextWindowBounds uses the remaining distance to the file edge before adding n, avoiding integer overflow.
func contextWindowBounds(pos, n, length int) (int, int) {
	return max(0, pos-n), pos + min(n, length-1-pos)
}

func (f *filter) cachedWaitFile(src source, wt *waitTracker) ([]hit, []time.Duration, []waitSample, transcript.Meta, error) {
	events, err := f.waitEvents(src.Path)
	if err != nil {
		return nil, nil, nil, transcript.Meta{}, fmt.Errorf("read %s: %w", src.Path, err)
	}
	for i := range events {
		if !f.opts.waitAll && !f.cachedWaitEventInWindow(&events[i]) {
			continue
		}
		wt.cachedEvent(events[i])
	}
	return nil, wt.waits, f.waitRowsInWindow(wt.waitRows), transcript.Meta{}, nil
}

func (f *filter) cachedWaitEventInWindow(event *cachedWaitEvent) bool {
	text := ""
	if event.Role == transcript.Cmd {
		text = event.Command
	}
	return f.inWindow(transcript.Event{
		Role: event.Role, Text: text, Time: event.Time, CallID: event.CallID, Line: event.Line,
		MessageID: event.MessageID, Who: event.Who, Sidechain: event.Sidechain,
	})
}

func (f *filter) waitRowsInWindow(rows []waitSample) []waitSample {
	if !f.opts.waitAll {
		return rows
	}
	selected := make([]waitSample, 0, len(rows))
	for _, row := range rows {
		if f.inWindow(transcript.Event{Role: transcript.Cmd, Time: row.At}) {
			selected = append(selected, row)
		}
	}
	return selected
}

// readEvents сохраняет прежний читатель для промахов и его диагностику ошибок.
func (f *filter) readEvents(path string, keep func([]byte, transcript.ToolNames) bool, consume func(transcript.Event)) (transcript.Meta, error) {
	if f.opts.replyCache != nil {
		events, meta, used, err := f.opts.replyCache.Events(path, len(f.needles) > 0)
		if err == nil && used {
			for i := range events {
				consume(events[i])
			}
			return meta, nil
		}
	}
	return transcript.ReadWithTools(path, keep, consume)
}

// waitEvents: through the cache when there is one; a codex rollout without it is
// read the same way, since its command completions need matching to the calls.
func (f *filter) waitEvents(path string) ([]cachedWaitEvent, error) {
	if f.opts.waitCache != nil {
		return f.opts.waitCache.events(path)
	}
	return readWaitEvents(path)
}

// scan читает файлы параллельно; ошибка одного файла не мешает остальным и уходит в итог (result.errs).
func scan(sources []source, opts options, home string) (result, error) {
	f, err := newFilter(opts)
	if err != nil {
		return result{}, err
	}
	allPaths, err := transcriptPaths(home)
	if err != nil {
		return result{}, fmt.Errorf("list transcript names: %w", err)
	}
	// A record being written does not stop reading transcripts, as in find; the summary names it.
	liveNames, liveErr := liveSessionNames(home)
	res := result{names: map[string]string{}, linkIDs: sourceLinkIDs(sources, allPaths, home, liveNames), searchPaths: allPaths, liveNames: liveNames, liveErr: liveErr, files: len(sources)}
	type fileResult struct {
		hits     []hit
		waits    []time.Duration
		waitRows []waitSample
		meta     transcript.Meta
		raw      rawSummary
		err      error
	}
	results := make([]fileResult, len(sources))
	var (
		wg   sync.WaitGroup
		jobs = make(chan int)
	)
	for range scanWorkerCount() {
		wg.Go(func() {
			for i := range jobs {
				hits, waits, waitRows, meta, raw, err := f.fileWithRawSummary(sources[i])
				results[i] = fileResult{hits, waits, waitRows, meta, raw, err}
			}
		})
	}
	for i := range sources {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for i := range results {
		item := &results[i]
		src := sources[i]
		res.hits = append(res.hits, item.hits...)
		res.waits = append(res.waits, item.waits...)
		res.waitRows = append(res.waitRows, item.waitRows...)
		res.raw = res.raw.add(item.raw)
		res.bad += item.meta.Bad
		if item.meta.FirstBad != nil && res.firstBad == nil {
			res.firstBad = fmt.Errorf("%s %w", res.linkIDs[src.Path], item.meta.FirstBad)
		}
		if item.meta.Name != "" {
			res.names[src.Path] = item.meta.Name
		}
		if item.err != nil {
			res.errs = append(res.errs, item.err)
		}
	}
	sort.SliceStable(res.hits, func(i, j int) bool {
		a, b := res.hits[i], res.hits[j]
		if !a.ev.Time.Equal(b.ev.Time) {
			return a.ev.Time.Before(b.ev.Time)
		}
		if a.src.Path != b.src.Path {
			return a.src.Path < b.src.Path
		}
		return a.ev.Line < b.ev.Line
	})
	return res, nil
}

// problems — что не прочиталось, для итоговой строки: события оттуда в ответ не вошли.
func (res result) problems() string {
	var s string
	if res.raw.err != nil {
		problem := res.raw.err.Error()
		if len(problem) > 120 {
			problem = problem[:117] + "..."
		}
		s += fmt.Sprintf("; raw summary unavailable: %s", problem)
	}
	if n := len(res.errs); n > 0 {
		s += fmt.Sprintf("; %d transcript(s) unreadable, first: %v", n, res.errs[0])
	}
	if res.bad > 0 {
		s += fmt.Sprintf("; %d unparsable line(s) skipped, first: %v", res.bad, res.firstBad)
	}
	if res.liveErr != nil {
		s += fmt.Sprintf("; live session names partly read: %v", res.liveErr)
	}
	return s
}

// report печатает события: сначала id и пути файлов, где они нашлись, затем по строке на событие.
func report(res result, opts options, e env, w io.Writer) int {
	buffered := bufio.NewWriterSize(w, 64<<10)
	defer buffered.Flush()
	w = buffered
	hits := res.hits
	if len(hits) == 0 {
		fmt.Fprintf(w, "tl: no events in %d transcript(s)%s%s%s\n", res.files, windowNote(opts, e), res.raw.notes(), res.problems())
		if len(res.errs) > 0 {
			return exitUsage
		}
		return exitNone
	}
	skipped := 0
	if opts.max > 0 && len(hits) > opts.max {
		skipped = len(hits) - opts.max
		hits = hits[skipped:]
	}
	seen := map[string]bool{}
	for i := range hits {
		h := &hits[i]
		if seen[h.src.Path] {
			continue
		}
		seen[h.src.Path] = true
		id := res.linkIDs[h.src.Path]
		name := res.names[h.src.Path]
		if h.src.ParentID != "" {
			parentPath := strings.TrimSuffix(filepath.Dir(h.src.Path), string(filepath.Separator)+"subagents") + ".jsonl"
			parentName := res.names[parentPath]
			parentID := res.linkIDs[parentPath]
			if parentID == "" {
				parent := source{Path: parentPath, ID: parentPath}
				parentID = sourceLinkIDs([]source{parent}, res.searchPaths, e.home, res.liveNames)[parentPath]
			}
			if parentName != "" {
				parentName = " " + parentName
			}
			description := ""
			if h.src.Description != "" {
				description = fmt.Sprintf(" %q", h.src.Description)
			}
			fmt.Fprintf(w, "== %s sub of %s%s%s %s\n", id, parentID, parentName, description, tilde(h.src.Path, e.home))
			continue
		}
		if name != "" {
			name += " "
		}
		if h.src.ID != h.src.Path {
			fmt.Fprintf(w, "== %s %s%s\n", id, name, tilde(h.src.Path, e.home))
		}
	}
	cuts := maskedShorts(hits, opts.width)
	for i := range hits {
		displayHit := hits[i]
		displayHit.src.ID = res.linkIDs[displayHit.src.Path]
		if displayHit.cut == nil && cuts != nil {
			displayHit.cut = &cuts[i]
		}
		writeHitLine(w, &displayHit, opts.width)
	}
	zone, _ := e.now.Zone()
	summary := fmt.Sprintf("tl: %d event(s), times %s", len(hits), zone)
	if skipped > 0 {
		summary += fmt.Sprintf("; %d earlier not shown (--since, --grep or --max 0)", skipped)
	}
	fmt.Fprintln(w, summary+res.raw.notes()+res.problems()+"; full event: tl <id:line>")
	return 0
}

func writeHitLine(w io.Writer, h *hit, width int) {
	who := ""
	if h.ev.Who != "" {
		who = strings.TrimSpace(h.ev.Who+" "+h.ev.Subtype) + ": "
	}
	text := ""
	if h.cut != nil {
		text = *h.cut
	} else {
		text = maskedShort(h.ev.Shown(), width)
	}
	fmt.Fprintf(w, "%s %s:%d %s %s%s\n",
		h.ev.Time.Local().Format("02.01 15:04:05"), h.src.ID, h.ev.Line, h.ev.Role, who, text)
}

func line(h *hit, width int) string {
	who := ""
	if h.ev.Who != "" {
		who = strings.TrimSpace(h.ev.Who+" "+h.ev.Subtype) + ": "
	}
	text := ""
	if h.cut != nil {
		text = *h.cut
	} else {
		text = maskedShort(h.ev.Shown(), width)
	}
	return fmt.Sprintf("%s %s:%d %s %s%s", h.ev.Time.Local().Format("02.01 15:04:05"), h.src.ID, h.ev.Line, h.ev.Role,
		who, text)
}

// parallelHits is the shortest listing whose texts are masked and cut on all processors: masking is most of the
// time of a long listing, and the texts do not depend on each other.
const parallelHits = 256

// maskedShorts are maskedShort of the texts of hits without a cut, in their order, worked out on all processors;
// nil for a short listing, which writeHitLine cuts itself.
func maskedShorts(hits []hit, width int) []string {
	if len(hits) < parallelHits {
		return nil
	}
	cuts := make([]string, len(hits))
	eachParallel(len(hits), func(i int) {
		if hits[i].cut == nil {
			cuts[i] = maskedShort(hits[i].ev.Shown(), width)
		}
	})
	return cuts
}

// eachParallel calls do for 0..n-1 on all processors, in batches; it returns once every call is over.
func eachParallel(n int, do func(i int)) {
	const batch = 64
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), n/batch+1) {
		wg.Go(func() {
			for {
				from := int(next.Add(batch)) - batch
				if from >= n {
					return
				}
				for i := from; i < min(from+batch, n); i++ {
					do(i)
				}
			}
		})
	}
	wg.Wait()
}

// maskWindow is how far past the cut the text is masked first: a secret that
// starts before the cut keeps its prefix and context in the masked part.
const maskWindow = 512

// maskedShort cuts text to width like transcript.Short after masking it, so a
// cut inside a secret does not leave a piece the mask no longer recognizes.
// Only the start of a long text is masked: the rest is not shown. Masking shrinks
// secrets to short labels, so the window grows until its masked form still runs
// maskWindow past the cut; a key cut by the window's end then stays out of sight.
// The hidden count is of the masked text: of the whole text once it is all masked.
func maskedShort(text string, width int) string {
	if width <= 0 {
		flat, _ := flatten(text, -1)
		return flat
	}
	// a listing shows the start of a long text: its first widening is flattened, the rest only counted
	start, total := flatten(text, textCut(width))
	if total <= width {
		return start
	}
	if cut, ok := maskedCut(start, total, width); ok {
		return cut
	}
	flat, _ := flatten(text, -1)
	cut, _ := maskedCut(flat, total, width)
	return cut
}

// flatten is transcript.Short(text, 0) in one pass: its first limit characters (all of it when limit < 0) and the
// count of all of them. Spaces are unicode.IsSpace, trimmed at both ends; a run of them between two words is one
// space, with " ⏎" before it for each line break in the run.
func flatten(text string, limit int) (string, int) {
	var b strings.Builder
	if limit < 0 {
		b.Grow(len(text))
	} else {
		b.Grow(min(len(text), 4*limit))
	}
	total, breaks, gap := 0, 0, false
	put := func(s string, n int) {
		if limit < 0 || total+n <= limit {
			b.WriteString(s)
		} else if total < limit {
			b.WriteString(string([]rune(s)[:limit-total]))
		}
		total += n
	}
	putGap := func() {
		put(" ", 1)
		for range breaks {
			put("⏎ ", 2)
		}
		gap, breaks = false, 0
	}
	word, words := 0, 0 // the start of the current word and its characters so far
	for i := 0; i < len(text); {
		r, size := rune(text[i]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(text[i:])
		}
		if !unicode.IsSpace(r) {
			if words == 0 && gap {
				putGap()
			}
			if words == 0 {
				word = i
			}
			words++
			i += size
			continue
		}
		if words > 0 {
			put(text[word:i], words)
			words = 0
		}
		if total > 0 {
			gap = true
			if r == '\n' {
				breaks++
			}
		}
		i += size
	}
	if words > 0 {
		put(text[word:], words)
	}
	return b.String(), total
}

// maskedCut is the cut of maskedShort for a flattened text of total runes known by its start; false — the start is
// too short for the cut, the whole text is needed. It counts runes in place: a byte that is not UTF-8 is a rune
// and is cut as U+FFFD, as a conversion to []rune and back has it; masking a valid text leaves it valid.
func maskedCut(start string, total, width int) (string, bool) {
	if !utf8.ValidString(start) {
		start = string([]rune(start))
	}
	n := min(total, width+maskWindow)
	for {
		end, ok := runeEnd(start, n)
		if !ok {
			return "", false
		}
		masked, _ := hide(start[:end])
		m := utf8.RuneCountInString(masked)
		if n == total && m <= width {
			return masked, true
		}
		if n == total || m >= width+maskWindow {
			cut, _ := runeEnd(masked, width)
			return masked[:cut] + fmt.Sprintf("…(+%d)", m-width+total-n), true
		}
		n = min(total, 2*n)
	}
}

// runeEnd is the byte offset in s after its first n runes; false — s has fewer.
func runeEnd(s string, n int) (int, bool) {
	i := 0
	for range n {
		if i >= len(s) {
			return 0, false
		}
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return i, true
}

func windowNote(opts options, e env) string {
	var parts []string
	if !opts.since.IsZero() {
		parts = append(parts, "since "+opts.since.Local().Format("02.01 15:04"))
	} else if opts.all {
		parts = append(parts, "since "+e.now.Add(-sinceDefault).Format("02.01 15:04")+" (default)")
	}
	if !opts.until.IsZero() {
		parts = append(parts, "until "+opts.until.Local().Format("02.01 15:04"))
	}
	if opts.grep != "" {
		parts = append(parts, fmt.Sprintf("grep %q", opts.grep))
	}
	if opts.raw != "" {
		parts = append(parts, fmt.Sprintf("raw %q", opts.raw))
	}
	roles := make([]string, 0, len(opts.roles))
	for _, r := range transcript.Roles {
		if opts.roles[r] {
			roles = append(roles, string(r))
		}
	}
	parts = append(parts, "roles "+strings.Join(roles, ","))
	return " (" + strings.Join(parts, ", ") + ")"
}

func tilde(path, home string) string {
	if rest, ok := strings.CutPrefix(path, home+"/"); ok {
		return "~/" + rest
	}
	return path
}

// showRef печатает одно событие полностью: все события строки ref (у вызова их бывает несколько).
func showRef(ref string, lineNo, width int, e env, w, stderr io.Writer) int {
	path, err := find(ref, e.home)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	if _, err := transcript.SourceAt(path); err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	var found []sortedHit
	_, err = transcript.Read(path, nil, func(ev transcript.Event) {
		if ev.Line == lineNo {
			found = append(found, sortedHit{hit: hit{src: source{Path: path}, ev: ev}})
		}
	})
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	if e.collect != nil {
		for i := range found {
			e.collect.refs = append(e.collect.refs, found[i].hit)
		}
	}
	return printRef(ref, lineNo, width, found, w)
}

// maskEventText sees the complete event, including any embedded line breaks.
func maskEventText(text string) string {
	masked, _ := hide(text)
	return masked
}
