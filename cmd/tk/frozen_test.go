package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/akit/proc"
)

// frozenProject makes HOME/work/sample with a ticket and an optional frozen file.
func frozenProject(t *testing.T, frozen string) (home, project, worktree string) {
	t.Helper()
	project, worktree = linkedProject(t)
	home = filepath.Dir(filepath.Dir(project))
	store := filepath.Join(home, "work", "tasks", "sample")
	if err := os.WriteFile(filepath.Join(store, "20261101-1000-HIGH-DO-subject.md"), []byte("# subject\n\nСтатус: открыт\nПричина: x\n\n## Приёмка\n- check\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if frozen != "" {
		dir := filepath.Join(home, ".config", "queue")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "frozen"), []byte(frozen), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home, project, worktree
}

// liveSession registers this test process under name, so it counts as running.
func liveSession(t *testing.T, home, name string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"pid":%d,"procStart":%q,"name":%q,"startedAt":1}`, os.Getpid(), st.Start, name)
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid())), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tkIn(t *testing.T, dir, name string, args ...string) (int, string, string) {
	t.Helper()
	t.Chdir(dir)
	previous := session
	session = func() string { return name }
	t.Cleanup(func() { session = previous })
	o, err := parseArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run(o, &out, &errOut)
	return code, out.String(), errOut.String()
}

const frozenLine = "~/work/sample\tuser\t2026-11-09\tstopped on purpose\n"

func refused(t *testing.T, dir, actor string, args ...string) {
	t.Helper()
	code, out, errOut := tkIn(t, dir, actor, append([]string{"next"}, args...)...)
	if code != exitNone || strings.Contains(out, "subject") || !strings.Contains(errOut, "stopped on purpose") {
		t.Errorf("next by %q: code %d, out %q, err %q", actor, code, out, errOut)
	}
	code, out, errOut = tkIn(t, dir, actor, append([]string{"start", "subject"}, args...)...)
	if code != exitNone || !strings.Contains(errOut, "stopped on purpose") {
		t.Errorf("start by %q: code %d, out %q, err %q", actor, code, out, errOut)
	}
}

func TestFrozenRefusesAnyoneButLiveMaster(t *testing.T) {
	home, project, worktree := frozenProject(t, frozenLine)
	refused(t, project, "admin")
	refused(t, worktree, "admin")
	refused(t, project, "", "--by", "sample-master") // no session: nothing says it is the master
	refused(t, project, "sample-master")
	liveSession(t, home, "admin")
	refused(t, project, "admin")
}

func TestFrozenNameGivenByFlagDoesNotCount(t *testing.T) {
	home, project, _ := frozenProject(t, frozenLine)
	liveSession(t, home, "sample-master")
	refused(t, project, "admin", "--by", "sample-master")
}

func nextGives(t *testing.T, dir, actor string, args ...string) {
	t.Helper()
	code, out, errOut := tkIn(t, dir, actor, append([]string{"next"}, args...)...)
	if code != 0 || !strings.Contains(out, "subject") {
		t.Errorf("next by %q %v: code %d, out %q, err %q", actor, args, code, out, errOut)
	}
}

func writeNames(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "cl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "names.tsv"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenByCountsOnlyWithoutSessionAndOnlyForNext(t *testing.T) {
	home, project, _ := frozenProject(t, frozenLine)
	liveSession(t, home, "sample-master")
	nextGives(t, project, "", "--by", "sample-master")
	if code, _, errOut := tkIn(t, project, "", "start", "subject", "--by", "sample-master"); code != exitNone || !strings.Contains(errOut, "stopped on purpose") {
		t.Errorf("start without a session: code %d, err %q", code, errOut)
	}
	if code, out, errOut := tkIn(t, project, "", "next", "--by", "someone-else"); code != exitNone || strings.Contains(out, "subject") {
		t.Errorf("next by another name: code %d, out %q, err %q", code, out, errOut)
	}
	if code, out, _ := tkIn(t, project, "", "next"); code == 0 || strings.Contains(out, "subject") {
		t.Errorf("next without a name: code %d, out %q", code, out)
	}
	if code, out, _ := tkIn(t, project, "other-master", "next", "--by", "sample-master"); code != exitNone || strings.Contains(out, "subject") {
		t.Errorf("known session of another master with --by: code %d, out %q", code, out)
	}
}

func TestFrozenByOfADeadMasterIsRefused(t *testing.T) {
	home, project, _ := frozenProject(t, frozenLine)
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"pid":%d,"procStart":"1","name":"sample-master","startedAt":1}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := tkIn(t, project, "", "next", "--by", "sample-master"); code != exitNone || strings.Contains(out, "subject") {
		t.Errorf("pid reused by another process: code %d, out %q, err %q", code, out, errOut)
	}
}

func TestFrozenMasterNameFromTheTable(t *testing.T) {
	home, project, worktree := frozenProject(t, frozenLine)
	writeNames(t, home, "# comment\n~\tlead\n~/work\twork-admin\n~/work/sample\tspecial-master\n")
	liveSession(t, home, "sample-master")
	if code, out, errOut := tkIn(t, project, "sample-master", "next"); code != exitNone || strings.Contains(out, "subject") || !strings.Contains(errOut, "special-master") {
		t.Errorf("the folder's name when the table gives another: code %d, out %q, err %q", code, out, errOut)
	}
	if code, out, _ := tkIn(t, project, "", "next", "--by", "sample-master"); code != exitNone || strings.Contains(out, "subject") {
		t.Errorf("--by the folder's name: code %d, out %q", code, out)
	}
	liveSession(t, home, "special-master")
	sessionsDir := filepath.Join(home, ".claude", "sessions")
	own, err := os.ReadFile(filepath.Join(sessionsDir, fmt.Sprintf("%d.json", os.Getpid())))
	if err != nil {
		t.Fatal(err)
	}
	// Both alive: the folder's name still is not the master.
	if err := os.WriteFile(filepath.Join(sessionsDir, "folder-name.json"), []byte(strings.Replace(string(own), "special-master", "sample-master", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := tkIn(t, project, "", "next", "--by", "sample-master"); code != exitNone || strings.Contains(out, "subject") {
		t.Errorf("--by the folder's name with both alive: code %d, out %q", code, out)
	}
	for _, dir := range []string{project, worktree} {
		nextGives(t, dir, "special-master")
	}
	nextGives(t, project, "", "--by", "special-master")
	_, _, errOut := tkIn(t, project, "special-master", "next")
	if strings.Contains(errOut, "line ignored") {
		t.Errorf("a home line of the table is not an error: %q", errOut)
	}
}

func TestFrozenTableWithoutTheProjectKeepsTheFolderName(t *testing.T) {
	home, project, _ := frozenProject(t, frozenLine)
	writeNames(t, home, "~/work/other\tother-name\n")
	liveSession(t, home, "sample-master")
	nextGives(t, project, "sample-master")
}

func TestFrozenTableProblemsGiveNoOtherName(t *testing.T) {
	home, project, _ := frozenProject(t, frozenLine)
	liveSession(t, home, "sample-master")
	writeNames(t, home, "~/work/sample\t\n")
	if code, out, errOut := tkIn(t, project, "sample-master", "next"); code == 0 || strings.Contains(out, "subject") || !strings.Contains(errOut, "names.tsv:1") {
		t.Errorf("line without a name: code %d, out %q, err %q", code, out, errOut)
	}
	writeNames(t, home, "relative\tx\n~/work/sample\tsample-master\n")
	if code, _, errOut := tkIn(t, project, "sample-master", "next"); code != 0 || !strings.Contains(errOut, "names.tsv:1: line ignored") {
		t.Errorf("a bad line concerns only itself: code %d, err %q", code, errOut)
	}
	path := filepath.Join(home, ".config", "cl", "names.tsv")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := tkIn(t, project, "sample-master", "next"); code == 0 || strings.Contains(out, "subject") || !strings.Contains(errOut, "read session names") {
		t.Errorf("unreadable table: code %d, out %q, err %q", code, out, errOut)
	}
}

func TestFrozenLiveMasterWorksAsUsual(t *testing.T) {
	home, project, worktree := frozenProject(t, frozenLine)
	liveSession(t, home, "sample-master")
	for _, dir := range []string{project, worktree} {
		if code, out, errOut := tkIn(t, dir, "sample-master", "next"); code != 0 || !strings.Contains(out, "subject") {
			t.Errorf("next in %s: code %d, out %q, err %q", dir, code, out, errOut)
		}
	}
	if code, _, errOut := tkIn(t, project, "SAMPLE-master", "start", "subject"); code != 0 {
		t.Errorf("start, name in another case: code %d, err %q", code, errOut)
	}
}

func TestFrozenTicketsStillAccumulate(t *testing.T) {
	_, project, _ := frozenProject(t, frozenLine)
	if code, _, errOut := tkIn(t, project, "admin", "new", "more", "--by", "admin"); code != 0 {
		t.Fatalf("new: code %d, err %q", code, errOut)
	}
	if code, out, errOut := tkIn(t, project, "admin", "ls"); code != 0 || !strings.Contains(out, "more") {
		t.Fatalf("ls: code %d, out %q, err %q", code, out, errOut)
	}
	if code, _, errOut := tkIn(t, project, "admin", "note", "more", "text", "--by", "admin"); code != 0 {
		t.Fatalf("note: code %d, err %q", code, errOut)
	}
}

func TestFrozenOtherProjectOrNoFileAsUsual(t *testing.T) {
	for name, frozen := range map[string]string{
		"no file":        "",
		"other project":  "~/work/sample-api\tuser\t2026-11-09\tnot this one\n",
		"comment only":   "# ~/work/sample\tuser\t2026-11-09\tnot in force\n",
		"another prefix": "~/work/sam\tuser\t2026-11-09\tnot this one\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, project, _ := frozenProject(t, frozen)
			if code, out, errOut := tkIn(t, project, "admin", "next"); code != 0 || !strings.Contains(out, "subject") {
				t.Errorf("next: code %d, out %q, err %q", code, out, errOut)
			}
			if code, _, errOut := tkIn(t, project, "admin", "start", "subject"); code != 0 {
				t.Errorf("start: code %d, err %q", code, errOut)
			}
		})
	}
}

// An incomplete line still names the project: it stays frozen, and says so.
func TestFrozenIncompleteLineStaysFrozen(t *testing.T) {
	for name, line := range map[string]string{"path only": "~/work/sample\n", "spaces": "~/work/sample user 2026-11-09 why\n", "no newline": "~/work/sample\tuser", "words before the first tab": "~/work/sample user\t2026-11-09\twhy\n", "tab only at the end": "~/work/sample u d why with\ttab\n"} {
		t.Run(name, func(t *testing.T) {
			_, project, _ := frozenProject(t, line)
			code, out, errOut := tkIn(t, project, "admin", "next")
			if code != exitNone || strings.Contains(out, "subject") || !strings.Contains(errOut, "incomplete") {
				t.Errorf("code %d, out %q, err %q", code, out, errOut)
			}
		})
	}
}

func TestFrozenLeadingWhitespaceKeepsTheFields(t *testing.T) {
	_, project, _ := frozenProject(t, "\t  ~/work/sample\tuser\t2026-11-09\tgiven reason\n")
	code, out, errOut := tkIn(t, project, "admin", "next")
	if code != exitNone || strings.Contains(out, "subject") || !strings.Contains(errOut, "given reason") {
		t.Errorf("code %d, out %q, err %q", code, out, errOut)
	}
}

func TestFrozenUnreadableFileIsAnError(t *testing.T) {
	home, project, _ := frozenProject(t, "")
	dir := filepath.Join(home, ".config", "queue", "frozen")
	if err := os.MkdirAll(dir, 0o755); err != nil { // a directory cannot be read as a file
		t.Fatal(err)
	}
	code, out, errOut := tkIn(t, project, "admin", "next")
	if code != exitUsage || strings.Contains(out, "subject") || !strings.Contains(errOut, "frozen") {
		t.Errorf("code %d, out %q, err %q", code, out, errOut)
	}
}

func TestFrozenSourceAndDoneHintGiveNoWork(t *testing.T) {
	home, project, _ := frozenProject(t, frozenLine)
	writeSource(t, filepath.Dir(project), `echo '[{"key":"K-ext","title":"ext","status":"To Do","prio":"HIGH","url":"u"}]'`)
	code, out, errOut := tkIn(t, project, "admin", "next")
	if code != exitNone || strings.Contains(out, "K-ext") {
		t.Errorf("next: code %d, out %q, err %q", code, out, errOut)
	}
	store := filepath.Join(home, "work", "tasks", "sample")
	if err := os.WriteFile(filepath.Join(store, "20261101-1001-MED-DO-begun.md"), []byte("# begun\n\nСтатус: в работе\nИсполнитель: gone\n\n## Приёмка\n- check\n\n## Журнал\n- 2026-11-01 10:00 в работе — gone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = tkIn(t, project, "checker", "done", "begun", "--check", "checked by test", "--by", "checker")
	if code != 0 || strings.Contains(out, "следующий") {
		t.Errorf("done: code %d, out %q, err %q", code, out, errOut)
	}
}

func TestFrozenSymlinkedProjectIsTheSame(t *testing.T) {
	home, project, _ := frozenProject(t, "~/link\tuser\t2026-11-09\tstopped on purpose\n")
	if err := os.Symlink(project, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	refused(t, project, "admin")
}

func TestFrozenCasesOfPathAreDifferentProjects(t *testing.T) {
	_, project, _ := frozenProject(t, "~/work/SAMPLE\tuser\t2026-11-09\tanother folder\n")
	if code, out, errOut := tkIn(t, project, "admin", "next"); code != 0 || !strings.Contains(out, "subject") {
		t.Errorf("code %d, out %q, err %q", code, out, errOut)
	}
}

// A listed parent or name prefix that exists is another folder, and a comment
// never names a project, whatever the working directory holds.
func TestFrozenExistingPrefixAndCommentDoNotCount(t *testing.T) {
	for name, frozen := range map[string]string{
		"existing prefix": "~/work/sam\tuser\t2026-11-09\tnot this one\n",
		"parent":          "~/work\tuser\t2026-11-09\tnot this one\n",
		"comment":         "#self\tuser\t2026-11-09\tnot in force\n",
	} {
		t.Run(name, func(t *testing.T) {
			home, project, _ := frozenProject(t, frozen)
			if err := os.MkdirAll(filepath.Join(home, "work", "sam"), 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(project, "#self")
			if err := os.Symlink(project, link); err != nil {
				t.Fatal(err)
			}
			if code, out, errOut := tkIn(t, project, "admin", "next"); code != 0 || !strings.Contains(out, "subject") {
				t.Errorf("code %d, out %q, err %q", code, out, errOut)
			}
		})
	}
}

// The hint of tk done is for the ticket's executor, whoever closes the ticket.
func TestFrozenDoneHintFollowsExecutor(t *testing.T) {
	home, project, _ := frozenProject(t, frozenLine)
	liveSession(t, home, "sample-master")
	store := filepath.Join(home, "work", "tasks", "sample")
	begun := "# begun\n\nСтатус: в работе\nИсполнитель: sample-master\n\n## Приёмка\n- check\n\n## Журнал\n- 2026-11-01 10:00 в работе — sample-master\n"
	if err := os.WriteFile(filepath.Join(store, "20261101-1001-MED-DO-begun.md"), []byte(begun), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := tkIn(t, project, "checker", "done", "begun", "--check", "checked by test", "--by", "checker")
	if code != 0 || !strings.Contains(out, "следующий: subject") {
		t.Errorf("code %d, out %q, err %q", code, out, errOut)
	}
}

func TestFrozenPathIsTheFirstTabField(t *testing.T) {
	home, project, _ := frozenProject(t, "~/work/sample x\tuser\t2026-11-09\tanother folder\n")
	if err := os.MkdirAll(filepath.Join(home, "work", "sample x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := tkIn(t, project, "admin", "next"); code != 0 || !strings.Contains(out, "subject") {
		t.Errorf("a path with a space froze its neighbour: code %d, out %q, err %q", code, out, errOut)
	}
	if matches, err := frozenLineNames("~/work/sample x\tuser\td\twhy", home, filepath.Join(home, "work", "sample x")); err != nil || !matches {
		t.Errorf("a path with a space is not matched: %v, %v", matches, err)
	}
}

// Anything but an absolute or ~/ path could name whatever the working directory
// is; an unresolvable line concerns its own folder only.
func TestFrozenBadPathsConcernOnlyThemselves(t *testing.T) {
	for line, warning := range map[string]string{".": "absolute", "work/sample": "absolute", "~/.config/queue/frozen/x": "not a directory"} {
		t.Run(line, func(t *testing.T) {
			_, project, _ := frozenProject(t, line+"\tuser\t2026-11-09\tbad path\n")
			code, out, errOut := tkIn(t, project, "admin", "next")
			if code != 0 || !strings.Contains(out, "subject") {
				t.Errorf("code %d, out %q, err %q", code, out, errOut)
			}
			if !strings.Contains(errOut, warning) {
				t.Errorf("warning %q is missing: %q", warning, errOut)
			}
		})
	}
}
