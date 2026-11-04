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
  tk new SLUG [--prio CRIT|HIGH|MED|MEDIUM|LOW] [--diff DO|HUNT|ARCH] [--why TEXT] [--to PROJECT] [--back SESSION] [--title TEXT] [--by NAME] [--journal] [--text FILE|-] [--blocks ID]
  tk set ID [--prio P] [--diff D] [--why TEXT] [--by NAME] [--journal] [--blocks ID] [--unblocks ID]
  tk ls [--all] [--status STATUS] [--slug PREFIX] [--root DIR] [--by NAME] [--due] [--json]
  tk note ID TEXT [--by NAME]
  tk plan ID --add 'NAME: COMMAND' [--required] [--by NAME]
  tk plan ID --repin 'NAME: COMMAND' --why TEXT [--by NAME]
  tk skip ID NAME --why TEXT [--by NAME]
  tk check ID [--timeout D]
  tk lint [--root DIR]
  tk start ID [--by NAME] [--prio P] [--diff D] [--why TEXT] [--take]
  tk next [--by NAME] [--json]
  tk external --json   the project's whole task source, no gates and no local tickets
  tk transfer ID [--parent KEY] [--by NAME]
  tk wait ID --until TEXT [--by NAME]
  tk review ID [--by NAME] [--note TEXT]
  tk done ID --check TEXT [--by NAME] [--doer NAME] [--glint-debt SLUG] [--timeout D]   ends with the next ticket, as tk next chooses it for the executor
  tk reject ID --why TEXT [--by NAME]
  tk show ID [--json]
  tk archive [--older DURATION] [--root DIR]

Flags:
  --prio P       new/set/start: CRIT, HIGH, MED, MEDIUM, or LOW (new default MED)
  --diff D       new/set/start: difficulty DO, HUNT, or ARCH
  --why TEXT     new: required for CRIT/HIGH; set/start: required when promoting to CRIT/HIGH; reject: reason
  --to PROJECT   new: create in PROJECT/docs/tasks; absolute path or project name
  --journal      new/set: mark the ticket as a journal entry, hidden from next unless a wait is due
  --parent KEY   transfer: parent item key passed to the transfer command
  --blocks ID    new/set: ticket this ticket blocks (repeatable)
  --unblocks ID  set: remove a blocking relation (repeatable)
  --back NAME    new: requesting session
  --title TEXT   new: ticket title (default: slug)
  --by NAME      actor name (default: current session); ls: show tickets assigned to NAME
  --text FILE|-  new: fill ticket sections from FILE, or standard input with -
  --all          ls: include closed tickets

  --due          ls: show waiting tickets whose parsed deadline has passed
  --json         ls: output JSON array; show: ticket and journal JSON; next: top ticket JSON
  --status S     ls: show only status S
  --slug PREFIX  ls: show only slugs starting with PREFIX (case-insensitive)
  --root DIR     ls/lint/archive: scan DIR and its first-level repositories
  --note TEXT    review: add a review note to the journal
  --check TEXT   done: describe independent verification (required)
  --doer NAME    done: specify the executor of an older ticket
  --glint-debt S done: link findings to an open ticket when glint reports them
  --why TEXT     reject: why declined; skip: why item skipped (required)
  --take         start: take a ticket assigned to another session
  --add TEXT     plan: add a named command to the ticket plan (required)
  --required     plan: the item cannot be skipped (tk skip refuses, exit 2), dropped from the plan or given another command; command syntax is checked with bash -n
  --repin TEXT   plan: replace a required command from another session; current and previous executors and pin authors are refused; the session and old/new commands are journaled
  --until TEXT   wait: time or condition to wait for (required)
  --older D      archive: closed tickets older than D (default 14d; supports Go durations and d)
  --timeout D    check, done: limit for each plan command (default 8m)

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
var fileRE = regexp.MustCompile(`^([0-9]{8})-([0-9]{4})-([A-Z]+)-(?:(DO|HUNT|ARCH)-)?([a-z0-9]+(?:-[a-z0-9]+)*)\.md$`)
var nameRE = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._:@-]*$`)
var ticketIDRE = regexp.MustCompile(`^[0-9]{8}-[0-9]{4}-[A-Z]+-(?:(?:DO|HUNT|ARCH)-)?[a-z0-9]+(?:-[a-z0-9]+)*(?:\.md)?$`)
var planNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

const mandatoryWhySince = "20261104-0000" // с этого штампа причина обязательна

// planSkipRE — строка журнала, которую пишет tk skip (и прежняя форма без автора), целиком с начала:
// «пропущен:» внутри заметки или причины другого пункта пропуском не считается.
var planSkipRE = regexp.MustCompile(`^- \d{4}-\d{2}-\d{2} \d{2}:\d{2} (?:план — \S+: )?пропущен: ([a-z0-9-]+) — (.*)$`)

// planAddRE — строка журнала tk plan --add; «обязательный» — с --required, тогда после имени — команда пункта.
var planAddRE = regexp.MustCompile(`^- \d{4}-\d{2}-\d{2} \d{2}:\d{2} план — (\S+): добавлен (обязательный )?пункт ([a-z0-9-]+)(?:: (.+))?$`)
var planRepinRE = regexp.MustCompile(`^- \d{4}-\d{2}-\d{2} \d{2}:\d{2} план — (\S+): перезакреплён обязательный пункт ([a-z0-9-]+): (.+) → (.+) — (.+)$`)
var executorHistoryRE = regexp.MustCompile(`^- \d{4}-\d{2}-\d{2} \d{2}:\d{2} (?:в работе|на проверке) — (\S+)(?:$| — )`)

// requiredMentionRE — запись «обязательный» где угодно в тексте, без привязки к строке: сверка с целыми строками
// журнала ловит запись, спрятанную склейкой или невидимым символом.
var requiredMentionRE = regexp.MustCompile(`план — \S+: (?:добавлен|перезакреплён) обязательный пункт ([a-z0-9-]+)`)

var errHelp = errors.New("help")
var session = currentSession

// planTimeout — предел одного пункта плана по умолчанию: проверка вроде гейта проекта ждёт очереди тяжёлых задач
// и идёт минуты, а вызов агента на переднем плане длится не дольше 10 минут. Свой предел — check/done --timeout.
var planTimeout = 8 * time.Minute

type options struct {
	cmd, id, prio, diff, title, by, note, check, doer, why, status, slug, root, until, text, glintDebt, baseRoot, mainRoot, add, repin, item, to, back, parent string
	all, due, json, required, take, journal                                                                                                                    bool
	older, timeout                                                                                                                                             time.Duration
	blocks, unblocks                                                                                                                                           []string
}

// planLimit — предел пункта плана: --timeout или умолчание.
func (o options) planLimit() time.Duration {
	if o.timeout > 0 {
		return o.timeout
	}
	return planTimeout
}

type task struct {
	path, slug, status, prio, diff, title, body string
	created, changed                            time.Time
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
	if o.timeout != 0 && o.cmd != "check" && o.cmd != "done" {
		return o, errors.New("--timeout is a flag of tk check and tk done")
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
	for _, f := range []struct{ flag, value string }{{"note", o.note}, {"--title", o.title}, {"--check", o.check}, {"--why", o.why}, {"--back", o.back}, {"--until", o.until}, {"--add", o.add}, {"--repin", o.repin}} {
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
	values := map[string]*string{"--prio": &o.prio, "--diff": &o.diff, "--to": &o.to, "--back": &o.back, "--title": &o.title, "--by": &o.by, "--note": &o.note, "--check": &o.check, "--doer": &o.doer, "--why": &o.why, "--status": &o.status, "--slug": &o.slug, "--root": &o.root, "--until": &o.until, "--text": &o.text, "--glint-debt": &o.glintDebt, "--add": &o.add, "--repin": &o.repin, "--parent": &o.parent}
	flags := map[string]func(){"--all": func() { o.all = true }, "--due": func() { o.due = true }, "--json": func() { o.json = true }, "--required": func() { o.required = true }, "--take": func() { o.take = true }, "--journal": func() { o.journal = true }}
	var pos []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		v := args[i]
		if v == "--blocks" || v == "--unblocks" {
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "--") || strings.TrimSpace(args[i+1]) == "" {
				return nil, fmt.Errorf("%s needs a value", v)
			}
			i++
			if v == "--blocks" {
				o.blocks = append(o.blocks, args[i])
			} else {
				o.unblocks = append(o.unblocks, args[i])
			}
			continue
		}
		if set, ok := flags[v]; ok {
			set()
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
		if v == "--older" || v == "--timeout" {
			if i+1 == len(args) {
				return nil, fmt.Errorf("%s needs a value", v)
			}
			i++
			d, err := duration(args[i])
			if err != nil {
				return nil, err
			}
			if v == "--older" {
				o.older = d
			} else if d <= 0 {
				return nil, errors.New("--timeout must be positive")
			} else {
				o.timeout = d
			}
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
	if err := validateCommandFlags(o); err != nil {
		return err
	}
	switch o.cmd {
	case "plan":
		return validatePlanCommand(o, pos)
	case "skip":
		return validateSkipCommand(o, pos)
	case "check":
		return validateCheckCommand(o, pos)
	case "note":
		return validateNoteCommand(o, pos)
	case "transfer":
		return validateTransfer(o, pos)
	default:
		return validateSimpleCommand(o, pos)
	}
}

func validateCommandFlags(o *options) error {
	checks := []func(*options) error{validateTakeFlag, validateDestinationFlags, validateJournalFlag, validatePriorityFlags, validateWhyFlag, validateJSONFlag}
	for _, check := range checks {
		if err := check(o); err != nil {
			return err
		}
	}
	if (len(o.blocks) > 0 && o.cmd != "new" && o.cmd != "set") || (len(o.unblocks) > 0 && o.cmd != "set") {
		return errors.New("--blocks is for new/set; --unblocks is for set")
	}
	return nil
}

func validateTakeFlag(o *options) error {
	if o.take && o.cmd != "start" {
		return errors.New("--take: flag only for start")
	}
	return nil
}

func validateDestinationFlags(o *options) error {
	if (o.to != "" || o.back != "") && o.cmd != "new" {
		return errors.New("--to/--back: flag only for new")
	}
	return nil
}

func validateJournalFlag(o *options) error {
	if o.journal && o.cmd != "new" && o.cmd != "set" {
		return errors.New("--journal: flag only for new or set")
	}
	if o.parent != "" && o.cmd != "transfer" {
		return errors.New("--parent: flag only for transfer")
	}
	return nil
}

func validatePriorityFlags(o *options) error {
	if (o.prio != "" || o.diff != "") && o.cmd != "new" && o.cmd != "set" && o.cmd != "start" {
		return errors.New("--prio/--diff: flag only for new, set, or start")
	}
	return nil
}

func validateWhyFlag(o *options) error {
	if o.why == "" || o.cmd == "new" || o.cmd == "set" || o.cmd == "start" || o.cmd == "reject" || o.cmd == "skip" || o.cmd == "plan" && o.repin != "" {
		return nil
	}
	return errors.New("--why: flag only for new, set, start, reject, skip, or plan --repin")
}

func validateJSONFlag(o *options) error {
	if o.json && o.cmd != "ls" && o.cmd != "show" && o.cmd != "next" && o.cmd != "external" {
		return errors.New("--json: flag only for ls, show, next, or external")
	}
	return nil
}

func validateNoteCommand(o *options, pos []string) error {
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
	return validateActorFlag(o)
}

func validateTransfer(o *options, pos []string) error {
	if len(pos) != 1 || !validID(pos[0]) {
		return errors.New("transfer needs a ticket ID")
	}
	o.id = pos[0]
	return validateActorFlag(o)
}

func validateSimpleCommand(o *options, pos []string) error {
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
	case "start", "wait", "review", "done", "reject", "show", "set":
		return validateTicketAction(o, pos)
	case "next":
		if len(pos) != 0 {
			return errors.New("next takes no id")
		}
		return validateActorFlag(o)
	case "external":
		if len(pos) != 0 {
			return errors.New("external takes no id")
		}
		if !o.json {
			return errors.New("external: --json is required")
		}
		return nil
	case "lint":
		if len(pos) > 0 {
			return errors.New("lint takes no id")
		}
		return nil
	case "archive":
		if len(pos) > 0 {
			return errors.New("archive takes no id")
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q", o.cmd)
	}
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
		o.prio = "MED"
	}
	o.prio = normalizePrio(o.prio)
	if !validPrio(o.prio) {
		return errors.New("--prio must be CRIT, HIGH, MED, MEDIUM, or LOW")
	}
	if !validDiff(o.diff) {
		return errors.New("--diff must be DO, HUNT, or ARCH")
	}
	if requiresWhyForPromotion("LOW", o.prio) && strings.TrimSpace(o.why) == "" {
		return errors.New("--prio CRIT/HIGH requires --why")
	}
	if o.by != "" && !nameRE.MatchString(o.by) {
		return errors.New("--by must be a name without spaces; use --by NAME")
	}
	if o.back != "" && !nameRE.MatchString(o.back) {
		return errors.New("--back must be a name without spaces; use --back NAME")
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
	if err := validateActionIdentity(o); err != nil {
		return err
	}
	return validateActionOptions(o)
}

func validateActionIdentity(o *options) error {
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
	return nil
}

func validateActionOptions(o *options) error {
	if o.cmd == "set" || o.cmd == "start" {
		if err := validatePriorityOptions(o); err != nil {
			return err
		}
	}

	if o.cmd == "done" && o.check == "" {
		return errors.New("done requires --check")
	}
	if o.cmd == "reject" && o.why == "" {
		return errors.New("reject requires --why")
	}

	return nil
}

func validatePriorityOptions(o *options) error {
	if o.prio != "" {
		o.prio = normalizePrio(o.prio)
		if !validPrio(o.prio) {
			return errors.New("--prio must be CRIT, HIGH, MED, MEDIUM, or LOW")
		}
	}
	if !validDiff(o.diff) {
		return errors.New("--diff must be DO, HUNT, or ARCH")
	}
	if o.cmd == "set" && o.prio == "" && o.diff == "" && o.why == "" && !o.journal && len(o.blocks) == 0 && len(o.unblocks) == 0 {
		return errors.New("set needs --prio, --diff, --why, or --journal")
	}
	return nil
}
func validID(id string) bool {
	return slugRE.MatchString(id) || ticketIDRE.MatchString(id)
}
func normalizePrio(p string) string {
	if p == "MEDIUM" {
		return "MED"
	}
	return p
}
func validPrio(p string) bool { return p == "CRIT" || p == "HIGH" || p == "MED" || p == "LOW" }

func requiresWhyForPromotion(oldPrio, newPrio string) bool {
	newPrio = normalizePrio(newPrio)
	if newPrio != "CRIT" && newPrio != "HIGH" {
		return false
	}
	return prioRank(newPrio) < prioRank(oldPrio)
}

func prioRank(prio string) int {
	switch normalizePrio(prio) {
	case "CRIT":
		return 0
	case "HIGH":
		return 1
	case "MED":
		return 2
	default:
		return 3
	}
}
func validDiff(d string) bool { return d == "" || d == "DO" || d == "HUNT" || d == "ARCH" }
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
	if o.cmd == "external" {
		return externalAtBase(root, main, found, out, errout)
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
	o.mainRoot = main
	if shared, err := sharedTaskRoot(root, main); err != nil {
		return fail(errout, err)
	} else if shared != "" {
		root, dir = shared, filepath.Join(shared, "docs/tasks")
	}
	return runAtBase(root, dir, o, out, errout)
}

// externalAtBase needs no ticket folder: a project without docs/tasks can have
// a source. Worktrees read it for the main tree, as tk next does.
func externalAtBase(root, main string, found bool, out, errout io.Writer) int {
	project := root
	if main != "" {
		project = main
	}
	if !found {
		cwd, err := os.Getwd()
		if err != nil {
			return fail(errout, fmt.Errorf("get current directory: %w", err))
		}
		project = cwd
	}
	return listExternal(project, out, errout)
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
	if code, handled := runBaseCommand(root, dir, o, out, errout); handled {
		return code
	}
	return runLockedCommand(root, dir, o, out, errout)
}

func runBaseCommand(root, dir string, o options, out, errout io.Writer) (int, bool) {
	switch o.cmd {
	case "new":
		return createAtBase(root, dir, o, out, errout), true
	case "ls":
		return list(root, o, out, errout), true
	case "lint":
		return lint(root, "", out, errout), true
	case "archive":
		return archive(root, dir, o, out, errout), true
	case "next":
		return nextTask(root, o.mainRoot, dir, o, out, errout), true
	default:
		return 0, false
	}
}

func createAtBase(root, dir string, o options, out, errout io.Writer) int {
	main := o.mainRoot
	if o.to != "" {
		target, err := destinationProject(o.to)
		if err != nil {
			return fail(errout, err)
		}
		root, dir = target, filepath.Join(target, "docs/tasks")
		main = ""
	}
	if o.to != "" {
		var err error
		main, err = gitMainRoot(root)
		if err != nil {
			return fail(errout, err)
		}
	}
	if o.to != "" {
		// --to names a project: a worktree path leads to its main tree whatever the tickets are.
		if main != "" && main != root {
			bare, err := bareRepository(main)
			if err != nil {
				return fail(errout, err)
			}
			if !bare {
				root, dir = main, filepath.Join(main, "docs/tasks")
			}
		}
	} else if main != "" && o.baseRoot != "" {
		shared, err := sharedTaskRoot(o.baseRoot, main)
		if err != nil {
			return fail(errout, err)
		}
		if shared != "" {
			root, dir = shared, filepath.Join(shared, "docs/tasks")
		}
	}
	if code := validateNewBlockTargets(root, main, o, errout); code != 0 {
		return code
	}
	if err := prepareTaskDirectory(root, main); err != nil {
		return fail(errout, err)
	}
	return createAtBaseTask(root, dir, main, o, out, errout)
}

func validateNewBlockTargets(root, main string, o options, errout io.Writer) int {
	for _, ref := range o.blocks {
		probe := o
		probe.baseRoot = root
		probe.mainRoot = main
		target, err := resolveBlockTarget(probe, ref)
		if err != nil {
			return fail(errout, err)
		}
		if target.status == "готово" || target.status == "отклонён" {
			return fail(errout, fmt.Errorf("target %s is closed", ref))
		}
		target.ref, err = ticketRef(root, target.root, target.id)
		if err != nil {
			return fail(errout, err)
		}
		target.back, err = ticketRef(target.root, root, o.id)
		if err != nil {
			return fail(errout, err)
		}
		if _, err := addHeaderLink(target.body, blockedPrefix, target.back); err != nil {
			return fail(errout, fmt.Errorf("target %s: %w", ref, err))
		}
	}
	return 0
}

func createAtBaseTask(root, dir, main string, o options, out, errout io.Writer) int {
	blocks := o.blocks
	o.blocks = nil
	if len(blocks) == 0 {
		return create(root, dir, o, out, errout)
	}
	// The name and the hint wait for the links: a link can raise the new ticket and rename it.
	var createdErr bytes.Buffer
	if code := create(root, dir, o, io.Discard, &createdErr); code != 0 {
		_, _ = createdErr.WriteTo(errout)
		return code
	}
	t, err := find(dir, o.id)
	if err != nil {
		return fail(errout, err)
	}
	o.blocks = blocks
	o.baseRoot = root
	o.mainRoot = main
	return finishCreateAtBase(root, dir, t, o, out, errout, blocks)
}

func finishCreateAtBase(root, dir string, t task, o options, out, errout io.Writer, blocks []string) int {
	var relationOutput bytes.Buffer
	code := attachBlocks(t, o, &relationOutput, errout, blocks)
	fresh, err := readTaskLocked(dir, o.id)
	if err != nil {
		return fail(errout, err)
	}
	createdPath := relative(root, fresh.path)
	if o.to != "" {
		createdPath, err = filepath.Abs(fresh.path)
		if err != nil {
			return fail(errout, err)
		}
	}
	fmt.Fprintln(out, createdPath)
	if code == 0 {
		fmt.Fprintln(errout, hintAfterNew(o, createdPath, fresh.body))
	}
	_, _ = relationOutput.WriteTo(out)
	return code
}

func gitMainRoot(target string) (string, error) {
	b, err := exec.Command("git", "-C", target, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 128 {
			return "", nil
		}
		return "", fmt.Errorf("git rev-parse in %s: %w", target, err)
	}
	common := strings.TrimSpace(string(b))
	if filepath.Base(common) != ".git" {
		return "", nil
	}
	return filepath.Dir(common), nil
}

func destinationProject(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine destination project: %w", err)
	}
	target := name
	if !filepath.IsAbs(target) {
		gitPath, workPath := filepath.Join(home, "git", name), filepath.Join(home, "work", name)
		gi, ge := os.Stat(gitPath)
		wi, we := os.Stat(workPath)
		if ge != nil && !errors.Is(ge, os.ErrNotExist) {
			return "", fmt.Errorf("inspect project %s: %w", gitPath, ge)
		}
		if we != nil && !errors.Is(we, os.ErrNotExist) {
			return "", fmt.Errorf("inspect project %s: %w", workPath, we)
		}
		gok, wok := ge == nil && gi.IsDir(), we == nil && wi.IsDir()
		if gok && wok {
			return "", fmt.Errorf("project %q exists in both %s and %s; pass an absolute path", name, gitPath, workPath)
		}
		if !gok && !wok {
			return "", fmt.Errorf("project %q not found at %s or %s", name, gitPath, workPath)
		}
		if wok {
			target = workPath
		} else {
			target = gitPath
		}
	}
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("project %q does not exist", name)
	}
	if err != nil {
		return "", fmt.Errorf("inspect project %q: %w", name, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project %q is not a directory", name)
	}
	return target, nil
}

func runLockedCommand(root, dir string, o options, out, errout io.Writer) int {
	release, err := lockForCommand(dir, o.cmd)
	if err != nil {
		return fail(errout, err)
	}
	if o.cmd == "transfer" {
		return runTransferCommand(root, dir, o, out, errout, release)
	}
	t, err := find(dir, o.id)
	if err != nil {
		release()
		return reportFindError(err, o.id, errout)
	}
	if o.cmd == "set" || o.cmd == "start" {
		var code int
		t, release, code = prepareSetOrStart(dir, t, o, errout, release)
		if code != 0 {
			return code
		}
	}
	// With new links the name is printed last known: a link can raise and rename the ticket.
	namesLater := o.cmd == "set" && len(o.blocks) > 0
	commandOut, relationOut := out, out
	var relations bytes.Buffer
	if namesLater {
		commandOut, relationOut = io.Discard, &relations
	}
	code := runFoundTask(root, t, o, commandOut, errout)
	release()
	return finishLockedCommand(code, dir, t, o, out, errout, relationOut, &relations, namesLater)
}

func prepareSetOrStart(dir string, t task, o options, errout io.Writer, release func()) (task, func(), int) {
	release()
	blockOptions := o
	blockOptions.baseRoot = taskProject(t.path)
	if len(o.blocks) > 0 {
		if _, code := validateBlockAttachments(t, blockOptions, o.blocks, errout); code != 0 {
			return task{}, nil, code
		}
	}
	for _, ref := range o.unblocks {
		if _, _, _, _, err := resolveBlockLocation(blockOptions, ref); err != nil {
			return task{}, nil, fail(errout, err)
		}
	}
	locked, err := lockForCommand(dir, o.cmd)
	if err != nil {
		return task{}, nil, fail(errout, err)
	}
	t, err = find(dir, o.id)
	if err != nil {
		locked()
		return task{}, nil, reportFindError(err, o.id, errout)
	}
	// A priority raise writes the blocked ticket before waiting here, so one lock at a time is enough.
	if code := validatePriorityFloor(t, blockOptions, errout); code != 0 {
		locked()
		return task{}, nil, code
	}
	return t, locked, 0
}

func finishLockedCommand(code int, dir string, t task, o options, out, errout, relationOut io.Writer, relations *bytes.Buffer, namesLater bool) int {
	if code == 0 && (o.cmd == "set" || o.cmd == "start") {
		code = finishSetOrStart(dir, t, o, out, errout, relationOut, relations, namesLater)
	}
	if code == 0 && (o.cmd == "done" || o.cmd == "reject") {
		code = closeBlockRelations(t, o, out, errout, code)
	}
	if o.cmd == "done" && code == 0 {
		hintNext(t, dir, o, out, errout)
	}
	return code
}

func runTransferCommand(root, dir string, o options, out, errout io.Writer, release func()) int {
	before, findErr := find(dir, o.id)
	if findErr != nil && !errors.Is(findErr, os.ErrNotExist) {
		release()
		return reportFindError(findErr, o.id, errout)
	}
	if errors.Is(findErr, os.ErrNotExist) {
		release()
		code := transferTask(root, dir, o, out, errout)
		if code == 0 {
			finishTransferRelations(dir, o.id, task{}, o, errout)
		}
		return code
	}
	code := transferTask(root, dir, o, out, errout)
	release()
	if code == 0 {
		finishTransferRelations(dir, strings.TrimSuffix(filepath.Base(before.path), ".md"), before, o, errout)
	}
	return code
}

func finishSetOrStart(dir string, t task, o options, out, errout, relationOut io.Writer, relations *bytes.Buffer, namesLater bool) int {
	current, err := readTaskLocked(dir, t.slug)
	if err != nil {
		return fail(errout, err)
	}
	code := 0
	if len(o.blocks) > 0 {
		blockOptions := o
		blockOptions.baseRoot = taskProject(current.path)
		code = attachBlocks(current, blockOptions, relationOut, errout, o.blocks)
		if code == 0 {
			current, err = readTaskLocked(dir, t.slug)
			if err != nil {
				return fail(errout, err)
			}
		}
	}
	if code == 0 && len(o.unblocks) > 0 {
		blockOptions := o
		blockOptions.baseRoot = taskProject(current.path)
		code = removeBlocks(current, blockOptions, errout, o.unblocks)
		if code == 0 {
			current, err = readTaskLocked(dir, t.slug)
			if err != nil {
				return fail(errout, err)
			}
		}
	}
	if code == 0 && o.prio != "" {
		code = propagatePriority(current.path, o, relationOut, errout)
		if code == 0 {
			current, err = readTaskLocked(dir, t.slug)
			if err != nil {
				return fail(errout, err)
			}
		}
	}
	if code == 0 && o.prio != "" {
		refreshed, err := readTaskLocked(dir, t.slug)
		if err != nil {
			return fail(errout, err)
		}
		lowered := o
		lowered.prio = refreshed.prio
		loweredTicket := t
		loweredTicket.body = refreshed.body
		notePriorityLowered(loweredTicket, lowered, errout)
	}
	if namesLater {
		final, err := readTaskLocked(dir, t.slug)
		if err != nil {
			return fail(errout, err)
		}
		fmt.Fprintln(out, filepath.Base(final.path))
		_, _ = relations.WriteTo(out)
	}
	return code
}

func reportFindError(err error, id string, errout io.Writer) int {
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(errout, "tk: task %q not found\n", id)
		return exitNone
	}
	fmt.Fprintln(errout, "tk:", err)
	return exitUsage
}

func runFoundTask(root string, t task, o options, out, errout io.Writer) int {
	switch o.cmd {
	case "show":
		return showTask(root, t, o, out, errout)
	case "check":
		return checkPlan(o.baseRoot, t, o.planLimit(), out)
	case "plan":
		if o.repin != "" {
			return repinPlanItem(t, o, out, errout)
		}
		return addPlanItem(t, o, out, errout)
	case "skip":
		return skipPlanItem(t, o, out, errout)
	case "note":
		return addNote(t, o, out, errout)
	default:
		return transition(t, o, out, errout)
	}
}

func showTask(root string, t task, o options, out, errout io.Writer) int {
	if o.json {
		return showJSON(root, t, out, errout)
	}
	fmt.Fprintf(out, "%s\n%s", relative(root, t.path), wrapLines(t.body, showWidth))
	if !strings.HasSuffix(t.body, "\n") {
		fmt.Fprintln(out)
	}
	return 0
}

func bareRepository(main string) (bool, error) {
	bare, err := exec.Command("git", "-C", main, "rev-parse", "--is-bare-repository").Output()
	if err != nil {
		return false, fmt.Errorf("git rev-parse in %s: %w", main, err)
	}
	return strings.TrimSpace(string(bare)) == "true", nil
}

// sharedTaskRoot возвращает основное дерево main, если tk запущен из другого
// его worktree и в основном дереве docs/tasks — симлинк на хранилище или
// игнорируется: тогда тикеты лежат там одни на все ветки. Иначе — пусто, тикеты
// в текущем дереве.
func sharedTaskRoot(root, main string) (string, error) {
	if main == "" || main == root {
		return "", nil
	}
	// Каталог .git бывает и bare-репо (рабочего дерева у него нет): тогда тикеты в worktree.
	bare, err := bareRepository(main)
	if err != nil {
		return "", err
	}
	if bare {
		return "", nil
	}
	if info, lerr := os.Lstat(filepath.Join(main, "docs/tasks")); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		return main, nil
	}
	err = exec.Command("git", "-C", main, "check-ignore", "-q", "--no-index", "docs/tasks/").Run()
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
	if o.cmd == "ls" || o.cmd == "archive" || o.cmd == "show" || o.cmd == "lint" || o.cmd == "check" || o.cmd == "external" {
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
	release, e := lockTasks(dir)
	if e != nil {
		return fail(errout, e)
	}
	defer release()
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
	meta := ""
	if o.why != "" {
		meta += "Причина: " + o.why + "\n"
	}
	if o.back != "" {
		meta += "Заказчик: " + o.back + "\n"
	}
	if o.journal {
		meta += "Вид: журнал\n"
	}
	body := fmt.Sprintf("# %s\n\nСтатус: открыт\n%s\n## Что сделать\n\n## Приёмка\n\n## Журнал\n- %s открыт — %s\n",
		title, meta, now.Format("2006-01-02 15:04"), o.by)
	if o.text != "" {
		body = fillTicket(body, o.text)
	}
	if err := validatePlanSection(body); err != nil {
		return fail(errout, err)
	}
	name := now.Format("20060102-1504") + "-" + o.prio + "-"
	if o.diff != "" {
		name += o.diff + "-"
	}
	p := filepath.Join(dir, name+o.id+".md")
	if e = writeTask(p, body); e != nil {
		return fail(errout, e)
	}
	createdPath := relative(root, p)
	if o.to != "" {
		createdPath, e = filepath.Abs(p)
		if e != nil {
			return fail(errout, e)
		}
	}
	fmt.Fprintln(out, createdPath)
	fmt.Fprintln(errout, hintAfterNew(o, createdPath, body))
	return 0
}

// hintAfterNew and hintAfterStart name the next step on stderr: stdout of tk new
// is the ticket path, and scripts read it.
func hintAfterNew(o options, path, body string) string {
	fill := fillHint(body, path)
	switch {
	case o.journal:
		return "дальше: записи — tk note " + o.id + " TEXT"
	case o.to != "" && fill != "":
		return "дальше: " + fill
	case o.to != "":
		return "дальше: тикет в очереди проекта, его возьмёт исполнитель"
	case fill != "":
		return "дальше: " + fill + ", затем tk start " + o.id
	default:
		return "дальше: tk start " + o.id
	}
}

func hintAfterStart(t task, body, path string) string {
	hint := "дальше: "
	if fill := fillHint(body, path); fill != "" {
		hint += fill + "; "
	}
	return hint + "по ходу tk note " + t.slug + " TEXT; закончил — независимая проверка, затем tk done " + t.slug + " --check TEXT"
}

// fillHint names the sections tk new left empty, or nothing when both are filled.
func fillHint(body, path string) string {
	var empty []string
	if section, ok := sectionBody(body, "## Что сделать"); ok && strings.TrimSpace(section) == "" {
		empty = append(empty, "«Что сделать»")
	}
	if noCheck(body) {
		empty = append(empty, "«Приёмка»")
	}
	if len(empty) == 0 {
		return ""
	}
	return "заполни " + strings.Join(empty, " и ") + " в " + path
}

func prepareTaskDirectory(target, knownMain string) error {
	main := knownMain
	if main == "" {
		var err error
		main, err = gitMainRoot(target)
		if err != nil || main == "" {
			return err
		}
	}
	common, err := exec.Command("git", "-C", target, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse in %s: %w", target, err)
	}
	commonDir := strings.TrimSpace(string(common))
	_, applicable, pathErr := taskSourcePath(main)
	if pathErr != nil {
		return pathErr
	}
	if !applicable {
		return nil
	}
	tasksRoot := filepath.Dir(main)
	docsTasks := filepath.Join(main, "docs/tasks")
	ready, err := existingTaskDirectory(docsTasks)
	if err != nil || ready {
		return err
	}
	store := filepath.Join(tasksRoot, "tasks")
	storeInfo, err := os.Stat(store)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("ticket store %s is missing", store)
	}
	if err != nil {
		return fmt.Errorf("inspect ticket store %s: %w", store, err)
	}
	if !storeInfo.IsDir() {
		return fmt.Errorf("ticket store %s is not a directory", store)
	}
	if err := os.Remove(docsTasks); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty directory %s: %w", docsTasks, err)
	}
	project := filepath.Base(main)
	if err := os.MkdirAll(filepath.Join(tasksRoot, "tasks", project), dirMode); err != nil {
		return fmt.Errorf("create ticket store for %s: %w", project, err)
	}
	if err := os.MkdirAll(filepath.Join(main, "docs"), dirMode); err != nil {
		return fmt.Errorf("create docs directory: %w", err)
	}
	link := "../../tasks/" + project
	if err := installTaskLink(docsTasks, link); err != nil {
		return err
	}
	return ensureTaskExclude(main, commonDir)
}

func existingTaskDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if _, err = os.Stat(path); err != nil {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return false, fmt.Errorf("read link %s: %w", path, readErr)
			}
			if errors.Is(err, os.ErrNotExist) {
				return false, fmt.Errorf("%s points to missing target %s", path, target)
			}
			return false, fmt.Errorf("inspect link target %s from %s: %w", target, path, err)
		}
		return true, nil
	}
	if !info.IsDir() {
		return true, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return len(entries) > 0, nil
}

func installTaskLink(path, target string) error {
	err := os.Symlink(target, path)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create symlink %s: %w", path, err)
	}
	got, err := os.Readlink(path)
	if err != nil {
		return fmt.Errorf("inspect concurrent symlink %s: %w", path, err)
	}
	if got != target {
		return fmt.Errorf("%s already points to %s, expected %s", path, got, target)
	}
	return nil
}

func ensureTaskExclude(main, commonDir string) error {
	check := exec.Command("git", "-C", main, "check-ignore", "-q", "--no-index", "docs/tasks")
	err := check.Run()
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return fmt.Errorf("git check-ignore in %s: %w", main, err)
	}
	path := filepath.Join(commonDir, "info/exclude")
	if err = os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return fmt.Errorf("create exclude directory: %w", err)
	}
	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if len(content) > 0 && content[len(content)-1] != '\n' {
		if _, err = file.Write([]byte("\n")); err != nil {
			file.Close()
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	if _, err = file.Write([]byte("/docs/tasks\n")); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
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
		name, ok := parseTaskName(en.Name())
		if !ok {
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
		created, err := time.ParseInLocation("20060102-1504", name.date+"-"+name.clock, time.Local)
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
		ts = append(ts, task{path: p, slug: name.slug, status: st, prio: name.prio, diff: name.diff, title: title(s), body: s, created: created, changed: lastChange(s, info.ModTime())})
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
	if t.slug == id || base == id || strings.TrimSuffix(base, ".md") == id {
		return true
	}
	for _, line := range journalLines(t.body) {
		if m := renameHistoryRE.FindStringSubmatch(line); m != nil && (m[1] == id || strings.TrimSuffix(m[1], ".md") == id) {
			return true
		}
	}
	return false
}

type taskName struct{ date, clock, prio, diff, slug string }

var renameHistoryRE = regexp.MustCompile(`^- \d{4}-\d{2}-\d{2} \d{2}:\d{2} переименован — \S+: (\S+) → (\S+)$`)

func parseTaskName(name string) (taskName, bool) {
	m := fileRE.FindStringSubmatch(name)
	if m == nil {
		return taskName{}, false
	}
	return taskName{date: m[1], clock: m[2], prio: m[3], diff: m[4], slug: m[5]}, true
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
		printTask(out, all[i], labels[all[i].path], errout)
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
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
				if _, ok := parseTaskName(entry.Name()); !ok {
					skipped = append(skipped, entry.Name())
				}
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

func printTask(out io.Writer, t task, label string, errout io.Writer) {
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
	fmt.Fprintf(out, "%s%s %s %s %s %s%s%s%s\n", label, t.slug, t.status, t.prio, ageText, t.title, wait, checkMark, blockMark(t, errout))
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
	if err := writeTask(t.path, t.body); err != nil {
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
	if requiredMentionRE.MatchString(command) || requiredMentionRE.MatchString(o.why) {
		fmt.Fprintln(errout, "tk: --repin command and reason cannot contain a required plan journal record")
		return exitUsage
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
	if !requiredPlanAfterRepin(t.body, body, name, command) {
		fmt.Fprintf(errout, "tk: required plan item %q would be edited by hand\n", name)
		return exitUsage
	}
	if err := writeTask(t.path, body); err != nil {
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
		if m := executorHistoryRE.FindStringSubmatch(line); m != nil && strings.EqualFold(m[1], currentSession) {
			return errors.New("a current or previous executor cannot repin a required plan item")
		}
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

func requiredPlanAfterRepin(before, after, name, command string) bool {
	oldPins, newPins := requiredPlan(before), requiredPlan(after)
	oldPin, existed := oldPins[name]
	newPin, exists := newPins[name]
	if !existed || oldPin.conflict || !exists || newPin.conflict || newPin.cmd != command {
		return false
	}
	for pinName, old := range oldPins {
		if pinName == name {
			continue
		}
		next, ok := newPins[pinName]
		if !ok || next != old || next.conflict {
			return false
		}
	}
	for pinName, next := range newPins {
		if pinName == name {
			continue
		}
		if old, ok := oldPins[pinName]; !ok || next != old {
			return false
		}
	}
	return true
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
	if err := writeTask(t.path, body); err != nil {
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
	limit    time.Duration
}

// shellEnvDropped — переменные, которые убираются из окружения пункта плана вместе с LD_*, MALLOC_* и
// BASH_FUNC_* (см. runPlanItem): стартовые bash и остальной список UNSECURE_ENVVARS glibc без TMPDIR — тот кода
// не грузит, а командам проверки нужен.
var shellEnvDropped = []string{"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS",
	"GCONV_PATH", "GETCONF_DIR", "GLIBC_TUNABLES", "HOSTALIASES", "LOCALDOMAIN", "LOCPATH", "NIS_PATH", "NLSPATH",
	"RESOLV_HOST_CONF", "RES_OPTIONS", "TZDIR"}

func runPlanItem(root string, item planJSON, timeout time.Duration) planResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
		result.limit = timeout
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

func runPlan(root string, t task, timeout time.Duration) []planResult {
	items := planItems(t.body)
	results := make([]planResult, 0, len(items))
	for _, item := range items {
		if item.Invalid != "" {
			results = append(results, planResult{item: item, stderr: item.Invalid})
		} else if item.Skipped {
			results = append(results, planResult{item: item, skipped: true})
		} else {
			results = append(results, runPlanItem(root, item, timeout))
		}
	}
	return results
}

func checkPlan(root string, t task, timeout time.Duration, out io.Writer) int {
	results := runPlan(root, t, timeout)
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
		return fmt.Sprintf("%s: timeout after %s (raise with --timeout)\n", result.item.Name, result.limit)
	}
	if result.stderr != "" {
		return fmt.Sprintf("%s: exit %d, %s\n", result.item.Name, result.exit, result.stderr)
	}
	return fmt.Sprintf("%s: exit %d\n", result.item.Name, result.exit)
}

func executePlan(root string, t *task, timeout time.Duration, errout io.Writer) int {
	var failed []planResult
	for _, result := range runPlan(root, *t, timeout) {
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
// Проверки идут без блокировки папки, чтобы заметки других не ждали их; сверка и запись — под ней: при коде 0
// вызывающий держит её до записи и снимает release.
func checkDone(t *task, o options, errout io.Writer) (int, func(), int) {
	loaded := t.body
	findings, code := prepareDone(t, o, errout)
	if code != 0 {
		return findings, nil, code
	}
	if code = executePlan(o.baseRoot, t, o.planLimit(), errout); code != 0 {
		return findings, nil, code
	}
	release, err := lockTasks(filepath.Dir(t.path))
	if err != nil {
		return findings, nil, fail(errout, err)
	}
	current, err := os.ReadFile(t.path)
	if err != nil {
		release()
		return findings, nil, fail(errout, err)
	}
	if string(current) != loaded {
		release()
		fmt.Fprintf(errout, "tk: %s changed while plan checks ran; run done again\n", t.slug)
		return findings, nil, exitNone
	}
	return findings, release, 0
}

func transition(t task, o options, out, errout io.Writer) int {
	if o.cmd == "set" {
		return setTask(t, o, out, errout)
	}
	if code, handled := guardStart(t, o, out, errout); handled {
		return code
	}
	glintFindings, release, code := prepareTransition(t, o, errout)
	if code != 0 {
		return code
	}
	if release != nil {
		defer release()
	}
	next := transitionStatus(o.cmd)
	_, hadStatus := explicitStatus(t.body)
	body, err := transitionBody(t.body, &o, next, errout)
	if err != nil {
		return fail(errout, err)
	}
	body = setStatus(body, next)
	if o.cmd == "start" && !hadStatus {
		body = moveMetadataAfterStatus(body)
	}
	body = journal(body, transitionJournal(o, next, glintFindings))
	newPath, err := transitionPath(&t, o)
	if err != nil {
		return fail(errout, err)
	}
	if newPath != t.path {
		body = journal(body, renameJournal(o.by, t.path, newPath))
		if err := renameTaskFile(t.path, newPath); err != nil {
			return fail(errout, err)
		}
	}
	if err := writeRenamedTask(t.path, newPath, body); err != nil {
		return fail(errout, err)
	}
	fmt.Fprintf(out, "%s %s\n", t.slug, next)
	if o.cmd == "start" {
		fmt.Fprintln(errout, hintAfterStart(t, body, newPath))
	}
	return 0
}

func guardStart(t task, o options, out, errout io.Writer) (int, bool) {
	if o.cmd != "start" || t.status != "в работе" {
		return 0, false
	}
	owner := executor(t.body)
	if strings.EqualFold(owner, o.by) && o.prio == "" && o.diff == "" && o.why == "" {
		fmt.Fprintf(out, "%s %s\n", t.slug, t.status)
		fmt.Fprintln(errout, hintAfterStart(t, t.body, t.path))
		return 0, true
	}
	if owner != "" && !strings.EqualFold(owner, o.by) && !o.take {
		fmt.Fprintf(errout, "tk: ticket is assigned to %s\n", owner)
		return exitNone, true
	}
	return 0, false
}

func prepareTransition(t task, o options, errout io.Writer) (int, func(), int) {
	if o.cmd == "start" && (o.prio != "" || o.diff != "" || o.why != "") {
		prio := t.prio
		if o.prio != "" {
			prio = o.prio
		}
		if requiresWhyForPromotion(t.prio, prio) && strings.TrimSpace(o.why) == "" && lineValue(t.body, "Причина: ") == "" {
			fmt.Fprintln(errout, "tk: --prio CRIT/HIGH requires --why or an existing Причина")
			return 0, nil, exitUsage
		}
	}
	if o.cmd != "done" {
		return 0, nil, 0
	}
	findings, release, code := checkDone(&t, o, errout)
	return findings, release, code
}

func transitionStatus(cmd string) string {
	return map[string]string{"start": "в работе", "wait": "ждёт", "review": "на проверке", "done": "готово", "reject": "отклонён"}[cmd]
}

func transitionBody(body string, o *options, next string, errout io.Writer) (string, error) {
	if o.cmd == "start" || o.cmd == "review" && executor(body) == "" {
		body = setExecutor(body, o.by)
	}
	if o.cmd == "wait" {
		o.until = datedWait(o.until, time.Now())
		if _, ok := waitDue(o.until, time.Now()); !ok {
			fmt.Fprintln(errout, "tk: --until has no time at the start: a condition, tk ls --due will not show it")
		}
		body = setWaiting(setStatus(body, next), o.until)
	}
	if o.cmd == "start" {
		body = removeWaiting(body)
		var err error
		body, err = applyMetadata(body, *o)
		if err != nil {
			return "", err
		}
	}
	return body, nil
}

func transitionJournal(o options, next string, glintFindings int) string {
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
	return line
}

func transitionPath(t *task, o options) (string, error) {
	if o.cmd != "start" {
		return t.path, nil
	}
	name, err := retaggedName(filepath.Base(t.path), o)
	if err != nil {
		return "", err
	}
	newPath := filepath.Join(filepath.Dir(t.path), name)
	if newPath == t.path {
		return newPath, nil
	}
	if _, err := os.Stat(newPath); err == nil {
		return "", fmt.Errorf("target ticket name already exists: %s", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := ensureRenameableTicket(t.path); err != nil {
		return "", err
	}
	return newPath, nil
}

// retaggedName is a ticket file name with the priority and difficulty from
// options. Without either the name stays as it is, old spellings included: old
// names are not renamed on their own. A written name uses the current spelling
// of the priority; an old word without a current form needs --prio.
func retaggedName(base string, o options) (string, error) {
	if o.prio == "" && o.diff == "" {
		return base, nil
	}
	parsed, ok := parseTaskName(base)
	if !ok {
		return "", errors.New("cannot update an unrecognized ticket name")
	}
	prio := normalizePrio(parsed.prio)
	if o.prio != "" {
		prio = o.prio
	}
	if !validPrio(prio) {
		return "", fmt.Errorf("old priority %s has no current form: pass --prio", prio)
	}
	diff := parsed.diff
	if o.diff != "" {
		diff = o.diff
	}
	name := parsed.date + "-" + parsed.clock + "-" + prio + "-"
	if diff != "" {
		name += diff + "-"
	}
	return name + parsed.slug + ".md", nil
}

func applyMetadata(body string, o options) (string, error) {
	if o.why == "" {
		return body, nil
	}
	return setHeaderValue(body, "Причина: ", o.why)
}

func setJournalKind(body string) (string, error) {
	lines := strings.Split(body, "\n")
	if slices.Contains(lines, "Вид: журнал") {
		return body, nil
	}
	for i, line := range lines {
		if strings.HasPrefix(line, "Статус: ") {
			lines = append(lines[:i+1], append([]string{"Вид: журнал"}, lines[i+1:]...)...)
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", errors.New("ticket has no Статус line")
}

func setHeaderValue(body, prefix, value string) (string, error) {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			lines[i] = prefix + value
			return strings.Join(lines, "\n"), nil
		}
	}
	for i, line := range lines {
		if strings.HasPrefix(line, "Статус: ") {
			lines = append(lines[:i+1], append([]string{prefix + value}, lines[i+1:]...)...)
			return strings.Join(lines, "\n"), nil
		}
	}
	for i, line := range lines {
		if strings.HasPrefix(line, "# ") {
			lines = append(lines[:i+1], append([]string{prefix + value}, lines[i+1:]...)...)
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", errors.New("cannot insert Причина: ticket has no title or status")
}

func moveMetadataAfterStatus(body string) string {
	lines := strings.Split(body, "\n")
	metadata := make([]string, 0, 2)
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, "Причина: ") || strings.HasPrefix(line, "Заказчик: ") {
			metadata = append(metadata, line)
			continue
		}
		kept = append(kept, line)
	}
	if len(metadata) == 0 {
		return body
	}
	for i, line := range kept {
		if strings.HasPrefix(line, "Статус: ") {
			kept = append(kept[:i+1], append(metadata, kept[i+1:]...)...)
			return strings.Join(kept, "\n")
		}
	}
	return body
}

func setTask(t task, o options, out, errout io.Writer) int {
	prio := t.prio
	if o.prio != "" {
		prio = o.prio
	}
	if requiresWhyForPromotion(t.prio, prio) && strings.TrimSpace(o.why) == "" && lineValue(t.body, "Причина: ") == "" {
		fmt.Fprintln(errout, "tk: --prio CRIT/HIGH requires --why or an existing Причина")
		return exitUsage
	}
	body, err := applyMetadata(t.body, o)
	if err != nil {
		return fail(errout, err)
	}
	if o.journal {
		body, err = setJournalKind(body)
		if err != nil {
			return fail(errout, err)
		}
	}
	newName, err := retaggedName(filepath.Base(t.path), o)
	if err != nil {
		return fail(errout, err)
	}
	newPath := filepath.Join(filepath.Dir(t.path), newName)
	if newPath != t.path {
		if _, err := os.Stat(newPath); err == nil {
			return fail(errout, fmt.Errorf("target ticket name already exists: %s", newName))
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(errout, err)
		}
		if err := ensureRenameableTicket(t.path); err != nil {
			return fail(errout, err)
		}
		body = journal(body, renameJournal(o.by, t.path, newPath))
		if err := renameTaskFile(t.path, newPath); err != nil {
			return fail(errout, err)
		}
	}
	if err := writeRenamedTask(t.path, newPath, body); err != nil {
		return fail(errout, err)
	}
	fmt.Fprintln(out, newName)
	return 0
}

func writeRenamedTask(oldPath, newPath, body string) error {
	if err := writeTask(newPath, body); err != nil {
		if newPath != oldPath {
			if rollbackErr := renameTaskFile(newPath, oldPath); rollbackErr != nil {
				return fmt.Errorf("write ticket: %v; restore old name: %w", err, rollbackErr)
			}
		}
		return err
	}
	return nil
}

func ensureRenameableTicket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect ticket before rename: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("cannot rename a symlink ticket")
	}
	return nil
}

func renameJournal(actor, oldPath, newPath string) string {
	return fmt.Sprintf("- %s переименован — %s: %s → %s", time.Now().Format("2006-01-02 15:04"), actor, filepath.Base(oldPath), filepath.Base(newPath))
}

var renameTaskFile = os.Rename

func addNote(t task, o options, out, errout io.Writer) int {
	line := fmt.Sprintf("- %s note — %s: %s", time.Now().Format("2006-01-02 15:04"), o.by, o.note)
	body := journal(t.body, line)
	if err := writeTask(t.path, body); err != nil {
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
	Diff           string     `json:"diff"`
	Back           string     `json:"back"`
	Why            string     `json:"why"`
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
	Journal        any        `json:"journal,omitempty"` // ls: true for journals; show: journal lines
	Kind           string     `json:"kind,omitempty"`
	Plan           []planJSON `json:"plan,omitempty"`
	Blocks         []string   `json:"blocks,omitempty"`
	BlockedBy      []string   `json:"blocked_by,omitempty"`
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
	prio := normalizePrio(t.prio)
	journalEntry := isJournal(t)
	obj := taskJSON{Repo: repo, Slug: t.slug, Status: t.status, Prio: prio, Diff: t.diff, Back: lineValue(t.body, "Заказчик: "), Why: lineValue(t.body, "Причина: "), Title: t.title,
		Created: t.created.Format(time.RFC3339), Changed: t.changed.Format(time.RFC3339),
		Path: relative(root, t.path), Executor: executor(t.body), Waits: waits, Overdue: overdue, NoCheck: noCheck(t.body)}
	if details {
		obj.Journal = journalLines(t.body)
		if journalEntry {
			obj.Kind = "журнал"
		}
	} else if journalEntry {
		obj.Journal = true
	}
	obj.Checker, obj.Check, obj.CheckerSession = completion(t.body)
	obj.Plan = planItems(t.body)
	obj.Blocks = headerLinks(t.body, blocksPrefix)
	obj.BlockedBy = headerLinks(t.body, blockedPrefix)
	return obj
}

type taskGate struct {
	At         string               `json:"at"`
	Live       []string             `json:"live"`
	Open       []string             `json:"open"`
	OpenByRoot *map[string][]string `json:"open_by_root"`
}

func gateOpen(t task, open []string) bool {
	p := normalizePrio(t.prio)
	if p == "CRIT" || p == "HIGH" {
		return true
	}
	if t.diff != "" {
		return pairOpen(p, t.diff, open)
	}
	// A ticket without a difficulty may turn out to be any of them.
	for _, diff := range []string{"DO", "HUNT", "ARCH"} {
		if !pairOpen(p, diff, open) {
			return false
		}
	}
	return true
}

func pairOpen(prio, diff string, open []string) bool {
	for _, pair := range open {
		if pair == prio+"-"+diff || prio == "MED" && pair == "MEDIUM-"+diff {
			return true
		}
	}
	return false
}

type gateState struct {
	data    taskGate
	path    string
	problem string
	valid   bool
	open    []string
}

func readTaskGate() (gateState, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return gateState{}, fmt.Errorf("determine gate directory: %w", err)
	}
	state := gateState{path: filepath.Join(home, ".local/state/queue/gate.json"), problem: "gate.json is missing"}
	content, err := os.ReadFile(state.path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		state.problem = "cannot read gate.json: " + err.Error()
		return state, nil
	}
	if err := json.Unmarshal(content, &state.data); err != nil {
		state.problem = "cannot parse gate.json: " + err.Error()
		return state, nil
	}
	at, err := time.Parse(time.RFC3339, state.data.At)
	if err != nil {
		state.problem = "gate.json has invalid at timestamp: " + err.Error()
		return state, nil
	}
	if time.Since(at) > 26*time.Hour {
		state.problem = "gate.json is older than 26 hours"
		return state, nil
	}
	if at.After(time.Now()) {
		state.problem = "gate.json timestamp is in the future"
		return state, nil
	}
	if state.data.OpenByRoot == nil {
		state.problem = "gate.json has no open_by_root"
		return state, nil
	}
	state.valid = true
	return state, nil
}

func gatePairs(data taskGate, project string) ([]string, error) {
	if data.OpenByRoot == nil || len(*data.OpenByRoot) == 0 {
		return data.Open, nil
	}
	resolvedProject, err := normalizedRoot(project)
	if err != nil {
		return nil, fmt.Errorf("resolve project root %s: %w", project, err)
	}
	best := ""
	pairs := data.Open
	for candidate, rootPairs := range *data.OpenByRoot {
		if !filepath.IsAbs(candidate) {
			continue
		}
		resolvedRoot, err := normalizedRoot(candidate)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("resolve gate root %s: %w", candidate, err)
		}
		rel, err := filepath.Rel(resolvedRoot, resolvedProject)
		if err != nil {
			return nil, fmt.Errorf("compare gate root %s with project %s: %w", resolvedRoot, resolvedProject, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || len(resolvedRoot) <= len(best) {
			continue
		}
		best, pairs = resolvedRoot, rootPairs
	}
	return pairs, nil
}

func nextTask(root, main, dir string, o options, out, errout io.Writer) int {
	project := root
	if main != "" {
		project = main
	}
	choice, err := chooseNext(dir, project, o.by)
	if err != nil {
		return fail(errout, err)
	}
	if !choice.gate.valid {
		fmt.Fprintf(errout, "tk: %s; showing started work and of new work CRIT and HIGH only (%s)\n", choice.gate.problem, choice.gate.path)
	}
	visible, sourceFailed, err := visibleExternal(project, choice, errout)
	if err != nil {
		return fail(errout, err)
	}
	localGroup := 99
	if choice.found {
		localGroup = nextGroup(choice.task, o.by, choice.gate, choice.live)
	}
	if len(visible) > 0 && (!choice.found || externalWinsLocal(visible[0], localGroup)) {
		return printExternalNext(root, choice, visible, choice.found, o.json, out, errout)
	}
	if !choice.found {
		if sourceFailed {
			fmt.Fprintln(out, "местных тикетов нет, источник недоступен: очередь неполная")
		} else {
			fmt.Fprintln(out, "работы нет")
		}
		return exitNone
	}
	return printCombinedNext(root, choice, visible, o.json, out, errout)
}

func externalWinsLocal(t externalTask, localGroup int) bool {
	return nextGroup(task{prio: t.Prio, status: "открыт"}, "", gateState{}, false) <= localGroup
}

type nextChoice struct {
	task  task
	found bool
	gate  gateState
	live  bool
}

func chooseNext(dir, project, actor string) (nextChoice, error) {
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if _, e := os.Stat(dir); errors.Is(e, os.ErrNotExist) {
			target, readErr := os.Readlink(dir)
			if readErr != nil {
				return nextChoice{}, fmt.Errorf("read dangling symlink %s: %w", dir, readErr)
			}
			return nextChoice{}, fmt.Errorf("%s is a dangling symlink to %s", dir, target)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nextChoice{}, fmt.Errorf("inspect %s: %w", dir, err)
	}
	tasks, err := loadTasks(dir, true)
	if err != nil {
		return nextChoice{}, err
	}
	gate, err := readTaskGate()
	if err != nil {
		return nextChoice{}, err
	}
	live := slices.Contains(gate.data.Live, filepath.Base(project))
	if gate.valid {
		gate.open, err = gatePairs(gate.data, project)
		if err != nil {
			return nextChoice{}, err
		}
	}
	choice := nextChoice{gate: gate, live: live}
	candidates := nextCandidates(tasks, actor, gate, choice.live)
	if len(candidates) != 0 {
		sortNextCandidates(candidates, actor, gate, choice.live)
		choice.task, choice.found = candidates[0], true
	}
	return choice, nil
}

// hintNext ends tk done with the ticket tk next would give its executor, who
// takes the next one even when a checker closes this one. The ticket is already
// closed, so a failed lookup only warns.
func hintNext(t task, dir string, o options, out, errout io.Writer) {
	project := o.mainRoot
	if project == "" {
		project = o.baseRoot
	}
	actor := executor(t.body)
	if actor == "" {
		actor = o.by
	}
	choice, err := chooseNext(dir, project, actor)
	if err != nil {
		fmt.Fprintf(errout, "tk: next ticket is unknown: %v\n", err)
		return
	}
	ext, _, err := visibleExternal(project, choice, errout)
	if err != nil {
		fmt.Fprintf(errout, "tk: next ticket is unknown: %v\n", err)
		return
	}
	if len(ext) > 0 && (!choice.found || externalWinsLocal(ext[0], nextGroup(choice.task, actor, choice.gate, choice.live))) {
		fmt.Fprintln(out, "следующий: "+externalLine(ext[0], choice.gate.valid && !choice.live))
	} else if choice.found {
		fmt.Fprintln(out, "следующий: "+nextLine(choice, errout))
	}
}

func sortExternal(items []externalTask) {
	sort.SliceStable(items, func(i, j int) bool {
		left := nextGroup(task{prio: items[i].Prio, status: "открыт"}, "", gateState{}, false)
		right := nextGroup(task{prio: items[j].Prio, status: "открыт"}, "", gateState{}, false)
		return left < right
	})
}

// readExternal returns the project's whole source list, sorted. A project
// without a source has an empty list; failure names why the source was not read.
func readExternal(project string, errout io.Writer) (items []externalTask, path, failure string, err error) {
	path, ok, err := taskSourcePath(project)
	if err != nil || !ok {
		return nil, "", "", err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, path, "", nil
	} else if err != nil {
		return nil, path, "cannot inspect source: " + err.Error(), nil
	}
	items, bad, reason, failed := runTaskSource(path, filepath.Base(project))
	if failed {
		return nil, path, reason, nil
	}
	if bad > 0 {
		fmt.Fprintf(errout, "tk: source %s: skipped %d bad records (first: %s)\n", path, bad, firstStderrLine(reason))
	}
	sortExternal(items)
	return items, path, "", nil
}

// listExternal is the source list for an outside reader. A failed source prints
// nothing on stdout: an empty array would mean the project has no such tasks.
func listExternal(project string, out, errout io.Writer) int {
	items, path, failure, err := readExternal(project, errout)
	if err != nil {
		return fail(errout, err)
	}
	if failure != "" {
		return fail(errout, fmt.Errorf("source %s: %s", path, firstStderrLine(failure)))
	}
	if items == nil {
		items = []externalTask{}
	}
	if err := json.NewEncoder(out).Encode(items); err != nil {
		return fail(errout, err)
	}
	return 0
}

func visibleExternal(project string, choice nextChoice, errout io.Writer) (items []externalTask, failed bool, err error) {
	items, path, failure, err := readExternal(project, errout)
	if err != nil {
		return nil, false, err
	}
	if failure != "" {
		sourceWarning(errout, path, failure)
		return nil, true, nil
	}
	visible := items[:0]
	for _, item := range items {
		if gateVisible(task{prio: item.Prio, status: "открыт"}, choice.gate, choice.live) {
			visible = append(visible, item)
		}
	}
	return visible, false, nil
}
func externalLine(t externalTask, closed bool) string {
	s := strings.TrimSpace(strings.Join([]string{t.Key, t.Prio, t.Status, t.Title, t.URL}, " "))
	if closed {
		s += " закрыто"
	}
	return s
}
func externalJSON(t externalTask, closed bool) map[string]any {
	return map[string]any{"source": "external", "key": t.Key, "title": t.Title, "status": t.Status, "prio": t.Prio, "url": t.URL, "closed": closed}
}
func localNextObject(root string, t task, closed bool) (map[string]any, error) {
	b, err := json.Marshal(taskObject(root, t, "", false))
	if err != nil {
		return nil, fmt.Errorf("encode local ticket: %w", err)
	}
	var obj map[string]any
	if err = json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("decode local ticket: %w", err)
	}
	obj["source"], obj["closed"] = "local", closed
	return obj, nil
}
func printCombinedNext(root string, choice nextChoice, ext []externalTask, jsonOutput bool, out, errout io.Writer) int {
	if !jsonOutput {
		fmt.Fprintln(out, nextLine(choice, errout))
		if len(ext) > 0 {
			fmt.Fprintln(out, "также: "+externalLine(ext[0], choice.gate.valid && !choice.live))
			if len(ext) > 1 {
				fmt.Fprintf(out, "ещё из источника: %d\n", len(ext)-1)
			}
		}
		return 0
	}
	obj, e := localNextObject(root, choice.task, choice.gate.valid && !choice.live)
	if e != nil {
		return fail(errout, e)
	}
	if len(ext) > 0 {
		obj["also"] = externalJSON(ext[0], choice.gate.valid && !choice.live)
		if len(ext) > 1 {
			obj["more"] = len(ext) - 1
		}
	}
	return encodeNext(obj, out, errout)
}
func printExternalNext(root string, choice nextChoice, ext []externalTask, local bool, jsonOutput bool, out, errout io.Writer) int {
	closed := choice.gate.valid && !choice.live
	if !jsonOutput {
		fmt.Fprintln(out, externalLine(ext[0], closed))
		if local {
			fmt.Fprintln(out, "также: "+nextLine(choice, errout))
		}
		if len(ext) > 1 {
			n := len(ext) - 1
			fmt.Fprintf(out, "ещё из источника: %d\n", n)
		}
		return 0
	}
	obj := externalJSON(ext[0], closed)
	if local {
		other, err := localNextObject(root, choice.task, closed)
		if err != nil {
			return fail(errout, err)
		}
		obj["also"] = other
	}
	if len(ext) > 1 {
		obj["more"] = len(ext) - 1
	}
	return encodeNext(obj, out, errout)
}
func encodeNext(obj any, out, errout io.Writer) int {
	if err := json.NewEncoder(out).Encode(obj); err != nil {
		return fail(errout, err)
	}
	return 0
}

func nextCandidates(tasks []task, actor string, gate gateState, live bool) []task {
	var candidates []task
	for i := range tasks {
		t := tasks[i]
		if !eligibleForNext(t, actor) {
			continue
		}
		if !gateVisible(t, gate, live) {
			continue
		}
		candidates = append(candidates, t)
	}
	return candidates
}

// gateVisible: gates close the start of new work only, so what an agent already
// began and a wait that came due stay visible. Without readable gates nothing
// tells which pairs are open or which projects are live, so of new work only what
// is always open stays visible.
func gateVisible(t task, gate gateState, live bool) bool {
	return t.status != "открыт" || gatePasses(t, gate, live)
}

func gatePasses(t task, gate gateState, live bool) bool {
	if !gate.valid {
		return gateOpen(t, nil)
	}
	return !live || gateOpen(t, gate.open)
}

func eligibleForNext(t task, actor string) bool {
	waitDueNow := false
	if t.status == "ждёт" {
		at, ok := waitDue(waiting(t.body), time.Now())
		waitDueNow = ok && !at.After(time.Now())
	}
	if isJournal(t) && !waitDueNow {
		return false
	}
	if t.status == "открыт" || t.status == "в работе" && strings.EqualFold(executor(t.body), actor) {
		return true
	}
	if t.status != "ждёт" {
		return false
	}
	return waitDueNow
}

func isJournal(t task) bool { return lineValue(t.body, "Вид: ") == "журнал" }

// nextGroup orders the queue. Work already begun under a closed pair stays in the
// queue without pushing out CRIT and HIGH, and goes ahead of new work.
func nextGroup(t task, actor string, gate gateState, live bool) int {
	prio := normalizePrio(t.prio)
	if prio == "CRIT" {
		return 0
	}
	if t.status != "открыт" && !gatePasses(t, gate, live) {
		return 3
	}
	if t.status == "в работе" && strings.EqualFold(executor(t.body), actor) {
		return 1
	}
	if prio == "HIGH" {
		return 2
	}
	if prio == "MED" {
		return 4
	}
	return 5
}

func sortNextCandidates(candidates []task, actor string, gate gateState, live bool) {
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		leftGroup, rightGroup := nextGroup(left, actor, gate, live), nextGroup(right, actor, gate, live)
		if leftGroup != rightGroup {
			return leftGroup < rightGroup
		}
		return left.created.Before(right.created)
	})
}

func nextLine(choice nextChoice, errout io.Writer) string {
	var row bytes.Buffer
	printTask(&row, choice.task, "", errout)
	line := strings.TrimSuffix(row.String(), "\n")
	if choice.gate.valid && !choice.live {
		line += " закрыто"
	}
	return line
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
	subjectStatus := lineValue(body, "Статус: ")
	subjectActive := subjectStatus != "готово" && subjectStatus != "отклонён"
	project := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	for _, ref := range headerLinks(body, blockedPrefix) {
		dir, neighbor, err := linkedPath(project, ref)
		if err != nil {
			issues = append(issues, shown+": blocking reference "+ref+" cannot be resolved: "+err.Error())
			continue
		}
		b, e := os.ReadFile(neighbor)
		if e != nil {
			continue
		}
		other, parseErr := parseTask(neighbor, string(b))
		if parseErr != nil {
			return nil, parseErr
		}
		name, valid := parseTaskName(filepath.Base(path))
		if !valid {
			continue
		}
		active := other.status != "готово" && other.status != "отклонён"
		if active && blockPrioRank(other.prio) > blockPrioRank(name.prio) {
			issues = append(issues, shown+": priority below blocking ticket "+ref)
		}
		if !active || !subjectActive {
			continue
		}
		reverse, refErr := ticketRef(dirProject(dir), project, name.slug)
		if refErr != nil {
			return nil, refErr
		}
		if !slices.Contains(headerLinks(other.body, blocksPrefix), reverse) {
			issues = append(issues, shown+": blocking link "+ref+" has no reverse Блокирует reference")
		}
	}
	name, validName := parseTaskName(filepath.Base(path))
	if !validName {
		issues = append(issues, shown+": invalid file name")
	} else if (name.prio == "CRIT" || name.prio == "HIGH") && name.date+"-"+name.clock >= mandatoryWhySince && lineValue(body, "Причина: ") == "" {
		issues = append(issues, shown+": "+name.prio+" without Причина")
	}
	return append(issues, lintTicketBody(body, shown)...), nil
}

func lintTicketBody(body, shown string) []string {
	var issues []string
	statusValue, hasStatus := explicitStatus(body)
	if hasStatus && !validStatus(statusValue) {
		issues = append(issues, shown+": unknown status "+statusValue)
	}
	if !hasStatus {
		return issues
	}
	issues = append(issues, lintTicketSections(body, shown)...)
	if statusValue == "готово" {
		issues = append(issues, lintCompletion(body, shown)...)
	}
	return issues
}

func lintTicketSections(body, shown string) []string {
	var issues []string
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
	return issues
}

func lintCompletion(body, shown string) []string {
	checker, check, _ := completion(body)
	if checker == "" || check == "" {
		return []string{shown + ": готово without journal verification"}
	}
	if doer := executor(body); doer != "" && strings.EqualFold(checker, doer) {
		return []string{shown + ": checker equals executor"}
	}
	return nil
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
		got, err := archiveRepo(repo, base, cut)
		moved = append(moved, got...)
		if err != nil {
			return fail(errout, err)
		}
	}
	fmt.Fprintf(out, "archived %d\n", len(moved))
	for _, p := range moved {
		fmt.Fprintln(out, p)
	}
	return 0
}

// archiveRepo moves the closed tickets of one repository older than cut, under the folder lock: a note
// racing the move then finds the ticket gone instead of writing it back at the old path.
func archiveRepo(repo, base string, cut time.Time) ([]string, error) {
	taskDir := filepath.Join(repo, "docs/tasks")
	release, err := lockForCommand(taskDir, "archive")
	if err != nil {
		return nil, err
	}
	defer release()
	ts, err := load(taskDir)
	if err != nil {
		return nil, err
	}
	dstDir, err := archiveDir(repo)
	if err != nil {
		return nil, err
	}
	var moved []string
	for i := range ts {
		t := ts[i]
		if (t.status != "готово" && t.status != "отклонён") || t.changed.After(cut) {
			continue
		}
		if err = os.MkdirAll(dstDir, dirMode); err != nil {
			return moved, fmt.Errorf("create archive folder: %w", err)
		}
		dst := filepath.Join(dstDir, filepath.Base(t.path))
		if _, err = os.Stat(dst); err == nil {
			return moved, fmt.Errorf("archive destination exists: %s", dst)
		} else if !errors.Is(err, os.ErrNotExist) {
			return moved, err
		}
		if err = os.Rename(t.path, dst); err != nil {
			return moved, fmt.Errorf("archive %s: %w", filepath.Base(t.path), err)
		}
		moved = append(moved, relative(base, dst))
	}
	return moved, nil
}
