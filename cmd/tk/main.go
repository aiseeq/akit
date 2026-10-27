// tk — учёт задач в docs/tasks репозитория.
// Коды выхода: 0 — успех, 1 — задача не найдена или действие отклонено, 2 — ошибка вызова или ввода-вывода.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/proc"
	"github.com/aiseeq/akit/timearg"
)

const usage = `tk — track repository tasks in docs/tasks.

Usage:
  tk new SLUG [--prio HIGH|MEDIUM|LOW] [--title TEXT] [--by NAME] [--text FILE|-]
  tk ls [--all] [--status STATUS] [--root DIR]
  tk start ID [--by NAME]
  tk wait ID --until TEXT [--by NAME]
  tk review ID [--by NAME] [--note TEXT]
  tk done ID --check TEXT [--by NAME] [--doer NAME]
  tk reject ID --why TEXT [--by NAME]
  tk show ID
  tk archive [--older DURATION]

Flags:
  --prio P       new: priority HIGH, MEDIUM, or LOW (default MEDIUM)
  --title TEXT   new: ticket title (default: slug)
  --by NAME      actor name (default: current session)
  --text FILE|-  new: fill ticket sections from FILE, or standard input with -
  --all          ls: include closed tickets
  --status S     ls: show only status S
  --root DIR     ls: scan DIR and its first-level repositories
  --note TEXT    review: add a review note to the journal
  --check TEXT   done: describe independent verification (required)
  --doer NAME    done: specify the executor of a legacy ticket
  --why TEXT     reject: explain why the ticket was declined (required)
  --until TEXT   wait: time or condition to wait for (required)
  --older D      archive: closed tickets older than D (default 14d; supports Go durations and d)

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
var fileRE = regexp.MustCompile(`^([0-9]{8})-([0-9]{4})-([A-Z]+)-([a-z0-9]+(?:-[a-z0-9]+)*)\.md$`)
var nameRE = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._:@-]*$`)
var ticketIDRE = regexp.MustCompile(`^[0-9]{8}-[0-9]{4}-[A-Z]+-[a-z0-9]+(?:-[a-z0-9]+)*(?:\.md)?$`)
var errHelp = errors.New("help")

type options struct {
	cmd, id, prio, title, by, note, check, doer, why, status, root, until, text string
	all                                                                         bool
	older                                                                       time.Duration
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
	if err := validateCommand(&o, pos); err != nil {
		return o, err
	}
	return o, nil
}

func parseOptions(args []string, o *options) ([]string, error) {
	values := map[string]*string{"--prio": &o.prio, "--title": &o.title, "--by": &o.by, "--note": &o.note, "--check": &o.check, "--doer": &o.doer, "--why": &o.why, "--status": &o.status, "--root": &o.root, "--until": &o.until, "--text": &o.text}
	var pos []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		v := args[i]
		if v == "--all" {
			o.all = true
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
	case "archive":
		if len(pos) > 0 {
			return errors.New("archive takes no id")
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q", o.cmd)
	}
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
	return nil
}

func validateTicketAction(o *options, pos []string) error {
	if len(pos) != 1 || (!slugRE.MatchString(o.id) && !ticketIDRE.MatchString(o.id)) {
		return errors.New("ID is the ticket slug (e.g. graft-hooks) or its file name")
	}
	if o.by != "" && !nameRE.MatchString(o.by) {
		return errors.New("--by must be a name without spaces; use --by NAME")
	}
	if o.doer != "" && !nameRE.MatchString(o.doer) {
		return errors.New("--doer must be a name without spaces; use --doer NAME")
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
	return nil
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
	if o.cmd == "ls" && o.root != "" {
		return list("", o, out, errout)
	}
	b, e := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if e != nil {
		fmt.Fprintf(errout, "tk: repository root: %v\n", e)
		return exitUsage
	}
	root := strings.TrimSpace(string(b))
	dir := filepath.Join(root, "docs/tasks")
	switch o.cmd {
	case "new":
		return create(root, dir, o, out, errout)
	case "ls":
		return list(root, o, out, errout)
	case "archive":
		return archive(root, dir, o, out, errout)
	}
	t, e := find(dir, o.id)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			fmt.Fprintf(errout, "tk: task %q not found\n", o.id)
			return exitNone
		}
		fmt.Fprintln(errout, "tk:", e)
		return exitUsage
	}
	if o.cmd == "show" {
		fmt.Fprintf(out, "%s\n%s", relative(root, t.path), t.body)
		if !strings.HasSuffix(t.body, "\n") {
			fmt.Fprintln(out)
		}
		return 0
	}
	return transition(t, o, out, errout)
}
func prepareActor(o *options, errout io.Writer) int {
	if o.cmd == "ls" || o.cmd == "archive" || o.cmd == "show" {
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

func session() string {
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
	body := fmt.Sprintf("# %s\n\nСтатус: открыт\n\n## Вход\n- Проблема:\n- Метод:\n- Дешевле:\n- Инструмент:\n- Цена:\n- Если не делать:\n\n## Что сделать\n\n## Приёмка\n\n## Журнал\n- %s открыт — %s\n",
		title, now.Format("2006-01-02 15:04"), o.by)
	if o.text != "" {
		body = fillTicket(body, o.text)
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
			return nil, fmt.Errorf("%s: %w", p, err)
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
	for l := range strings.SplitSeq(s, "\n") {
		if after, ok := strings.CutPrefix(l, "# "); ok {
			return after
		}
	}
	return "(no title)"
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
	repos, err := taskRepos(root, o.root)
	if err != nil {
		return fail(errout, err)
	}
	var all []task
	labels := make(map[string]string)
	var skipped []string
	for _, repo := range repos {
		dir := filepath.Join(repo, "docs/tasks")
		ts, err := load(dir)
		if err != nil {
			return fail(errout, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(errout, err)
		}
		label := ""
		if o.root != "" {
			label, err = filepath.Rel(o.root, repo)
			if err != nil {
				return fail(errout, err)
			}
			if label == "." {
				label = filepath.Base(repo)
			}
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") && fileRE.FindStringSubmatch(entry.Name()) == nil {
				skipped = append(skipped, entry.Name())
			}
		}
		for i := range ts {
			t := ts[i]
			if includeTask(t, o) {
				all = append(all, t)
				labels[t.path] = label
			}
		}
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
		if at, err := timearg.Parse(wait, time.Now()); err == nil {
			shown = "до " + at.Format("2006-01-02 15:04")
			if !at.After(time.Now()) {
				shown += " overdue"
			}
		}
		wait = " [" + shown + "]"
	}
	fmt.Fprintf(out, "%s%s %s %s %s %s%s\n", label, t.slug, t.status, t.prio, ageText, t.title, wait)
}

func waiting(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if value, ok := strings.CutPrefix(line, "Ждёт: "); ok {
			return value
		}
	}
	return ""
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

func standardTicketSection(section string) bool {
	return section == "## Что сделать" || section == "## Приёмка" || section == "## Вход"
}

func replaceTicketSections(template string, sections map[string][]string, extra []string) string {
	lines := strings.Split(template, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if standardTicketSection(line) {
			out = append(out, line)
			content, ok := sections[line]
			if ok && len(content) > 0 {
				out = append(out, content...)
			} else if line == "## Вход" {
				out = append(out, lines[i+1:i+7]...)
				i += 6
			}
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
		out = append(out, line)
	}
	return strings.Join(out, "\n")
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
func transition(t task, o options, out, errout io.Writer) int {
	next := map[string]string{"start": "в работе", "wait": "ждёт", "review": "на проверке", "done": "готово", "reject": "отклонён"}[o.cmd]
	if o.cmd == "done" {
		doer := executor(t.body)
		if o.doer != "" {
			doer = o.doer
		}
		if doer == "" || !nameRE.MatchString(doer) {
			fmt.Fprintln(errout, "tk: executor unknown or invalid; pass --doer NAME")
			return exitNone
		}
		if strings.EqualFold(doer, o.by) {
			fmt.Fprintln(errout, "tk: reviewer must differ from executor")
			return exitNone
		}
		if o.doer != "" {
			t.body = setExecutor(t.body, o.doer)
		}
	}
	if o.cmd == "start" || o.cmd == "review" && executor(t.body) == "" {
		t.body = setExecutor(t.body, o.by)
	}
	if o.cmd == "wait" {
		t.body = setWaiting(setStatus(t.body, next), o.until)
	}
	if o.cmd == "start" {
		t.body = removeWaiting(t.body)
	}
	line := fmt.Sprintf("- %s %s — %s", time.Now().Format("2006-01-02 15:04"), next, o.by)
	if o.cmd == "done" {
		line += " — " + o.check
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
func archive(root, dir string, o options, out, errout io.Writer) int {
	ts, e := load(dir)
	if e != nil {
		return fail(errout, e)
	}
	dstDir := filepath.Join(root, "docs/tasks-archive")
	cut := time.Now().Add(-o.older)
	var moved []string
	for i := range ts {
		t := ts[i]
		if (t.status != "готово" && t.status != "отклонён") || t.changed.After(cut) {
			continue
		}
		if e = os.MkdirAll(dstDir, dirMode); e != nil {
			return fail(errout, e)
		}
		dst := filepath.Join(dstDir, filepath.Base(t.path))
		if _, e = os.Stat(dst); e == nil {
			return fail(errout, fmt.Errorf("archive destination exists: %s", dst))
		}
		if e = os.Rename(t.path, dst); e != nil {
			return fail(errout, e)
		}
		moved = append(moved, relative(root, dst))
	}
	fmt.Fprintf(out, "archived %d\n", len(moved))
	for _, p := range moved {
		fmt.Fprintln(out, p)
	}
	return 0
}
