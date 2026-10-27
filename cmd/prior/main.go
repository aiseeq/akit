// prior — было ли это уже и что у нас уже есть: решения, механизмы, артефакты, память, пробы, бэклог, отчёты
// ночного разбора и реплики пользователя одной командой. Запускается перед вопросом или предложением пользователю и
// перед своим новым механизмом; итог — в строку «Искал: …». Источники — в ~/.claude: журнал решений, карточки
// механизмов, память пространств и состояние ночного разбора (там же список опубликованных артефактов claude.ai).
// Программы разбора (inventory, daydigest) зовутся внешними по путям в ~/.claude/improve/bin: их код живёт в своём
// репозитории.
//
// Коды выхода: 0 — поиск прошёл (найдено или нет — в выводе), 2 — ошибка вызова.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

const usage = `prior — было ли это уже и что у нас уже есть: решения, механизмы, артефакты, память, пробы, бэклог,
отчёты ночного разбора и реплики.
Запускать перед вопросом или предложением пользователю; итог - в строку «Искал: …».

usage: prior <слово> [слово…] - слова темы, любое из них (регулярные выражения, без учёта регистра);
       берите разные формы и синонимы: prior компак compact 'окн[оа] сжатия' '300 ?тыс'

Порядок вывода - по весу источника: журнал решений, сказанное пользователем (правила, решения и обещания дословно),
механизмы (карточки наших инструментов), опубликованные артефакты claude.ai (по названию), память пространств,
пробы и очереди ночного разбора, его отчёты, реплики (свежие внизу).
`

const exitUsage = 2

var errHelp = errors.New("help")

func main() {
	words, err := parseArgs(os.Args[1:])
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "prior: %v\n(prior -h for usage)\n", err)
		os.Exit(exitUsage)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "prior: %v\n", err)
		os.Exit(exitUsage)
	}
	q, err := newQuery(words)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prior: %v\n", err)
		os.Exit(exitUsage)
	}
	run(q, home, os.Stdout)
}

func parseArgs(args []string) ([]string, error) {
	var words []string
	for _, a := range args {
		switch {
		case a == "-h" || a == "--help":
			return nil, errHelp
		case a == "-i" || a == "--ignore-case":
			// поиск и так без учёта регистра; раньше «-i» становился словом и забивал вывод
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf(`флагов нет, кроме -h; слово с дефисом в начале - через '\-': %s`, a)
		default:
			words = append(words, a)
		}
	}
	if len(words) == 0 {
		return nil, errors.New("нужно хотя бы одно слово")
	}
	return words, nil
}

// query — слова поиска: каждое отдельно (для карточек важно, сколько разных слов совпало) и все вместе.
type query struct {
	words []*regexp.Regexp
	any   *regexp.Regexp
}

func newQuery(words []string) (query, error) {
	var q query
	for _, w := range words {
		re, err := regexp.Compile("(?i)" + w)
		if err != nil {
			return q, fmt.Errorf("слово %q: %w", w, err)
		}
		q.words = append(q.words, re)
	}
	q.any = regexp.MustCompile("(?i)(?:" + strings.Join(words, ")|(?:") + ")") // каждое слово уже скомпилировалось
	return q, nil
}

// width — потолок строки вывода в рунах.
const width = 400

func cut(s string) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width])
}

func section(w io.Writer, title string) { fmt.Fprintf(w, "\n== %s\n", title) }

func run(q query, home string, w io.Writer) {
	s := sources(home)
	section(w, "журнал решений (~/.claude/decisions.md)")
	if !grepReport(w, q, s.decisions, "") {
		fmt.Fprintln(w, "(нет)")
	}
	section(w, "сказанное пользователем (~/.claude/said.tsv): правила, решения, обещания дословно, до 10")
	said(w, q, s.said)
	section(w, "механизмы: что у нас уже есть (~/.claude/improve/mechanisms), до 40 строк")
	mechanisms(w, q, s)
	section(w, fmt.Sprintf("артефакты claude.ai по названию (~/.claude/improve/state/artifacts.tsv), до %d", maxArtifacts))
	artifacts(w, q, s.artifacts)
	section(w, fmt.Sprintf("память пространств (~/.claude/projects/*/memory), до %d файлов", maxMemories))
	memories(w, q, s.projects)
	section(w, "пробы и очереди ночного разбора (~/.claude/improve/state)")
	found := false
	for _, f := range stateFiles {
		if grepReport(w, q, s.state+"/"+f, f+":") {
			found = true
		}
	}
	if !found {
		fmt.Fprintln(w, "(нет)")
	}
	section(w, "отчёты ночного разбора (~/.claude/improve/runs), свежие сверху, до 10 файлов")
	runs(w, q, s.runs)
	section(w, "реплики пользователя (~/.claude/user-replies.md), последние 8")
	replies(w, q, s)
}

// paths — где prior ищет и какие программы разбора зовёт.
type paths struct {
	decisions, said, mechanisms, state, runs, replies string
	artifacts, projects                               string
	inventory, daydigest                              string
}

func sources(home string) paths {
	c := home + "/.claude"
	return paths{
		decisions:  c + "/decisions.md",
		said:       c + "/said.tsv",
		mechanisms: c + "/improve/mechanisms",
		state:      c + "/improve/state",
		runs:       c + "/improve/runs",
		replies:    c + "/user-replies.md",
		artifacts:  c + "/improve/state/artifacts.tsv",
		projects:   c + "/projects",
		inventory:  c + "/improve/bin/inventory",
		daydigest:  c + "/improve/bin/daydigest",
	}
}

// stateFiles — пробы и очереди ночного разбора, по весу.
var stateFiles = []string{"experiments.tsv", "backlog.md", "approved.md", "declined.md", "project-queue.md", "workarounds.md"}

// grepReport — grepFile, а ошибка чтения — строкой в выводе: молчание читалось бы как «не было».
func grepReport(w io.Writer, q query, path, prefix string) bool {
	found, err := grepFile(w, q, path, prefix)
	if err != nil {
		fmt.Fprintf(w, "(%v)\n", err)
	}
	return found
}

// grepFile печатает совпавшие строки файла как «префикс номер:строка»; нет файла или совпадений — false.
func grepFile(w io.Writer, q query, path, prefix string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("не прочитался: %w", err)
	}
	found := false
	for i, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if q.any.MatchString(l) {
			found = true
			fmt.Fprintln(w, cut(fmt.Sprintf("%s%d:%s", prefix, i+1, l)))
		}
	}
	return found, nil
}
