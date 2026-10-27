package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	out, err := exec.CommandContext(ctx, inventory, "-mechanisms", "-budget", "1s", "-q").CombinedOutput()
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
	for i, re := range q.words {
		if re.MatchString(c.file) {
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
		for j, re := range q.words {
			if re.MatchString(l) {
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

// runs — отчёты ночного разбора с совпадением: путь от runs и первая совпавшая строка.
func runs(w io.Writer, q query, dir string) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// нечитаемая папка отчётов не мешает искать в остальных, но в выводе видна
			fmt.Fprintf(w, "(%v)\n", err)
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(w, "(отчёты не прочитались: %v)\n", err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	shown := 0
	for _, f := range files {
		if shown == maxRuns {
			break
		}
		line, err := firstMatch(q, f)
		if err != nil {
			fmt.Fprintf(w, "(%v)\n", err)
			continue
		}
		if line != "" {
			fmt.Fprintf(w, "%s: %s\n", strings.TrimPrefix(f, dir+"/"), cut(line))
			shown++
		}
	}
	if shown == 0 {
		fmt.Fprintln(w, "(нет)")
	}
}

// firstMatch — первая строка файла со словом, пусто — нет такой.
func firstMatch(q query, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("отчёт не прочитался: %w", err)
	}
	for l := range strings.Lines(string(data)) {
		if l = strings.TrimSuffix(l, "\n"); q.any.MatchString(l) {
			return l, nil
		}
	}
	return "", nil
}

// maxReplies — сколько последних совпавших реплик показывать.
const maxReplies = 8

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
	var hits []string
	head, ctx := "", ""
	for l := range strings.Lines(string(data)) {
		l = strings.TrimSuffix(l, "\n")
		if strings.HasPrefix(l, "### ") {
			head, ctx = l, ""
			continue
		}
		if strings.HasPrefix(l, "в ответ на: ") {
			ctx = l
		}
		if !q.any.MatchString(l) {
			continue
		}
		hit := head + "\n  "
		if ctx != "" && ctx != l {
			hit += cut(ctx) + "\n  "
		}
		hits = append(hits, hit+cut(l))
	}
	if len(hits) == 0 {
		fmt.Fprintln(w, "(нет)")
		return
	}
	if len(hits) > maxReplies {
		fmt.Fprintf(w, "(совпадений %d, ниже последние %d: старый ответ мог не войти - сузьте слова)\n", len(hits), maxReplies)
		hits = hits[len(hits)-maxReplies:]
	}
	for _, h := range hits {
		fmt.Fprintln(w, h)
	}
}
