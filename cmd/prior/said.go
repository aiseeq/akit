package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// maxSaid — сколько записей указателя показывать.
const maxSaid = 10

// saidFields — поля строки said.tsv: time session name project scope kind words quote ref supersedes.
const saidFields = 10

// saidEntry — запись указателя сказанного: правило, решение или обещание пользователя дословно.
type saidEntry struct {
	time, name, project, scope, kind, quote, ref, supersedes string
	words                                                    int // сколько разных слов запроса совпало в словах и цитате
	// changedBy — ref более поздней записи, которая эту отменяет или меняет (её supersedes — ref этой).
	changedBy string
}

// said — указатель сказанного (~/.claude/said.tsv, пишет наблюдатель aguard): дословные правила, решения и обещания
// пользователя. Файл только дописывается; запись отменена, если у более поздней supersedes — её ref. Порядок —
// действующие выше отменённых, затем больше разных слов, при равенстве свежие сверху; на запись — время, сессия,
// тип, область, цитата и ссылка для tl. Испорченные строки — числом с первой, чтобы молчание не читалось как «не было».
func said(w io.Writer, q query, path string) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(w, "(нет указателя)")
		return
	}
	if err != nil {
		fmt.Fprintf(w, "(не прочитался: %v)\n", err)
		return
	}
	hits, bad, firstBad := saidHits(string(data), q)
	if bad > 0 {
		fmt.Fprintf(w, "(строк не по формату: %d, первая — %d)\n", bad, firstBad)
	}
	if len(hits) == 0 {
		fmt.Fprintln(w, "(нет)")
		return
	}
	if len(hits) > maxSaid {
		fmt.Fprintf(w, "(совпадений %d, ниже %d лучших - сузьте слова)\n", len(hits), maxSaid)
		hits = hits[:maxSaid]
	}
	for _, e := range hits {
		where := e.scope
		if e.project != "" && e.project != e.scope {
			where += ", сказано в " + e.project
		}
		note := ""
		if e.changedBy != "" {
			note = " ОТМЕНЕНА или изменена записью " + e.changedBy
		}
		at, _, _ := strings.Cut(e.ref, "#") // <id>:<строка>#2 — вторая запись той же реплики; tl знает строку
		fmt.Fprintf(w, "%s %s %s %s (tl %s)%s\n  %s\n", e.time, e.name, e.kind, where, at, note,
			cut(strings.ReplaceAll(e.quote, "\n", " ⏎ ")))
	}
}

// saidHits — совпавшие записи по порядку выдачи, число испорченных строк и номер первой.
func saidHits(data string, q query) (hits []*saidEntry, bad, firstBad int) {
	changed := map[string]string{} // ref отменённой → ref отменившей
	for i, l := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
		if i == 0 && strings.HasPrefix(l, "time\t") || l == "" {
			continue
		}
		f := strings.Split(l, "\t")
		if len(f) != saidFields {
			bad++
			if firstBad == 0 {
				firstBad = i + 1
			}
			continue
		}
		e := &saidEntry{time: f[0], name: f[2], project: f[3], scope: f[4], kind: f[5], quote: unescape(f[7]), ref: f[8],
			supersedes: f[9]}
		if e.supersedes != "" {
			changed[e.supersedes] = e.ref
		}
		if e.name == "" {
			e.name = shortID(f[1])
		}
		for _, re := range q.words {
			if re.MatchString(f[6]) || re.MatchString(e.quote) {
				e.words++
			}
		}
		if e.words > 0 {
			hits = append(hits, e)
		}
	}
	for _, e := range hits {
		e.changedBy = changed[e.ref]
	}
	slices.SortStableFunc(hits, func(a, b *saidEntry) int {
		return cmp.Or(cmp.Compare(len(a.changedBy), len(b.changedBy)), cmp.Compare(b.words, a.words),
			cmp.Compare(b.time, a.time))
	}) // len(changedBy): 0 у действующих, они выше
	return hits, bad, firstBad
}

// unescape раскрывает \\, \t и \n цитаты; другие пары с \ остаются как есть.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '\\':
			b.WriteByte('\\')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
			continue
		}
		i++
	}
	return b.String()
}

func shortID(id string) string { return id[:min(len(id), 8)] }
