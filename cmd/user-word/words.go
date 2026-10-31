package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/transcript"
)

// line — реплика пользователя или текст ответа агента.
type line struct {
	no   int // строка в файле
	t    time.Time
	user bool
	ask  bool // ответ в окне вопроса
	text string
}

// readLines — реплики пользователя и ответы агента по порядку; keep отсеивает сырые строки до разбора.
func readLines(path string, keep func([]byte) bool) ([]line, error) {
	var out []line
	_, err := transcript.Read(path, keep, func(ev transcript.Event) {
		switch ev.Role {
		case transcript.User, transcript.Ask:
			out = append(out, line{no: ev.Line, t: ev.Time, user: true, ask: ev.Role == transcript.Ask, text: ev.Text})
		case transcript.Agent:
			out = append(out, line{no: ev.Line, t: ev.Time, text: ev.Text})
		}
	})
	return out, err
}

// lastWith — индекс последней реплики пользователя с куском, -1 — нет такой.
func lastWith(lines []line, frag string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].user && hasFragment(lines[i].text, frag) {
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
	var best string
	for fragment := range strings.SplitSeq(frag, "…") {
		for w := range strings.FieldsSeq(fold(fragment)) {
			r := []rune(w)
			for part := range strings.SplitSeq(string(r[1:]), "е") {
				if len(part) > len(best) {
					best = part
				}
			}
		}
	}
	if best == "" {
		return func([]byte) bool { return true }
	}
	low, up := jsonEscaped(best), jsonEscaped(strings.ToUpper(best))
	return func(raw []byte) bool { return bytes.Contains(raw, low) || bytes.Contains(raw, up) }
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

func showContext(w io.Writer, lines []line, at int, loc *time.Location, view laterView) {
	for i := at - 1; i >= 0; i-- {
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
		if !l.user || !view.include(l, u) {
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
}

// fullHits — до стольких совпадений --search показывает целиком, больше — списком.
const fullHits = 5

func search(opts options, e env, stdout, stderr io.Writer) int {
	hits, err := findHits(opts, e)
	if err != nil {
		fmt.Fprintf(stderr, "user-word: %v\n", err)
		return exitUsage
	}
	if len(hits) == 0 {
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
func findHits(opts options, e env) ([]hit, error) {
	paths, err := transcripts(e.home, opts.since, opts.project, e.loc)
	if err != nil {
		return nil, err
	}
	has := prefilter(opts.frag)
	// Вызов окна вопроса нужен парсеру, чтобы узнать в его результате ответ человека, а не вывод инструмента.
	ask := []byte(`"name":"AskUserQuestion"`)
	keep := func(raw []byte) bool { return bytes.Contains(raw, ask) || has(raw) }
	var hits []hit
	seen := map[string]bool{} // запись, повторённая в другом транскрипте (продолжение сессии), — один раз
	for _, p := range paths {
		lines, err := readLines(p, keep)
		if errors.Is(err, fs.ErrNotExist) {
			continue // файл ушёл между списком и чтением
		}
		if err != nil {
			return nil, err
		}
		for _, l := range lines {
			if !l.user || !hasFragment(l.text, opts.frag) {
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
	return hits, nil
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

var briefLink = regexp.MustCompile("user-word\\s+((?:/|~/)[^\\s'\"`]*(?:\\.jsonl|…jsonl))\\s+(?:'([^']+)'|\"([^\"]+)\")")

// briefLinkLike — всё, что похоже на ссылку: не подошедшее под briefLink попадает в итог как «не найдено», а не
// пропадает молча. Образец вроде `user-word <jsonl> '<кусок>'` без .jsonl сюда не попадает.
var briefLinkLike = regexp.MustCompile("user-word\\s+[^\\s'\"`]*(?:\\.jsonl|…jsonl)")

var userMarker = regexp.MustCompile(`[Пп]ользовател[а-яё]*\s+(\d{2}\.\d{2})(?:\s+(\d{2}:\d{2}(?::\d{2})?))?`)

// markerDate — дата пометки в тексте между пометкой и цитатой: значит, цитата принадлежит другой пометке.
var markerDate = regexp.MustCompile(`\b\d{2}\.\d{2}\b`)

// briefWord — слово пользователя, указанное ссылкой или цитатой в брифе.
type briefWord struct {
	raw          string
	path         string
	fragment     string
	day          time.Time
	markedAt     time.Time
	isQuote      bool
	invalidCause string
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
	contexts := make(map[string][]line)
	found, ambiguous, missing := 0, 0, 0
	for i, word := range words {
		fmt.Fprintf(stdout, "== %d. %s\n", i+1, word.raw)
		hits, note, err := briefHits(word, e, contexts)
		if err != nil {
			fmt.Fprintf(stderr, "user-word: %v\n", err)
			return exitUsage
		}
		// Реплика, которой нет при перечитывании (живая сессия переписала файл), — не совпадение.
		var located []hit
		for _, h := range hits {
			if findLine(contexts[h.path], h.line) >= 0 {
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
			lines := contexts[h.path]
			showBrief(stdout, lines, findLine(lines, h.line), until, e.loc)
		}
	}
	fmt.Fprintf(stdout, "brief: %d user words: %d found, %d ambiguous, %d not found\n", len(words), found, ambiguous, missing)
	if missing > 0 {
		return exitNone
	}
	return 0
}

func findLine(lines []line, target line) int {
	for i, candidate := range lines {
		if candidate.user && candidate.no == target.no && candidate.text == target.text && candidate.t.Equal(target.t) {
			return i
		}
	}
	return -1
}

func extractBriefWords(text string, until time.Time, loc *time.Location) []briefWord {
	var words []briefWord
	for row := range strings.SplitSeq(text, "\n") {
		words = append(words, extractBriefLinks(row)...)
		words = append(words, extractBriefQuotes(row, until, loc)...)
	}
	return words
}

func extractBriefLinks(row string) []briefWord {
	var words []briefWord
	parsed := map[int]bool{}
	for _, match := range briefLink.FindAllStringSubmatchIndex(row, -1) {
		parsed[match[0]] = true
		frag := match[4:6] // 'кусок'
		if frag[0] < 0 {
			frag = match[6:8] // "кусок"
		}
		fragment := row[frag[0]:frag[1]]
		words = append(words, briefWord{raw: row[match[0]:match[1]], path: row[match[2]:match[3]], fragment: fragment})
	}
	for _, match := range briefLinkLike.FindAllStringIndex(row, -1) {
		if !parsed[match[0]] {
			words = append(words, briefWord{raw: row[match[0]:match[1]], invalidCause: "link not in the form user-word /path.jsonl 'fragment' (absolute or ~/ path, quoted fragment)"})
		}
	}
	return words
}

func extractBriefQuotes(row string, until time.Time, loc *time.Location) []briefWord {
	var words []briefWord
	for _, marker := range userMarker.FindAllStringSubmatchIndex(row, -1) {
		start, ok := quoteStart(row, marker[1])
		end := -1
		if ok {
			end = quoteEnd(row, start)
		}
		if end < 0 {
			// Дата пометки без цитаты — пересказ: проверяющему нечего сверить, это видно в итоге.
			words = append(words, briefWord{raw: row[marker[0]:marker[1]], isQuote: true, invalidCause: "no «quote» after a colon right after the marker"})
			continue
		}
		fragment := row[start+len("«") : end-len("»")]
		if strings.Trim(fragment, "… \t") == "" {
			words = append(words, briefWord{raw: row[marker[0]:end], isQuote: true, invalidCause: "empty quote"})
			continue
		}
		day, cause := markerDay(row[marker[2]:marker[3]], until, loc)
		markedAt := time.Time{}
		if cause == "" && marker[4] >= 0 {
			markedAt, cause = markerTime(row[marker[4]:marker[5]], day)
		}
		words = append(words, briefWord{raw: row[marker[0]:end], fragment: fragment, day: day, markedAt: markedAt, isQuote: true, invalidCause: cause})
	}
	return words
}

// quoteStart — начало цитаты пометки: первая «» после пометки, перед ней двоеточие, а между ними ни другой даты, ни
// «;» — иначе цитата принадлежит другому («пользователь 31.10 просил; лид 31.10: «…»»). Пояс, время и вставка в
// скобках до двоеточия допустимы.
func quoteStart(row string, after int) (int, bool) {
	open := strings.Index(row[after:], "«")
	if open < 0 {
		return 0, false
	}
	between := row[after : after+open]
	if !strings.HasSuffix(strings.TrimRight(between, " \t"), ":") || strings.Contains(between, ";") || markerDate.MatchString(between) {
		return 0, false
	}
	return after + open, true
}

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

func markerDay(dateText string, until time.Time, loc *time.Location) (time.Time, string) {
	date, err := time.ParseInLocation("02.01", dateText, loc)
	if err != nil {
		return time.Time{}, "invalid marker date " + dateText + ": " + err.Error()
	}
	localUntil := until.In(loc)
	year := localUntil.Year()
	if date.Month() > localUntil.Month() || (date.Month() == localUntil.Month() && date.Day() > localUntil.Day()) {
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

func briefHits(word briefWord, e env, contexts map[string][]line) ([]hit, string, error) {
	if word.invalidCause != "" {
		return nil, word.invalidCause, nil
	}
	if word.isQuote {
		return briefQuoteHits(word, e, contexts)
	}
	return briefLinkHits(word, e, contexts)
}

func briefLinkHits(word briefWord, e env, contexts map[string][]line) ([]hit, string, error) {
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
	lines, err := contextLines(path, contexts)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "transcript not found", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read transcript %s: %w", path, err)
	}
	return lastBriefLinkHit(path, lines, word.fragment)
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
		if l.user && hasFragment(l.text, fragment) {
			last = l
			matched++
		}
	}
	if matched == 0 {
		return nil, "fragment not found in transcript", nil
	}
	// Ссылка указывает на свежую реплику с куском, как и обычный режим; о прежних — заметка.
	note := ""
	if matched > 1 {
		note = fmt.Sprintf("the last of %d user lines with the fragment; a longer fragment for an earlier one", matched)
	}
	return []hit{{path: path, line: last}}, note, nil
}

func briefQuoteHits(word briefWord, e env, contexts map[string][]line) ([]hit, string, error) {
	if word.day.IsZero() {
		return nil, "invalid marker date", nil
	}
	date := word.day.In(e.loc).Format(time.DateOnly)
	hits, err := findHits(options{frag: word.fragment, since: date}, e)
	if err != nil {
		return nil, "", err
	}
	dayHits := quoteHitsOnDay(hits, date, word.fragment, e.loc)
	if len(dayHits) == 0 {
		return nil, "no matching user line on marker day", nil
	}
	if !word.markedAt.IsZero() {
		near := hitsNearMarker(dayHits, word.markedAt)
		if len(near) == 0 {
			return nil, fmt.Sprintf("no lines within %s of marker time (%d matches on marker day)", markerWindow, len(dayHits)), nil
		}
		dayHits = near
	}
	// Одинаковый текст в разное время — разные реплики (два «да» за день): неоднозначность, а не одна находка.
	// Копию той же записи в продолжении сессии findHits уже свёл.
	if err := loadHitContexts(dayHits, contexts); err != nil {
		return nil, "", err
	}
	return dayHits, "", nil
}

// quoteHitsOnDay — совпадения дня пометки, где цитата стоит целыми словами.
func quoteHitsOnDay(hits []hit, date, fragment string, loc *time.Location) []hit {
	var found []hit
	for _, h := range hits {
		if h.t.In(loc).Format(time.DateOnly) == date && hasWords(h.text, fragment) {
			found = append(found, h)
		}
	}
	return found
}

func hitsNearMarker(hits []hit, markedAt time.Time) []hit {
	var found []hit
	for _, h := range hits {
		if d := h.t.Sub(markedAt); d >= -markerWindow && d <= markerWindow {
			found = append(found, h)
		}
	}
	return found
}

func loadHitContexts(hits []hit, contexts map[string][]line) error {
	for _, h := range hits {
		if _, err := contextLines(h.path, contexts); err != nil {
			return fmt.Errorf("read transcript %s: %w", h.path, err)
		}
	}
	return nil
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
	var out []string
	for _, p := range paths {
		if project != "" && !strings.Contains(p, project) && !strings.Contains(p, notAlnum.ReplaceAllString(project, "-")) {
			continue
		}
		if !from.IsZero() {
			st, err := os.Stat(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("transcript %s: %w", p, err)
			}
			if st.ModTime().Before(from) {
				continue
			}
		}
		out = append(out, p)
	}
	return out, nil
}
