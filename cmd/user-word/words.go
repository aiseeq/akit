package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/transcript"
)

// line — реплика пользователя или текст ответа агента.
type line struct {
	no           int // строка в файле
	t            time.Time
	user         bool
	ask          bool // ответ в окне вопроса
	text         string
	match        string
	warning      string
	contextHuman bool
	moreLater    int
}

// readLines — реплики пользователя и ответы агента по порядку; keep отсеивает сырые строки до разбора.
func readLines(path string, keep func([]byte) bool) ([]line, error) {
	var out []line
	meta, err := transcript.Read(path, keep, func(ev transcript.Event) {
		switch ev.Role {
		case transcript.User, transcript.Ask:
			l := line{no: ev.Line, t: ev.Time, user: true, ask: ev.Role == transcript.Ask, text: ev.Text, match: ev.Text}
			if l.ask {
				l.match = ev.Answer
			}
			out = append(out, l)
		case transcript.Agent:
			out = append(out, line{no: ev.Line, t: ev.Time, text: ev.Text})
		}
	})
	if meta.Bad > 0 {
		out = append(out, malformedTranscriptWarning(meta.Bad, meta.FirstBad))
	}
	return out, err
}

func searchableText(l line) string {
	if l.ask {
		return l.match
	}
	return l.text
}

func malformedTranscriptWarning(count int, first error) line {
	return line{warning: fmt.Sprintf("skipped %d malformed transcript lines (%v)", count, first)}
}

// lastWith — индекс последней реплики пользователя с куском, -1 — нет такой.
func lastWith(lines []line, frag string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].user && hasFragment(searchableText(lines[i]), frag) {
			return i
		}
	}
	return -1
}

// Кусок сверяется без регистра, ё как е и с любыми пробелами и переносами между словами: отправитель цитирует
// по памяти или из окна, и при точной сверке цитата не находилась в реплике, где одно слово начиналось с прописной.
var yo = strings.NewReplacer("ё", "е", "Ё", "е")

// fold — текст для сверки куска.
func fold(s string) string { return strings.Join(strings.Fields(yo.Replace(strings.ToLower(s))), " ") }

// hasFragment сверяет целый фрагмент или последовательность фрагментов, разделённых многоточием.
func hasFragment(text, fragment string) bool { return matchParts(text, fragment, false) }

// hasWords — как hasFragment, но каждый кусок цитаты начинается и кончается на границе слова: короткая цитата
// «да» не находит «удали».
func hasWords(text, fragment string) bool { return matchParts(text, fragment, true) }

func matchParts(text, fragment string, bounded bool) bool {
	if strings.TrimSpace(fragment) == "" {
		return false
	}
	whole := fold(text)
	at := 0
	for part := range strings.SplitSeq(fragment, "…") {
		part = fold(part)
		if part == "" {
			continue
		}
		i := indexFrom(whole, part, at, bounded)
		if i < 0 {
			return false
		}
		at = i + len(part)
	}
	return true
}

// indexFrom — первое вхождение part в s не раньше from; bounded — только на границах слова.
func indexFrom(s, part string, from int, bounded bool) int {
	for from <= len(s) {
		i := strings.Index(s[from:], part)
		if i < 0 {
			return -1
		}
		i += from
		if !bounded || (wordEdge(s[:i], true) && wordEdge(s[i+len(part):], false)) {
			return i
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		from = i + size
	}
	return -1
}

// wordEdge — по эту сторону (before — конец s, иначе начало) нет буквы или цифры.
func wordEdge(s string, before bool) bool {
	var r rune
	if before {
		r, _ = utf8.DecodeLastRuneInString(s)
	} else {
		r, _ = utf8.DecodeRuneInString(s)
	}
	return r == utf8.RuneError || !(unicode.IsLetter(r) || unicode.IsDigit(r))
}

// prefilter — отсев сырых строк транскрипта до разбора. Перевод в нижний регистр каждой строки втрое замедлял
// --search (4,4 → 14,4 с на неделе транскриптов), поэтому отсев ищет как есть самую длинную часть слова куска
// без первой буквы и без е (её могут цитировать как ё), строчными и прописными. Нет такой части — без отсева.
func prefilter(frag string) func(raw []byte) bool {
	best, bestWord := prefilterBest(frag)
	if best == "" {
		return func([]byte) bool { return true }
	}
	if len([]rune(best)) == 1 && len([]rune(bestWord)) <= 3 {
		variants := wordVariants([]rune(bestWord))
		terms := make([][]byte, len(variants))
		for i, variant := range variants {
			terms[i] = jsonEscaped(variant)
		}
		return func(raw []byte) bool {
			for _, term := range terms {
				if rawContainsWord(raw, term) {
					return true
				}
			}
			return false
		}
	}
	term := jsonEscaped(best)
	pattern, prefix := foldedRawTerm(term)
	if asciiPattern := asciiFoldedPattern(pattern); asciiPattern != nil {
		return func(raw []byte) bool { return rawContainsFoldedASCII(raw, asciiPattern) }
	}
	return func(raw []byte) bool { return rawContainsFolded(raw, pattern, prefix) }
}

func asciiFoldedPattern(pattern []rune) []byte {
	bytes := make([]byte, len(pattern))
	for i, r := range pattern {
		if r >= utf8.RuneSelf {
			return nil
		}
		bytes[i] = byte(r)
	}
	return bytes
}

func rawContainsFoldedASCII(raw, pattern []byte) bool {
	if len(pattern) == 0 || len(pattern) > len(raw) {
		return false
	}
	firstLower := pattern[0]
	firstUpper := firstLower
	if firstLower >= 'a' && firstLower <= 'z' {
		firstUpper -= 'a' - 'A'
	}
	for from := 0; from <= len(raw)-len(pattern); {
		rest := raw[from:]
		lower := bytes.IndexByte(rest, firstLower)
		upper := lower
		if firstUpper != firstLower {
			upper = bytes.IndexByte(rest, firstUpper)
		}
		i := lower
		if i < 0 || (upper >= 0 && upper < i) {
			i = upper
		}
		if i < 0 {
			return false
		}
		i += from
		if i > len(raw)-len(pattern) {
			return false
		}
		candidate := raw[i : i+len(pattern)]
		matched := true
		for j, b := range candidate {
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if b != pattern[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
		from = i + 1
	}
	return false
}

type rawFilterNode struct {
	next     map[rune]int
	fail     int
	terminal bool
}

func prefilterAny(fragments []string) func([]byte) bool {
	nodes, short, broad := buildRawFilterTrie(fragments)
	if broad {
		return func([]byte) bool { return true }
	}
	ascii := rawFilterTrieASCII(nodes)
	return func(raw []byte) bool { return rawFilterTrieContains(raw, nodes, short, ascii) }
}

func rawFilterTrieASCII(nodes []rawFilterNode) [][128]uint16 {
	if len(nodes) > int(^uint16(0)) {
		return nil
	}
	table := make([][128]uint16, len(nodes))
	for state := range nodes {
		for b := range 128 {
			r := rune(b)
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			if next, ok := nodes[state].next[r]; ok {
				table[state][b] = uint16(next)
			} else if state != 0 {
				table[state][b] = table[nodes[state].fail][b]
			}
		}
	}
	return table
}

func buildRawFilterTrie(fragments []string) ([]rawFilterNode, []func([]byte) bool, bool) {
	nodes := []rawFilterNode{{next: make(map[rune]int)}}
	var short []func([]byte) bool
	for _, fragment := range fragments {
		best, bestWord := prefilterBest(fragment)
		if best == "" {
			return nil, nil, true
		}
		if len([]rune(best)) == 1 && len([]rune(bestWord)) <= 3 {
			short = append(short, prefilter(fragment))
			continue
		}
		var state int
		nodes, state = addRawFilterPattern(nodes, jsonEscaped(best))
		nodes[state].terminal = true
	}
	addRawFilterFailures(nodes)
	return nodes, short, false
}

func addRawFilterPattern(nodes []rawFilterNode, pattern []byte) ([]rawFilterNode, int) {
	state := 0
	for _, r := range string(pattern) {
		r = prefilterRune(r)
		next, ok := nodes[state].next[r]
		if !ok {
			next = len(nodes)
			nodes[state].next[r] = next
			nodes = append(nodes, rawFilterNode{next: make(map[rune]int)})
		}
		state = next
	}
	return nodes, state
}

func addRawFilterFailures(nodes []rawFilterNode) {
	queue := make([]int, 0, len(nodes))
	for _, child := range nodes[0].next {
		queue = append(queue, child)
	}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		for r, child := range nodes[state].next {
			fallback := nodes[state].fail
			for fallback != 0 {
				if _, ok := nodes[fallback].next[r]; ok {
					break
				}
				fallback = nodes[fallback].fail
			}
			if next, ok := nodes[fallback].next[r]; ok {
				nodes[child].fail = next
			}
			nodes[child].terminal = nodes[child].terminal || nodes[nodes[child].fail].terminal
			queue = append(queue, child)
		}
	}
}

func rawFilterTrieContains(raw []byte, nodes []rawFilterNode, short []func([]byte) bool, ascii [][128]uint16) bool {
	original := raw
	state := 0
	for len(raw) > 0 {
		if raw[0] < utf8.RuneSelf && len(ascii) > 0 {
			state = int(ascii[state][raw[0]])
			raw = raw[1:]
			if nodes[state].terminal {
				return true
			}
			continue
		}
		var r rune
		if raw[0] < utf8.RuneSelf {
			r = rune(raw[0])
			raw = raw[1:]
		} else {
			var size int
			r, size = utf8.DecodeRune(raw)
			raw = raw[size:]
		}
		r = prefilterRune(r)
		for state != 0 {
			if _, ok := nodes[state].next[r]; ok {
				break
			}
			state = nodes[state].fail
		}
		if next, ok := nodes[state].next[r]; ok {
			state = next
		}
		if nodes[state].terminal {
			return true
		}
	}
	for _, filter := range short {
		if filter(original) {
			return true
		}
	}
	return false
}

func prefilterBest(frag string) (string, string) {
	best, bestWord := "", ""
	for fragment := range strings.SplitSeq(frag, "…") {
		for w := range strings.FieldsSeq(fold(fragment)) {
			r := []rune(w)
			for part := range strings.SplitSeq(string(r[1:]), "е") {
				if len(part) > len(best) {
					best, bestWord = part, w
				}
			}
		}
	}
	return best, bestWord
}

func foldedRawTerm(term []byte) ([]rune, []int) {
	pattern := []rune(string(term))
	if len(pattern) == 0 {
		return nil, nil
	}
	for i := range pattern {
		pattern[i] = prefilterRune(pattern[i])
	}
	prefix := make([]int, len(pattern))
	for i, matched := 1, 0; i < len(pattern); i++ {
		for matched > 0 && pattern[i] != pattern[matched] {
			matched = prefix[matched-1]
		}
		if pattern[i] == pattern[matched] {
			matched++
		}
		prefix[i] = matched
	}
	return pattern, prefix
}

func rawContainsFolded(raw []byte, pattern []rune, prefix []int) bool {
	matched := 0
	for len(raw) > 0 {
		var r rune
		if raw[0] < utf8.RuneSelf {
			r = rune(raw[0])
			raw = raw[1:]
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
		} else {
			var size int
			r, size = utf8.DecodeRune(raw)
			raw = raw[size:]
			r = prefilterRune(r)
		}
		for matched > 0 && r != pattern[matched] {
			matched = prefix[matched-1]
		}
		if r == pattern[matched] {
			matched++
			if matched == len(pattern) {
				return true
			}
		}
	}
	return false
}

func prefilterRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	if r < utf8.RuneSelf {
		return r
	}
	r = unicode.ToLower(r)
	if r == 'ё' {
		return 'е'
	}
	return r
}

func rawContainsWord(raw, term []byte) bool {
	if len(term) > len(raw) {
		return false
	}
	for from := 0; from <= len(raw)-len(term); {
		i := bytes.Index(raw[from:], term)
		if i < 0 {
			return false
		}
		i += from
		if rawWordEdge(raw[:i], true) && rawWordEdge(raw[i+len(term):], false) {
			return true
		}
		from = i + len(term)
	}
	return false
}

func rawWordEdge(raw []byte, before bool) bool {
	var r rune
	if before {
		r, _ = utf8.DecodeLastRune(raw)
	} else {
		r, _ = utf8.DecodeRune(raw)
	}
	return r == utf8.RuneError || !(unicode.IsLetter(r) || unicode.IsDigit(r))
}

func wordVariants(word []rune) []string {
	variants := []string{""}
	for _, r := range word {
		choices := []rune{unicode.ToLower(r), unicode.ToUpper(r)}
		if unicode.ToLower(r) == 'е' {
			choices = append(choices, 'ё', 'Ё')
		}
		next := make([]string, 0, len(variants)*len(choices))
		for _, prefix := range variants {
			for _, choice := range choices {
				next = append(next, prefix+string(choice))
			}
		}
		variants = next
	}
	return variants
}

var briefBracketedOmission = regexp.MustCompile(`\[[^\]\n]*\]`)

func briefQuoteFragment(text string) string {
	text = briefBracketedOmission.ReplaceAllString(text, "…")
	return briefTextFragment(text)
}

func briefTextFragment(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '…' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			return r
		}
		return ' '
	}, text)
}

// agentTail — сколько рун конца ответа агента показывать перед репликой.
const agentTail = 1500

const stamp = "02.01 15:04:05"

// briefLater — сколько поздних реплик до времени брифа показывать: между пометкой и брифом бывают часы работы.
const briefLater = 20

// laterView — какие поздние реплики показывать после найденной и что сказать, если их нет.
type laterView struct {
	include func(l, u line) bool
	max     int // 0 — без предела
	none    string
}

func show(w io.Writer, lines []line, at, mins int, loc *time.Location) {
	showContext(w, lines, at, loc, laterView{
		include: func(l, u line) bool { return l.t.Sub(u.t) <= time.Duration(mins)*time.Minute },
		none:    fmt.Sprintf("--- no other user lines within %d min", mins),
	})
}

func showBrief(w io.Writer, lines []line, at int, until time.Time, loc *time.Location) {
	showContext(w, lines, at, loc, laterView{
		include: func(l, u line) bool { return !l.t.Before(u.t) && !l.t.After(until) },
		max:     briefLater,
		none:    "--- no other user lines before brief time",
	})
}

func showBriefQuote(w io.Writer, lines []line, at int, word briefWord, until time.Time, loc *time.Location) {
	none := "--- no other user lines within marker time window"
	if word.markedAt.IsZero() {
		none = "--- no other user lines before brief time"
	}
	showContext(w, lines, at, loc, laterView{
		include: func(l, u line) bool { return !l.t.Before(u.t) && !l.t.After(until) },
		max:     briefLater,
		none:    none,
	})
	if lines[at].moreLater > 0 {
		fmt.Fprintf(w, "--- %d more user lines not shown (user-word TRANSCRIPT 'FRAGMENT' MINUTES for a window)\n", lines[at].moreLater)
	}
}

func showContext(w io.Writer, lines []line, at int, loc *time.Location, view laterView) {
	for _, l := range lines {
		if l.warning != "" {
			fmt.Fprintf(w, "--- warning: %s\n", l.warning)
		}
	}
	for i := at - 1; i >= 0; i-- {
		if lines[i].warning != "" {
			continue
		}
		if !lines[i].user {
			text := []rune(lines[i].text)
			if len(text) > agentTail {
				text = text[len(text)-agentTail:]
			}
			fmt.Fprintf(w, "--- agent before the line (%s), end:\n%s\n", lines[i].t.In(loc).Format(stamp), string(text))
			break
		}
	}
	u := lines[at]
	fmt.Fprintf(w, "--- user %s%s:\n%s\n", u.t.In(loc).Format(stamp), askNote(u), u.text)
	more, hidden := 0, 0
	for _, l := range lines[at+1:] {
		if l.warning != "" || !l.user || !view.include(l, u) {
			continue
		}
		if view.max > 0 && more >= view.max {
			hidden++
			continue
		}
		more++
		fmt.Fprintf(w, "--- user later, %s%s (may be about something else):\n%s\n", l.t.In(loc).Format(stamp), askNote(l), l.text)
	}
	if hidden > 0 {
		fmt.Fprintf(w, "--- %d more user lines not shown (user-word TRANSCRIPT 'FRAGMENT' MINUTES for a window)\n", hidden)
	}
	if more == 0 {
		fmt.Fprintln(w, view.none)
	}
}

func askNote(l line) string {
	if l.ask {
		return ", answer in a question dialog"
	}
	return ""
}

// hit — реплика с куском в одном из транскриптов.
type hit struct {
	path string
	line
	context *briefQuoteContext
}

type briefQuoteContext struct {
	lines []line
}

// fullHits — до стольких совпадений --search показывает целиком, больше — списком.
const fullHits = 5

func search(opts options, e env, stdout, stderr io.Writer) int {
	hits, warnings, err := findHits(opts, e)
	if err != nil {
		fmt.Fprintf(stderr, "user-word: %v\n", err)
		return exitUsage
	}
	if len(hits) == 0 {
		writeWarnings(stderr, warnings)
		fmt.Fprintf(stderr, "user-word: no user line with %q\n", opts.frag)
		return exitNone
	}
	if len(hits) <= fullHits {
		for _, h := range hits {
			if err := showHit(stdout, h, opts.mins, e.loc); err != nil {
				fmt.Fprintf(stderr, "user-word: %v\n", err)
				return exitUsage
			}
		}
		return 0
	}
	fmt.Fprintf(stdout, "%d hits, by time:\n", len(hits))
	for _, h := range hits {
		fmt.Fprintf(stdout, "%s  %s\n    …%s…\n", h.t.In(e.loc).Format("2006-01-02 15:04"), h.path, around(h.text, opts.frag))
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "open one: user-word TRANSCRIPT.jsonl 'FRAGMENT' (the last hit in the file; for an earlier one, a longer fragment);")
	fmt.Fprintln(stdout, "narrow: --since YYYY-MM-DD, --project S, a longer fragment")
	return 0
}

// findHits — реплики пользователя с куском по всем транскриптам, по времени.
func findHits(opts options, e env) ([]hit, []line, error) {
	return findHitsUsing(opts, e, readLines)
}

func findHitsUsing(opts options, e env, read func(string, func([]byte) bool) ([]line, error)) ([]hit, []line, error) {
	paths, err := transcripts(e.home, opts.since, opts.project, e.loc)
	if err != nil {
		return nil, nil, err
	}
	has := prefilter(opts.frag)
	keep := func(raw []byte) bool {
		return bytes.Contains(raw, []byte(`"name":"AskUserQuestion"`)) || has(raw)
	}
	var hits []hit
	var warnings []line
	seen := map[string]bool{} // запись, повторённая в другом транскрипте (продолжение сессии), — один раз
	for _, p := range paths {
		lines, err := read(p, keep)
		if errors.Is(err, fs.ErrNotExist) {
			continue // файл ушёл между списком и чтением
		}
		if err != nil {
			return nil, nil, err
		}
		for _, l := range lines {
			if l.warning != "" {
				warnings = append(warnings, l)
				continue
			}
			if !l.user || !hasFragment(searchableText(l), opts.frag) {
				continue
			}
			if opts.since != "" && l.t.In(e.loc).Format(time.DateOnly) < opts.since {
				continue
			}
			key := l.t.String() + "\x00" + l.text
			if seen[key] {
				continue
			}
			seen[key] = true
			hits = append(hits, hit{path: p, line: l})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].t.Before(hits[j].t) })
	return hits, warnings, nil
}

func writeWarnings(w io.Writer, lines []line) {
	for _, l := range lines {
		if l.warning != "" {
			fmt.Fprintf(w, "user-word: warning: %s\n", l.warning)
		}
	}
}

// showHit — совпадение целиком: файл перечитывается полностью, ради ответа агента перед репликой.
func showHit(w io.Writer, h hit, mins int, loc *time.Location) error {
	fmt.Fprintf(w, "=== %s · %s\n", h.path, h.t.In(loc).Format(time.DateTime))
	lines, err := readLines(h.path, nil)
	if err != nil {
		return err
	}
	at := -1
	for i, l := range lines {
		if l.user && l.no == h.no && l.text == h.text {
			at = i
		}
	}
	if at < 0 {
		fmt.Fprintf(w, "(line %d changed since the search)\n\n", h.no)
		return nil
	}
	show(w, lines, at, mins, loc)
	fmt.Fprintln(w)
	return nil
}

// Сколько рун вокруг куска в списке совпадений: до и после.
const (
	snippetBefore = 50
	snippetAfter  = 70
)

// around — кусок с соседним текстом в одну строку. Кусок сверялся через fold, поэтому место в тексте ищется по
// сложенному тексту одной строкой, а показывается исходный текст.
func around(text, frag string) string {
	flat := []rune(strings.Join(strings.Fields(text), " "))
	folded := fold(string(flat)) // ToLower и замена ё обычно не меняют число рун, иначе — начало текста
	f := fold(frag)
	i := strings.Index(folded, f)
	if i < 0 || utf8.RuneCountInString(folded) != len(flat) {
		return string(flat[:min(len(flat), snippetBefore+snippetAfter)])
	}
	at, n := utf8.RuneCountInString(folded[:i]), utf8.RuneCountInString(f)
	return string(flat[max(at-snippetBefore, 0):min(at+n+snippetAfter, len(flat))])
}

// jsonEscape — как Claude Code (JSON.stringify) экранирует строку: обратный слэш, кавычка, управляющие символы;
// < > & — как есть.
var jsonEscape = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\b", `\b`, "\f", `\f`)

// jsonEscaped — кусок так, как он лежит в строке JSON транскрипта.
func jsonEscaped(s string) []byte { return []byte(jsonEscape.Replace(s)) }

var notAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

var briefLink = regexp.MustCompile("user-word\\s+((?:/|~/)[^\\s'\"`]*(?:\\.jsonl|…jsonl))\\s+")

// briefLinkLike — всё, что похоже на ссылку: не подошедшее под briefLink попадает в итог как «не найдено», а не
// пропадает молча. Образец вроде `user-word <jsonl> '<кусок>'` без .jsonl сюда не попадает.
var briefLinkLike = regexp.MustCompile("user-word\\s+[^\\s'\"`]*(?:\\.jsonl|…jsonl)")

var userMarker = regexp.MustCompile(`(?i)пользовател[а-яё]*\s*,?\s+\(?\s*(\d{1,2}\.\d{1,2}(?:\.(?:\d{4}|\d{2}))?)`)

// markerDate — дата пометки в тексте между пометкой и цитатой: значит, цитата принадлежит другой пометке.
var markerDate = regexp.MustCompile(`\b\d{1,2}\.\d{1,2}(?:\.\d{2,4})?\b`)

// briefWord — слово пользователя, указанное ссылкой или цитатой в брифе.
type briefWord struct {
	raw          string
	path         string
	fragment     string
	day          time.Time
	markedAt     time.Time
	window       time.Duration
	isQuote      bool
	invalidCause string
}

type briefTranscript struct {
	path     string
	modified time.Time
}

func runBrief(opts options, e env, stdout, stderr io.Writer) int {
	data, err := os.ReadFile(opts.path)
	if err != nil {
		fmt.Fprintf(stderr, "user-word: brief %s: %v\n", opts.path, err)
		return exitUsage
	}
	st, err := os.Stat(opts.path)
	if err != nil {
		fmt.Fprintf(stderr, "user-word: brief %s: %v\n", opts.path, err)
		return exitUsage
	}
	until := st.ModTime().In(e.loc)
	if opts.until != "" {
		until, err = time.ParseInLocation("2006-01-02 15:04", opts.until, e.loc)
		if err != nil {
			fmt.Fprintf(stderr, "user-word: --until: %v\n", err)
			return exitUsage
		}
	}
	words := extractBriefWords(string(data), until, e.loc)
	if len(words) == 0 {
		fmt.Fprintf(stdout, "no user words in %s\n", opts.path)
		return 0
	}
	dayHits, warnings, err := briefQuoteHitsByDay(words, e, until)
	if err != nil {
		fmt.Fprintf(stderr, "user-word: %v\n", err)
		return exitUsage
	}
	return reportBriefWords(words, dayHits, warnings, e, until, stdout, stderr)
}

func briefQuoteHitsByDay(words []briefWord, e env, until time.Time) (map[string][]hit, []line, error) {
	dayFragments := make(map[string][]string)
	daySeen := make(map[string]map[string]bool)
	for _, word := range words {
		if word.isQuote && word.invalidCause == "" {
			for _, day := range briefWordScanDays(word, e.loc) {
				date := day.Format(time.DateOnly)
				if daySeen[date] == nil {
					daySeen[date] = make(map[string]bool)
				}
				if daySeen[date][word.fragment] {
					continue
				}
				daySeen[date][word.fragment] = true
				dayFragments[date] = append(dayFragments[date], word.fragment)
			}
		}
	}
	if len(dayFragments) == 0 {
		return map[string][]hit{}, nil, nil
	}
	inventory, err := briefTranscriptInventory(e.home)
	if err != nil {
		return nil, nil, err
	}
	return scanBriefQuotes(inventory, dayFragments, until, e.loc)
}

func briefWordScanDays(word briefWord, loc *time.Location) []time.Time {
	start, end := word.day, word.day.AddDate(0, 0, 1).Add(-time.Nanosecond)
	if !word.markedAt.IsZero() {
		start = word.markedAt.Add(-word.window)
		end = word.markedAt.Add(word.window)
	}
	first := localDate(start, loc)
	last := localDate(end, loc)
	var days []time.Time
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		days = append(days, day)
	}
	return days
}

func localDate(t time.Time, loc *time.Location) time.Time {
	local := t.In(loc)
	year, month, day := local.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, loc)
}

func reportBriefWords(words []briefWord, dayHits map[string][]hit, warnings []line, e env, until time.Time, stdout, stderr io.Writer) int {
	writeWarnings(stderr, warnings)
	contexts := make(map[string][]line)
	quoteContexts := make(map[string]briefQuoteContext)
	found, ambiguous, missing := 0, 0, 0
	for i, word := range words {
		fmt.Fprintf(stdout, "== %d. %s\n", i+1, word.raw)
		hits, note, err := briefHitsUntil(word, e, contexts, dayHits, until)
		if err != nil {
			fmt.Fprintf(stderr, "user-word: %v\n", err)
			return exitUsage
		}
		// Реплика, которой нет при перечитывании (живая сессия переписала файл), — не совпадение.
		var located []hit
		for _, h := range hits {
			lines, loaded := contexts[h.path]
			if !loaded || findLine(lines, h.line) >= 0 {
				located = append(located, h)
			}
		}
		if len(hits) > 0 && len(located) == 0 {
			note = "transcript changed during lookup"
		}
		switch {
		case len(located) == 0:
			fmt.Fprintf(stdout, "not found: %s — %s\n", word.raw, note)
			missing++
			continue
		case len(located) > 1:
			ambiguous++
			shown := ""
			if len(located) > 5 {
				shown = ", first 5 shown"
			}
			fmt.Fprintf(stdout, "ambiguous: %d lines%s\n", len(located), shown)
		default:
			found++
			if note != "" {
				fmt.Fprintf(stdout, "note: %s\n", note)
			}
		}
		for _, h := range located[:min(len(located), 5)] {
			lines, err := briefContextLines(h, word, contexts, quoteContexts, until)
			if err != nil {
				fmt.Fprintf(stderr, "user-word: read transcript %s: %v\n", h.path, err)
				return exitUsage
			}
			at := findLine(lines, h.line)
			if at < 0 && word.isQuote {
				lines, err = contextLines(h.path, contexts)
				if err != nil {
					fmt.Fprintf(stderr, "user-word: read transcript %s: %v\n", h.path, err)
					return exitUsage
				}
				at = findLine(lines, h.line)
			}
			if at < 0 {
				fmt.Fprintf(stdout, "note: transcript changed during lookup: %s\n", h.path)
				continue
			}
			if word.isQuote {
				showBriefQuote(stdout, lines, at, word, until, e.loc)
			} else {
				showBrief(stdout, lines, at, until, e.loc)
			}
		}
	}
	fmt.Fprintf(stdout, "brief: %d user words: %d found, %d ambiguous, %d not found\n", len(words), found, ambiguous, missing)
	if missing > 0 {
		return exitNone
	}
	return 0
}

func briefContextLines(h hit, word briefWord, contexts map[string][]line, quoteContexts map[string]briefQuoteContext, until time.Time) ([]line, error) {
	if word.isQuote {
		if h.context != nil {
			return h.context.lines, nil
		}
		key := h.path + "\x00" + h.t.String() + "\x00" + h.text
		if cached, ok := quoteContexts[key]; ok {
			return cached.lines, nil
		}
		lines, _, err := readBriefQuoteContext(h.path, h.t, until)
		if err != nil {
			return nil, err
		}
		quoteContexts[key] = briefQuoteContext{lines: lines}
		return lines, nil
	}
	return contextLines(h.path, contexts)
}

// briefAskAnswer — ответ человека в окне вопроса: ключ "answers" внутри toolUseResult. Тот же ключ есть
// в схеме AskUserQuestion в снимках запроса (prompt_snapshot, api-request-shape), но не после toolUseResult;
// в тексте вывода инструментов кавычки экранированы.
func briefAskAnswer(raw []byte) bool {
	_, rest, ok := bytes.Cut(raw, []byte(`"toolUseResult":`))
	return ok && bytes.Contains(rest, []byte(`"answers":`))
}

func briefContextUser(raw []byte) bool {
	return briefUserRecord(raw) || briefAskAnswer(raw)
}

func briefContextHuman(raw []byte) bool {
	if briefAskAnswer(raw) ||
		bytes.Contains(raw, []byte(`"type":"event_msg","payload":{"type":"user_message"`)) {
		return true
	}
	if bytes.Contains(raw, []byte(`"kind":"human"`)) {
		return true
	}
	if !bytes.Contains(raw, []byte(`"kind"`)) && !bytes.Contains(raw, []byte(`user_message`)) {
		return false
	}
	var record struct {
		Type   string `json:"type"`
		Role   string `json:"role"`
		Origin struct {
			Kind string `json:"kind"`
		} `json:"origin"`
		Attachment struct {
			Type   string `json:"type"`
			Origin struct {
				Kind string `json:"kind"`
			} `json:"origin"`
		} `json:"attachment"`
		Payload struct {
			Type string `json:"type"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &record) != nil {
		return false
	}
	return record.Type == "user" && record.Origin.Kind == "human" ||
		record.Role == "user" && record.Origin.Kind == "human" ||
		record.Type == "attachment" && record.Attachment.Type == "queued_command" && record.Attachment.Origin.Kind == "human" ||
		record.Type == "event_msg" && record.Payload.Type == "user_message" ||
		briefAskAnswer(raw)
}

func briefAgentRecord(raw []byte) bool {
	return bytes.Contains(raw, []byte(`"type":"assistant"`)) && bytes.Contains(raw, []byte(`"type":"text"`)) ||
		bytes.Contains(raw, []byte(`"type":"agent_message"`))
}

func findLine(lines []line, target line) int {
	for i, candidate := range lines {
		if candidate.user && (target.no == 0 || candidate.no == target.no) && candidate.text == target.text && candidate.t.Equal(target.t) {
			return i
		}
	}
	return -1
}

func extractBriefWords(text string, until time.Time, loc *time.Location) []briefWord {
	var words []briefWord
	for row := range strings.SplitSeq(text, "\n") {
		words = append(words, extractBriefLinks(row)...)
	}
	for paragraph := range strings.SplitSeq(text, "\n\n") {
		words = append(words, extractBriefQuotes(paragraph, until, loc)...)
	}
	return words
}

func extractBriefLinks(row string) []briefWord {
	var words []briefWord
	parsed := map[int]bool{}
	for _, match := range briefLink.FindAllStringSubmatchIndex(row, -1) {
		parsed[match[0]] = true
		fragment, end, ok := shellWord(row, match[1])
		if !ok {
			words = append(words, briefWord{raw: row[match[0]:match[1]], invalidCause: "link fragment is not a shell quoted word"})
			continue
		}
		if strings.TrimSpace(fragment) == "" {
			words = append(words, briefWord{raw: row[match[0]:end], invalidCause: "empty link fragment"})
			continue
		}
		words = append(words, briefWord{raw: row[match[0]:end], path: row[match[2]:match[3]], fragment: fragment})
	}
	for _, match := range briefLinkLike.FindAllStringIndex(row, -1) {
		if !parsed[match[0]] {
			words = append(words, briefWord{raw: row[match[0]:match[1]], invalidCause: "link not in the form user-word /path.jsonl 'fragment' (absolute or ~/ path, quoted fragment)"})
		}
	}
	return words
}

// shellWord reads one quoted shell word, including the standard '\\” escape.
func shellWord(s string, at int) (string, int, bool) {
	for at < len(s) && (s[at] == ' ' || s[at] == '\t') {
		at++
	}
	if at >= len(s) || (s[at] != '\'' && s[at] != '"') {
		return "", at, false
	}
	quote := s[at]
	at++
	var b strings.Builder
	for at < len(s) {
		if quote == '\'' && strings.HasPrefix(s[at:], "'\\''") {
			b.WriteByte('\'')
			at += 4
			continue
		}
		if s[at] == quote {
			return b.String(), at + 1, true
		}
		if quote == '"' && s[at] == '\\' && at+1 < len(s) {
			at++
			b.WriteByte(s[at])
			at++
			continue
		}
		b.WriteByte(s[at])
		at++
	}
	return "", at, false
}

func extractBriefQuotes(row string, until time.Time, loc *time.Location) []briefWord {
	var words []briefWord
	for _, marker := range userMarker.FindAllStringSubmatchIndex(row, -1) {
		start, colon, ok := quoteStart(row, marker[1])
		if !ok {
			// Дата пометки без цитаты — пересказ: проверяющему нечего сверить, это видно в итоге.
			words = append(words, briefWord{raw: row[marker[0]:marker[1]], isQuote: true, invalidCause: "no «quote» after a colon right after the marker"})
			continue
		}
		zone, zoneCause := markerZone(row[marker[2]:colon], loc)
		day, cause := markerDay(row[marker[2]:marker[3]], until, zone)
		if cause == "" {
			cause = zoneCause
		}
		markedAt := time.Time{}
		window := markerWindow
		if cause == "" {
			markedAt, window, cause = markerTimeAndZone(row[marker[1]:colon], day, window)
		}
		for pos := start; pos >= 0; {
			end := quoteEnd(row, pos)
			if end < 0 {
				break
			}
			fragment := row[pos+len("«") : end-len("»")]
			word := briefWord{raw: row[marker[0]:end], fragment: fragment, day: day, markedAt: markedAt, window: window, isQuote: true, invalidCause: cause}
			if strings.Trim(strings.ReplaceAll(fragment, "[…]", "…"), "… \t") == "" {
				word.invalidCause = "empty quote"
			}
			words = append(words, word)
			next := strings.Index(row[end:], "«")
			if next < 0 {
				break
			}
			pos = end + next
			between := strings.Trim(row[end:pos], " \t\r\n,;.")
			if between != "" {
				break
			}
		}
	}
	return words
}

// quoteStart — начало цитаты пометки: первая «» после пометки, перед ней двоеточие, а между ними ни другой даты, ни
// «;» — иначе цитата принадлежит другому («пользователь 31.10 просил; лид 31.10: «…»»). Пояс, время и вставка в
// скобках до двоеточия допустимы.
func quoteStart(row string, after int) (int, int, bool) {
	for search := after; search < len(row); {
		rel := strings.Index(row[search:], "«")
		if rel < 0 {
			return 0, 0, false
		}
		open := search + rel
		colon := markerColon(row, after, open)
		if colon >= 0 {
			between := row[after:colon]
			if !strings.Contains(between, ";") && !markerDate.MatchString(between) {
				return open, colon, true
			}
		}
		search = open + len("«")
	}
	return 0, 0, false
}

// markerColon skips the colon between hour and minute in a marker timestamp.
func markerColon(row string, after, before int) int {
	for i := before - 1; i >= after; i-- {
		if row[i] != ':' {
			continue
		}
		if i > after && i+1 < before && isDigit(row[i-1]) && isDigit(row[i+1]) {
			continue
		}
		return i
	}
	return -1
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func quoteEnd(row string, start int) int {
	depth := 0
	for pos := start; pos < len(row); {
		switch {
		case strings.HasPrefix(row[pos:], "«"):
			depth++
			pos += len("«")
		case strings.HasPrefix(row[pos:], "»"):
			depth--
			pos += len("»")
			if depth == 0 {
				return pos
			}
		default:
			_, size := utf8.DecodeRuneInString(row[pos:])
			pos += size
		}
	}
	return -1
}

var markerClock = regexp.MustCompile(`([~≈])?(\d{1,2}:\d{2}(?::\d{2})?)`)

// markerZoneName — поддерживаемые пояса в любом регистре, с необязательными скобками.
var markerZoneName = regexp.MustCompile(`(?i)^\(?(?:(cest|cet|z)|(?:utc|gmt)(?:([+-])(\d{1,2})(?::(\d{2}))?)?)\)?$`)

// markerZone распознаёт пояс после часов или даты; другое слово после часов даёт причину not found.
func markerZone(prefix string, loc *time.Location) (*time.Location, string) {
	clock := markerClock.FindStringIndex(prefix)
	if clock != nil {
		if zone, present := markerZoneToken(prefix[clock[1]:]); present {
			return parseMarkerZone(zone, loc)
		}
	}
	date := markerDate.FindStringIndex(prefix)
	if date != nil {
		end := len(prefix)
		if clock != nil && clock[0] >= date[1] {
			end = clock[0]
		}
		if date[1] <= end {
			if zone, present := markerZoneToken(prefix[date[1]:end]); present {
				if parsed, cause := parseMarkerZone(zone, loc); cause == "" {
					return parsed, ""
				}
			}
		}
	}
	return loc, ""
}

func markerZoneToken(text string) (string, bool) {
	text = strings.TrimLeft(text, " \t")
	first, _ := utf8.DecodeRuneInString(text)
	if !unicode.IsLetter(first) && !strings.ContainsRune("(+-", first) {
		return "", false
	}
	token, _, _ := strings.Cut(text, " ")
	return strings.TrimRight(token, ",;.:!?"), true
}

func parseMarkerZone(zone string, loc *time.Location) (*time.Location, string) {
	m := markerZoneName.FindStringSubmatch(zone)
	if m == nil {
		return loc, "unknown zone " + zone + " (known: CET, CEST, UTC, GMT, Z, UTC+H)"
	}
	switch strings.ToUpper(m[1]) {
	case "CET":
		return time.FixedZone("CET", 3600), ""
	case "CEST":
		return time.FixedZone("CEST", cestOffsetSeconds), ""
	case "Z":
		return time.UTC, ""
	}
	if m[2] == "" {
		return time.UTC, ""
	}
	if m[4] == "" {
		m[4] = "0"
	}
	hours, err := strconv.Atoi(m[3])
	if err != nil {
		return loc, "unknown zone " + zone + ": " + err.Error()
	}
	minutes, err := strconv.Atoi(m[4])
	if err != nil {
		return loc, "unknown zone " + zone + ": " + err.Error()
	}
	if hours > 14 || minutes > 59 {
		return loc, "unknown zone " + zone
	}
	offset := (hours*60 + minutes) * 60
	if m[2] == "-" {
		offset = -offset
	}
	return time.FixedZone(strings.ToUpper(zone), offset), ""
}

// markerDay — день пометки; без года — последний такой день не позже until.
func markerDay(dateText string, until time.Time, loc *time.Location) (time.Time, string) {
	layout := "2.1"
	if strings.Count(dateText, ".") == 2 {
		layout = "2.1.2006"
		if len(strings.Split(dateText, ".")[2]) == 2 {
			layout = "2.1.06"
		}
	}
	date, err := time.ParseInLocation(layout, dateText, loc)
	if err != nil {
		return time.Time{}, "invalid marker date " + dateText + ": " + err.Error()
	}
	localUntil := until.In(loc)
	year := localUntil.Year()
	if layout != "2.1" {
		year = date.Year()
	} else if date.Month() > localUntil.Month() || (date.Month() == localUntil.Month() && date.Day() > localUntil.Day()) {
		year--
	}
	day := time.Date(year, date.Month(), date.Day(), 0, 0, 0, 0, loc)
	if day.Day() != date.Day() {
		return time.Time{}, fmt.Sprintf("invalid marker date %s: no such day in %d", dateText, year)
	}
	return day, ""
}

func markerTime(clockText string, day time.Time) (time.Time, string) {
	layout := "15:04"
	if strings.Count(clockText, ":") == 2 {
		layout = "15:04:05"
	}
	clock, err := time.ParseInLocation(layout, clockText, day.Location())
	if err != nil {
		return time.Time{}, "invalid marker time " + clockText + ": " + err.Error()
	}
	return time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), clock.Second(), 0, day.Location()), ""
}

const markerWindow = 2 * time.Minute

func markerTimeAndZone(prefix string, day time.Time, window time.Duration) (time.Time, time.Duration, string) {
	m := markerClock.FindStringSubmatch(prefix)
	if m == nil {
		return time.Time{}, window, ""
	}
	if m[1] != "" {
		window = 15 * time.Minute
	}
	clock, cause := markerTime(m[2], day)
	if cause != "" {
		return time.Time{}, window, cause
	}
	clock = time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), clock.Second(), 0, day.Location())
	return clock, window, ""
}

const cestOffsetSeconds = 2 * 60 * 60

func briefHitsUntil(word briefWord, e env, contexts map[string][]line, cached map[string][]hit, until time.Time) ([]hit, string, error) {
	if word.invalidCause != "" {
		return nil, word.invalidCause, nil
	}
	if !word.isQuote {
		return briefLinkHitsUntil(word, e, contexts, until)
	}
	dayHits := makeBriefWordHits(word, cached, e.loc)
	if len(dayHits) == 0 && !word.markedAt.IsZero() {
		return nil, fmt.Sprintf("no matching user line within %v of marker time", word.window), nil
	}
	if len(dayHits) == 0 {
		return nil, "no matching user line on marker day", nil
	}
	return dayHits, "", nil
}

func makeBriefWordHits(word briefWord, cached map[string][]hit, loc *time.Location) []hit {
	var hits []hit
	seen := make(map[string]bool)
	for _, day := range briefWordScanDays(word, loc) {
		for _, h := range cached[day.Format(time.DateOnly)] {
			if !hasWords(briefTextFragment(searchableText(h.line)), briefQuoteFragment(word.fragment)) || !hitOnMarkerDay(h, word) {
				continue
			}
			key := h.path + "\x00" + h.t.String() + "\x00" + h.text
			if !seen[key] {
				seen[key] = true
				hits = append(hits, h)
			}
		}
	}
	return hits
}

func hitOnMarkerDay(h hit, word briefWord) bool {
	if !word.markedAt.IsZero() {
		delta := h.t.Sub(word.markedAt)
		return delta >= -word.window && delta <= word.window
	}
	return !h.t.Before(word.day) && h.t.Before(word.day.AddDate(0, 0, 1))
}

type briefQuoteFilters struct {
	from     time.Time
	byDay    map[string][]func([]byte) bool
	byRawDay map[string][]func([]byte) bool
}

func makeBriefQuoteFilters(fragments map[string][]string, loc *time.Location) (briefQuoteFilters, error) {
	filters := briefQuoteFilters{
		byDay:    make(map[string][]func([]byte) bool, len(fragments)),
		byRawDay: make(map[string][]func([]byte) bool),
	}
	rawSeen := make(map[string]map[string]bool)
	rawQuotes := make(map[string][]string)

	for date, quotes := range fragments {
		day, err := time.ParseInLocation(time.DateOnly, date, loc)
		if err != nil {
			return briefQuoteFilters{}, fmt.Errorf("parse marker day %q: %w", date, err)
		}
		if filters.from.IsZero() || day.Before(filters.from) {
			filters.from = day
		}
		for _, quote := range quotes {
			if quote == "" {
				continue
			}
			filter := prefilter(briefQuoteFragment(quote))
			filters.byDay[date] = append(filters.byDay[date], filter)
			for offset := -1; offset <= 1; offset++ {
				rawDay := day.AddDate(0, 0, offset).Format(time.DateOnly)
				if rawSeen[rawDay] == nil {
					rawSeen[rawDay] = make(map[string]bool)
				}
				if rawSeen[rawDay][quote] {
					continue
				}
				rawSeen[rawDay][quote] = true
				rawQuotes[rawDay] = append(rawQuotes[rawDay], briefQuoteFragment(quote))
			}
		}
	}
	for day, quotes := range rawQuotes {
		filters.byRawDay[day] = []func([]byte) bool{prefilterAny(quotes)}
	}
	return filters, nil
}

type briefQuoteScanResult struct {
	path     string
	hits     []hit
	warnings []line
	err      error
}

func scanBriefQuotes(inventory []briefTranscript, fragments map[string][]string, until time.Time, loc *time.Location) (map[string][]hit, []line, error) {
	filters, err := makeBriefQuoteFilters(fragments, loc)
	if err != nil {
		return nil, nil, err
	}
	if filters.from.IsZero() {
		return map[string][]hit{}, nil, nil
	}
	foldedFragments := foldBriefQuotes(fragments)
	paths := recentBriefTranscripts(inventory, filters.from)
	results := scanBriefTranscriptPaths(paths, filters, foldedFragments, until, loc)
	hits, warnings, err := mergeBriefQuoteResults(results, len(fragments), loc)
	if err != nil {
		return nil, nil, err
	}
	return hits, warnings, nil
}

const briefContextLookback = 24 * time.Hour

func readBriefQuoteContext(path string, at, until time.Time) ([]line, bool, error) {
	from := at.Add(-briefContextLookback)
	lines, agent, err := scanBriefQuoteContext(path, from, at, until)
	if err != nil {
		return nil, false, fmt.Errorf("read quote context %s: %w", path, err)
	}
	if len(agent) > 0 {
		lines, err = prependBriefAgent(lines, agent)
		if err != nil {
			return nil, false, fmt.Errorf("parse preceding agent response: %w", err)
		}
		return lines, false, nil
	}
	lines, agent, err = scanBriefQuoteContext(path, time.Time{}, at, until)
	if err != nil {
		return nil, false, fmt.Errorf("read quote context %s: %w", path, err)
	}
	if len(agent) > 0 {
		lines, err = prependBriefAgent(lines, agent)
		if err != nil {
			return nil, false, fmt.Errorf("parse preceding agent response: %w", err)
		}
	}
	return lines, true, nil
}

func scanBriefQuoteContext(path string, from, at, until time.Time) ([]line, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat transcript: %w", err)
	}
	var agent []byte
	later := briefLater + 1
	var timestampErr error
	keep := func(raw []byte) bool {
		if !json.Valid(raw) {
			return true
		}
		if briefAgentRecord(raw) {
			stamp, parseErr := transcriptTimestamp(raw)
			if parseErr != nil {
				timestampErr = parseErr
				return false
			}
			if !stamp.After(at) {
				agent = raw
			}
			return false
		}
		askCall := bytes.Contains(raw, []byte(`"name":"AskUserQuestion"`))
		if !briefContextUser(raw) && !askCall {
			return false
		}
		stamp, parseErr := transcriptTimestamp(raw)
		if parseErr != nil {
			timestampErr = parseErr
			return false
		}
		if stamp.Before(at) {
			return false
		}
		if !briefContextHuman(raw) {
			return true
		}
		if later == 0 {
			return false
		}
		later--
		return true
	}
	lines, err := readChronologicalBriefLines(path, f, info.Size(), from, until, keep, nil)
	if err == nil && timestampErr != nil {
		err = timestampErr
	}
	return lines, agent, err
}

func prependBriefAgent(lines []line, raw []byte) ([]line, error) {
	events, err := transcript.NewParser().Parse(raw)
	if err != nil {
		return nil, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Role == transcript.Agent {
			agent := line{t: events[i].Time, text: events[i].Text}
			return append([]line{agent}, lines...), nil
		}
	}
	return lines, nil
}

func recentBriefTranscripts(inventory []briefTranscript, from time.Time) []string {
	paths := make([]string, 0, len(inventory))
	for _, file := range inventory {
		if !file.modified.Before(from) {
			paths = append(paths, file.path)
		}
	}
	return paths
}

func scanBriefTranscriptPaths(paths []string, filters briefQuoteFilters, fragments map[string][]string, until time.Time, loc *time.Location) []briefQuoteScanResult {
	results := make([]briefQuoteScanResult, len(paths))
	workers := min(16, len(paths))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				results[i] = scanBriefTranscript(paths[i], filters, fragments, until, loc)
			}
		})
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

type briefQuoteCollector struct {
	filters        briefQuoteFilters
	fragments      map[string][]string
	loc            *time.Location
	until          time.Time
	parser         *transcript.Parser
	humanStamps    map[string]bool
	agentRaw       map[string][]byte
	lastAgent      []byte
	timestampErr   error
	parsedRaw      []byte
	parsedEvents   []transcript.Event
	parsedErr      error
	hasParsed      bool
	quoteChecks    map[briefQuoteLineKey]uint8
	lateCounts     map[briefQuoteLineKey]int
	lateQuotes     []briefLateQuote
	lateQuoteIndex map[briefQuoteLineKey]int
	lateShown      map[briefQuoteLineKey]map[string]bool
	lateRawFrom    string
	lateRawUntil   string
	rawDay         []byte
}

type briefLateQuote struct {
	key   briefQuoteLineKey
	time  time.Time
	count int
}

type briefQuoteLineKey struct {
	time  int64
	text  string
	match string
}

func quoteLineKey(l line) briefQuoteLineKey {
	return briefQuoteLineKey{time: l.t.UnixNano(), text: l.text, match: searchableText(l)}
}

func (c *briefQuoteCollector) setRawDay(day []byte) {
	c.rawDay = day
}

func (c *briefQuoteCollector) keep(raw []byte) bool {
	userKnown, userRecord := false, false
	isUserRecord := func() bool {
		if !userKnown {
			userRecord = briefUserRecord(raw)
			userKnown = true
		}
		return userRecord
	}
	if keepBriefQuoteRecord(raw, c.filters, c.rawDay, isUserRecord) {
		askCall := bytes.Contains(raw, []byte(`"name":"AskUserQuestion"`))
		if askCall {
			// Запоминаем связь вызова и результата для разбора ответа AskUserQuestion.
			if _, err := c.parser.Parse(raw); err != nil {
				return true // Hand it to the reader, not "parsed": the reader parses the record again and reports it as malformed.
			}
			if stamp, err := transcriptTimestamp(raw); err == nil {
				c.agentRaw[stamp.Format(time.RFC3339Nano)] = c.lastAgent
			}
			return true
		}
		if c.keepQuote(raw) {
			return true
		}
		if briefAskAnswer(raw) {
			if stamp, err := transcriptTimestamp(raw); err == nil {
				c.agentRaw[stamp.Format(time.RFC3339Nano)] = c.lastAgent
			}
			// Ответ в окне вопроса после цитаты — тоже поздняя реплика: в показ и в счёт.
			c.keepLaterUser(raw, isUserRecord)
			return true
		}
	}
	if briefAgentRecord(raw) {
		c.lastAgent = raw
		return false
	}
	if c.keepMalformed(raw, isUserRecord) {
		return true
	}
	return c.keepLaterUser(raw, isUserRecord)
}

func (c *briefQuoteCollector) keepQuote(raw []byte) bool {
	events, err := c.parser.Parse(raw)
	c.parsedRaw = append(c.parsedRaw[:0], raw...)
	c.parsedEvents = events
	c.parsedErr = err
	c.hasParsed = true
	if err != nil {
		return true
	}
	matched := false
	for i := range events {
		ev := &events[i]
		if ev.Role != transcript.User && ev.Role != transcript.Ask {
			continue
		}
		l := line{t: ev.Time, user: true, ask: ev.Role == transcript.Ask, text: ev.Text, match: ev.Text}
		if l.ask {
			l.match = ev.Answer
		}
		date := ev.Time.In(c.loc).Format(time.DateOnly)
		if !ev.Time.After(c.until) && len(c.fragments[date]) > 0 {
			quoteMatch := matchesBriefQuote(l, c.fragments[date])
			check := uint8(1)
			if quoteMatch {
				matched = true
				check = 2
				c.agentRaw[ev.Time.Format(time.RFC3339Nano)] = c.lastAgent
				if c.trackLateUser(l) {
					c.humanStamps[ev.Time.Format(time.RFC3339Nano)] = true
				}
				key := quoteLineKey(l)
				if _, exists := c.lateQuoteIndex[key]; !exists {
					c.lateQuoteIndex[key] = len(c.lateQuotes)
					c.lateQuotes = append(c.lateQuotes, briefLateQuote{key: key, time: ev.Time})
					c.lateShown[key] = make(map[string]bool)
					c.enableLateRawDays(ev.Time)
				}
			}
			c.quoteChecks[quoteLineKey(l)] = check
		}
	}
	return matched
}

func (c *briefQuoteCollector) keepMalformed(raw []byte, isUserRecord func() bool) bool {
	day := c.rawDay
	if len(day) < len(time.DateOnly) {
		_, stamp, ok := bytes.Cut(raw, []byte(`"timestamp":"`))
		if !ok || len(stamp) < len(time.DateOnly) {
			return false
		}
		day = stamp[:len(time.DateOnly)]
	}
	_, relevant := c.filters.byRawDay[string(day[:len(time.DateOnly)])]
	return relevant && isUserRecord() && !json.Valid(raw)
}

func (c *briefQuoteCollector) takeParsed(raw []byte) ([]transcript.Event, error, bool) {
	if !c.hasParsed || !bytes.Equal(c.parsedRaw, raw) {
		return nil, nil, false
	}
	c.hasParsed = false
	return c.parsedEvents, c.parsedErr, true
}

func (c *briefQuoteCollector) keepLaterUser(raw []byte, isUserRecord func() bool) bool {
	if len(c.lateQuotes) == 0 {
		return false
	}
	if len(c.rawDay) >= len(time.DateOnly) && (string(c.rawDay[:len(time.DateOnly)]) < c.lateRawFrom || string(c.rawDay[:len(time.DateOnly)]) > c.lateRawUntil) {
		return false
	}
	if (!isUserRecord() && !briefAskAnswer(raw)) || !briefContextHuman(raw) {
		return false
	}
	stamp, err := transcriptTimestamp(raw)
	if err != nil {
		c.timestampErr = err
		return true
	}
	if stamp.After(c.until) {
		return false
	}
	l := line{t: stamp, user: true}
	keep := c.trackLateUser(l)
	if !keep {
		return false
	}
	c.humanStamps[stamp.Format(time.RFC3339Nano)] = true
	return true
}

func (c *briefQuoteCollector) enableLateRawDays(stamp time.Time) {
	from := localDate(stamp, c.loc).AddDate(0, 0, -1).Format(time.DateOnly)
	until := localDate(c.until, c.loc).AddDate(0, 0, 1).Format(time.DateOnly)
	if c.lateRawFrom == "" || from < c.lateRawFrom {
		c.lateRawFrom = from
	}
	c.lateRawUntil = until
}

func (c *briefQuoteCollector) trackLateUser(l line) bool {
	stamp := l.t.Format(time.RFC3339Nano)
	keep := false
	for i := range c.lateQuotes {
		quote := &c.lateQuotes[i]
		if !l.t.After(quote.time) {
			continue
		}
		quote.count++
		if quote.count <= briefLater {
			c.lateShown[quote.key][stamp] = true
			keep = true
		}
	}
	return keep
}

func scanBriefTranscript(path string, filters briefQuoteFilters, fragments map[string][]string, until time.Time, loc *time.Location) briefQuoteScanResult {
	collector := briefQuoteCollector{
		filters:        filters,
		fragments:      fragments,
		loc:            loc,
		until:          until,
		parser:         transcript.NewParser(),
		humanStamps:    make(map[string]bool),
		agentRaw:       make(map[string][]byte),
		quoteChecks:    make(map[briefQuoteLineKey]uint8),
		lateCounts:     make(map[briefQuoteLineKey]int),
		lateQuoteIndex: make(map[briefQuoteLineKey]int),
		lateShown:      make(map[briefQuoteLineKey]map[string]bool),
	}
	keep := collector.keep
	// Один хронологический проход собирает цитаты и строки их контекста, включая прежние ответы агента.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return briefQuoteScanResult{}
		}
		return briefQuoteScanResult{path: path, err: err}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return briefQuoteScanResult{path: path, err: err}
	}
	lines, err := readChronologicalBriefLines(path, f, info.Size(), filters.from, until, keep, collector.setRawDay, collector.takeParsed)
	if err == nil && collector.timestampErr != nil {
		err = collector.timestampErr
	}
	if errors.Is(err, fs.ErrNotExist) {
		return briefQuoteScanResult{}
	}
	if err != nil {
		return briefQuoteScanResult{path: path, err: err}
	}
	hits := matchBriefQuoteLines(path, lines, filters.byDay, fragments, until, loc, collector.quoteChecks)
	for i := range lines {
		lines[i].contextHuman = collector.humanStamps[lines[i].t.Format(time.RFC3339Nano)]
	}
	for i := range hits {
		key := quoteLineKey(hits[i].line)
		if index, ok := collector.lateQuoteIndex[key]; ok {
			collector.lateCounts[key] = collector.lateQuotes[index].count
		}
		hits[i].moreLater = max(0, collector.lateCounts[key]-briefLater)
		var agent *line
		if raw := collector.agentRaw[hits[i].t.Format(time.RFC3339Nano)]; len(raw) > 0 {
			var parseErr error
			agent, parseErr = briefAgentFromRaw(raw)
			if parseErr != nil {
				return briefQuoteScanResult{path: path, err: parseErr}
			}
		}
		at := findLine(lines, hits[i].line)
		var contextLines []line
		if at >= 0 {
			contextLines = briefQuoteContextWithAgent(lines, at, hits[i].t, agent, collector.lateShown[key])
			if contextAt := findLine(contextLines, hits[i].line); contextAt >= 0 {
				contextLines[contextAt].moreLater = max(0, collector.lateCounts[quoteLineKey(hits[i].line)]-briefLater)
			}
		}
		hits[i].context = &briefQuoteContext{lines: contextLines}
	}
	var warnings []line
	for _, l := range lines {
		if l.warning != "" {
			warnings = append(warnings, l)
		}
	}
	return briefQuoteScanResult{path: path, hits: hits, warnings: warnings}
}

func briefQuoteContextWithAgent(lines []line, at int, targetTime time.Time, agent *line, shownForQuote ...map[string]bool) []line {
	out := make([]line, 0, briefLater+2)
	if agent != nil {
		out = append(out, *agent)
	}
	later := 0
	for _, l := range lines[at:] {
		if !l.user {
			continue
		}
		if !l.t.Equal(targetTime) {
			shown := l.contextHuman
			if len(shownForQuote) > 0 {
				shown = shownForQuote[0][l.t.Format(time.RFC3339Nano)]
			}
			if !shown {
				continue
			}
		}
		out = append(out, l)
		if l.contextHuman && !l.t.Equal(targetTime) {
			later++
			if later >= briefLater {
				break
			}
		}
	}
	return out
}

func briefAgentFromRaw(raw []byte) (*line, error) {
	events, err := transcript.NewParser().Parse(raw)
	if err != nil {
		return nil, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Role == transcript.Agent {
			return &line{t: events[i].Time, text: events[i].Text}, nil
		}
	}
	return nil, nil
}

func keepBriefQuoteRecord(raw []byte, filters briefQuoteFilters, cachedDay []byte, isUserRecord func() bool) bool {
	var dayBytes []byte
	if len(cachedDay) >= len(time.DateOnly) {
		dayBytes = cachedDay[:len(time.DateOnly)]
	} else {
		_, stamp, ok := bytes.Cut(raw, []byte(`"timestamp":"`))
		if !ok {
			return true // Fast format not recognized; let the transcript parser decide.
		}
		if len(stamp) < len(time.DateOnly) {
			return true
		}
		dayBytes = stamp[:len(time.DateOnly)]
	}
	day := string(dayBytes)
	dayFilters := filters.byRawDay[day]
	if dayFilters == nil {
		return false
	}
	ask := bytes.Contains(raw, []byte(`"name":"AskUserQuestion"`))
	if !ask && !isUserRecord() {
		if !bytes.Contains(raw, []byte(`"type":"`)) && !bytes.Contains(raw, []byte(`"role":"`)) {
			return true // Whitespace or another valid JSON layout needs full parsing.
		}
		return false
	}
	if ask {
		return transcript.WithoutResults(raw)
	}
	if briefAskAnswer(raw) {
		return true
	}

	for _, filter := range dayFilters {
		if filter(raw) {
			return transcript.WithoutResults(raw)
		}
	}
	return false
}

func matchBriefQuoteLines(path string, lines []line, byDay map[string][]func([]byte) bool, fragments map[string][]string, until time.Time, loc *time.Location, checked map[briefQuoteLineKey]uint8) []hit {
	var hits []hit
	seen := make(map[string]bool)
	for _, l := range lines {
		if !l.user || l.t.After(until) {
			continue
		}
		date := l.t.In(loc).Format(time.DateOnly)
		if len(byDay[date]) == 0 {
			continue
		}
		result, wasChecked := checked[quoteLineKey(l)]
		if wasChecked {
			if result != 2 {
				continue
			}
		} else if !matchesBriefQuote(l, fragments[date]) {
			continue
		}
		key := date + "\x00" + l.t.String() + "\x00" + l.text
		if !seen[key] {
			seen[key] = true
			hits = append(hits, hit{path: path, line: l})
		}
	}
	return hits
}

func matchesBriefQuote(line line, fragments []string) bool {
	text := fold(briefTextFragment(searchableText(line)))
	for _, fragment := range fragments {
		if matchPartsFolded(text, fragment, true) {
			return true
		}
	}
	return false
}

func foldBriefQuotes(fragments map[string][]string) map[string][]string {
	folded := make(map[string][]string, len(fragments))
	for date, quotes := range fragments {
		for _, quote := range quotes {
			folded[date] = append(folded[date], fold(briefQuoteFragment(quote)))
		}
	}
	return folded
}

func matchPartsFolded(text, fragment string, bounded bool) bool {
	if strings.TrimSpace(fragment) == "" {
		return false
	}
	at := 0
	for part := range strings.SplitSeq(fragment, "…") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := indexFrom(text, part, at, bounded)
		if i < 0 {
			return false
		}
		at = i + len(part)
	}
	return true
}

func mergeBriefQuoteResults(results []briefQuoteScanResult, capacity int, loc *time.Location) (map[string][]hit, []line, error) {
	out := make(map[string][]hit, capacity)
	var warnings []line
	seen := make(map[string]bool)
	for _, result := range results {
		if result.err != nil {
			return nil, nil, fmt.Errorf("read transcript %s: %w", result.path, result.err)
		}
		warnings = append(warnings, result.warnings...)
		for _, h := range result.hits {
			date := h.t.In(loc).Format(time.DateOnly)
			key := date + "\x00" + h.t.String() + "\x00" + h.text
			if seen[key] {
				continue
			}
			seen[key] = true
			out[date] = append(out[date], h)
		}
	}
	return out, warnings, nil
}

func briefUserRecord(raw []byte) bool {
	return bytes.Contains(raw, []byte(`"type":"user"`)) ||
		bytes.Contains(raw, []byte(`"role":"user"`)) ||
		bytes.Contains(raw, []byte(`"type":"attachment"`)) ||
		bytes.Contains(raw, []byte(`"type":"event_msg","payload":{"type":"user_message"`))
}

func readChronologicalBriefLines(path string, f *os.File, size int64, from, until time.Time, keep func([]byte) bool, setDay func([]byte), parsed ...func([]byte) ([]transcript.Event, error, bool)) ([]line, error) {
	var head [4 << 10]byte
	n, err := f.ReadAt(head[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read transcript header: %w", err)
	}

	firstStamp, err := transcriptTimestampPrefix(head[:n])
	if err != nil {
		return nil, err
	}
	ordered, samples, err := transcriptChronological(f, size)
	if err != nil {
		return nil, err
	}
	if !ordered {
		lines, err := readLines(path, keep)
		for i := range lines {
			if lines[i].warning != "" {
				lines[i].warning = fmt.Sprintf("%s in %s", lines[i].warning, path)
			}
		}
		return lines, err
	}
	if firstStamp.After(until) {
		return nil, nil
	}
	start := int64(0)
	if size > 256<<10 {
		start, err = transcriptOffset(f, size, from, samples)
		if err != nil || start >= size {
			return nil, err
		}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to transcript range: %w", err)
	}
	r := bufio.NewReaderSize(f, 1<<20)
	parser := transcript.NewParser()
	// The line number of a malformed record counts from the file start; the
	// lines before the seek point are counted only when a warning needs them.
	scan := briefScan{path: path, linesBefore: func() (int, error) { return countLines(f, start) }}
	return scan.lines(r, parser, until, keep, setDay, parsed...)
}

// countLines counts the line breaks in the first n bytes of f.
func countLines(f *os.File, n int64) (int, error) {
	count := 0
	buf := make([]byte, 1<<20)
	for off := int64(0); off < n; {
		k, err := f.ReadAt(buf[:min(int64(len(buf)), n-off)], off)
		count += bytes.Count(buf[:k], []byte{'\n'})
		off += int64(k)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, fmt.Errorf("count transcript lines: %w", err)
		}
	}
	return count, nil
}

// briefScan reads one chronological transcript for --brief and reports its
// first malformed record by file line.
type briefScan struct {
	path        string
	linesBefore func() (int, error)
	bad         int
	badLine     int
	badErr      error
}

func (s *briefScan) lines(r *bufio.Reader, parser *transcript.Parser, until time.Time, keep func([]byte) bool, setDay func([]byte), parsed ...func([]byte) ([]transcript.Event, error, bool)) ([]line, error) {
	var out []line
	lineNo := 0
	untilDate := []byte(until.UTC().Format(time.DateOnly))
	for {
		raw, consumed, readErr := readBriefRecord(r)
		if consumed {
			lineNo++
		}
		if len(raw) > 0 {
			rawDay, past, err := briefRecordPast(raw, untilDate, until)
			if err != nil {
				return nil, err
			}
			if past {
				return s.finish(out)
			}
			if setDay != nil {
				setDay(rawDay)
			}
			if keep == nil || keep(raw) {
				events, err := parseBriefRecord(raw, parser, parsed)
				if err != nil {
					s.noteBad(lineNo, err)
				} else if appendBriefEvents(&out, events, until) {
					return s.finish(out)
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return s.finish(out)
			}
			return nil, fmt.Errorf("read transcript record: %w", readErr)
		}
	}
}

func parseBriefRecord(raw []byte, parser *transcript.Parser, parsed []func([]byte) ([]transcript.Event, error, bool)) ([]transcript.Event, error) {
	if len(parsed) > 0 && parsed[0] != nil {
		if events, err, ok := parsed[0](raw); ok {
			return events, err
		}
	}
	return parser.Parse(raw)
}

// briefRecordPast reports whether a record is later than until, by its day
// when the day decides it and by its timestamp otherwise.
func briefRecordPast(raw, untilDate []byte, until time.Time) ([]byte, bool, error) {
	rawDay, after, known, err := briefRecordStatus(raw, untilDate, until)
	if err != nil || known {
		return rawDay, known && after, err
	}
	stamp, err := transcriptTimestamp(raw)
	if err != nil {
		return nil, false, err
	}
	return rawDay, !stamp.IsZero() && stamp.After(until), nil
}

// noteBad counts a malformed record and keeps the first one for the warning.
func (s *briefScan) noteBad(lineNo int, err error) {
	s.bad++
	if s.badErr == nil {
		s.badLine, s.badErr = lineNo, err
	}
}

func (s *briefScan) finish(out []line) ([]line, error) {
	if s.bad == 0 {
		return out, nil
	}
	before, err := s.linesBefore()
	if err != nil {
		return nil, err
	}
	first := fmt.Errorf("line %d: %w", before+s.badLine, s.badErr)
	warning := malformedTranscriptWarning(s.bad, first)
	warning.warning += " in " + s.path
	return append(out, warning), nil
}

func briefRecordStatus(raw, untilDate []byte, until time.Time) ([]byte, bool, bool, error) {
	_, value, ok := bytes.Cut(raw, []byte(`"timestamp":"`))
	if !ok {
		return nil, false, false, nil
	}
	value, _, ok = bytes.Cut(value, []byte{'"'})
	if !ok || len(value) < len(time.DateOnly) {
		return nil, false, false, nil
	}
	date := value[:len(time.DateOnly)]
	if !validUTCTimestampDate(value) {
		return nil, false, false, nil
	}
	if comparison := bytes.Compare(date, untilDate); comparison != 0 {
		return date, comparison > 0, true, nil
	}
	if timestamp, ok := normalizeUTCTimestamp(value); ok {
		cutoff := until.UTC().Format("2006-01-02T15:04:05.000000000Z")
		return date, bytes.Compare(timestamp, []byte(cutoff)) > 0, true, nil
	}
	stamp, err := transcriptTimestamp(raw)
	return date, !stamp.IsZero() && stamp.After(until), true, err
}

func normalizeUTCTimestamp(value []byte) ([]byte, bool) {
	if len(value) < 20 || value[len(value)-1] != 'Z' || len(value) != 20 && value[19] != '.' || len(value) == 20 && value[19] != 'Z' {
		return nil, false
	}
	var normalized [30]byte
	copy(normalized[:19], value[:19])
	normalized[19] = '.'
	var fraction []byte
	if len(value) > 20 {
		fraction = value[20 : len(value)-1]
	}
	if len(fraction) > 9 {
		return nil, false
	}
	for _, digit := range fraction {
		if !isDigit(digit) {
			return nil, false
		}
	}
	copy(normalized[20:], fraction)
	for i := 20 + len(fraction); i < 29; i++ {
		normalized[i] = '0'
	}
	normalized[29] = 'Z'
	return normalized[:], true
}

func validUTCTimestampDate(stamp []byte) bool {
	if len(stamp) < 20 || stamp[len(stamp)-1] != 'Z' || stamp[4] != '-' || stamp[7] != '-' || stamp[10] != 'T' || stamp[13] != ':' || stamp[16] != ':' {
		return false
	}
	for _, i := range [...]int{0, 1, 2, 3, 5, 6, 8, 9} {
		if !isDigit(stamp[i]) {
			return false
		}
	}
	return true
}

// readBriefRecord reads one transcript line. A tool result without answers is
// skipped (nil record), but consumed still reports that a line was read, so the
// caller's line count matches the file.
func readBriefRecord(r *bufio.Reader) ([]byte, bool, error) {
	var raw []byte
	skippingResult := false
	consumed := false
	for {
		chunk, err := r.ReadSlice('\n')
		consumed = consumed || len(chunk) > 0
		if !skippingResult {
			raw = append(raw, chunk...)
			if bytes.Contains(raw, []byte(`"type":"tool_result"`)) && !bytes.Contains(raw, []byte(`"answers":`)) {
				raw = nil
				skippingResult = true
			}
		}
		if err != bufio.ErrBufferFull {
			if skippingResult {
				return nil, consumed, err
			}
			return bytes.TrimSuffix(raw, []byte{'\n'}), consumed, err
		}
	}
}

func appendBriefEvents(out *[]line, events []transcript.Event, until time.Time) bool {
	for i := range events {
		ev := &events[i]
		if ev.Time.After(until) {
			return true
		}
		if ev.Role == transcript.User || ev.Role == transcript.Ask {
			l := line{t: ev.Time, user: true, ask: ev.Role == transcript.Ask, text: ev.Text, match: ev.Text}
			if l.ask {
				l.match = ev.Answer
			}
			*out = append(*out, l)
			continue
		}
		if ev.Role == transcript.Agent {
			*out = append(*out, line{t: ev.Time, text: ev.Text})
		}
	}
	return false
}

type transcriptSample struct {
	offset int64
	stamp  time.Time
}

func transcriptChronological(f *os.File, size int64) (bool, []transcriptSample, error) {
	var previous time.Time
	const samples = 4
	checked := make([]transcriptSample, 0, samples)
	for i := range int64(samples) {
		stamp, offset, err := transcriptTimestampAt(f, size*i/samples, size)
		if err != nil {
			return false, nil, err
		}
		if stamp.IsZero() || (!previous.IsZero() && stamp.Before(previous)) {
			return false, nil, nil
		}
		checked = append(checked, transcriptSample{offset: offset, stamp: stamp})
		previous = stamp
	}
	return true, checked, nil
}

func transcriptTimestampAt(f *os.File, offset, size int64) (time.Time, int64, error) {
	pos := offset
	if pos > 0 {
		var chunk [64 << 10]byte
		for pos > 0 {
			start := max(int64(0), pos-int64(len(chunk)))
			n, err := f.ReadAt(chunk[:pos-start], start)
			if err != nil && !errors.Is(err, io.EOF) {
				return time.Time{}, 0, err
			}
			if i := bytes.LastIndexByte(chunk[:n], '\n'); i >= 0 {
				pos = start + int64(i+1)
				break
			}
			pos = start
		}
	}
	if pos >= size {
		return time.Time{}, pos, nil
	}
	var prefix [4096]byte
	n, err := f.ReadAt(prefix[:], pos)
	if err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}, 0, err
	}
	stamp, err := transcriptTimestampPrefix(prefix[:n])
	if err != nil {
		return time.Time{}, 0, err
	}
	if stamp.IsZero() && pos < size {
		for {
			if end := bytes.IndexByte(prefix[:n], '\n'); end >= 0 {
				pos += int64(end + 1)
				break
			}
			pos += int64(n)
			if pos >= size {
				return time.Time{}, pos, nil
			}
			n, err = f.ReadAt(prefix[:], pos)
			if err != nil && !errors.Is(err, io.EOF) {
				return time.Time{}, 0, err
			}
		}
		n, err = f.ReadAt(prefix[:], pos)
		if err != nil && !errors.Is(err, io.EOF) {
			return time.Time{}, 0, err
		}
		stamp, err = transcriptTimestampPrefix(prefix[:n])
		if err != nil {
			return time.Time{}, 0, err
		}
	}
	return stamp, pos, nil
}

func transcriptTimestamp(raw []byte) (time.Time, error) {
	_, value, ok := bytes.Cut(raw, []byte(`"timestamp":"`))
	if !ok {
		return time.Time{}, nil
	}
	value, _, ok = bytes.Cut(value, []byte{'"'})
	if !ok {
		return time.Time{}, fmt.Errorf("transcript timestamp has no closing quote")
	}
	return parseTranscriptTimestamp(value)
}

func transcriptTimestampPrefix(raw []byte) (time.Time, error) {
	_, value, ok := bytes.Cut(raw, []byte(`"timestamp":"`))
	if !ok {
		return time.Time{}, nil
	}
	value, _, ok = bytes.Cut(value, []byte{'"'})
	if !ok {
		return time.Time{}, nil
	}
	return parseTranscriptTimestamp(value)
}

func parseTranscriptTimestamp(value []byte) (time.Time, error) {
	stamp, err := time.Parse(time.RFC3339Nano, string(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("parse transcript timestamp %q: %w", value, err)
	}
	return stamp, nil
}

func transcriptOffset(f *os.File, size int64, target time.Time, samples []transcriptSample) (int64, error) {
	lo, hi := int64(0), size
	for _, sample := range samples {
		if sample.stamp.Before(target) {
			lo = sample.offset
			continue
		}
		hi = sample.offset
		break
	}
	for lo < hi {
		pos, stamp, end, err := transcriptRecordAt(f, lo+(hi-lo)/2, size)
		if err != nil {
			return 0, err
		}
		if pos >= size {
			break
		}
		if stamp.IsZero() || stamp.Before(target) {
			if end <= lo {
				break
			}
			lo = end
		} else {
			if pos >= hi {
				break
			}
			hi = pos
		}
	}
	return hi, nil
}

func transcriptRecordAt(f *os.File, offset, size int64) (int64, time.Time, int64, error) {
	pos := offset
	if pos > 0 {
		var chunk [64 << 10]byte
		for pos > 0 {
			start := max(int64(0), pos-int64(len(chunk)))
			n, err := f.ReadAt(chunk[:pos-start], start)
			if err != nil && !errors.Is(err, io.EOF) {
				return 0, time.Time{}, 0, err
			}
			if i := bytes.LastIndexByte(chunk[:n], '\n'); i >= 0 {
				pos = start + int64(i+1)
				break
			}
			pos = start
			if n == 0 {
				return size, time.Time{}, size, nil
			}
		}
	}
	if pos >= size {
		return size, time.Time{}, size, nil
	}
	var prefix [4096]byte
	n, err := f.ReadAt(prefix[:], pos)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, time.Time{}, 0, err
	}
	var stamp time.Time
	stamp, err = transcriptTimestampPrefix(prefix[:n])
	if err != nil {
		return 0, time.Time{}, 0, err
	}
	end := pos
	var chunk [64 << 10]byte
	for end < size {
		count, readErr := f.ReadAt(chunk[:], end)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, time.Time{}, 0, readErr
		}
		if i := bytes.IndexByte(chunk[:count], '\n'); i >= 0 {
			return pos, stamp, end + int64(i+1), nil
		}
		end += int64(count)
		if count == 0 {
			break
		}
	}
	return pos, stamp, size, nil
}

func briefLinkHitsUntil(word briefWord, e env, contexts map[string][]line, until time.Time) ([]hit, string, error) {
	path := word.path
	if after, ok := strings.CutPrefix(path, "~/"); ok {
		path = filepath.Join(e.home, after)
	}
	matches, err := briefPathMatches(path)
	if err != nil {
		return nil, "invalid transcript path pattern: " + err.Error(), nil
	}
	if len(matches) == 0 {
		return nil, "transcript not found", nil
	}
	if len(matches) != 1 {
		return nil, fmt.Sprintf("transcript path matches %d files", len(matches)), nil
	}
	path = matches[0]
	_, err = os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "transcript not found", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("stat transcript %s: %w", path, err)
	}
	lines, err := contextLines(path, contexts)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "transcript not found", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read transcript %s: %w", path, err)
	}
	var prior []line
	for _, l := range lines {
		if !l.t.After(until) {
			prior = append(prior, l)
		}
	}
	return lastBriefLinkHit(path, prior, word.fragment)
}

func briefPathMatches(path string) ([]string, error) {
	if !strings.Contains(path, "…") {
		return []string{path}, nil
	}
	return filepath.Glob(strings.ReplaceAll(path, "…", "*"))
}

func lastBriefLinkHit(path string, lines []line, fragment string) ([]hit, string, error) {
	var last line
	matched := 0
	for _, l := range lines {
		if l.user && hasFragment(searchableText(l), fragment) {
			last = l
			matched++
		}
	}
	if matched == 0 {
		var note strings.Builder
		note.WriteString("fragment not found in transcript")
		for _, l := range lines {
			if l.warning != "" {
				note.WriteString("; " + l.warning)
			}
		}
		return nil, note.String(), nil
	}
	// Ссылка указывает на свежую реплику с куском, как и обычный режим; о прежних — заметка.
	note := ""
	if matched > 1 {
		note = fmt.Sprintf("the last of %d user lines with the fragment; a longer fragment for an earlier one", matched)
	}
	return []hit{{path: path, line: last}}, note, nil
}

func contextLines(path string, contexts map[string][]line) ([]line, error) {
	if lines, ok := contexts[path]; ok {
		return lines, nil
	}
	lines, err := readLines(path, nil)
	if err != nil {
		return nil, err
	}
	contexts[path] = lines
	return lines, nil
}

// transcripts — транскрипты сессий (без суб-агентов), изменённые не раньше начала дня since. Папка проекта — путь,
// где всё, кроме латинских букв и цифр, заменено на «-»: --project годится и подстрокой папки, и путём.
func transcripts(home, since, project string, loc *time.Location) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(session.ProjectsDir(home), "*", "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("list transcripts: %w", err)
	}
	var from time.Time
	if since != "" {
		if from, err = time.ParseInLocation(time.DateOnly, since, loc); err != nil {
			return nil, fmt.Errorf("--since: %w", err)
		}
	}
	var candidates []string
	for _, p := range paths {
		if project == "" || strings.Contains(p, project) || strings.Contains(p, notAlnum.ReplaceAllString(project, "-")) {
			candidates = append(candidates, p)
		}
	}
	if from.IsZero() {
		return candidates, nil
	}
	return transcriptsSince(candidates, from)
}

func briefTranscriptInventory(home string) ([]briefTranscript, error) {
	paths, err := transcripts(home, "", "", time.UTC)
	if err != nil {
		return nil, err
	}
	type result struct {
		file briefTranscript
		err  error
	}
	results := make([]result, len(paths))
	workers := min(16, len(paths))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				st, err := os.Stat(paths[i])
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					results[i].err = fmt.Errorf("transcript %s: %w", paths[i], err)
					continue
				}
				results[i].file = briefTranscript{path: paths[i], modified: st.ModTime()}
			}
		})
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	out := make([]briefTranscript, 0, len(paths))
	for _, result := range results {
		if result.err != nil {
			return nil, result.err
		}
		if result.file.path != "" {
			out = append(out, result.file)
		}
	}
	return out, nil
}

func transcriptsSince(paths []string, from time.Time) ([]string, error) {
	type result struct {
		keep bool
		err  error
	}
	results := make([]result, len(paths))
	workers := min(64, len(paths))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				st, err := os.Stat(paths[i])
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					results[i].err = fmt.Errorf("transcript %s: %w", paths[i], err)
					continue
				}
				results[i].keep = !st.ModTime().Before(from)
			}
		})
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	var out []string
	for i, result := range results {
		if result.err != nil {
			return nil, result.err
		}
		if result.keep {
			out = append(out, paths[i])
		}
	}
	return out, nil
}
