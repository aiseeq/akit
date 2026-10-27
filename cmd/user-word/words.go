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
	f := fold(frag)
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].user && strings.Contains(fold(lines[i].text), f) {
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

// prefilter — отсев сырых строк транскрипта до разбора. Перевод в нижний регистр каждой строки втрое замедлял
// --search (4,4 → 14,4 с на неделе транскриптов), поэтому отсев ищет как есть самую длинную часть слова куска
// без первой буквы и без е (её могут цитировать как ё), строчными и прописными. Нет такой части — без отсева.
func prefilter(frag string) func(raw []byte) bool {
	var best string
	for w := range strings.FieldsSeq(fold(frag)) {
		r := []rune(w)
		for part := range strings.SplitSeq(string(r[1:]), "е") {
			if len(part) > len(best) {
				best = part
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

func show(w io.Writer, lines []line, at, mins int, loc *time.Location) {
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
	more := 0
	for _, l := range lines[at+1:] {
		if !l.user || l.t.Sub(u.t) > time.Duration(mins)*time.Minute {
			continue
		}
		more++
		fmt.Fprintf(w, "--- user later, %s%s (may be about something else):\n%s\n", l.t.In(loc).Format(stamp), askNote(l), l.text)
	}
	if more == 0 {
		fmt.Fprintf(w, "--- no other user lines within %d min\n", mins)
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
	frag, has := fold(opts.frag), prefilter(opts.frag)
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
			if !l.user || !strings.Contains(fold(l.text), frag) {
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
