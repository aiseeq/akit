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
)

const usage = `tk — track repository tasks in docs/tasks.
Usage: tk new SLUG [--prio HIGH|MEDIUM|LOW] [--title TEXT] [--by NAME]
       tk ls [--all] [--status STATUS] [--root DIR]
       tk start|review|done|reject ID [flags]
       tk archive [--older DURATION]
       tk show ID
Statuses: открыт, в работе, на проверке, готово, отклонён.
Exit: 0 success, 1 not found or declined, 2 usage or I/O error.
`
const (
	exitNone  = 1
	exitUsage = 2
	dirMode   = 0755
	fileMode  = 0644
)

var slugRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
var fileRE = regexp.MustCompile(`^([0-9]{8})-([0-9]{4})-(HIGH|MEDIUM|LOW)-([a-z0-9]+(?:-[a-z0-9]+)*)\.md$`)
var errHelp = errors.New("help")

type options struct {
	cmd, id, prio, title, by, note, check, doer, why, status, root string
	all                                                            bool
	older                                                          time.Duration
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
	values := map[string]*string{"--prio": &o.prio, "--title": &o.title, "--by": &o.by, "--note": &o.note, "--check": &o.check, "--doer": &o.doer, "--why": &o.why, "--status": &o.status, "--root": &o.root}
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
	case "start", "review", "done", "reject", "show":
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
	if len(pos) != 1 || !slugRE.MatchString(o.id) {
		return fmt.Errorf("%s needs a valid task id", o.cmd)
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
	case "открыт", "в работе", "на проверке", "готово", "отклонён":
		return true
	}
	return false
}
func run(o options, out, errout io.Writer) int {
	if o.cmd != "ls" && o.cmd != "archive" && o.cmd != "show" && o.by == "" {
		o.by = session()
		if o.by == "" {
			fmt.Fprintln(errout, "tk: cannot determine session; pass --by NAME")
			return exitUsage
		}
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
		fmt.Fprintf(out, "%s\n%s\n", relative(root, t.path), t.status)
		return 0
	}
	return transition(t, o, out, errout)
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
		if t.slug == o.id {
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
	body := fmt.Sprintf("# %s\n\nСтатус: открыт\n\n## Постановка\n\n## Что сделать\n\n## Чем проверить\n\n## Журнал\n- %s открыт — %s\n",
		title, now.Format("2006-01-02 15:04"), o.by)
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
		if t.slug == id {
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
func list(root string, o options, out, errout io.Writer) int {
	repos, err := taskRepos(root, o.root)
	if err != nil {
		return fail(errout, err)
	}
	var all []task
	labels := make(map[string]string)
	for _, repo := range repos {
		ts, err := load(filepath.Join(repo, "docs/tasks"))
		if err != nil {
			return fail(errout, err)
		}
		label := ""
		if o.root != "" {
			label = filepath.Base(repo)
		}
		for i := range ts {
			t := ts[i]
			if includeTask(t, o) {
				all = append(all, t)
				labels[t.path] = label
			}
		}
	}
	if len(all) == 0 {
		fmt.Fprintln(out, "no tasks")
		return 0
	}
	sort.Slice(all, func(i, j int) bool { return all[i].created.Before(all[j].created) })
	for i := range all {
		printTask(out, all[i], labels[all[i].path])
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
	return o.all || t.status == "открыт" || t.status == "в работе" || t.status == "на проверке"
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
	fmt.Fprintf(out, "%s%s %s %s %s %s\n", label, t.slug, t.status, t.prio, ageText, t.title)
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
	next := map[string]string{"start": "в работе", "review": "на проверке", "done": "готово", "reject": "отклонён"}[o.cmd]
	if o.cmd == "done" {
		doer := executor(t.body)
		if o.doer != "" {
			doer = o.doer
		}
		if doer == "" {
			fmt.Fprintln(errout, "tk: executor unknown; pass --doer NAME")
			return exitNone
		}
		if doer == o.by {
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
	dstDir := filepath.Join(dir, "archive")
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
