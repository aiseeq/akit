package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aiseeq/akit/proc"
)

// Потолки раздела механизмов: строк на раздел и совпавших строк на карточку.
const (
	maxCardLines = 40
	hitsPerCard  = 3
)

// inventoryWait — страховка от зависшей сверки карточек: её бюджет (1 с) проверяется между пересборками, а не внутри.
const inventoryWait = 5 * time.Second

// card — карточка механизма и её совпадения со словами.
type card struct {
	file, title, what, status string
	named                     bool // слово совпало с именем файла карточки
	words                     int  // сколько разных слов совпало
	hits                      []cardHit
}

type cardHit struct {
	weight int // число совпавших слов, ручная часть «Важно знать» — +10
	text   string
}

func (c card) score() int {
	s := c.words*100 + min(len(c.hits), 99)
	if c.named {
		s += 1000
	}
	return s
}

func (c card) fresh() bool { return strings.Contains(c.status, "актуальна") }

// mechanisms — карточки наших инструментов. Сначала сверка со штампами источников: устаревшее пересобирается или
// помечается «УСТАРЕЛА», так что выдача не врёт о свежести. Порядок: совпало имя карточки, потом больше разных слов,
// потом больше строк. На карточку — заголовок со строкой «Что», статус, если не актуальна, и до 3 совпавших строк:
// сначала из ручной части «Важно знать», потом с большим числом разных слов.
func mechanisms(w io.Writer, q query, s paths) {
	syncCards(w, s.inventory)
	files, err := filepath.Glob(filepath.Join(s.mechanisms, "*.md"))
	if err != nil || len(files) == 0 {
		fmt.Fprintln(w, "(нет карточек)")
		return
	}
	var cards []card
	for _, f := range files {
		c, err := readCard(f, q)
		if err != nil {
			fmt.Fprintf(w, "(%v)\n", err)
			continue
		}
		if c.words > 0 {
			cards = append(cards, c)
		}
	}
	if len(cards) == 0 {
		fmt.Fprintln(w, "(нет)")
		return
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].score() > cards[j].score() })
	out := 0
	var rest []string
	for _, c := range cards {
		n := 1 + min(len(c.hits), hitsPerCard)
		if !c.fresh() {
			n++
		}
		if out+n > maxCardLines {
			rest = append(rest, c.file)
			continue
		}
		head := c.file + ":1:" + strings.TrimPrefix(c.title, "# ")
		if c.what != "" {
			head += " | " + c.what
		}
		fmt.Fprintln(w, cut(head))
		if !c.fresh() {
			fmt.Fprintln(w, cut("  "+c.status))
		}
		for _, h := range bestHits(c.hits) {
			fmt.Fprintln(w, cut(h.text))
		}
		out += n
	}
	if len(rest) > 0 {
		fmt.Fprintln(w, cut(fmt.Sprintf("(ещё %d карточек - сузьте слова: %s)", len(rest), strings.Join(rest, ", "))))
	}
}

// syncCards — сверка карточек программой разбора inventory; молчит, если всё свежо.
func syncCards(w io.Writer, inventory string) {
	if _, err := os.Stat(inventory); err != nil {
		fmt.Fprintf(w, "(карточки не сверены с источниками: %v)\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), inventoryWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, inventory, "-mechanisms", "-budget", "1s", "-q")
	if err := proc.CancelByStop(cmd, time.Second, syscall.SIGTERM); err != nil {
		fmt.Fprintf(w, "(сверка карточек не запущена: %v)\n", err)
		return
	}
	out, err := cmd.CombinedOutput()
	for l := range strings.Lines(string(out)) {
		fmt.Fprintln(w, cut(strings.TrimSuffix(l, "\n")))
	}
	if err != nil {
		fmt.Fprintf(w, "(сверка карточек упала или не уложилась в %s - статусы ниже могут отставать: %v)\n", inventoryWait, err)
	}
}

// readCard разбирает карточку: первая строка — заголовок, «Статус:», «Что:», ручная часть после «## Важно знать».
func readCard(path string, q query) (card, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return card{}, fmt.Errorf("карточка не прочиталась: %w", err)
	}
	c := card{file: filepath.Base(path)}
	seen := make([]bool, len(q.words))
	for i := range q.words {
		if q.matchWord(i, c.file) {
			seen[i], c.named = true, true
		}
	}
	manual := 0
	for i, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		switch {
		case i == 0:
			c.title = l
			continue
		case strings.HasPrefix(l, "<!--"):
			continue
		case strings.HasPrefix(l, "Статус: "):
			c.status = l
			continue
		case l == "## Важно знать":
			manual = 10
			continue
		}
		isWhat := strings.HasPrefix(l, "Что: ")
		if isWhat {
			c.what = l
		}
		hit := 0
		folded := foldText(l)
		for j := range q.words {
			if q.matchWordFolded(j, l, folded) {
				seen[j] = true
				hit++
			}
		}
		if hit > 0 && !isWhat {
			c.hits = append(c.hits, cardHit{weight: hit + manual, text: fmt.Sprintf("%s:%d:%s", c.file, i+1, l)})
		}
	}
	for _, s := range seen {
		if s {
			c.words++
		}
	}
	return c, nil
}

// bestHits — до трёх строк с наибольшим весом, при равном — раньше в карточке.
func bestHits(hits []cardHit) []cardHit {
	best := make([]cardHit, len(hits))
	copy(best, hits)
	sort.SliceStable(best, func(i, j int) bool { return best[i].weight > best[j].weight })
	return best[:min(len(best), hitsPerCard)]
}

// maxRuns — сколько отчётов ночного разбора показывать, свежие сверху.
const maxRuns = 10

const maxRunsPerDay = 3

type runMatch struct {
	path string
	line string
	day  string
}

// runs — отчёты ночного разбора с совпадением: путь от runs и первая совпавшая строка.
func runs(w io.Writer, q query, dir string) {
	files, walkErrors := runMarkdownFiles(dir)
	for _, err := range walkErrors {
		fmt.Fprintf(w, "(%v)\n", err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))

	results := matchRunFiles(q, files, q.runs)
	var matches []runMatch
	for i, f := range files {
		line, err := results[i].line, results[i].err
		if err != nil {
			fmt.Fprintf(w, "(%v)\n", err)
			continue
		}
		if line == "" {
			continue
		}
		rel, err := filepath.Rel(dir, f)
		if err != nil {
			fmt.Fprintf(w, "(путь отчёта не вычислился: %v)\n", err)
			continue
		}
		day := strings.Split(filepath.ToSlash(rel), "/")[0]
		matches = append(matches, runMatch{path: f, line: line, day: day})
	}

	selected, shown := selectRuns(matches)
	for i, m := range matches {
		if selected[i] {
			fmt.Fprintf(w, "%s: %s\n", strings.TrimPrefix(m.path, dir+"/"), cut(m.line))
		}
	}
	if shown == 0 {
		fmt.Fprintln(w, "(нет)")
		return
	}
	if omitted := len(matches) - shown; omitted > 0 {
		fmt.Fprintf(w, "… ещё %d с совпадением, старейший: %s\n", omitted, oldestOmitted(matches, selected, dir))
	}
}

// runMarkdownFiles lists reports without sorting every directory. The result is sorted once by runs;
// non-Markdown entries are inspected only as directory entries, never statted or opened as files.
func runMarkdownFiles(root string) ([]string, []error) {
	var files []string
	var walkErrors []runWalkError
	info, err := os.Lstat(root)
	if err != nil {
		return nil, []error{err}
	}
	if !info.IsDir() {
		if strings.HasSuffix(root, ".md") {
			files = append(files, root)
		}
		return files, nil
	}
	var walk func(string)
	walk = func(dir string) {
		f, err := os.Open(dir)
		if err != nil {
			walkErrors = append(walkErrors, runWalkError{err: err})
			return
		}
		entries, err := f.ReadDir(-1)
		closeErr := f.Close()
		if err != nil {
			walkErrors = append(walkErrors, runWalkError{err: err})
		}
		if closeErr != nil {
			walkErrors = append(walkErrors, runWalkError{err: closeErr})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				walk(path)
			} else if strings.HasSuffix(entry.Name(), ".md") {
				files = append(files, path)
			}
		}
	}
	walk(root)
	errors := make([]error, len(walkErrors))
	for i, walkErr := range walkErrors {
		errors[i] = walkErr.err
	}
	return files, errors
}

type runWalkError struct {
	err error
}

// matchRunFiles читает отчёты параллельно, сохраняя результаты в порядке входного списка.
func matchRunFiles(q query, files []string, filter []runNeedle) []runFileResult {
	results := make([]runFileResult, len(files))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(len(files), runtime.GOMAXPROCS(0), 4) {
		workers.Go(func() {
			for i := range jobs {
				results[i].line, results[i].err = firstMatchFiltered(q, files[i], filter)
			}
		})
	}
	for i := range files {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	return results
}

type runFileResult struct {
	line string
	err  error
}

func selectRuns(matches []runMatch) ([]bool, int) {
	selected := make([]bool, len(matches))
	perDay := make(map[string]int)
	shown := 0
	for i, m := range matches {
		if shown == maxRuns {
			break
		}
		if perDay[m.day] < maxRunsPerDay {
			selected[i] = true
			perDay[m.day]++
			shown++
		}
	}
	for i := range matches {
		if shown == maxRuns {
			break
		}
		if !selected[i] {
			selected[i] = true
			shown++
		}
	}
	return selected, shown
}

func oldestOmitted(matches []runMatch, selected []bool, dir string) string {
	for i := len(matches) - 1; i >= 0; i-- {
		if !selected[i] {
			return strings.TrimPrefix(matches[i].path, dir+"/")
		}
	}
	return ""
}

// firstMatch — первая строка файла со словом, пусто — нет такой.
func firstMatch(q query, path string) (string, error) {
	return firstMatchFiltered(q, path, nil)
}

func firstMatchFiltered(q query, path string, filter []runNeedle) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("отчёт не прочитался: %w", err)
	}
	if filter == nil {
		filter = q.runs
	}
	for at := 0; at < len(data); {
		end := bytes.IndexByte(data[at:], '\n')
		if end < 0 {
			end = len(data) - at
		}
		line := data[at : at+end]
		// The cheap literal filter rejects most lines before the regexp processes its alternatives.
		if len(filter) > 0 && !runMayMatchASCII(string(line), filter) {
			if at+end == len(data) {
				break
			}
			at += end + 1
			continue
		}
		folded := foldText(string(line))
		if (len(filter) == 0 || runMayMatch(folded, filter)) && q.any.Match(line) {
			return string(line), nil
		}
		if at+end == len(data) {
			break
		}
		at += end + 1
	}
	return "", nil
}

// maxReplies — сколько совпавших реплик показывать.
const maxReplies = 8

// replyHeader — заголовок блока журнала реплик: время UTC и сессия claude, по ним находится строка транскрипта.
var replyHeader = regexp.MustCompile(`^### (\S+Z) .* · claude ([0-9a-f]{8})[0-9a-f-]*$`)

// replyHit — блок журнала реплик с совпадением: фраза агента «в ответ на», совпавшие строки пользователя (или первая
// строка ответа, если совпала только фраза агента) и сколько разных слов темы нашлось во всём блоке.
type replyHit struct {
	head, ctx string
	lines     []string
	words     int
}

// replyBlocks — блоки журнала, где совпало хоть что-то: один блок - одно место в выводе, даже если совпали и фраза
// агента, и ответ.
func replyBlocks(data string, q query) []replyHit {
	var hits []replyHit
	var cur replyHit
	first, matched := "", false
	seen := make([]bool, len(q.words))
	flush := func() {
		if matched {
			if len(cur.lines) == 0 && first != "" {
				cur.lines = []string{first}
			}
			hits = append(hits, cur)
		}
		cur, first, matched = replyHit{}, "", false
		clear(seen)
	}
	for l := range strings.Lines(data) {
		l = strings.TrimSuffix(l, "\n")
		if strings.HasPrefix(l, "### ") {
			flush()
			cur.head = l
			continue
		}
		ctx := strings.HasPrefix(l, "в ответ на: ")
		if ctx {
			cur.ctx = l
		} else if first == "" && l != "" {
			first = l
		}
		folded := foldText(l)
		if !matchFilteredFolded(q.any, q.runs, l, folded) {
			continue
		}
		matched = true
		for i := range q.words {
			if !seen[i] && q.matchWordFolded(i, l, folded) {
				seen[i] = true
				cur.words++
			}
		}
		if !ctx {
			cur.lines = append(cur.lines, l)
		}
	}
	flush()
	return hits
}

// replies — реплики пользователя из журнала, который дописывает программа разбора daydigest: заголовок блока, «в ответ на»
// (последняя фраза агента) и сама строка.
func replies(w io.Writer, q query, s paths) {
	if err := exec.Command(s.daydigest, "-log", s.replies).Run(); err != nil {
		fmt.Fprintf(w, "(daydigest не дописал журнал - он может отставать: %v)\n", err)
	}
	data, err := os.ReadFile(s.replies)
	if err != nil {
		fmt.Fprintf(w, "(нет журнала: %v)\n", err)
		return
	}
	hits := replyBlocks(string(data), q)
	if len(hits) == 0 {
		fmt.Fprintln(w, "(нет)")
		return
	}
	if len(hits) > maxReplies {
		fmt.Fprintf(w, "(совпадений %d, ниже %d с наибольшим числом слов темы в блоке (вопрос агента и ответ), при равенстве - свежие: нужного нет - сузьте слова)\n", len(hits), maxReplies)
		hits = topReplies(hits)
	}
	var transcripts transcriptIndex
	for _, h := range hits {
		if replyHeader.MatchString(h.head) && transcripts == nil {
			transcripts = indexTranscripts(s.projects)
		}
		ref, err := replyRef(h.head, transcripts)
		if err != nil {
			ref = " · tl: " + err.Error()
		}
		fmt.Fprintln(w, h.head+ref)
		if h.ctx != "" {
			fmt.Fprintln(w, "  "+cut(h.ctx))
		}
		for _, l := range h.lines {
			fmt.Fprintln(w, "  "+cut(l))
		}
	}
}

// topReplies — maxReplies блоков с наибольшим числом разных слов темы во фразе агента и ответе вместе, при равенстве -
// поздние; печатаются в порядке журнала. Иначе последние по времени вытесняли старый ответ, где слов темы больше;
// счёт по блоку целиком держит короткий ответ «да» на вопрос агента ровно по теме.
func topReplies(hits []replyHit) []replyHit {
	order := make([]int, len(hits))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		if hits[order[a]].words != hits[order[b]].words {
			return hits[order[a]].words > hits[order[b]].words
		}
		return order[a] > order[b]
	})
	keep := order[:maxReplies]
	sort.Ints(keep)
	top := make([]replyHit, 0, maxReplies)
	for _, i := range keep {
		top = append(top, hits[i])
	}
	return top
}

type transcriptIndex map[string][]string

// indexTranscripts walks project directories once per prior call. The old per-reply glob reopened every
// project directory for each of up to eight references.
func indexTranscripts(projects string) transcriptIndex {
	index := make(transcriptIndex)
	projectsDirs, err := os.ReadDir(projects)
	if err != nil { //nolint:silent-error-handling // The previous filepath.Glob returned zero matches for an unreadable root.
		return index
	}
	for _, project := range projectsDirs {
		path := filepath.Join(projects, project.Name())
		files, err := os.ReadDir(path)
		if err != nil { //nolint:silent-error-handling // filepath.Glob skipped unreadable project directories.
			continue // filepath.Glob ignored unreadable project directories too.
		}
		for _, file := range files {
			name := file.Name()
			if len(name) >= 8 && filepath.Ext(name) == ".jsonl" {
				index[name[:8]] = append(index[name[:8]], filepath.Join(path, name))
			}
		}
	}
	return index
}

// replyRef — ссылка tl на запись пользователя в транскрипте claude: журнал хранит время и сессию, номер строки
// ищется по времени записи. Не нашлась - ошибка, её печатают вместо ссылки, а не молчат.
func replyRef(head string, transcripts transcriptIndex) (string, error) {
	m := replyHeader.FindStringSubmatch(head)
	if m == nil {
		return "", nil
	}
	files := transcripts[m[2]]
	if len(files) != 1 {
		return "", fmt.Errorf("транскриптов сессии %s найдено %d", m[2], len(files))
	}
	at, err := time.Parse(time.RFC3339Nano, m[1])
	if err != nil {
		return "", fmt.Errorf("время %s не разобралось: %w", m[1], err)
	}
	// Журнал печатает время без хвостовых нулей (.44Z), транскрипт claude — с миллисекундами тремя знаками (.440Z).
	stamp := at.UTC().Format("2006-01-02T15:04:05.000Z")
	n, err := lineWith(files[0], []byte(`"timestamp":"`+stamp+`"`))
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", fmt.Errorf("строки с %s в транскрипте нет", stamp)
	}
	return fmt.Sprintf(" · tl %s:%d", m[2], n), nil
}

// lineWith — номер первой строки файла (с 1), где есть needle; 0 - нет такой. Читает потоком: строки транскрипта
// бывают в мегабайты.
func lineWith(path string, needle []byte) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("транскрипт не открылся: %w", err)
	}
	defer f.Close()
	return lineWithStream(f, needle)
}

func lineWithStream(f *os.File, needle []byte) (int, error) {
	r := bufio.NewReaderSize(f, 1<<20)
	var line []byte
	for n := 1; ; {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if bytes.Contains(line, needle) {
			return n, nil
		}
		if err == io.EOF {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("транскрипт не прочитался: %w", err)
		}
		line, n = line[:0], n+1
	}
}
