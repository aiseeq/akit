// Package edit — правка файлов точным текстом с ожидаемым числом совпадений: все правки считаются в памяти, и файлы
// меняются только если у каждой правки число совпадений сошлось с ожидаемым. Иначе не меняется ни один файл, а
// Report говорит, где расхождение.
package edit

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// Edit — одна правка: заменить Old на New во всех Files; совпадений по всем файлам вместе должно быть ровно Want.
type Edit struct {
	Files  []string
	Old    string
	New    string
	Want   int
	Origin string // откуда правка, для ответа: «аргументы» или «блок 2 (строка 7)»
}

// Маркеры блока — как в SEARCH/REPLACE-блоках aider: этот вид агентам привычен.
const (
	markSearch  = "<<<<<<< SEARCH"
	markDivider = "======="
	markReplace = ">>>>>>> REPLACE"
)

// SpecHelp — описание формата задания для справки утилиты.
const SpecHelp = `Spec format (stdin or -F file), one or more blocks:

  path/to/file.go
  <<<<<<< SEARCH
  exact old text, any number of lines
  =======
  new text (may be empty)
  >>>>>>> REPLACE

The path line names the file for the blocks after it, until the next path line.
With "sub FILE" the blocks need no path line: all of them go to FILE; a path
line, if present, must name FILE.
"<<<<<<< SEARCH 3" expects exactly 3 matches (default 1). Blocks apply in order,
each to the file as the previous blocks left it. Text is matched exactly, as a
substring: whitespace and indentation count. A line that is a marker (spaces
around it ignored) cannot be part of the text; use OLD NEW FILE... for such
edits.`

// ParseSpec разбирает задание из блоков SEARCH/REPLACE. Строки вне блоков — только путь к файлу и пустые: любая
// другая строка — ошибка, чтобы опечатка в маркере не превратилась в молча пропущенную правку.
func ParseSpec(r io.Reader) ([]Edit, error) {
	return parseSpec(r, "")
}

// ParseSpecFor разбирает задание, все блоки которого правят file: строка пути не нужна, а если есть — должна
// называть тот же файл. Агенты пишут `sub FILE <<'EOF'` с блоками без строки пути (6 из 19 ошибок sub
// за сутки).
func ParseSpecFor(r io.Reader, file string) ([]Edit, error) {
	return parseSpec(r, file)
}

func parseSpec(r io.Reader, fixed string) ([]Edit, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	p := &specParser{file: fixed, fixed: fixed}
	for sc.Scan() {
		p.lineNo++
		if err := p.line(sc.Text()); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read spec: %w", err)
	}
	if p.section != outside {
		return nil, fmt.Errorf("%s: not closed with %q", p.cur.Origin, markReplace)
	}
	if len(p.edits) == 0 {
		return nil, fmt.Errorf("spec has no SEARCH/REPLACE blocks")
	}
	return p.edits, nil
}

// Где разбор задания: вне блока, в части SEARCH или в части REPLACE.
const (
	outside = iota
	inSearch
	inReplace
)

type specParser struct {
	edits   []Edit
	file    string
	fixed   string // файл из аргумента: строка пути может быть только им
	cur     Edit
	section int
	oldBuf  []string
	newBuf  []string
	lineNo  int
}

func (p *specParser) line(line string) error {
	// Маркер узнаётся и с отступом: пример в инструкциях агента стоит внутри списка, и его копируют как есть.
	mark := strings.TrimSpace(line)
	switch p.section {
	case inSearch:
		return p.searchLine(line, mark)
	case inReplace:
		return p.replaceLine(line, mark)
	}
	if mark == "" {
		return nil
	}
	if want, ok, err := parseSearchMark(mark); ok {
		if err != nil {
			return fmt.Errorf("line %d: %w", p.lineNo, err)
		}
		if p.file == "" {
			return fmt.Errorf("line %d: SEARCH block without a file path line before it", p.lineNo)
		}
		p.cur = Edit{Files: []string{p.file}, Want: want, Origin: fmt.Sprintf("block %d (line %d)", len(p.edits)+1, p.lineNo)}
		p.section, p.oldBuf, p.newBuf = inSearch, nil, nil
		return nil
	}
	if mark == markDivider || mark == markReplace {
		return fmt.Errorf("line %d: %q outside a SEARCH block", p.lineNo, mark)
	}
	path := strings.TrimSpace(line)
	if p.fixed != "" {
		if !samePath(path, p.fixed) {
			return fmt.Errorf("line %d: path %q differs from the FILE argument %q", p.lineNo, path, p.fixed)
		}
		return nil
	}
	p.file = path
	return nil
}

// samePath — два написания одного пути: ./a.go и a.go, относительный и абсолютный.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}

func (p *specParser) searchLine(line, mark string) error {
	if mark == markDivider {
		p.section = inReplace
		return nil
	}
	if mark == markReplace || strings.HasPrefix(mark, markSearch) {
		return fmt.Errorf("line %d: %q before %q in %s", p.lineNo, mark, markDivider, p.cur.Origin)
	}
	p.oldBuf = append(p.oldBuf, line)
	return nil
}

func (p *specParser) replaceLine(line, mark string) error {
	if mark == markDivider || strings.HasPrefix(mark, markSearch) {
		return fmt.Errorf("line %d: %q inside the REPLACE part of %s", p.lineNo, mark, p.cur.Origin)
	}
	if mark != markReplace {
		p.newBuf = append(p.newBuf, line)
		return nil
	}
	p.cur.Old, p.cur.New = strings.Join(p.oldBuf, "\n"), strings.Join(p.newBuf, "\n")
	if p.cur.Old == "" {
		return fmt.Errorf("%s: empty SEARCH text", p.cur.Origin)
	}
	p.edits = append(p.edits, p.cur)
	p.section = outside
	return nil
}

// parseSearchMark узнаёт строку «<<<<<<< SEARCH [N]»; ok — строка начинается с маркера.
func parseSearchMark(mark string) (want int, ok bool, err error) {
	rest, found := strings.CutPrefix(mark, markSearch)
	if !found {
		return 0, false, nil
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return 1, true, nil
	}
	want, err = ParseWant(rest)
	return want, true, err
}

// ParseWant разбирает ожидаемое число совпадений: целое не меньше 1.
func ParseWant(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("expected match count must be an integer >= 1, got %q", s)
	}
	return n, nil
}
