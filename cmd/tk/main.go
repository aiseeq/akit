// tk — учёт задач в docs/tasks репозитория.
// Коды выхода: 0 — успех, 1 — задача не найдена или действие отклонено, 2 — ошибка вызова или ввода-вывода.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/timearg"
)

const usage = `tk — track repository tasks in docs/tasks.

Usage:
  tk new SLUG [--prio HIGH|MEDIUM|LOW] [--title TEXT] [--by NAME] [--text FILE|-]
  tk ls [--all] [--status STATUS] [--slug PREFIX] [--root DIR] [--by NAME] [--due] [--json]
  tk note ID TEXT [--by NAME]
  tk plan ID --add 'NAME: COMMAND' [--required] [--by NAME]
  tk plan ID --repin 'NAME: COMMAND' --why TEXT [--by NAME]
  tk skip ID NAME --why TEXT [--by NAME]
  tk check ID
  tk lint [--root DIR]
  tk start ID [--by NAME]
  tk wait ID --until TEXT [--by NAME]
  tk review ID [--by NAME] [--note TEXT]
  tk done ID --check TEXT [--by NAME] [--doer NAME] [--glint-debt SLUG]
  tk reject ID --why TEXT [--by NAME]
  tk show ID [--json]
  tk archive [--older DURATION] [--root DIR]

Flags:
  --prio P       new: priority HIGH, MEDIUM, or LOW (default MEDIUM)
  --title TEXT   new: ticket title (default: slug)
  --by NAME      actor name (default: current session); ls: show tickets assigned to NAME
  --text FILE|-  new: fill ticket sections from FILE, or standard input with -
  --all          ls: include closed tickets

  --due          ls: show waiting tickets whose parsed deadline has passed
  --json         ls: output JSON array; show: output the ticket and journal as JSON
  --status S     ls: show only status S
  --slug PREFIX  ls: show only slugs starting with PREFIX (case-insensitive)
  --root DIR     ls/lint/archive: scan DIR and its first-level repositories
  --note TEXT    review: add a review note to the journal
  --check TEXT   done: describe independent verification (required)
  --doer NAME    done: specify the executor of a legacy ticket
  --glint-debt S done: link findings to an open ticket when glint reports them
  --why TEXT     reject: why the ticket was declined; skip: why the plan item is skipped (required)
  --add TEXT     plan: add a named command to the ticket plan (required)
  --required     plan: the item cannot be skipped (tk skip refuses, exit 2), dropped from the plan or given another command; command syntax is checked with bash -n
  --repin TEXT   plan: replace a required command from another session; the session and old/new commands are recorded in the journal
  --until TEXT   wait: time or condition to wait for (required)
  --older D      archive: closed tickets older than D (default 14d; supports Go durations and d)

show wraps lines longer than 150 characters at spaces (continuation indented
by two more spaces, ` + "```" + ` blocks as is); the ticket file keeps them whole.

Statuses: открыт, в работе, ждёт, на проверке, готово, отклонён.
Exit: 0 success, 1 not found or declined, 2 usage or I/O error.
`
const (
	exitNone  = 1
	exitUsage = 2
	dirMode   = 0755
	fileMode  = 0644
)

var slugRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
var slugPrefixRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*-?$`)
var fileRE = regexp.MustCompile(`^([0-9]{8})-([0-9]{4})-([A-Z]+)-([a-z0-9]+(?:-[a-z0-9]+)*)\.md$`)
var nameRE = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._:@-]*$`)
var ticketIDRE = regexp.MustCompile(`^[0-9]{8}-[0-9]{4}-[A-Z]+-[a-z0-9]+(?:-[a-z0-9]+)*(?:\.md)?$`)
var planNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// planSkipRE — строка журнала, которую пишет tk skip (и прежняя форма без автора), целиком с начала:
// «пропущен:» внутри заметки или причины другого пункта пропуском не считается.
var planSkipRE = regexp.MustCompile(`^- \d{4}-\d{2}-\d{2} \d{2}:\d{2} (?:план — \S+: )?пропущен: ([a-z0-9-]+) — (.*)$`)

// planAddRE — строка журнала tk plan --add; «обязательный» — с --required, тогда после имени — команда пункта.
var planAddRE = regexp.MustCompile(`^- \d{4}-\d{2}-\d{2} \d{2}:\d{2} план — (\S+): добавлен (обязательный )?пункт ([a-z0-9-]+)(?:: (.+))?$`)
var planRepinRE = regexp.MustCompile(`^- \d{4}-\d{2}-\d{2} \d{2}:\d{2} план — (\S+): перезакреплён обязательный пункт ([a-z0-9-]+): (.+) → (.+) — (.+)$`)

// requiredMentionRE — запись «обязательный» где угодно в тексте, без привязки к строке: сверка с целыми строками
// журнала ловит запись, спрятанную склейкой или невидимым символом.
var requiredMentionRE = regexp.MustCompile(`план — \S+: (?:добавлен|перезакреплён) обязательный пункт ([a-z0-9-]+)`)

var errHelp = errors.New("help")
var session = currentSession
var planTimeout = 60 * time.Second

type options struct {
	cmd, id, prio, title, by, note, check, doer, why, status, slug, root, until, text, glintDebt, baseRoot, add, repin, item string
	all, due, json, required                                                                                                 bool
	older                                                                                                                    time.Duration
}
type task struct {
	path, slug, status, prio, title, body string
	created, changed                      time.Time
}

func main() {
	o, e := parseArgs(os.Args[1:])
	if errors.Is(e, errHelp) {
		fmt.Print(usage)
		return
	}
	if e == nil {
		e = validateApproxTime(o, time.Now())
	}
	if e != nil {
		fmt.Fprintf(os.Stderr, "tk: %v\n(tk -h for usage)\n", e)
		os.Exit(exitUsage)
	}
	os.Exit(run(o, os.Stdout, os.Stderr))
}
func parseArgs(a []string) (options, error) {
	if len(a) == 0 {
		return options{}, errors.New("need a command")
	}
	for _, v := range a {
		if v == "-h" || v == "--help" {
			return options{}, errHelp
		}
	}
	o := options{cmd: a[0], older: 14 * 24 * time.Hour}
	pos, err := parseOptions(a[1:], &o)
	if err != nil {
		return o, err
	}
	if o.required && (o.cmd != "plan" || o.repin != "") {
		return o, errors.New("--required is a flag of tk plan --add")
	}
	if err := validateCommand(&o, pos); err != nil {
		return o, err
	}
	if err := validateOneLine(o); err != nil {
		return o, err
	}
	return o, nil
}

// validateOneLine — текст, который ложится в строку журнала или заголовок, — одна строка: перевод строки
// вписал бы в тикет свою строку журнала (пропуск пункта плана, «готово») или свой раздел. Текст журнала без
// угаданного времени: у записи есть точная метка.
func validateOneLine(o options) error {
	for _, f := range []struct{ flag, value string }{{"note", o.note}, {"--title", o.title}, {"--check", o.check}, {"--why", o.why}, {"--until", o.until}, {"--add", o.add}, {"--repin", o.repin}} {
		if strings.ContainsFunc(f.value, notInLine) {
			return fmt.Errorf("%s must be one line", f.flag)
		}
	}
	for _, f := range []struct{ flag, value string }{{"note", o.note}, {"--check", o.check}, {"--why", o.why}, {"--until", o.until}} {
		if m := guessedTime.FindString(f.value); m != "" {
			return fmt.Errorf("%s: %q looks like a guessed time; the time of the entry is its journal stamp, write the exact time from date or drop it", f.flag, strings.TrimSpace(m))
		}
	}
	return nil
}

// validateApproxTime — время в тексте журнала позже метки записи — угаданное будущее время: запись пишется сейчас,
// и её метка уже точная. Проверяются «~03:00» в любом месте и время в начале текста («17:40 проверка …» —
// самодельная метка события); время в середине текста бывает сроком или планом («ждёт до 18:00») и не проверяется.
// Сравнение — по кругу суток: «~00:05» в 23:58 — тоже будущее, «~23:50» в 00:05 — прошлое; будущим считается сдвиг
// больше минуты и меньше 12 часов.
func validateApproxTime(o options, now time.Time) error {
	for _, f := range []struct{ flag, value string }{{"note", o.note}, {"--check", o.check}, {"--why", o.why}} {
		found := approxTime.FindAllStringSubmatchIndex(f.value, -1)
		if ix := leadingTime.FindStringSubmatchIndex(f.value); ix != nil && !timeRange.MatchString(f.value[ix[1]:]) {
			found = append(found, ix) // промежуток в начале («17:40-18:00 окно») — план, не метка
		}
		for _, ix := range found {
			if end := ix[1]; end < len(f.value) && f.value[end] >= '0' && f.value[end] <= '9' {
				continue // не время: «~03:001»
			}
			if err := futureTime(f.flag, f.value, ix, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// futureTime проверяет одно найденное время: группы ix — дата, час, минута.
func futureTime(flag, value string, ix []int, now time.Time) error {
	m := make([]string, 4)
	for g := range m {
		if ix[2*g] >= 0 {
			m[g] = value[ix[2*g]:ix[2*g+1]]
		}
	}
	if m[1] != "" && m[1] != now.Format("02.01") && m[1] != now.Format("2006-01-02") {
		return nil // время другого дня
	}
	h, err := strconv.Atoi(m[2])
	if err != nil {
		return fmt.Errorf("%s: hour %q: %w", flag, m[2], err)
	}
	mi, err := strconv.Atoi(m[3])
	if err != nil {
		return fmt.Errorf("%s: minute %q: %w", flag, m[3], err)
	}
	// По часам на стене, не по длительности: в день перевода часов сутки не 24 часа.
	const minutesPerHour, minutesPerDay = 60, 24 * 60
	ahead := ((h*minutesPerHour+mi-now.Hour()*minutesPerHour-now.Minute())%minutesPerDay + minutesPerDay) % minutesPerDay
	if ahead > 1 && ahead < minutesPerDay/2 {
		return fmt.Errorf("%s: %q is later than the entry time %s; the time of the entry is its journal stamp, write the exact time from date or drop it", flag, strings.TrimSpace(value[ix[0]:ix[1]]), now.Format("15:04"))
	}
	return nil
}

// approxTime — «~ЧЧ:ММ», приблизительное время в тексте, с датой перед ним, если она есть («31.10 ~03:00»).
var approxTime = regexp.MustCompile(`(?:(\d\d\.\d\d|\d{4}-\d\d-\d\d)\s+)?~\s?([01]\d|2[0-3]):([0-5]\d)`)

// leadingTime — время в начале текста, с датой перед ним, если она есть («02.11 03:12: …»).
var leadingTime = regexp.MustCompile(`^[\s\p{Zs}]*(?:(\d\d\.\d\d|\d{4}-\d\d-\d\d)[\s\p{Zs}]+)?([01]\d|2[0-3]):([0-5]\d)`)

// timeRange — продолжение промежутка после времени: «-18:00», « – 18:00».
var timeRange = regexp.MustCompile(`^[\s\p{Zs}]*[-–—][\s\p{Zs}]*([01]?\d|2[0-3]):[0-5]\d(?:$|\D)`)

// guessedTime — время суток с угаданной последней цифрой («06:4x», кириллическая «х» тоже). Границы — явные
// классы: \b в regexp Go знает только ASCII-буквы. Справа не ASCII-буква и не цифра: «06:4xМСК» — время,
// «06:4xyz» — нет.
var guessedTime = regexp.MustCompile(`(?i)(?:^|[^\p{N}:])(?:[01]\d|2[0-3]):[0-5][xх](?:$|[^a-z0-9])`)

func parseOptions(args []string, o *options) ([]string, error) {
	values := map[string]*string{"--prio": &o.prio, "--title": &o.title, "--by": &o.by, "--note": &o.note, "--check": &o.check, "--doer": &o.doer, "--why": &o.why, "--status": &o.status, "--slug": &o.slug, "--root": &o.root, "--until": &o.until, "--text": &o.text, "--glint-debt": &o.glintDebt, "--add": &o.add, "--repin": &o.repin}
	var pos []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		v := args[i]
		switch v {
		case "--all":
			o.all = true
			continue
		case "--due":
			o.due = true
			continue
		case "--json":
			o.json = true
			continue
		case "--required":
			o.required = true
			continue
		}
		if p, ok := values[v]; ok {
			if seen[v] || i+1 == len(args) || strings.HasPrefix(args[i+1], "--") {
				return nil, fmt.Errorf("%s needs one value", v)
			}
			seen[v] = true
			i++
			*p = args[i]
			continue
		}
		if v == "--older" {
			if i+1 == len(args) {
				return nil, errors.New("--older needs a value")
			}
			i++
			d, err := duration(args[i])
			if err != nil {
				return nil, err
			}
			o.older = d
			continue
		}
		if strings.HasPrefix(v, "-") {
			return nil, fmt.Errorf("unknown flag %s", v)
		}
		pos = append(pos, v)
	}
	return pos, nil
}

func validateCommand(o *options, pos []string) error {
	switch o.cmd {
	case "plan":
		return validatePlanCommand(o, pos)
	case "skip":
		return validateSkipCommand(o, pos)
	case "check":
		return validateCheckCommand(o, pos)
	}
	if o.cmd == "note" {
		if len(pos) != 2 {
			return errors.New("note needs a ticket ID and text")
		}
		if !validID(pos[0]) {
			return errors.New("ID is the ticket slug (e.g. graft-hooks) or its file name")
		}
		o.id, o.note = pos[0], pos[1]
		if strings.TrimSpace(o.note) == "" {
			return errors.New("note text must not be empty")
		}
		if o.by != "" && !nameRE.MatchString(o.by) {
			return errors.New("--by must be a name without spaces; use --by NAME")
		}
		return nil
	}
	if len(pos) > 1 {
		return errors.New("too many arguments")
	}
	if len(pos) == 1 {
		o.id = pos[0]
	}
	switch o.cmd {
	case "new":
		return validateNew(o, pos)
	case "ls":
		return validateList(o, pos)
	case "start", "wait", "review", "done", "reject", "show":
		return validateTicketAction(o, pos)
	case "lint":
		if len(pos) > 0 {
			return errors.New("lint takes no id")
		}
	case "archive":
		if len(pos) > 0 {
			return errors.New("archive takes no id")
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q", o.cmd)
	}
	return nil
}

func validatePlanCommand(o *options, pos []string) error {
	if len(pos) != 1 || !validID(pos[0]) {
		return errors.New("plan needs a ticket ID")
	}
	o.id = pos[0]
	if (o.add == "") == (o.repin == "") {
		return errors.New("plan requires exactly one of --add or --repin")
	}
	if o.repin != "" && strings.TrimSpace(o.why) == "" {
		return errors.New("--repin requires --why")
	}
	value := o.add
	if o.repin != "" {
		value = o.repin
	}
	if _, _, err := splitPlanItem(value); err != nil {
		return err
	}
	return validateActorFlag(o)
}

func validateSkipCommand(o *options, pos []string) error {
	if len(pos) != 2 || !validID(pos[0]) {
		return errors.New("skip needs a ticket ID and plan name")
	}
	o.id, o.item = pos[0], pos[1]
	if !planNameRE.MatchString(o.item) {
		return errors.New("skip needs a valid plan name")
	}
	if strings.TrimSpace(o.why) == "" {
		return errors.New("skip requires --why")
	}

	return validateActorFlag(o)
}

func validateCheckCommand(o *options, pos []string) error {
	if len(pos) != 1 || !validID(pos[0]) {
		return errors.New("check needs a ticket ID")
	}
	o.id = pos[0]
	return nil
}

func validateActorFlag(o *options) error {
	if o.by != "" && !nameRE.MatchString(o.by) {
		return errors.New("--by must be a name without spaces; use --by NAME")
	}
	return nil
}

func validateNew(o *options, pos []string) error {
	if len(pos) != 1 || !slugRE.MatchString(o.id) {
		return errors.New("new needs a lowercase slug")
	}
	if o.prio == "" {
		o.prio = "MEDIUM"
	}
	if o.prio != "HIGH" && o.prio != "MEDIUM" && o.prio != "LOW" {
		return errors.New("--prio must be HIGH, MEDIUM, or LOW")
	}
	if o.by != "" && !nameRE.MatchString(o.by) {
		return errors.New("--by must be a name without spaces; use --by NAME")
	}
	return nil
}

func validateList(o *options, pos []string) error {
	if len(pos) > 0 {
		return errors.New("ls takes no id")
	}
	if o.status != "" && !validStatus(o.status) {
		return fmt.Errorf("unknown status %q", o.status)
	}
	if o.by != "" && !nameRE.MatchString(o.by) {
		return errors.New("--by must be a name without spaces; use --by NAME")
	}
	if o.slug != "" && !slugPrefixRE.MatchString(strings.ToLower(o.slug)) {
		return errors.New("--slug must be a lowercase slug prefix")
	}
	return nil
}

func validateTicketAction(o *options, pos []string) error {
	if len(pos) != 1 || !validID(o.id) {
		return errors.New("ID is the ticket slug (e.g. graft-hooks) or its file name")
	}
	if o.by != "" && !nameRE.MatchString(o.by) {
		return errors.New("--by must be a name without spaces; use --by NAME")
	}
	if o.doer != "" && !nameRE.MatchString(o.doer) {
		return errors.New("--doer must be a name without spaces; use --doer NAME")
	}
	if o.glintDebt != "" && (!slugRE.MatchString(o.glintDebt) || o.cmd != "done") {
		return errors.New("--glint-debt needs a ticket slug and is only valid for done")
	}
	if o.cmd == "wait" && o.until == "" {
		return errors.New("wait requires --until")
	}
	if o.cmd == "done" && o.check == "" {
		return errors.New("done requires --check")
	}
	if o.cmd == "reject" && o.why == "" {
		return errors.New("reject requires --why")
	}
	if o.cmd != "show" && o.json {
		return errors.New("--json is only valid for ls and show")
	}
	return nil
}
func validID(id string) bool {
	return slugRE.MatchString(id) || ticketIDRE.MatchString(id)
}
func duration(s string) (time.Duration, error) {
	if before, ok := strings.CutSuffix(s, "d"); ok {
		n, e := strconv.ParseFloat(before, 64)
		if e != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, e := time.ParseDuration(s)
	if e != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}
func validStatus(s string) bool {
	switch s {
	case "открыт", "в работе", "ждёт", "на проверке", "готово", "отклонён":
		return true
	}
	return false
}
func run(o options, out, errout io.Writer) int {
	if code := prepareActor(&o, errout); code != 0 {
		return code
	}
	if code := readTicketText(&o, errout); code != 0 {
		return code
	}
	if (o.cmd == "ls" || o.cmd == "lint" || o.cmd == "archive") && o.root != "" {
		return runRootScan(o, out, errout)
	}
	root, dir, main, found, err := resolveTaskBase()
	if err != nil {
		return fail(errout, err)
	}
	if !found && o.cmd != "new" {
		fmt.Fprintln(errout, "tk: no docs/tasks here or above up to $HOME")
		return exitNone
	}
	if !found {
		root, err = os.Getwd()
		if err != nil {
			return fail(errout, fmt.Errorf("get current directory: %w", err))
		}
		dir = filepath.Join(root, "docs/tasks")
	}
	// Дерево кода (glint, команды плана) остаётся текущим, а тикеты, которые
	// основное дерево не отслеживает, общие для всех его worktree.
	o.baseRoot = root
	if shared, err := sharedTaskRoot(root, main); err != nil {
		return fail(errout, err)
	} else if shared != "" {
		root, dir = shared, filepath.Join(shared, "docs/tasks")
	}
	return runAtBase(root, dir, o, out, errout)
}

func runRootScan(o options, out, errout io.Writer) int {
	if o.cmd == "ls" {
		return list("", o, out, errout)
	}
	if o.cmd == "lint" {
		return lint("", o.root, out, errout)
	}
	return archive("", "", o, out, errout)
}

func runAtBase(root, dir string, o options, out, errout io.Writer) int {
	if o.baseRoot == "" {
		o.baseRoot = root
	}
	switch o.cmd {
	case "new":
		return create(root, dir, o, out, errout)
	case "ls":
		return list(root, o, out, errout)
	case "lint":
		return lint(root, "", out, errout)
	case "archive":
		return archive(root, dir, o, out, errout)
	}
	t, err := find(dir, o.id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(errout, "tk: task %q not found\n", o.id)
			return exitNone
		}
		fmt.Fprintln(errout, "tk:", err)
		return exitUsage
	}
	if o.cmd == "show" {
		if o.json {
			return showJSON(root, t, out, errout)
		}
		fmt.Fprintf(out, "%s\n%s", relative(root, t.path), wrapLines(t.body, showWidth))
		if !strings.HasSuffix(t.body, "\n") {
			fmt.Fprintln(out)
		}
		return 0
	}
	if o.cmd == "check" {
		return checkPlan(o.baseRoot, t, out)
	}
	if o.cmd == "plan" {
		if o.repin != "" {
			return repinPlanItem(t, o, out, errout)
		}
		return addPlanItem(t, o, out, errout)
	}
	if o.cmd == "skip" {
		return skipPlanItem(t, o, out, errout)
	}
	if o.cmd == "note" {
		return addNote(t, o, out, errout)
	}
	return transition(t, o, out, errout)
}

// sharedTaskRoot возвращает основное дерево main, если tk запущен из другого
// его worktree и основное дерево игнорирует docs/tasks: тогда тикеты лежат там
// одни на все ветки. Иначе — пусто, тикеты в текущем дереве.
func sharedTaskRoot(root, main string) (string, error) {
	if main == "" || main == root {
		return "", nil
	}
	// Каталог .git бывает и bare-репо (рабочего дерева у него нет): тогда тикеты в worktree.
	bare, err := exec.Command("git", "-C", main, "rev-parse", "--is-bare-repository").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse in %s: %w", main, err)
	}
	if strings.TrimSpace(string(bare)) == "true" {
		return "", nil
	}
	// Симлинк для git — файл: путь со слешем за ним git отвергает (код 128), спрашивать без слеша.
	query := "docs/tasks/"
	if info, lerr := os.Lstat(filepath.Join(main, "docs/tasks")); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		query = "docs/tasks"
	}
	err = exec.Command("git", "-C", main, "check-ignore", "-q", "--no-index", query).Run()
	if err == nil {
		return main, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	return "", fmt.Errorf("git check-ignore in %s: %w", main, err)
}

// resolveTaskBase возвращает корень дерева кода, папку тикетов в нём и основное
// дерево репо — родитель общего каталога .git, одно и то же из любого worktree;
// вне git и у репо без основного дерева (bare) — пусто.
func resolveTaskBase() (string, string, string, bool, error) {
	b, err := exec.Command("git", "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir").Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) != 2 {
			return "", "", "", false, fmt.Errorf("git rev-parse: want toplevel and common dir, got %q", b)
		}
		root, main := lines[0], ""
		if filepath.Base(lines[1]) == ".git" {
			main = filepath.Dir(lines[1])
		}
		return root, filepath.Join(root, "docs/tasks"), main, true, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", "", false, fmt.Errorf("get current directory: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", false, fmt.Errorf("get home directory: %w", err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return "", "", "", false, fmt.Errorf("resolve current directory: %w", err)
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return "", "", "", false, fmt.Errorf("resolve home directory: %w", err)
	}
	for candidate := cwd; ; candidate = filepath.Dir(candidate) {
		if _, err := os.Stat(filepath.Join(candidate, "docs/tasks")); err == nil {
			return candidate, filepath.Join(candidate, "docs/tasks"), "", true, nil
		}
		if candidate == home || candidate == filepath.Dir(candidate) {
			break
		}
	}
	if _, err := os.Stat(filepath.Join(home, "docs/tasks")); err == nil {
		return home, filepath.Join(home, "docs/tasks"), "", true, nil
	}
	return cwd, filepath.Join(cwd, "docs/tasks"), "", false, nil
}
func prepareActor(o *options, errout io.Writer) int {
	if o.cmd == "ls" || o.cmd == "archive" || o.cmd == "show" || o.cmd == "lint" || o.cmd == "check" {
		return 0
	}
	if o.by == "" {
		o.by = session()
	}
	if o.by == "" {
		fmt.Fprintln(errout, "tk: cannot determine session; pass --by NAME")
		return exitUsage
	}
	if !nameRE.MatchString(o.by) {
		fmt.Fprintln(errout, "tk: --by must be a name without spaces; pass --by NAME")
		return exitUsage
	}
	return 0
}

func readTicketText(o *options, errout io.Writer) int {
	if o.cmd != "new" || o.text == "" {
		return 0
	}
	var b []byte
	var err error
	if o.text == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(o.text)
	}
	if err != nil {
		return fail(errout, err)
	}
	o.text = string(b)
	return 0
}

func currentSession() string {
	b, e := os.ReadFile("/proc/self/cgroup")
	if e != nil {
		return ""
	}
	n, _, ok := proc.SessionScope(proc.ParseCgroup(string(b)))
	if ok {
		return n
	}
	return ""
}
func relative(root, p string) string {
	r, e := filepath.Rel(root, p)
	if e != nil {
		return p
	}
	return filepath.ToSlash(r)
}
func create(root, dir string, o options, out, errout io.Writer) int {
	if e := os.MkdirAll(dir, dirMode); e != nil {
		return fail(errout, e)
	}
	ts, e := load(dir)
	if e != nil {
		return fail(errout, e)
	}
	for i := range ts {
		t := ts[i]
		if matchesID(t, o.id) {
			fmt.Fprintf(errout, "tk: task %q already exists\n", o.id)
			return exitNone
		}
	}
	now := time.Now()
	title := o.title
	if title == "" {
		title = o.id
	}
	// Исполнителя ставит start: автор тикета часто его же и проверяет.
	body := fmt.Sprintf("# %s\n\nСтатус: открыт\n\n## Что сделать\n\n## Приёмка\n\n## Журнал\n- %s открыт — %s\n",
		title, now.Format("2006-01-02 15:04"), o.by)
	if o.text != "" {
		body = fillTicket(body, o.text)
	}
	if err := validatePlanSection(body); err != nil {
		return fail(errout, err)
	}
	p := filepath.Join(dir, now.Format("20060102-1504")+"-"+o.prio+"-"+o.id+".md")
	if e = os.WriteFile(p, []byte(body), fileMode); e != nil {
		return fail(errout, e)
	}
	fmt.Fprintln(out, relative(root, p))
	return 0
}
func fail(w io.Writer, e error) int { fmt.Fprintf(w, "tk: %v\n", e); return exitUsage }
func load(dir string) ([]task, error) {
	return loadTasks(dir, false)
}

func loadTasks(dir string, tolerateUnknownStatus bool) ([]task, error) {
	es, e := os.ReadDir(dir)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var ts []task
	for _, en := range es {
		if en.IsDir() {
			continue
		}
		m := fileRE.FindStringSubmatch(en.Name())
		if m == nil {
			continue
		}
		p := filepath.Join(dir, en.Name())
		b, e := os.ReadFile(p)
		if e != nil {
			return nil, e
		}
		info, e := en.Info()
		if e != nil {
			return nil, e
		}
		created, err := time.ParseInLocation("20060102-1504", m[1]+"-"+m[2], time.Local)
		if err != nil {
			return nil, fmt.Errorf("invalid task filename %s: %w", en.Name(), err)
		}
		s := string(b)
		st, err := status(s)
		if err != nil {
			if !tolerateUnknownStatus {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			st, _ = explicitStatus(s)
		}
		ts = append(ts, task{p, m[4], st, m[3], title(s), s, created, lastChange(s, info.ModTime())})
	}
	return ts, nil
}

// status — статус из строки «Статус: …»; строки нет (тикет старше tk) — открыт: признака закрытия у таких тикетов
// нет, а спрятать незакрытую работу хуже длинного списка. Строка с неизвестным статусом — ошибка.
func status(s string) (string, error) {
	for l := range strings.SplitSeq(s, "\n") {
		if v, ok := strings.CutPrefix(l, "Статус: "); ok {
			if !validStatus(v) {
				return "", fmt.Errorf("unknown status %q", v)
			}
			return v, nil
		}
	}
	return "открыт", nil
}
func title(s string) string {
	return cmp.Or(lineValue(s, "# "), "(no title)")
}

// lineValue returns the rest of the first line starting with prefix, or "" when there is none.
func lineValue(s, prefix string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if after, ok := strings.CutPrefix(l, prefix); ok {
			return after
		}
	}
	return ""
}
func lastChange(s string, mtime time.Time) time.Time {
	inJournal := false
	var changed time.Time
	for line := range strings.SplitSeq(s, "\n") {
		if line == "## Журнал" {
			inJournal = true
			continue
		}
		if inJournal && strings.HasPrefix(line, "## ") {
			break
		}
		if !inJournal || !strings.HasPrefix(line, "- ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if parsed, err := time.ParseInLocation("2006-01-02 15:04", fields[1]+" "+fields[2], time.Local); err == nil {
			changed = parsed
		}
	}
	if changed.IsZero() {
		return mtime
	}
	return changed
}
func find(dir, id string) (task, error) {
	ts, e := load(dir)
	if e != nil {
		return task{}, e
	}
	var got []task
	for i := range ts {
		t := ts[i]
		if matchesID(t, id) {
			got = append(got, t)
		}
	}
	if len(got) == 0 {
		return task{}, os.ErrNotExist
	}
	if len(got) > 1 {
		return task{}, fmt.Errorf("task id %q is not unique", id)
	}
	return got[0], nil
}
func matchesID(t task, id string) bool {
	base := filepath.Base(t.path)
	return t.slug == id || base == id || strings.TrimSuffix(base, ".md") == id
}

func list(root string, o options, out, errout io.Writer) int {
	all, labels, skipped, err := gatherTasks(root, o)
	if err != nil {
		return fail(errout, err)
	}
	if o.json {
		return writeListJSON(root, all, labels, o, out, errout)
	}
	if len(all) == 0 && len(skipped) == 0 {
		fmt.Fprintln(out, "no tasks")
		return 0
	}
	sort.Slice(all, func(i, j int) bool { return all[i].created.Before(all[j].created) })
	for i := range all {
		printTask(out, all[i], labels[all[i].path])
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		fmt.Fprintf(out, "skipped %d files not named YYYYMMDD-HHMM-PRIO-slug.md: %s\n", len(skipped), strings.Join(skipped, ", "))
	}
	return 0
}

func gatherTasks(root string, o options) ([]task, map[string]string, []string, error) {
	repos, err := taskRepos(root, o.root)
	if err != nil {
		return nil, nil, nil, err
	}
	labels := make(map[string]string)
	var skipped []string
	var tasks []task
	for _, repo := range repos {
		dir := filepath.Join(repo, "docs/tasks")
		loaded, err := loadTasks(dir, true)
		if err != nil {
			return nil, nil, nil, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil, err
		}
		label, err := repoLabel(o.root, repo)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") && fileRE.FindStringSubmatch(entry.Name()) == nil {
				skipped = append(skipped, entry.Name())
			}
		}
		for i := range loaded {
			t := &loaded[i]
			if includeTask(*t, o) && matchesListFilters(*t, o) {
				tasks = append(tasks, *t)
				labels[t.path] = label
			}
		}
	}
	return tasks, labels, skipped, nil
}

func repoLabel(baseDir, repo string) (string, error) {
	if baseDir == "" {
		return "", nil
	}
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve list root: %w", err)
	}
	label, err := filepath.Rel(base, repo)
	if err != nil {
		return "", fmt.Errorf("make repository label: %w", err)
	}
	if label == "." {
		label = filepath.Base(repo)
	}
	return label, nil
}

func taskRepos(root, baseDir string) ([]string, error) {
	if baseDir == "" {
		return []string{root}, nil
	}
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve repository directory %q: %w", baseDir, err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("read repository directory %q: %w", base, err)
	}
	candidates := []string{base}
	for _, entry := range entries {
		if entry.IsDir() {
			candidates = append(candidates, filepath.Join(base, entry.Name()))
		}
	}
	var repos []string
	for _, repo := range candidates {
		gitInfo, gitErr := os.Lstat(filepath.Join(repo, ".git"))
		if gitErr == nil && !gitInfo.IsDir() {
			continue
		}
		if gitErr != nil && !errors.Is(gitErr, os.ErrNotExist) {
			return nil, fmt.Errorf("check repository marker in %q: %w", repo, gitErr)
		}
		if _, err := os.Stat(filepath.Join(repo, "docs/tasks")); err == nil {
			repos = append(repos, repo)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("check task directory in %q: %w", repo, err)
		}
	}
	return repos, nil
}

func includeTask(t task, o options) bool {
	if o.status != "" {
		return t.status == o.status
	}
	return o.all || t.status == "открыт" || t.status == "в работе" || t.status == "ждёт" || t.status == "на проверке"
}

func matchesListFilters(t task, o options) bool {
	if o.slug != "" && !strings.HasPrefix(strings.ToLower(t.slug), strings.ToLower(o.slug)) {
		return false
	}
	if o.by != "" {
		who := executor(t.body)
		if who == "" {
			who = opener(t.body)
		}
		if !strings.EqualFold(who, o.by) {
			return false
		}
	}
	if o.due {
		if t.status != "ждёт" {
			return false
		}
		at, ok := waitDue(waiting(t.body), time.Now())
		if !ok || at.After(time.Now()) {
			return false
		}
	}
	return true
}

func printTask(out io.Writer, t task, label string) {
	age := time.Since(t.created)
	var ageText string
	switch {
	case age >= 24*time.Hour:
		ageText = fmt.Sprintf("%dд", int(age/(24*time.Hour)))
	case age >= time.Hour:
		ageText = fmt.Sprintf("%dч", int(age/time.Hour))
	default:
		ageText = fmt.Sprintf("%dм", int(age/time.Minute))
	}
	if label != "" {
		label += " "
	}
	wait := waiting(t.body)
	if wait != "" {
		shown := wait
		if at, ok := waitDue(wait, time.Now()); ok {
			shown = "до " + at.Local().Format("2006-01-02 15:04")
			if !at.After(time.Now()) {
				shown += " overdue"
			}
		}
		wait = " [" + shown + "]"
	}
	checkMark := ""
	if noCheck(t.body) {
		checkMark = " no-check"
	}
	fmt.Fprintf(out, "%s%s %s %s %s %s%s%s\n", label, t.slug, t.status, t.prio, ageText, t.title, wait, checkMark)
}

func waiting(s string) string {
	return lineValue(s, "Ждёт: ")
}

// waitClock — ведущее время без даты: «12:00» в строке ожидания значит день, когда её записали.
var waitClock = regexp.MustCompile(`^[0-9]{1,2}:[0-9]{2}(:[0-9]{2})?$`)

// waitAgo — «назад» (30m, 2h): timearg его понимает, сроком ожидания он не бывает.
var waitAgo = regexp.MustCompile(`^[0-9]+[smhd]$`)

// waitISO — дата и часы через T, без пояса или с Z (2026-10-31T12:00, 2026-10-31T12:00Z), не RFC3339.
var waitISO = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})T([0-9]{1,2}:[0-9]{2}(:[0-9]{2})?)(Z?)$`)

// waitDue — срок ожидания: время в начале строки «Ждёт:» (дата, дата с часами, «ДД.ММ ЧЧ:ММ», ISO с T, RFC3339,
// часы; UTC после часов — в UTC, иначе местное), дальше — текст. Без времени в начале — условие, срока нет.
// «Назад» (30m) сроком не бывает.
func waitDue(wait string, now time.Time) (time.Time, bool) {
	f := strings.Fields(wait)
	if len(f) == 0 || waitAgo.MatchString(f[0]) {
		return time.Time{}, false
	}
	// «2026-10-31 17:30: текст», «2026-11-06, после…» — знак после срока к сроку не относится.
	for i := range min(2, len(f)) {
		f[i] = strings.TrimRight(f[i], ":,;")
	}
	if t, err := time.Parse(time.RFC3339, f[0]); err == nil {
		return t, true
	}
	if m := waitISO.FindStringSubmatch(f[0]); m != nil {
		f = append([]string{m[1], m[2]}, f[1:]...)
		if m[4] == "Z" {
			f = append(f[:2], append([]string{"UTC"}, f[2:]...)...)
		}
	}
	for n := min(2, len(f)); n >= 1; n-- {
		// Без двух слов срока пробуем одно: «2026-11-06 итог» — срок одним словом, второе — заметка.
		if t, err := timearg.Parse(strings.Join(f[:n], " "), now); err == nil {
			if len(f) > n && (f[n] == "UTC" || f[n] == "Z") {
				t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
			}
			return t, true
		}
	}
	return time.Time{}, false
}

// datedWait дописывает дату к ожиданию, которое начинается с часов: иначе срок сдвигался бы на каждый новый день.
func datedWait(wait string, now time.Time) string {
	f := strings.Fields(wait)
	if len(f) > 0 && waitClock.MatchString(f[0]) {
		return now.Format("2006-01-02 ") + strings.TrimSpace(wait)
	}
	return wait
}

func setWaiting(s, value string) string {
	ls := strings.Split(s, "\n")
	for i, line := range ls {
		if strings.HasPrefix(line, "Статус: ") {
			if i+1 < len(ls) && strings.HasPrefix(ls[i+1], "Ждёт: ") {
				ls[i+1] = "Ждёт: " + value
			} else {
				ls = append(ls[:i+1], append([]string{"Ждёт: " + value}, ls[i+1:]...)...)
			}
			return strings.Join(ls, "\n")
		}
	}
	return s
}

func removeWaiting(s string) string {
	ls := strings.Split(s, "\n")
	kept := ls[:0]
	for _, line := range ls {
		if !strings.HasPrefix(line, "Ждёт: ") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func fillTicket(template, text string) string {
	sections, extra := parseTicketSections(text)
	return replaceTicketSections(template, sections, extra)
}

func parseTicketSections(text string) (map[string][]string, []string) {
	sections := make(map[string][]string)
	var current string
	var extra []string
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			current = line
			// Разделы прежнего шаблона ложатся в свои нынешние.
			if alias, ok := sectionAliases[current]; ok {
				current = alias
			}
			if !standardTicketSection(current) && current != "## Журнал" {
				extra = append(extra, line)
			}
			continue
		}
		if current == "" {
			current = "## Что сделать"
		}
		if current == "## Журнал" {
			continue
		}
		sections[current] = append(sections[current], line)
		if !standardTicketSection(current) {
			extra = append(extra, line)
		}
	}
	return sections, extra
}

var sectionAliases = map[string]string{"## Постановка": "## Вход", "## Чем проверить": "## Приёмка"}

func standardTicketSection(section string) bool {
	return section == "## Что сделать" || section == "## Приёмка" || section == "## Вход" || section == "## План"
}

func replaceTicketSections(template string, sections map[string][]string, extra []string) string {
	lines := strings.Split(template, "\n")
	hasPlan := slices.Contains(lines, "## План")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		// «Вход» в шаблоне нет: раздел из текста встаёт перед «Что сделать».
		if intake := trimBlankEdges(sections["## Вход"]); line == "## Что сделать" && len(intake) > 0 {
			out = append(out, "## Вход")
			out = append(out, intake...)
			out = append(out, "")
		}
		if standardTicketSection(line) {
			out = append(out, line)
			out = append(out, trimBlankEdges(sections[line])...)
			for i+1 < len(lines) && lines[i+1] == "" {
				i++
			}
			out = append(out, "")
			continue
		}
		if line == "## Журнал" && len(extra) > 0 {
			out = append(out, extra...)
			out = append(out, "")
		}
		if line == "## Журнал" && !hasPlan {
			if plan := trimBlankEdges(sections["## План"]); len(plan) > 0 {
				out = append(out, "## План")
				out = append(out, plan...)
				out = append(out, "")
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func trimBlankEdges(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func opener(s string) string {
	lines := journalLines(s)
	if len(lines) == 0 {
		return ""
	}
	part := strings.SplitN(lines[0], " — ", 2)
	if len(part) != 2 {
		return ""
	}
	fields := strings.Fields(part[0])
	if len(fields) == 0 || fields[len(fields)-1] != "открыт" {
		return ""
	}
	return strings.TrimSpace(part[1])
}

func journalLines(s string) []string {
	lines := make([]string, 0)
	inJournal := false
	for line := range strings.SplitSeq(s, "\n") {
		if line == "## Журнал" {
			inJournal = true
			continue
		}
		if inJournal && strings.HasPrefix(line, "## ") {
			break
		}
		if inJournal && strings.HasPrefix(line, "- ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// noCheck — раздел проверки есть, но ни один не заполнен.
func noCheck(s string) bool {
	found := false
	for _, heading := range []string{"## Приёмка", "## Чем проверить"} {
		if body, ok := sectionBody(s, heading); ok {
			if strings.TrimSpace(body) != "" {
				return false
			}
			found = true
		}
	}
	return found
}

func sectionBody(s, heading string) (string, bool) {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line != heading {
			continue
		}
		end := i + 1
		for end < len(lines) && !strings.HasPrefix(lines[end], "## ") {
			end++
		}
		return strings.Join(lines[i+1:end], "\n"), true
	}
	return "", false
}

func executor(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if after, ok := strings.CutPrefix(l, "Исполнитель: "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}
func setExecutor(s, n string) string {
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		if strings.HasPrefix(l, "Исполнитель: ") {
			ls[i] = "Исполнитель: " + n
			return strings.Join(ls, "\n")
		}
	}
	for i, l := range ls {
		if strings.HasPrefix(l, "Статус: ") {
			ls = append(ls[:i+1], append([]string{"Исполнитель: " + n}, ls[i+1:]...)...)
			return strings.Join(ls, "\n")
		}
	}
	for i, l := range ls {
		if strings.HasPrefix(l, "# ") {
			ls = append(ls[:i+1], append([]string{"Исполнитель: " + n}, ls[i+1:]...)...)
			return strings.Join(ls, "\n")
		}
	}
	return s
}
func setStatus(s, n string) string {
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		if strings.HasPrefix(l, "Статус: ") {
			ls[i] = "Статус: " + n
			return strings.Join(ls, "\n")
		}
	}
	for i, l := range ls {
		if strings.HasPrefix(l, "# ") {
			ls = append(ls[:i+1], append([]string{"", "Статус: " + n}, ls[i+1:]...)...)
			return strings.Join(ls, "\n")
		}
	}
	return "Статус: " + n + "\n" + s
}
func journal(s, line string) string {
	ls := strings.Split(s, "\n")
	idx := -1
	for i, l := range ls {
		if l == "## Журнал" {
			idx = i
		}
	}
	if idx < 0 {
		return strings.TrimRight(s, "\n") + "\n\n## Журнал\n" + line + "\n"
	}
	at := len(ls)
	for i := idx + 1; i < len(ls); i++ {
		if strings.HasPrefix(ls[i], "## ") {
			at = i
			break
		}
	}
	if at > idx+1 && ls[at-1] == "" {
		at--
	}
	ls = append(ls[:at], append([]string{line}, ls[at:]...)...)
	return strings.Join(ls, "\n")
}

func splitPlanItem(value string) (string, string, error) {
	name, command, ok := strings.Cut(value, ":")
	name, command = strings.TrimSpace(name), strings.TrimSpace(command)

	if !ok || !planNameRE.MatchString(name) || command == "" {
		return name, command, errors.New("plan item needs 'NAME: COMMAND' with a lowercase name and non-empty command")
	}
	return name, command, nil
}

func planItems(body string) []planJSON {
	var items []planJSON
	exact, alike := planHeadings(body)
	if exact > 1 {
		items = append(items, planJSON{Invalid: "plan section appears more than once"})
	}
	if alike > 0 {
		items = append(items, planJSON{Invalid: "plan heading must be exactly '## План'"})
	}
	section, ok := sectionBody(body, "## План")
	if !ok {
		// Без раздела план пуст, но обязательные пункты из журнала никуда не деваются.
		return append(items, missingRequired(body, nil)...)
	}
	seen := make(map[string]bool)
	for line := range strings.SplitSeq(section, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			items = append(items, planJSON{Cmd: strings.TrimSpace(line), Invalid: "plan lines must use '- NAME: COMMAND'"})
			continue
		}
		name, command, err := splitPlanItem(strings.TrimPrefix(line, "- "))
		if err == nil && seen[name] {
			err = fmt.Errorf("plan name %q appears more than once", name)
		}
		if err != nil {
			items = append(items, planJSON{Name: name, Cmd: command, Invalid: err.Error()})
			continue
		}
		seen[name] = true
		why, skipped := planSkipReason(body, name)
		pin, required := requiredPlan(body)[name]
		item := planJSON{Name: name, Cmd: command, Skipped: skipped, Why: why, Required: required}
		if required {
			// Пропуск обязательного пункта не действует, даже вписанный в журнал руками; команда — та, что добавлена.
			item.Skipped, item.Why = false, ""
			item.Invalid = pin.invalid(name, command)
		}
		items = append(items, item)
	}
	return append(items, missingRequired(body, seen)...)
}

// missingRequired — обязательные пункты, убранные из плана правкой текста: закрытие их не пропускает.
func missingRequired(body string, present map[string]bool) []planJSON {
	var items []planJSON
	req := requiredPlan(body)
	for _, name := range slices.Sorted(maps.Keys(req)) {
		if !present[name] {
			items = append(items, planJSON{Name: name, Cmd: req[name].cmd, Required: true, Missing: true, Invalid: fmt.Sprintf("required plan item %q is missing from ## План", name)})
		}
	}
	return items
}

// isLineEnd — любой конец строки, который редактор или Markdown покажет переносом.
func isLineEnd(r rune) bool {
	return r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029' || r == '\u0085'
}

// notInLine — символ, которому не место в однострочном вводе: управляющий (кроме табуляции) или разделитель
// строк и абзацев Unicode.
func notInLine(r rune) bool {
	return r != '\t' && unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp)
}

// requiredPin — команда обязательного пункта; conflict — строки «обязательный» расходятся в команде: tk таких
// не пишет (повтор с другой командой он отвергает), значит, тикет правлен руками, и верной команды не знает.
type requiredPin struct {
	cmd      string
	conflict bool
}

// invalid — почему пункт name с командой command не годится как обязательный; пусто — годится.
func (p requiredPin) invalid(name, command string) string {
	switch {
	case p.conflict:
		return fmt.Sprintf("required plan item %q has --required lines with different commands or not on a journal line of their own: the ticket was edited by hand", name)
	case p.cmd != "" && command != p.cmd:
		return fmt.Sprintf("required plan item %q runs %q, but it is pinned with --required to %q", name, command, p.cmd)
	}
	return ""
}

// requiredPlan — пункты, добавленные с --required, и их команды; однажды обязательный остаётся обязательным.
// Строки ищутся по всему тикету и делятся любым концом строки: правка разметки журнала (заголовок, лишний
// раздел, `\r` или U+2028 перед строкой) не должна снимать обязательность, а лишняя строка её только добавляет.
func requiredPlan(body string) map[string]requiredPin {
	req := map[string]requiredPin{}
	clean := map[string]int{}
	for _, l := range strings.FieldsFunc(body, isLineEnd) {
		if m := planAddRE.FindStringSubmatch(l); m != nil && m[2] != "" {
			name, command := m[3], m[4]
			clean[name]++
			if p, ok := req[name]; !ok {
				req[name] = requiredPin{cmd: command}
			} else if p.cmd != command {
				req[name] = requiredPin{cmd: p.cmd, conflict: true}
			}
			continue
		}
		if m := planRepinRE.FindStringSubmatch(l); m != nil {
			name, old, next := m[2], m[3], m[4]
			clean[name]++
			p, ok := req[name]
			if !ok || p.cmd != old || p.conflict {
				req[name] = requiredPin{cmd: p.cmd, conflict: true}
			} else {
				req[name] = requiredPin{cmd: next}
			}
		}
	}
	// Запись «обязательный» в любом месте текста, которая не разбирается целой строкой журнала (склеена с соседней
	// любым символом, с невидимым символом в начале), — правка руками: пункт неверен, а не снят.
	mentions := map[string]int{}
	for _, m := range requiredMentionRE.FindAllStringSubmatch(body, -1) {
		mentions[m[1]]++
	}
	for name, n := range mentions {
		if n != clean[name] {
			req[name] = requiredPin{cmd: req[name].cmd, conflict: true}
		}
	}
	return req
}

// planHeadings считает заголовки «## План» и похожие на него (регистр, пробелы, `\r`, двоеточие, иное число
// `#`): похожий план не исполнился бы, и тикет закрылся бы мимо него.
func planHeadings(body string) (exact, alike int) {
	notLetter := func(r rune) bool { return !unicode.IsLetter(r) }
	for line := range strings.SplitSeq(body, "\n") {
		if line == "## План" {
			exact++
			continue
		}
		s := strings.TrimFunc(line, func(r rune) bool { return notLetter(r) && r != '#' })
		if rest, ok := strings.CutPrefix(s, "#"); ok && strings.ToLower(strings.TrimFunc(rest, notLetter)) == "план" {
			alike++
		}
	}
	return exact, alike
}

func validatePlanSection(body string) error {
	for _, item := range planItems(body) {
		if item.Invalid != "" {
			return errors.New(item.Invalid)
		}
	}
	return nil
}

// planSkipReason — причина последнего пропуска пункта; пропуск до последнего добавления пункта с тем же
// именем к новому пункту не относится.
func planSkipReason(body, name string) (string, bool) {
	lines := journalLines(body)
	for i := len(lines) - 1; i >= 0; i-- {
		if m := planAddRE.FindStringSubmatch(lines[i]); m != nil && m[3] == name {
			return "", false
		}
		if m := planSkipRE.FindStringSubmatch(lines[i]); m != nil && m[1] == name {
			return strings.TrimSpace(m[2]), true
		}
	}
	return "", false
}

func addPlanItem(t task, o options, out, errout io.Writer) int {
	name, command, err := splitPlanItem(o.add)
	if err != nil {
		return fail(errout, err)
	}
	for _, item := range planItems(t.body) {
		if item.Name == name && !item.Missing {
			fmt.Fprintf(errout, "tk: plan name %q already exists\n", name)
			return exitUsage
		}
	}
	if pin, ok := requiredPlan(t.body)[name]; ok {
		if why := pin.invalid(name, command); why != "" {
			fmt.Fprintf(errout, "tk: %s; a required item keeps its command\n", why)
			return exitUsage
		}
	}
	line := "- " + name + ": " + command
	entry := fmt.Sprintf("- %s план — %s: добавлен пункт %s", time.Now().Format("2006-01-02 15:04"), o.by, name)
	if o.required {
		if err := checkPlanSyntax(command); err != nil {
			fmt.Fprintf(errout, "tk: required plan command has invalid bash syntax: %v\n", err)
			return exitUsage
		}
		entry = fmt.Sprintf("- %s план — %s: добавлен обязательный пункт %s: %s", time.Now().Format("2006-01-02 15:04"), o.by, name, command)
	}
	t.body = insertPlanLine(t.body, line)
	t.body = journal(t.body, entry)
	if err := os.WriteFile(t.path, []byte(t.body), fileMode); err != nil {
		return fail(errout, err)
	}
	fmt.Fprintf(out, "%s plan added\n", t.slug)
	return 0
}

func checkPlanSyntax(command string) error {
	cmd := exec.Command("/bin/bash", "--norc", "--noprofile", "-n", "-c", command)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bash -n: %w: %s", err, strings.TrimSpace(output.String()))
	}
	return nil
}

func repinPlanItem(t task, o options, out, errout io.Writer) int {
	s := session()
	if s == "" {
		fmt.Fprintln(errout, "tk: --repin requires a current session")
		return exitUsage
	}
	if strings.TrimSpace(o.why) == "" {
		fmt.Fprintln(errout, "tk: --repin requires --why")
		return exitUsage
	}
	name, command, err := splitPlanItem(o.repin)
	if err != nil {
		return fail(errout, err)
	}
	if err := checkPlanSyntax(command); err != nil {
		fmt.Fprintf(errout, "tk: repinned command has invalid bash syntax: %v\n", err)
		return exitUsage
	}
	if err := checkRepinPermission(t.body, name, s); err != nil {
		fmt.Fprintf(errout, "tk: %v\n", err)
		return exitUsage
	}
	pin, err := checkRepinPlanItem(t.body, name, command)
	if err != nil {
		fmt.Fprintf(errout, "tk: %v\n", err)
		return exitUsage
	}

	entry, err := repinJournalEntry(time.Now().Format("2006-01-02 15:04"), s, name, pin.cmd, command, o.why)
	if err != nil {
		fmt.Fprintf(errout, "tk: %v\n", err)
		return exitUsage
	}
	oldLine := "- " + name + ": " + pin.cmd
	newLine := "- " + name + ": " + command
	body, replaced := replacePlanLine(t.body, oldLine, newLine)
	if !replaced {
		fmt.Fprintf(errout, "tk: required plan item %q was edited by hand\n", name)
		return exitUsage
	}
	body = journal(body, entry)
	updatedPin := requiredPlan(body)[name]
	if updatedPin.conflict || updatedPin.cmd != command {
		fmt.Fprintf(errout, "tk: required plan item %q would be edited by hand\n", name)
		return exitUsage
	}
	if err := os.WriteFile(t.path, []byte(body), fileMode); err != nil {
		return fail(errout, err)
	}
	fmt.Fprintf(out, "%s plan repinned\n", t.slug)
	return 0
}

func checkRepinPermission(body, name, currentSession string) error {
	if owner := executor(body); owner != "" && strings.EqualFold(owner, currentSession) {
		return errors.New("the ticket executor cannot repin its required plan item")
	}
	for _, line := range strings.FieldsFunc(body, isLineEnd) {
		if m := planAddRE.FindStringSubmatch(line); m != nil && m[2] != "" && m[3] == name && strings.EqualFold(m[1], currentSession) {
			return errors.New("the author of a required pin cannot repin it")
		}
		if m := planRepinRE.FindStringSubmatch(line); m != nil && m[2] == name && strings.EqualFold(m[1], currentSession) {
			return errors.New("a previous repin author cannot repin the item again")
		}
	}
	return nil
}

func checkRepinPlanItem(body, name, command string) (requiredPin, error) {
	pin, ok := requiredPlan(body)[name]
	if !ok || pin.conflict || pin.cmd == "" {
		return requiredPin{}, fmt.Errorf("required plan item %q is missing or edited by hand", name)
	}
	if pin.cmd == command {
		return requiredPin{}, fmt.Errorf("required plan item %q already has that command", name)
	}
	present := false
	for _, item := range planItems(body) {
		if item.Name != name || !item.Required || item.Invalid != "" || item.Missing || item.Skipped {
			continue
		}
		if item.Cmd != pin.cmd {
			return requiredPin{}, fmt.Errorf("required plan item %q was edited by hand", name)
		}
		present = true
	}
	if !present {
		return requiredPin{}, fmt.Errorf("required plan item %q is missing from ## План", name)
	}
	return pin, nil
}

func repinJournalEntry(stamp, currentSession, name, oldCommand, newCommand, why string) (string, error) {
	entry := fmt.Sprintf("- %s план — %s: перезакреплён обязательный пункт %s: %s → %s — %s", stamp, currentSession, name, oldCommand, newCommand, why)
	match := planRepinRE.FindStringSubmatch(entry)
	var delimiters []string
	for _, value := range []string{currentSession, name, oldCommand, newCommand, why} {
		for _, delimiter := range []string{" → ", " — "} {
			if strings.Contains(value, delimiter) && !slices.Contains(delimiters, delimiter) {
				delimiters = append(delimiters, delimiter)
			}
		}
	}
	if match != nil && match[1] == currentSession && match[2] == name && match[3] == oldCommand && match[4] == newCommand && match[5] == why && !strings.Contains(why, " → ") {
		return entry, nil
	}
	if len(delimiters) == 0 {
		delimiters = []string{" → ", " — "}
	}
	return "", fmt.Errorf("journal delimiter %s prevents unambiguous repin record", strings.Join(delimiters, " and "))
}

func replacePlanLine(body, old, replacement string) (string, bool) {
	lines := strings.Split(body, "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		if line == "## План" {
			if start >= 0 {
				return body, false
			}
			start = i + 1
			continue
		}
		if start >= 0 && strings.HasPrefix(line, "## ") {
			end = i
			break
		}
	}
	if start < 0 {
		return body, false
	}
	found := -1
	for i := start; i < end; i++ {
		if lines[i] == old {
			if found >= 0 {
				return body, false
			}
			found = i
		}
	}
	if found < 0 {
		return body, false
	}
	lines[found] = replacement
	return strings.Join(lines, "\n"), true
}

func insertPlanLine(body, line string) string {
	lines := strings.Split(body, "\n")
	start, end := -1, len(lines)
	for i, current := range lines {
		if current == "## План" {
			start = i + 1
			continue
		}
		if start >= 0 && strings.HasPrefix(current, "## ") {
			end = i
			break
		}
	}
	if start < 0 {
		for i, current := range lines {
			if current == "## Журнал" {
				lines = append(lines[:i], append([]string{"## План", line, ""}, lines[i:]...)...)
				return strings.Join(lines, "\n")
			}
		}
		return strings.TrimRight(body, "\n") + "\n\n## План\n" + line + "\n"
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	lines = append(lines[:end], append([]string{line}, lines[end:]...)...)
	return strings.Join(lines, "\n")
}

func skipPlanItem(t task, o options, out, errout io.Writer) int {
	found := false
	for _, item := range planItems(t.body) {
		if item.Name == o.item {
			found = true
			if item.Required {
				fmt.Fprintf(errout, "tk: plan item %q is required (added with --required) and cannot be skipped; make it pass, tk done runs it\n", o.item)
				return exitUsage
			}
			break
		}
	}
	if !found {
		fmt.Fprintf(errout, "tk: plan name %q not found\n", o.item)
		return exitNone
	}
	line := fmt.Sprintf("- %s план — %s: пропущен: %s — %s", time.Now().Format("2006-01-02 15:04"), o.by, o.item, o.why)
	body := journal(t.body, line)
	if err := os.WriteFile(t.path, []byte(body), fileMode); err != nil {
		return fail(errout, err)
	}
	fmt.Fprintf(out, "%s plan skipped\n", t.slug)
	return 0
}

type planResult struct {
	item     planJSON
	exit     int
	stderr   string
	timedOut bool
	skipped  bool
}

// shellEnvDropped — переменные, которые убираются из окружения пункта плана вместе с LD_*, MALLOC_* и
// BASH_FUNC_* (см. runPlanItem): стартовые bash и остальной список UNSECURE_ENVVARS glibc без TMPDIR — тот кода
// не грузит, а командам проверки нужен.
var shellEnvDropped = []string{"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS",
	"GCONV_PATH", "GETCONF_DIR", "GLIBC_TUNABLES", "HOSTALIASES", "LOCALDOMAIN", "LOCPATH", "NIS_PATH", "NLSPATH",
	"RESOLV_HOST_CONF", "RES_OPTIONS", "TZDIR"}

func runPlanItem(root string, item planJSON) planResult {
	ctx, cancel := context.WithTimeout(context.Background(), planTimeout)
	defer cancel()
	// bash — по абсолютному пути, без файлов старта и без того в окружении, что меняет сам bash, а не команду:
	// BASH_ENV, ENV, функция BASH_FUNC_* или ~/.bashrc (bash читает его и в -c при SSH_CLIENT без SHLVL)
	// исполнились бы раньше команды и подменили бы её итог, SHELLOPTS=noexec не дал бы ей исполниться (выход 0),
	// BASHOPTS меняет опции; LD_* и прочие переменные, которые glibc сама убирает у setuid-программ (GCONV_PATH,
	// LOCPATH, …), подкладывают bash свой код
	cmd := exec.CommandContext(ctx, "/bin/bash", "--norc", "--noprofile", "-c", item.Cmd)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return strings.HasPrefix(name, "BASH_FUNC_") || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "MALLOC_") ||
			slices.Contains(shellEnvDropped, name)
	})
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// stderr — в файл, не в трубу: оставленный пунктом фоновый процесс держал бы трубу открытой, и Wait
	// ждал бы его мимо таймаута. Остаток группы после выхода bash убивается: проверка не оставляет процессов.
	stderr, err := os.CreateTemp("", "tk-plan-*")
	if err != nil {
		return planResult{item: item, exit: 1, stderr: firstLine(err.Error())}
	}
	defer os.Remove(stderr.Name())
	defer stderr.Close()
	cmd.Stderr = stderr
	err = cmd.Run()
	if cmd.Process != nil {
		// ESRCH — группа уже пуста: остатков нет.
		if killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			return planResult{item: item, exit: 1, stderr: "kill leftover processes: " + killErr.Error()}
		}
	}
	result := planResult{item: item, stderr: firstLine(readHead(stderr.Name()))}
	if err == nil {
		return result
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.timedOut = true
		result.exit = -1
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.exit = exitErr.ExitCode()
	} else {
		result.exit = 1
		result.stderr = firstLine(err.Error())
	}
	return result
}

// readHead — начало файла вывода: пункту нужна только первая строка stderr.
func readHead(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err.Error()
	}
	return string(buf[:n])
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(value), "\n")
	return strings.TrimSpace(line)
}

func runPlan(root string, t task) []planResult {
	items := planItems(t.body)
	results := make([]planResult, 0, len(items))
	for _, item := range items {
		if item.Invalid != "" {
			results = append(results, planResult{item: item, stderr: item.Invalid})
		} else if item.Skipped {
			results = append(results, planResult{item: item, skipped: true})
		} else {
			results = append(results, runPlanItem(root, item))
		}
	}
	return results
}

func checkPlan(root string, t task, out io.Writer) int {
	results := runPlan(root, t)
	if len(results) == 0 {
		fmt.Fprintln(out, "no plan")
		return 0
	}
	failed := false
	for _, result := range results {
		if result.item.Invalid != "" {
			failed = true
			fmt.Fprintf(out, "%s: invalid, %s\n", result.item.label(), result.item.Invalid)
			continue
		}
		if result.skipped {
			fmt.Fprintf(out, "%s: skipped — %s\n", result.item.Name, result.item.Why)
			continue
		}
		if result.exit == 0 {
			fmt.Fprintf(out, "%s: ok\n", result.item.Name)
			continue
		}
		failed = true
		fmt.Fprint(out, planFailure(result))
	}
	if failed {
		return exitNone
	}
	return 0
}

func planFailure(result planResult) string {
	if result.item.Invalid != "" {
		return fmt.Sprintf("%s: invalid, %s\n", result.item.label(), result.stderr)
	}
	if result.timedOut {
		return fmt.Sprintf("%s: timeout\n", result.item.Name)
	}
	if result.stderr != "" {
		return fmt.Sprintf("%s: exit %d, %s\n", result.item.Name, result.exit, result.stderr)
	}
	return fmt.Sprintf("%s: exit %d\n", result.item.Name, result.exit)
}

func executePlan(root string, t *task, errout io.Writer) int {
	var failed []planResult
	for _, result := range runPlan(root, *t) {
		if result.skipped {
			continue
		}
		if result.item.Invalid != "" || result.exit != 0 || result.timedOut {
			failed = append(failed, result)
		}
	}
	if len(failed) == 0 {
		return 0
	}
	fmt.Fprintln(errout, "tk: plan checks failed:")
	for _, result := range failed {
		fmt.Fprint(errout, strings.TrimSuffix(planFailure(result), "\n"), "\n")
	}
	return exitNone
}

type glintReport struct {
	Summary *struct {
		Total *int `json:"total"`
	} `json:"summary"`
	Issues *[]struct {
		Rule     string `json:"rule"`
		Severity string `json:"severity"`
		File     string `json:"file"`
		Line     int    `json:"line"`
	} `json:"issues"`
}

func checkGlint(root string) (glintReport, error) {
	if _, err := os.Stat(filepath.Join(root, ".glint.yaml")); errors.Is(err, os.ErrNotExist) {
		return glintReport{}, nil
	} else if err != nil {
		return glintReport{}, fmt.Errorf("inspect .glint.yaml: %w", err)
	}
	path, err := exec.LookPath("glint")
	if err != nil {
		return glintReport{}, fmt.Errorf("find glint in PATH: %w", err)
	}
	cmd := exec.Command(path, "check", ".", "-s", "low", "-o", "json")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	dec := json.NewDecoder(&stdout)
	var report glintReport
	if err := dec.Decode(&report); err != nil {
		if runErr != nil {
			return glintReport{}, fmt.Errorf("glint check failed (%w): %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return glintReport{}, fmt.Errorf("parse glint JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return glintReport{}, errors.New("parse glint JSON: trailing data")
		}
		return glintReport{}, fmt.Errorf("parse glint JSON: %w", err)
	}
	if report.Summary == nil || report.Summary.Total == nil || report.Issues == nil || *report.Summary.Total < 0 {
		return glintReport{}, errors.New("parse glint JSON: missing summary.total or issues")
	}
	if runErr != nil && len(*report.Issues) == 0 && *report.Summary.Total == 0 {
		return glintReport{}, fmt.Errorf("glint check failed: %s", strings.TrimSpace(stderr.String()))
	}
	return report, nil
}

func openDebtTicket(dir, slug string) (bool, error) {
	t, err := find(dir, slug)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch t.status {
	case "открыт", "в работе", "ждёт", "на проверке":
		return true, nil
	default:
		return false, nil
	}
}

func prepareDone(t *task, o options, errout io.Writer) (int, int) {
	if noCheck(t.body) {
		fmt.Fprintln(errout, "tk: empty Приёмка: fill it before done")
		return 0, exitNone
	}
	doer := executor(t.body)
	if o.doer != "" {
		doer = o.doer
	}
	if doer == "" || !nameRE.MatchString(doer) {
		fmt.Fprintln(errout, "tk: executor unknown or invalid; pass --doer NAME")
		return 0, exitNone
	}
	if strings.EqualFold(doer, o.by) {
		fmt.Fprintln(errout, "tk: reviewer must differ from executor")
		return 0, exitNone
	}
	// Не отказ: свежий суб-агент исполнителя — независимый проверяющий, а сессия у него родительская.
	if s := session(); s != "" && strings.EqualFold(s, doer) {
		fmt.Fprintf(errout, "tk: checked from the executor's session %s: counts only as a fresh sub-agent's check\n", s)
	}
	if o.glintDebt != "" {
		valid, err := openDebtTicket(filepath.Dir(t.path), o.glintDebt)
		if err != nil {
			return 0, fail(errout, err)
		}
		if !valid {
			fmt.Fprintf(errout, "tk: glint debt ticket %q not found or not open\n", o.glintDebt)
			return 0, exitNone
		}
	}
	findings := 0
	if o.baseRoot != "" {
		report, err := checkGlint(o.baseRoot)
		if err != nil {
			return 0, fail(errout, err)
		}
		if report.Summary != nil {
			findings = *report.Summary.Total
		}
		if findings > 0 && o.glintDebt == "" {
			reportGlintFindings(errout, findings, *report.Issues)
			return 0, exitNone
		}
	}
	if o.doer != "" {
		t.body = setExecutor(t.body, o.doer)
	}
	return findings, 0
}

func reportGlintFindings(w io.Writer, findings int, issues []struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}) {
	fmt.Fprintf(w, "tk: glint: %d findings; fix them or pass --glint-debt SLUG (an open ticket for all of them)\n", findings)
	shown := min(len(issues), 10)
	for _, issue := range issues[:shown] {
		fmt.Fprintf(w, "%s %s:%d %s\n", issue.Severity, issue.File, issue.Line, issue.Rule)
	}
	if findings > shown {
		fmt.Fprintf(w, "… and %d more\n", findings-shown)
	}
}

// checkDone — всё, что done проверяет до записи: прежние условия, план и то, что тикет не изменился за время
// проверок (они идут до минуты на пункт: новый пункт или пропуск иначе затёрся бы записью или прошёл мимо).
func checkDone(t *task, o options, errout io.Writer) (int, int) {
	loaded := t.body
	findings, code := prepareDone(t, o, errout)
	if code != 0 {
		return findings, code
	}
	if code = executePlan(o.baseRoot, t, errout); code != 0 {
		return findings, code
	}
	current, err := os.ReadFile(t.path)
	if err != nil {
		return findings, fail(errout, err)
	}
	if string(current) != loaded {
		fmt.Fprintf(errout, "tk: %s changed while plan checks ran; run done again\n", t.slug)
		return findings, exitNone
	}
	return findings, 0
}

func transition(t task, o options, out, errout io.Writer) int {
	next := map[string]string{"start": "в работе", "wait": "ждёт", "review": "на проверке", "done": "готово", "reject": "отклонён"}[o.cmd]
	glintFindings := 0
	if o.cmd == "done" {
		var code int
		if glintFindings, code = checkDone(&t, o, errout); code != 0 {
			return code
		}
	}
	if o.cmd == "start" || o.cmd == "review" && executor(t.body) == "" {
		t.body = setExecutor(t.body, o.by)
	}
	if o.cmd == "wait" {
		o.until = datedWait(o.until, time.Now())
		if _, ok := waitDue(o.until, time.Now()); !ok {
			fmt.Fprintln(errout, "tk: --until has no time at the start: a condition, tk ls --due will not show it")
		}
		t.body = setWaiting(setStatus(t.body, next), o.until)
	}
	if o.cmd == "start" {
		t.body = removeWaiting(t.body)
	}
	line := fmt.Sprintf("- %s %s — %s", time.Now().Format("2006-01-02 15:04"), next, o.by)
	if o.cmd == "done" {
		checkSession := session()
		if checkSession != "" && !strings.EqualFold(checkSession, o.by) {
			line = fmt.Sprintf("- %s %s — %s (session %s)", time.Now().Format("2006-01-02 15:04"), next, o.by, checkSession)
		}
		line += ": " + o.check
		if glintFindings > 0 && o.glintDebt != "" {
			line += fmt.Sprintf("; glint: %d findings, debt %s", glintFindings, o.glintDebt)
		}
	}
	if o.cmd == "reject" {
		line += " — " + o.why
	}
	if o.cmd == "review" && o.note != "" {
		line += " — " + o.note
	}
	if o.cmd == "wait" {
		line += " — " + o.until
	}
	t.body = journal(setStatus(t.body, next), line)
	if e := os.WriteFile(t.path, []byte(t.body), fileMode); e != nil {
		return fail(errout, e)
	}
	fmt.Fprintf(out, "%s %s\n", t.slug, next)
	return 0
}
func addNote(t task, o options, out, errout io.Writer) int {
	line := fmt.Sprintf("- %s note — %s: %s", time.Now().Format("2006-01-02 15:04"), o.by, o.note)
	body := journal(t.body, line)
	if err := os.WriteFile(t.path, []byte(body), fileMode); err != nil {
		return fail(errout, err)
	}
	fmt.Fprintf(out, "%s noted\n", t.slug)
	return 0
}

type taskJSON struct {
	Repo           string     `json:"repo,omitempty"`
	Slug           string     `json:"slug"`
	Status         string     `json:"status"`
	Prio           string     `json:"prio"`
	Title          string     `json:"title"`
	Created        string     `json:"created"`
	Changed        string     `json:"changed"`
	Path           string     `json:"path"`
	Executor       string     `json:"executor"`
	Waits          string     `json:"waits"`
	Overdue        bool       `json:"overdue"`
	NoCheck        bool       `json:"no_check"`
	Check          string     `json:"check"`
	Checker        string     `json:"checker"`
	CheckerSession string     `json:"checker_session"`
	Journal        []string   `json:"journal"`
	Plan           []planJSON `json:"plan,omitempty"`
}

type planJSON struct {
	Name     string `json:"name"`
	Cmd      string `json:"cmd"`
	Skipped  bool   `json:"skipped"`
	Why      string `json:"why"`
	Required bool   `json:"required,omitempty"`
	Missing  bool   `json:"missing,omitempty"`
	Invalid  string `json:"invalid,omitempty"`
}

// label — имя пункта в отчёте; у строки, из которой имя не прочлось, — сама строка.
func (p planJSON) label() string {
	if p.Name != "" {
		return p.Name
	}
	return strconv.Quote(p.Cmd)
}

func taskObject(root string, t task, repo string, details bool) taskJSON {
	waits := waiting(t.body)
	overdue := false
	if t.status == "ждёт" {
		if at, ok := waitDue(waits, time.Now()); ok && !at.After(time.Now()) {
			overdue = true
		}
	}
	obj := taskJSON{Repo: repo, Slug: t.slug, Status: t.status, Prio: t.prio, Title: t.title,
		Created: t.created.Format(time.RFC3339), Changed: t.changed.Format(time.RFC3339),
		Path: relative(root, t.path), Executor: executor(t.body), Waits: waits, Overdue: overdue, NoCheck: noCheck(t.body)}
	if details {
		obj.Journal = journalLines(t.body)
	}
	obj.Checker, obj.Check, obj.CheckerSession = completion(t.body)
	obj.Plan = planItems(t.body)
	return obj
}

// completion — проверяющий и проверка последней строки «готово» журнала: тикет могли открыть заново.
func completion(s string) (checker, check, checkerSession string) {
	for _, line := range journalLines(s) {
		_, after, ok := strings.Cut(line, " готово — ")
		if !ok {
			continue
		}
		if who, what, ok := strings.Cut(after, ": "); ok {
			checker, check, checkerSession = completionParts(who, what)
		} else if who, what, ok := strings.Cut(after, " — "); ok {
			checker, check, checkerSession = completionParts(who, what)
		}
	}
	return checker, check, checkerSession
}

func completionParts(who, check string) (string, string, string) {
	checker := strings.TrimSpace(who)
	const marker = " (session "
	if i := strings.LastIndex(checker, marker); i >= 0 && strings.HasSuffix(checker, ")") {
		session := strings.TrimSuffix(checker[i+len(marker):], ")")
		checker = strings.TrimSpace(checker[:i])
		return checker, strings.TrimSpace(check), session
	}
	return checker, strings.TrimSpace(check), ""
}

func writeListJSON(root string, ts []task, labels map[string]string, o options, out, errout io.Writer) int {
	if o.root != "" {
		var err error
		root, err = filepath.Abs(o.root)
		if err != nil {
			return fail(errout, err)
		}
	}
	objects := make([]taskJSON, 0, len(ts))
	for i := range ts {
		t := &ts[i]
		repo := ""
		if o.root != "" {
			repo = labels[t.path]
		}
		objects = append(objects, taskObject(root, *t, repo, false))
	}
	if err := json.NewEncoder(out).Encode(objects); err != nil {
		return fail(errout, err)
	}
	return 0
}

func showJSON(root string, t task, out, errout io.Writer) int {
	obj := taskObject(root, t, "", true)
	if err := json.NewEncoder(out).Encode(obj); err != nil {
		return fail(errout, err)
	}
	return 0
}

func lint(root, scanDir string, out, errout io.Writer) int {
	repos, err := taskRepos(root, scanDir)
	if err != nil {
		return fail(errout, err)
	}
	var problems []string
	for _, repo := range repos {
		entries, err := os.ReadDir(filepath.Join(repo, "docs/tasks"))
		if err != nil {
			return fail(errout, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			path := filepath.Join(repo, "docs/tasks", entry.Name())
			shown := lintPath(root, scanDir, repo, entry.Name(), path)
			issues, err := lintTicket(path, shown)
			if err != nil {
				return fail(errout, err)
			}
			problems = append(problems, issues...)
		}
	}
	if len(problems) == 0 {
		fmt.Fprintln(out, "ok")
		return 0
	}
	for _, problem := range problems {
		fmt.Fprintln(out, problem)
	}
	return exitNone
}

func lintPath(root, scanDir, repo, name, path string) string {
	if scanDir != "" {
		return filepath.ToSlash(filepath.Join(filepath.Base(repo), "docs/tasks", name))
	}
	return relative(root, path)
}

func lintTicket(path, shown string) ([]string, error) {
	bodyBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ticket %s: %w", path, err)
	}
	body := string(bodyBytes)
	var issues []string
	if fileRE.FindStringSubmatch(filepath.Base(path)) == nil {
		issues = append(issues, shown+": invalid file name")
	}
	statusValue, hasStatus := explicitStatus(body)
	if hasStatus && !validStatus(statusValue) {
		issues = append(issues, shown+": unknown status "+statusValue)
	}
	if !hasStatus {
		return issues, nil
	}
	_, acceptance := sectionBody(body, "## Приёмка")
	_, oldCheck := sectionBody(body, "## Чем проверить")
	_, hasJournal := sectionBody(body, "## Журнал")
	if !acceptance && !oldCheck {
		issues = append(issues, shown+": missing Приёмка or Чем проверить section")
	}
	if !hasJournal {
		issues = append(issues, shown+": missing Журнал section")
	}
	if noCheck(body) {
		issues = append(issues, shown+": empty Приёмка")
	}
	if err := validatePlanSection(body); err != nil {
		issues = append(issues, shown+": "+err.Error())
	}
	if statusValue == "готово" {
		checker, check, _ := completion(body)
		if checker == "" || check == "" {
			issues = append(issues, shown+": готово without journal verification")
		} else if doer := executor(body); doer != "" && strings.EqualFold(checker, doer) {
			issues = append(issues, shown+": checker equals executor")
		}
	}
	return issues, nil
}

func explicitStatus(s string) (string, bool) {
	for line := range strings.SplitSeq(s, "\n") {
		if value, ok := strings.CutPrefix(line, "Статус:"); ok {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// archiveDir — куда tk archive переносит закрытые тикеты репо. docs/tasks-симлинк
// ведёт в отдельное хранилище: архив — внутри него (docs/tasks/archive), иначе
// он лёг бы в само репо мимо хранилища.
func archiveDir(repo string) (string, error) {
	info, err := os.Lstat(filepath.Join(repo, "docs/tasks"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return filepath.Join(repo, "docs/tasks", "archive"), nil
	}
	return filepath.Join(repo, "docs/tasks-archive"), nil
}

func archive(root, _ string, o options, out, errout io.Writer) int {
	repos, err := taskRepos(root, o.root)
	if err != nil {
		return fail(errout, err)
	}
	base := root
	if o.root != "" {
		base, err = filepath.Abs(o.root)
		if err != nil {
			return fail(errout, err)
		}
	}
	cut := time.Now().Add(-o.older)
	var moved []string
	for _, repo := range repos {
		taskDir := filepath.Join(repo, "docs/tasks")
		ts, err := load(taskDir)
		if err != nil {
			return fail(errout, err)
		}
		dstDir, err := archiveDir(repo)
		if err != nil {
			return fail(errout, err)
		}
		for i := range ts {
			t := ts[i]
			if (t.status != "готово" && t.status != "отклонён") || t.changed.After(cut) {
				continue
			}
			if err = os.MkdirAll(dstDir, dirMode); err != nil {
				return fail(errout, err)
			}
			dst := filepath.Join(dstDir, filepath.Base(t.path))
			if _, err = os.Stat(dst); err == nil {
				return fail(errout, fmt.Errorf("archive destination exists: %s", dst))
			} else if !errors.Is(err, os.ErrNotExist) {
				return fail(errout, err)
			}
			if err = os.Rename(t.path, dst); err != nil {
				return fail(errout, err)
			}
			moved = append(moved, relative(base, dst))
		}
	}
	fmt.Fprintf(out, "archived %d\n", len(moved))
	for _, p := range moved {
		fmt.Fprintln(out, p)
	}
	return 0
}
