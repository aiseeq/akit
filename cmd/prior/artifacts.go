package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Потолки разделов артефактов и памяти: сколько строк показать.
const (
	maxArtifacts = 10
	maxMemories  = 8
)

// distinct — сколько разных слов запроса нашлось в текстах.
func distinct(q query, texts ...string) int {
	n := 0
	for _, re := range q.words {
		if slices.ContainsFunc(texts, re.MatchString) {
			n++
		}
	}
	return n
}

// artifact — строка списка опубликованных артефактов: чей, название, ссылка, обновлён.
type artifact struct {
	owner, title, url, updated string
	words                      int
}

// artifacts — опубликованные артефакты claude.ai по названию из списка, который каждую ночь пишет ночной разбор. Порядок: больше
// разных слов в названии, потом свежее.
func artifacts(w io.Writer, q query, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(w, "(нет списка: %v)\n", err)
		return
	}
	var hits []artifact
	for l := range strings.Lines(string(data)) {
		l = strings.TrimSuffix(l, "\n")
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, "\t")
		if len(f) != 4 {
			fmt.Fprintln(w, cut("(строка не из 4 колонок: "+l+")"))
			continue
		}
		a := artifact{owner: f[0], title: f[1], url: f[2], updated: f[3]}
		if a.words = distinct(q, a.title); a.words > 0 {
			hits = append(hits, a)
		}
	}
	if len(hits) == 0 {
		fmt.Fprintln(w, "(нет)")
		return
	}
	slices.SortStableFunc(hits, func(a, b artifact) int {
		return cmp.Or(b.words-a.words, strings.Compare(b.updated, a.updated))
	})
	for _, a := range hits[:min(len(hits), maxArtifacts)] {
		fmt.Fprintln(w, cut(fmt.Sprintf("«%s» %s %s (%s)", a.title, a.url, a.updated, a.owner)))
	}
	if len(hits) > maxArtifacts {
		fmt.Fprintf(w, "(ещё %d - сузьте слова)\n", len(hits)-maxArtifacts)
	}
}

// memory — файл памяти пространства и его совпадения со словами.
type memory struct {
	file, description, hit string
	words, hits            int
}

// memories — память пространств (~/.claude/projects/*/memory, кроме оглавлений MEMORY.md). Слова ищутся в имени файла,
// описании и тексте, без служебных полей шапки. Порядок: больше разных слов, потом больше строк. На файл — путь,
// описание и строка текста с наибольшим числом слов.
func memories(w io.Writer, q query, projects string) {
	files, err := filepath.Glob(filepath.Join(projects, "*", "memory", "*.md"))
	if err != nil || len(files) == 0 {
		fmt.Fprintln(w, "(нет файлов памяти)")
		return
	}
	var hits []memory
	for _, f := range files {
		if filepath.Base(f) == "MEMORY.md" {
			continue
		}
		m, err := readMemory(f, q)
		if err != nil {
			fmt.Fprintf(w, "(%v)\n", err)
			continue
		}
		if m.words > 0 {
			m.file = strings.TrimPrefix(f, projects+"/")
			hits = append(hits, m)
		}
	}
	if len(hits) == 0 {
		fmt.Fprintln(w, "(нет)")
		return
	}
	slices.SortStableFunc(hits, func(a, b memory) int { return cmp.Or(b.words-a.words, b.hits-a.hits) })
	for _, m := range hits[:min(len(hits), maxMemories)] {
		fmt.Fprintln(w, cut(m.file+": "+m.description))
		if m.hit != "" {
			fmt.Fprintln(w, cut("  "+m.hit))
		}
	}
	if len(hits) > maxMemories {
		fmt.Fprintf(w, "(ещё %d - сузьте слова)\n", len(hits)-maxMemories)
	}
}

// readMemory разбирает файл памяти: из шапки между «---» берутся name и description, остальное — текст.
func readMemory(path string, q query) (memory, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return memory{}, nil
	}
	if err != nil {
		return memory{}, fmt.Errorf("память не прочиталась: %w", err)
	}
	var m memory
	texts := []string{filepath.Base(path)}
	head, best := false, 0
	for i, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		switch {
		case i == 0 && l == "---":
			head = true
			continue
		case head && l == "---":
			head = false
			continue
		case head && strings.HasPrefix(l, "description:"):
			m.description = strings.Trim(strings.TrimSpace(strings.TrimPrefix(l, "description:")), `"`)
			texts = append(texts, m.description)
			continue
		case head && strings.HasPrefix(l, "name:"):
			texts = append(texts, l)
			continue
		case head:
			continue
		}
		texts = append(texts, l)
		if n := distinct(q, l); n > 0 {
			m.hits++
			if n > best {
				best, m.hit = n, l
			}
		}
	}
	m.words = distinct(q, texts...)
	return m, nil
}
