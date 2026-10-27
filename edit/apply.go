package edit

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxLines — сколько номеров строк показывать в ответе на одно место, остальное — «…».
const maxLines = 8

// Hit — совпадения правки в одном файле: номера строк в содержимом после этой правки.
type Hit struct {
	File  string
	Lines []int
}

// Result — итог одной правки по всем её файлам.
type Result struct {
	Edit  Edit
	Found int
	Hits  []Hit
	Hints []Hit // нашлось меньше ожидаемого: строки, где текст сходится без учёта пробелов и отступов
}

// OK — совпало ли число с ожидаемым.
func (r Result) OK() bool { return r.Found == r.Edit.Want }

// file — содержимое файла в памяти и его снимок на момент чтения, чтобы не затереть чужую правку.
type file struct {
	path    string // куда писать: путь после разворота симлинков
	orig    string
	cur     string
	mode    fs.FileMode
	size    int64
	modTime time.Time
}

// Plan — все правки, посчитанные в памяти. Файлы не тронуты, пока не вызван Write.
type Plan struct {
	Results []Result
	files   map[string]*file
	order   []string // файлы в порядке первого упоминания
}

// OK — сошлось ли число совпадений у всех правок.
func (p *Plan) OK() bool {
	for i := range p.Results {
		if !p.Results[i].OK() {
			return false
		}
	}
	return true
}

// Build читает файлы и применяет правки по порядку в памяти; каждая правка видит файл таким, каким его оставили
// предыдущие. Ошибка — только чтение файла; расхождение числа совпадений — в Results.
func Build(edits []Edit) (*Plan, error) {
	p := &Plan{files: map[string]*file{}}
	for _, e := range edits {
		res := Result{Edit: e}
		for _, name := range e.Files {
			f, err := p.load(name)
			if err != nil {
				return nil, err
			}
			out, lines := replaceAll(f.cur, e.Old, e.New)
			if len(lines) > 0 {
				res.Found += len(lines)
				res.Hits = append(res.Hits, Hit{File: name, Lines: lines})
				f.cur = out
			}
		}
		if res.Found < e.Want {
			res.Hints = p.nearMisses(e, res.Hits)
		}
		p.Results = append(p.Results, res)
	}
	return p, nil
}

func (p *Plan) load(name string) (*file, error) {
	if f, ok := p.files[name]; ok {
		return f, nil
	}
	path, err := filepath.EvalSymlinks(name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", name, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	f := &file{path: path, orig: string(data), cur: string(data), mode: st.Mode().Perm(), size: st.Size(), modTime: st.ModTime()}
	// Один файл под двумя именами (симлинк и цель) — одно содержимое, иначе вторая запись затрёт первую.
	for _, other := range p.files {
		if other.path == path {
			p.files[name] = other
			return other, nil
		}
	}
	p.files[name] = f
	p.order = append(p.order, name)
	return f, nil
}

// nearMisses — места, где Old сходится без учёта пробелов и отступов, кроме уже найденных точно: поиск идёт по
// тексту после замены, а найденная точно строка может сойтись снова, если New похож на Old.
func (p *Plan) nearMisses(e Edit, hits []Hit) []Hit {
	exact := map[string]map[int]bool{}
	for _, h := range hits {
		exact[h.File] = map[int]bool{}
		for _, l := range h.Lines {
			exact[h.File][l] = true
		}
	}
	var out []Hit
	for _, name := range e.Files {
		var lines []int
		for _, l := range looseLines(p.files[name].cur, e.Old) {
			if !exact[name][l] {
				lines = append(lines, l)
			}
		}
		if len(lines) > 0 {
			out = append(out, Hit{File: name, Lines: lines})
		}
	}
	return out
}

// replaceAll заменяет все непересекающиеся вхождения old и возвращает номера строк, с которых начинается каждая
// вставка в новом тексте.
func replaceAll(s, old, repl string) (string, []int) {
	if !strings.Contains(s, old) {
		return s, nil
	}
	var (
		b     strings.Builder
		lines []int
		line  = 1
	)
	b.Grow(len(s))
	for {
		i := strings.Index(s, old)
		if i < 0 {
			b.WriteString(s)
			break
		}
		line += strings.Count(s[:i], "\n")
		lines = append(lines, line)
		b.WriteString(s[:i])
		b.WriteString(repl)
		line += strings.Count(repl, "\n")
		s = s[i+len(old):]
	}
	return b.String(), lines
}

// looseLines ищет old построчно без учёта пробелов: отступы и пробельные промежутки внутри строки сжимаются в один
// пробел. Самые частые промахи агента — отступ, табы против пробелов и выравнивание gofmt. Возвращает номера строк
// начала таких мест.
func looseLines(s, old string) []int {
	want := trimLines(old)
	for len(want) > 0 && want[0] == "" {
		want = want[1:]
	}
	if len(want) == 0 {
		return nil
	}
	have := trimLines(s)
	var lines []int
	for i := 0; i+len(want) <= len(have); i++ {
		if linesMatch(have[i:i+len(want)], want) {
			lines = append(lines, i+1)
		}
	}
	return lines
}

func trimLines(s string) []string {
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		parts[i] = strings.Join(strings.Fields(p), " ")
	}
	return parts
}

// linesMatch сравнивает строки; первая и последняя строка искомого могут быть хвостом и началом строки файла,
// как при поиске подстроки.
func linesMatch(have, want []string) bool {
	last := len(want) - 1
	for j, w := range want {
		h := have[j]
		switch {
		case last == 0:
			if !strings.Contains(h, w) {
				return false
			}
		case j == 0:
			if !strings.HasSuffix(h, w) {
				return false
			}
		case j == last:
			if !strings.HasPrefix(h, w) {
				return false
			}
		case h != w:
			return false
		}
	}
	return true
}
