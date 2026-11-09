package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// takeCase is one ticket alone in its own project, so that tk next answers about
// exactly that ticket.
type takeCase struct {
	project, file, status, extra string
	want                         bool
}

func takeCases() []takeCase {
	return []takeCase{
		{"lowclosed", "20261101-1000-LOW-DO-subject.md", "открыт", "", false},
		{"medopen", "20261101-1000-MED-DO-subject.md", "открыт", "", true},
		{"medclosed", "20261101-1000-MED-HUNT-subject.md", "открыт", "", false},
		{"high", "20261101-1000-HIGH-DO-subject.md", "открыт", "", true},
		{"waitfuture", "20261101-1000-HIGH-DO-subject.md", "ждёт", "Ждёт: 2099-01-01 09:00: later\n", false},
		{"waitdue", "20261101-1000-HIGH-DO-subject.md", "ждёт", "Ждёт: 2020-01-01 09:00: due\n", true},
		{"blocked", "20261101-1000-HIGH-DO-subject.md", "открыт", "Заблокирован: other\n", true},
		{"journal", "20261101-1000-HIGH-DO-subject.md", "открыт", "Вид: журнал\n", false},
		{"donealready", "20261101-1000-HIGH-DO-subject.md", "готово", "", false},
		{"startedbymaster", "20261101-1000-LOW-DO-subject.md", "в работе", "@startedbymaster-master", true},
		{"startedbyother", "20261101-1000-LOW-DO-subject.md", "в работе", "@someone-else", false},
		{"startedbytabled", "20261101-1000-LOW-DO-subject.md", "в работе", "@captain", true},
		{"frozendead", "20261101-1000-HIGH-DO-subject.md", "открыт", "", false},
		{"frozenlive", "20261101-1000-HIGH-DO-subject.md", "открыт", "", true},
	}
}

func writeTakeTicket(t *testing.T, base string, tc takeCase) string {
	t.Helper()
	project := filepath.Join(base, tc.project)
	dir := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	header := tc.extra
	if who, ok := strings.CutPrefix(tc.extra, "@"); ok {
		header = "Исполнитель: " + who + "\n"
	}
	body := "# subject\n\nСтатус: " + tc.status + "\n" + header + "\n## Приёмка\n- check\n\n## Журнал\n"
	if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return project
}

func writeTakeGate(t *testing.T, home string, live, open []string) {
	t.Helper()
	dir := filepath.Join(home, ".local", "state", "queue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(map[string]any{"at": time.Now().Format(time.RFC3339), "live": live, "open": open, "open_by_root": map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gate.json"), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func takeSetup(t *testing.T) (home, base string, cases []takeCase) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	base = filepath.Join(home, "git")
	cases = takeCases()
	live := make([]string, 0, len(cases))
	for _, tc := range cases {
		writeTakeTicket(t, base, tc)
		live = append(live, tc.project)
	}
	writeTakeGate(t, home, live, []string{"MED-DO"})
	frozenDir := filepath.Join(home, ".config", "queue")
	if err := os.MkdirAll(frozenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	frozen := "~/git/frozendead\tuser\t2026-11-09\tstopped\n~/git/frozenlive\tuser\t2026-11-09\tstopped\n"
	if err := os.WriteFile(filepath.Join(frozenDir, "frozen"), []byte(frozen), 0o644); err != nil {
		t.Fatal(err)
	}
	writeNames(t, home, "~/git/startedbytabled\tcaptain\n")
	liveSession(t, home, "frozenlive-master")
	return home, base, cases
}

type takeRow struct {
	Path    string `json:"path"`
	Repo    string `json:"repo"`
	CanTake *bool  `json:"can_take"`
}

func listTakes(t *testing.T, dir string, args ...string) (map[string]bool, string) {
	t.Helper()
	code, out, errOut := tkIn(t, dir, "", append([]string{"ls", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, errOut)
	}
	var rows []takeRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	takes := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.CanTake == nil {
			t.Fatalf("row without can_take: %s", out)
		}
		takes[row.Repo] = *row.CanTake
	}
	return takes, errOut
}

func masterOf(project string) string {
	if project == "startedbytabled" {
		return "captain"
	}
	return project + "-master"
}

func nextFinds(t *testing.T, project, master string) bool {
	t.Helper()
	code, out, errOut := tkIn(t, project, "", "next", "--by", master)
	if code != 0 && code != exitNone {
		t.Fatalf("next exit %d: %s", code, errOut)
	}
	return code == 0 && strings.Contains(out, "subject")
}

func TestCanTakeIsWhatNextGives(t *testing.T) {
	_, base, cases := takeSetup(t)
	takes, _ := listTakes(t, base, "--all", "--root", base)
	for _, tc := range cases {
		t.Run(tc.project, func(t *testing.T) {
			got, ok := takes[tc.project]
			if !ok {
				t.Fatalf("no row for %s in %v", tc.project, takes)
			}
			if got != tc.want {
				t.Errorf("can_take = %v, want %v", got, tc.want)
			}
			if next := nextFinds(t, filepath.Join(base, tc.project), masterOf(tc.project)); next != got {
				t.Errorf("tk next gives it: %v, can_take: %v", next, got)
			}
		})
	}
}

func TestCanTakeFollowsTheGate(t *testing.T) {
	home, base, cases := takeSetup(t)
	live := make([]string, 0, len(cases))
	for _, tc := range cases {
		live = append(live, tc.project)
	}
	writeTakeGate(t, home, live, []string{"LOW-DO"})
	takes, _ := listTakes(t, base, "--all", "--root", base)
	if !takes["lowclosed"] || takes["medopen"] {
		t.Fatalf("open bar moved to LOW-DO: lowclosed %v, medopen %v", takes["lowclosed"], takes["medopen"])
	}
	if !nextFinds(t, filepath.Join(base, "lowclosed"), "lowclosed-master") {
		t.Fatal("tk next does not give what can_take says")
	}
}

func TestCanTakeWithoutAGateIsOnlyCritAndHigh(t *testing.T) {
	home, base, _ := takeSetup(t)
	if err := os.Remove(filepath.Join(home, ".local", "state", "queue", "gate.json")); err != nil {
		t.Fatal(err)
	}
	takes, _ := listTakes(t, base, "--all", "--root", base)
	if takes["medopen"] || !takes["high"] {
		t.Fatalf("medopen %v, high %v", takes["medopen"], takes["high"])
	}
}

func TestCanTakeDoesNotDependOnTheStatusFilter(t *testing.T) {
	_, base, _ := takeSetup(t)
	takes, _ := listTakes(t, base, "--root", base, "--status", "ждёт")
	if !takes["waitdue"] || takes["waitfuture"] {
		t.Fatalf("waitdue %v, waitfuture %v", takes["waitdue"], takes["waitfuture"])
	}
}

func TestCanTakeOnlyInTheList(t *testing.T) {
	_, base, _ := takeSetup(t)
	project := filepath.Join(base, "high")
	code, out, errOut := tkIn(t, project, "", "show", "subject", "--json")
	if code != 0 {
		t.Fatalf("show exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "can_take") {
		t.Fatalf("show has the field: %s", out)
	}
}

func TestCanTakeInALinkedWorktreeIsOfItsProject(t *testing.T) {
	home, project, worktree := frozenProject(t, frozenLine)
	t.Setenv("HOME", home)
	code, out, errOut := tkIn(t, worktree, "", "ls", "--json")
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, `"can_take":false`) {
		t.Fatalf("a frozen project without its master, from a worktree: %s", out)
	}
	liveSession(t, home, filepath.Base(project)+"-master")
	code, out, errOut = tkIn(t, worktree, "", "ls", "--json")
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, `"can_take":true`) {
		t.Fatalf("a frozen project with its master, from a worktree: %s", out)
	}
}

func TestCanTakeBadNameRowOfAFrozenProjectCostsOnlyItself(t *testing.T) {
	home, base, _ := takeSetup(t)
	writeNames(t, home, "~/git/frozenlive\n")
	writeTakeTicket(t, base, takeCase{project: "frozenlive", file: "20261102-1000-HIGH-DO-second.md", status: "открыт"})
	takes, errOut := listTakes(t, base, "--all", "--root", base)
	if takes["frozenlive"] {
		t.Fatal("no master can be named: can_take must be false")
	}
	if !takes["medopen"] {
		t.Fatal("another project lost its answer")
	}
	if strings.Count(errOut, "can_take is false") != 1 || !strings.Contains(errOut, "names.tsv:1") {
		t.Fatalf("the reason once, with its place: %q", errOut)
	}
	if code, _, _ := tkIn(t, filepath.Join(base, "frozenlive"), "", "next", "--by", "frozenlive-master"); code == 0 {
		t.Fatal("tk next must not give work here")
	}
}

func TestCanTakeNameTableDecidesOnlyForAFrozenProject(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table func(t *testing.T, home string)
	}{
		{"row without a name", func(t *testing.T, home string) { writeNames(t, home, "~/git/high\n") }},
		{"table cannot be read", func(t *testing.T, home string) {
			if err := os.Remove(filepath.Join(home, ".config", "cl", "names.tsv")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(home, ".config", "cl", "names.tsv"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, base, _ := takeSetup(t)
			tc.table(t, home)
			takes, errOut := listTakes(t, filepath.Join(base, "high"))
			if !takes[""] {
				t.Fatal("tk next gives this ticket, can_take must say so")
			}
			if !strings.Contains(errOut, "work in progress is matched to high-master") {
				t.Fatalf("the less exact answer is not reported: %q", errOut)
			}
			if !nextFinds(t, filepath.Join(base, "high"), "high-master") {
				t.Fatal("tk next does not give it")
			}
		})
	}
}

func TestCanTakeFrozenMasterNamedByTheTable(t *testing.T) {
	home, base, _ := takeSetup(t)
	writeTakeTicket(t, base, takeCase{project: "frozennamed", file: "20261101-1000-HIGH-DO-subject.md", status: "открыт"})
	list := filepath.Join(home, ".config", "queue", "frozen")
	if err := os.WriteFile(list, []byte("~/git/frozennamed\tuser\t2026-11-09\tstopped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeNames(t, home, "~/git/frozennamed\tkeeper\n")
	liveSession(t, home, "keeper")
	takes, _ := listTakes(t, base, "--all", "--root", base)
	if !takes["frozennamed"] {
		t.Fatal("its master by the table is running")
	}
	if !nextFinds(t, filepath.Join(base, "frozennamed"), "keeper") {
		t.Fatal("tk next does not give it")
	}
}

func TestCanTakeBadGateRootGivesNothingAndSaysSo(t *testing.T) {
	home, base, _ := takeSetup(t)
	file := filepath.Join(home, "plain")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(map[string]any{"at": time.Now().Format(time.RFC3339), "live": []string{"high"}, "open": []string{}, "open_by_root": map[string][]string{filepath.Join(file, "below"): {"MED-DO"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".local", "state", "queue", "gate.json"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	takes, errOut := listTakes(t, filepath.Join(base, "high"))
	if takes[""] || !strings.Contains(errOut, "can_take is false") {
		t.Fatalf("takes %v, stderr %q", takes, errOut)
	}
	if code, _, _ := tkIn(t, filepath.Join(base, "high"), "", "next", "--by", "high-master"); code == 0 {
		t.Fatal("tk next must fail here too")
	}
}

func TestCanTakeThroughASymlinkedRootKnowsTheProject(t *testing.T) {
	home, base, _ := takeSetup(t)
	links := filepath.Join(home, "links")
	if err := os.MkdirAll(links, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "lowclosed"), filepath.Join(links, "other")); err != nil { // the name is not in the live list
		t.Fatal(err)
	}
	link := filepath.Join(links, "other")
	takes, _ := listTakes(t, link, "--all", "--root", link)
	if len(takes) != 1 {
		t.Fatalf("one row expected: %v", takes)
	}
	for _, can := range takes {
		if can {
			t.Fatalf("a project under the bar, by another name: %v", takes)
		}
	}
}

func TestCanTakeBadFileLinesAddNothingToExternalStderr(t *testing.T) {
	home, base, _ := takeSetup(t)
	writeNames(t, home, "relative/path\tname\n~/git/high\n")
	list := filepath.Join(home, ".config", "queue", "frozen")
	if err := os.WriteFile(list, []byte("relative/path\tuser\t2026-11-09\tstopped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSource(t, base, `echo '[{"key":"K1","title":"one","status":"open","prio":"HIGH","url":"u"}]'`)
	code, out, errOut := tkIn(t, filepath.Join(base, "high"), "", "external", "--json")
	if code != 0 || errOut != "" || !strings.Contains(out, `"can_take":true`) {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestCanTakeUnreadableFrozenListGivesNothingAndSaysSo(t *testing.T) {
	home, base, _ := takeSetup(t)
	frozen := filepath.Join(home, ".config", "queue", "frozen")
	if err := os.Remove(frozen); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(frozen, 0o755); err != nil {
		t.Fatal(err)
	}
	takes, errOut := listTakes(t, base, "--all", "--root", base)
	if takes["high"] || takes["medopen"] {
		t.Fatalf("tk next fails here and gives nothing: %v", takes)
	}
	if !strings.Contains(errOut, "read frozen list") {
		t.Fatalf("no reason: %q", errOut)
	}
}

func TestCanTakeExternal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frozen bool
		open   []string
		want   string
	}{
		{"open project", false, nil, "HIGH-1 true,LOW-2 false,LOW-3 true"},
		{"frozen without its master", true, nil, "HIGH-1 false,LOW-2 false,LOW-3 false"},
		{"one difficulty open is not enough", false, []string{"LOW-DO"}, "HIGH-1 true,LOW-2 false,LOW-3 true"},
		{"every difficulty open", false, []string{"LOW-DO", "LOW-HUNT", "LOW-ARCH"}, "HIGH-1 true,LOW-2 true,LOW-3 true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			project := filepath.Join(home, "git", "sample")
			if err := os.MkdirAll(filepath.Join(project, "docs", "tasks"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeTakeGate(t, home, []string{"sample"}, tc.open)
			if tc.frozen {
				dir := filepath.Join(home, ".config", "queue")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "frozen"), []byte("~/git/sample\tuser\t2026-11-09\tstopped\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			writeSource(t, filepath.Dir(project), `printf '%s\n' '[{"key":"HIGH-1","title":"a","status":"new","prio":"HIGH","url":"u"},{"key":"LOW-2","title":"b","status":"new","prio":"LOW","url":"u"},{"key":"LOW-3","title":"c","status":"doing","prio":"LOW","url":"u","started":true}]'`)
			code, out, errOut := tkIn(t, project, "", "external", "--json")
			if code != 0 {
				t.Fatalf("external exit %d: %s", code, errOut)
			}
			var items []externalTask
			if err := json.Unmarshal([]byte(out), &items); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			got := make([]string, 0, len(items))
			for _, item := range items {
				got = append(got, fmt.Sprintf("%s %v", item.Key, item.CanTake))
			}
			if joined := strings.Join(got, ","); joined != tc.want {
				t.Fatalf("got %s, want %s", joined, tc.want)
			}
		})
	}
}

// Tickets tracked in git live in each worktree, but belong to the main project.
func TestCanTakeInAWorktreeWithItsOwnTicketsIsOfTheMainProject(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	project := filepath.Join(home, "git", "tracked")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInTest(t, project, "init", "-q", "-b", "main")
	writeTakeTicket(t, filepath.Dir(project), takeCase{project: "tracked", file: "20261101-1000-HIGH-DO-subject.md", status: "открыт"})
	gitInTest(t, project, "add", "-A")
	gitInTest(t, project, "commit", "-q", "-m", "init")
	worktree := filepath.Join(home, "trees", "feature")
	gitInTest(t, project, "worktree", "add", "-q", "-b", "feature", worktree)
	dir := filepath.Join(home, ".config", "queue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frozen"), []byte("~/git/tracked\tuser\t2026-11-09\tstopped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := tkIn(t, worktree, "", "ls", "--json")
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, `"can_take":false`) {
		t.Fatalf("the worktree of a frozen project: %s", out)
	}
}
