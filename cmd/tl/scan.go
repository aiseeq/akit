package main

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/akit/secretmask"
	"github.com/aiseeq/akit/transcript"
)

// hit — событие с его файлом.
type hit struct {
	src source
	ev  transcript.Event
}

type result struct {
	hits  []hit
	waits []time.Duration   // --wait: ожидания вывода, сведённые по ходу чтения файлов
	names map[string]string // id → имя сессии
	files int
	errs  []error // файлы, которые не прочитались: их события не вошли
	bad   int     // строк, которые не разобрать, по всем файлам
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
	if !f.opts.roles[ev.Role] {
		return false
	}
	if !f.opts.since.IsZero() && ev.Time.Before(f.opts.since) {
		return false
	}
	return f.opts.until.IsZero() || !ev.Time.After(f.opts.until)
}

func (f *filter) match(ev transcript.Event) bool {
	return f.re == nil || f.re.MatchString(ev.Who+" "+ev.Shown())
}

// file читает один файл и отдаёт отобранные события с окрестностью -C; в режиме --wait вместо событий — ожидания
// вывода: события в памяти не копятся.
func (f *filter) file(src source) ([]hit, []time.Duration, transcript.Meta, error) {
	var keep func([]byte, transcript.ToolNames) bool
	if len(f.needles) > 0 || f.rawRE != nil {
		keep = f.keepWithState
	}
	var wt *waitTracker
	if f.opts.waitProg != "" {
		wt = newWaitTracker(f.opts)
	}
	if wt != nil && f.opts.waitCache != nil {
		events, err := f.opts.waitCache.events(src.Path)
		if err != nil {
			return nil, nil, transcript.Meta{}, fmt.Errorf("read %s: %w", src.Path, err)
		}
		for i := range events {
			event := &events[i]
			if f.inWindow(transcript.Event{Role: event.Role, Text: event.Text, Time: event.Time, CallID: event.CallID, Line: event.Line,
				MessageID: event.MessageID, Who: event.Who, Sidechain: event.Sidechain, Error: event.Error, Raw: event.Raw}) {
				wt.cachedEvent(*event)
			}
		}
		return nil, wt.waits, transcript.Meta{}, nil
	}
	var (
		out    []hit
		before []transcript.Event // последние -C событий окна до найденного
		after  int                // сколько событий после найденного ещё показать
	)
	meta, err := transcript.ReadWithTools(src.Path, keep, func(ev transcript.Event) {
		if !f.inWindow(ev) {
			return
		}
		if wt != nil {
			wt.event(ev)
			return
		}
		switch {
		case f.match(ev):
			for i := range before {
				out = append(out, hit{src, before[i]})
			}
			before = before[:0]
			out = append(out, hit{src, ev})
			after = f.opts.context
		case after > 0:
			out = append(out, hit{src, ev})
			after--
		case f.opts.context > 0:
			if len(before) == f.opts.context {
				before = before[1:]
			}
			before = append(before, ev)
		}
	})
	var waits []time.Duration
	if wt != nil {
		waits = wt.waits
	}
	if err != nil {
		return out, waits, meta, fmt.Errorf("read %s: %w", src.Path, err)
	}
	return out, waits, meta, nil
}

// scan читает файлы параллельно; ошибка одного файла не мешает остальным и уходит в итог (result.errs).
func scan(sources []source, opts options) (result, error) {
	f, err := newFilter(opts)
	if err != nil {
		return result{}, err
	}
	res := result{names: map[string]string{}, files: len(sources)}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		jobs = make(chan source)
	)
	for range scanWorkerCount() {
		wg.Go(func() {
			for src := range jobs {
				hits, waits, meta, err := f.file(src)
				mu.Lock()
				res.hits = append(res.hits, hits...)
				res.waits = append(res.waits, waits...)
				res.bad += meta.Bad
				if meta.FirstBad != nil && res.firstBad == nil {
					res.firstBad = fmt.Errorf("%s %w", src.ID, meta.FirstBad)
				}
				if meta.Name != "" {
					res.names[src.ID] = meta.Name
				}
				if err != nil {
					res.errs = append(res.errs, err)
				}
				mu.Unlock()
			}
		})
	}
	for _, s := range sources {
		jobs <- s
	}
	close(jobs)
	wg.Wait()
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
	if n := len(res.errs); n > 0 {
		s += fmt.Sprintf("; %d transcript(s) unreadable, first: %v", n, res.errs[0])
	}
	if res.bad > 0 {
		s += fmt.Sprintf("; %d unparsable line(s) skipped, first: %v", res.bad, res.firstBad)
	}
	return s
}

// report печатает события: сначала id и пути файлов, где они нашлись, затем по строке на событие.
func report(res result, opts options, e env, w io.Writer) int {
	hits := res.hits
	if len(hits) == 0 {
		fmt.Fprintf(w, "tl: no events in %d transcript(s)%s%s\n", res.files, windowNote(opts, e), res.problems())
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
		if seen[h.src.ID] {
			continue
		}
		seen[h.src.ID] = true
		name := res.names[h.src.ID]
		if h.src.ParentID != "" {
			parentName := res.names[h.src.ParentID]
			if parentName != "" {
				parentName = " " + parentName
			}
			description := ""
			if h.src.Description != "" {
				description = fmt.Sprintf(" %q", h.src.Description)
			}
			fmt.Fprintf(w, "== %s sub of %s%s%s %s\n", h.src.ID, h.src.ParentID, parentName, description, tilde(h.src.Path, e.home))
			continue
		}
		if name != "" {
			name += " "
		}
		if h.src.ID != h.src.Path {
			fmt.Fprintf(w, "== %s %s%s\n", h.src.ID, name, tilde(h.src.Path, e.home))
		}
	}
	for i := range hits {
		fmt.Fprintln(w, line(&hits[i], opts.width))
	}
	zone, _ := e.now.Zone()
	summary := fmt.Sprintf("tl: %d event(s), times %s", len(hits), zone)
	if skipped > 0 {
		summary += fmt.Sprintf("; %d earlier not shown (--since, --grep or --max 0)", skipped)
	}
	fmt.Fprintln(w, summary+res.problems()+"; full event: tl <id:line>")
	return 0
}

func line(h *hit, width int) string {
	who := ""
	if h.ev.Who != "" {
		who = strings.TrimSpace(h.ev.Who+" "+h.ev.Subtype) + ": "
	}
	return fmt.Sprintf("%s %s:%d %s %s%s", h.ev.Time.Local().Format("02.01 15:04:05"), h.src.ID, h.ev.Line, h.ev.Role,
		who, maskedShort(h.ev.Shown(), width))
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
	flat := transcript.Short(text, 0)
	r := []rune(flat)
	if width <= 0 || len(r) <= width {
		return flat
	}
	n := min(len(r), width+maskWindow)
	for {
		masked, _ := secretmask.Mask(string(r[:n]))
		m := []rune(masked)
		if n == len(r) && len(m) <= width {
			return masked
		}
		if n == len(r) || len(m) >= width+maskWindow {
			return string(m[:width]) + fmt.Sprintf("…(+%d)", len(m)-width+len(r)-n)
		}
		n = min(len(r), 2*n)
	}
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
	src, err := transcript.SourceAt(path)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	var found []transcript.Event
	_, err = transcript.Read(path, nil, func(ev transcript.Event) {
		if ev.Line == lineNo {
			found = append(found, ev)
		}
	})
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	if len(found) == 0 {
		fmt.Fprintf(w, "tl: no event at %s:%d (service record, unparsable line or out of range)\n", src.ID, lineNo)
		return exitNone
	}
	for i := range found {
		ev := &found[i]
		who := ""
		if ev.Who != "" {
			who = " " + ev.Who
		}
		text := ev.Shown()
		if width > 0 && utf8.RuneCountInString(text) > width {
			text = maskedShort(text, width)
		}
		fmt.Fprintf(w, "--- %s %s:%d %s%s\n%s\n", ev.Time.Local().Format("02.01.2006 15:04:05 MST"), src.ID, ev.Line, ev.Role, who, text)
	}
	return 0
}
