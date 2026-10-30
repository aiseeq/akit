package main

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/aiseeq/akit/transcript"
)

// hit — событие с его файлом.
type hit struct {
	src source
	ev  transcript.Event
}

type result struct {
	hits  []hit
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
	literal []byte // строчный литерал поиска для отсева сырых строк; nil — отсева нет
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
	// Отсев сырых строк годится, только если искомое — литерал без символов, которые JSON экранирует.
	if parsed, err := syntax.Parse(opts.grep, syntax.Perl); err == nil && opts.context == 0 {
		lit := parsed.Simplify()
		if lit.Op == syntax.OpLiteral && !strings.ContainsAny(string(lit.Rune), "\"\\\n\t\r") {
			f.literal = bytes.ToLower([]byte(string(lit.Rune)))
		}
	}
	return f, nil
}

func (f *filter) keepRaw(raw []byte) bool {
	if f.literal != nil && !bytes.Contains(bytes.ToLower(raw), f.literal) {
		return false
	}
	return f.rawRE == nil || f.rawRE.Match(raw)
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

// file читает один файл и отдаёт отобранные события с окрестностью -C.
func (f *filter) file(src source) ([]hit, transcript.Meta, error) {
	var keep func([]byte) bool
	if f.literal != nil || f.rawRE != nil {
		keep = f.keepRaw
	}
	var (
		out    []hit
		before []transcript.Event // последние -C событий окна до найденного
		after  int                // сколько событий после найденного ещё показать
	)
	meta, err := transcript.Read(src.path, keep, func(ev transcript.Event) {
		if !f.inWindow(ev) {
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
	if err != nil {
		return out, meta, fmt.Errorf("read %s: %w", src.path, err)
	}
	return out, meta, nil
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
	for range max(1, runtime.NumCPU()/2) {
		wg.Go(func() {
			for src := range jobs {
				hits, meta, err := f.file(src)
				mu.Lock()
				res.hits = append(res.hits, hits...)
				res.bad += meta.Bad
				if meta.FirstBad != nil && res.firstBad == nil {
					res.firstBad = fmt.Errorf("%s %w", src.id, meta.FirstBad)
				}
				if meta.Name != "" {
					res.names[src.id] = meta.Name
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
		if a.src.path != b.src.path {
			return a.src.path < b.src.path
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
		if seen[h.src.id] {
			continue
		}
		seen[h.src.id] = true
		name := res.names[h.src.id]
		if h.src.parentID != "" {
			parentName := res.names[h.src.parentID]
			if parentName != "" {
				parentName = " " + parentName
			}
			description := ""
			if h.src.description != "" {
				description = fmt.Sprintf(" %q", h.src.description)
			}
			fmt.Fprintf(w, "== %s sub of %s%s%s %s\n", h.src.id, h.src.parentID, parentName, description, tilde(h.src.path, e.home))
			continue
		}
		if name != "" {
			name += " "
		}
		if h.src.id != h.src.path {
			fmt.Fprintf(w, "== %s %s%s\n", h.src.id, name, tilde(h.src.path, e.home))
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
	return fmt.Sprintf("%s %s:%d %s %s%s", h.ev.Time.Local().Format("02.01 15:04:05"), h.src.id, h.ev.Line, h.ev.Role,
		who, transcript.Short(h.ev.Shown(), width))
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
func showRef(ref string, lineNo int, e env, w, stderr io.Writer) int {
	path, err := find(ref, e.home)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	src := newSource(path)
	if strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
		src, err = subSource(path)
		if err != nil {
			fmt.Fprintf(stderr, "tl: %v\n", err)
			return exitUsage
		}
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
		fmt.Fprintf(w, "tl: no event at %s:%d (service record, unparsable line or out of range)\n", src.id, lineNo)
		return exitNone
	}
	for i := range found {
		ev := &found[i]
		who := ""
		if ev.Who != "" {
			who = " " + ev.Who
		}
		fmt.Fprintf(w, "--- %s %s:%d %s%s\n%s\n", ev.Time.Local().Format("02.01.2006 15:04:05 MST"), src.id, ev.Line, ev.Role, who, ev.Shown())
	}
	return 0
}
