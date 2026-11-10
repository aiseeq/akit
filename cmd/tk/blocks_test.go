package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/akit/queuegate"
)

func TestCloseBlockingRelations(t *testing.T) {
	for _, tc := range []struct {
		name, closedSlug, closedBody, neighborSlug, neighborBody, wantOut, wantRemoved string
	}{
		{
			name: "blocking ticket closes", closedSlug: "x", closedBody: "# x\n\nСтатус: готово\nБлокирует: t\n",
			neighborSlug: "t", neighborBody: "# t\n\nСтатус: открыт\nЗаблокирован: x\n",
			wantOut: "разблокирован t", wantRemoved: blockedPrefix + "x",
		},
		{
			name: "blocked ticket closes", closedSlug: "t", closedBody: "# t\n\nСтатус: готово\nЗаблокирован: x\n",
			neighborSlug: "x", neighborBody: "# x\n\nСтатус: открыт\nБлокирует: t\n",
			wantRemoved: blocksPrefix + "t",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "docs", "tasks")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			closedPath := filepath.Join(dir, "20260101-0000-MED-"+tc.closedSlug+".md")
			neighborPath := filepath.Join(dir, "20260101-0000-MED-"+tc.neighborSlug+".md")
			if err := os.WriteFile(closedPath, []byte(tc.closedBody), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(neighborPath, []byte(tc.neighborBody), 0644); err != nil {
				t.Fatal(err)
			}
			var out, errout bytes.Buffer
			closed, err := find(dir, tc.closedSlug)
			if err != nil {
				t.Fatal(err)
			}
			if code := closeBlockRelations(closed, options{by: "tester"}, &out, &errout, 0); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, errout.String())
			}
			got, err := os.ReadFile(neighborPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(got), tc.wantRemoved) {
				t.Fatalf("relation remains:\n%s", got)
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("stdout=%q, want %q", out.String(), tc.wantOut)
			}
		})
	}
}

func TestAttachBlocksWalksEveryCycleLink(t *testing.T) {
	root, dir := blockTestProject(t)
	x := blockTestWrite(t, dir, "x", "MED", "# x\n\nСтатус: открыт\nЗаблокирован: missing\nЗаблокирован: t\n")
	blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: открыт\n")
	var out, errout bytes.Buffer
	code := attachBlocks(x, options{baseRoot: root, mainRoot: root, by: "tester"}, &out, &errout, []string{"t"})
	if code == 0 || !strings.Contains(errout.String(), "x → t") {
		t.Fatalf("cycle result code=%d stderr=%q", code, errout.String())
	}
	if got, _ := os.ReadFile(x.path); strings.Contains(string(got), blocksPrefix) {
		t.Fatalf("cycle wrote blocker:\n%s", got)
	}
}

func TestLintReportsBlockerBelowBlockedTicket(t *testing.T) {
	root, dir := blockTestProject(t)
	ticket := blockTestWrite(t, dir, "t", "HIGH", "# t\n\nСтатус: открыт\nЗаблокирован: x\n")
	blockTestWrite(t, dir, "x", "LOW", "# x\n\nСтатус: открыт\nБлокирует: t\n")
	issues, err := lintTicket(ticket.path, "t.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(issues, "\n"), "priority below blocking ticket x") {
		t.Fatalf("lint issues: %v (root %s)", issues, root)
	}
}

func TestLoweringBlockedPriorityNote(t *testing.T) {
	_, dir := blockTestProject(t)
	ticket := blockTestWrite(t, dir, "t", "HIGH", "# t\n\nСтатус: открыт\nЗаблокирован: x\n")
	blockTestWrite(t, dir, "x", "HIGH", "# x\n\nСтатус: открыт\nБлокирует: t\n")
	var errout bytes.Buffer
	notePriorityLowered(ticket, options{prio: "MED", by: "tester"}, &errout)
	got, err := os.ReadFile(filepath.Join(dir, "20260101-0000-HIGH-x.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "заблокированный t понижен, приоритет не менялся: понизь, если не нужен") {
		t.Fatalf("missing downgrade note:\n%s", got)
	}
}

func TestTicketRefNormalizesHomeAndRejectsNestedProject(t *testing.T) {
	home := t.TempDir()
	realHome := filepath.Join(home, "real")
	if err := os.MkdirAll(filepath.Join(realHome, "work", "a"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realHome, "work", "b"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realHome, "work", "sub", "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "home-link")
	if err := os.Symlink(realHome, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", alias)
	got, err := ticketRef(filepath.Join(alias, "work", "a"), filepath.Join(realHome, "work", "b"), "t")
	if err != nil || got != "work/b/t" {
		t.Fatalf("ref=%q err=%v", got, err)
	}
	if _, err = ticketRef(filepath.Join(alias, "work", "a"), filepath.Join(realHome, "work", "sub", "nested"), "t"); err == nil {
		t.Fatal("nested project got an unresolvable link")
	}
}

func TestRaiseLogsBlockedTicketReferenceFromBlocker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tProject := filepath.Join(home, "work", "a")
	xProject := filepath.Join(home, "git", "b")
	tDir := filepath.Join(tProject, "docs", "tasks")
	xDir := filepath.Join(xProject, "docs", "tasks")
	if err := os.MkdirAll(tDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(xDir, 0755); err != nil {
		t.Fatal(err)
	}
	ticket := blockTestWrite(t, tDir, "t", "HIGH", "# t\n\nСтатус: открыт\nПричина: urgent\nЗаблокирован: git/b/x\n")
	blockTestWrite(t, xDir, "x", "LOW", "# x\n\nСтатус: открыт\n")
	var out, errout bytes.Buffer
	if code := propagatePriority(ticket.path, options{by: "tester"}, &out, &errout); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errout.String())
	}
	if !strings.Contains(out.String(), "блокирует work/a/t") {
		t.Fatalf("output lacks blocker-relative target: %q", out.String())
	}
	got, err := os.ReadFile(filepath.Join(xDir, "20260101-0000-HIGH-x.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "Причина: блокирует work/a/t") || !strings.Contains(string(got), "приоритет LOW → HIGH: блокирует work/a/t") {
		t.Fatalf("journal/cause use wrong reference:\n%s", got)
	}
}

func TestPropagationSkipsArchivedNeighbor(t *testing.T) {
	root, dir := blockTestProject(t)
	archive, err := archiveDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archive, 0755); err != nil {
		t.Fatal(err)
	}
	ticket := blockTestWrite(t, dir, "t", "HIGH", "# t\n\nСтатус: открыт\nЗаблокирован: x\n")
	xPath := filepath.Join(archive, "20260101-0000-LOW-x.md")
	if err := os.WriteFile(xPath, []byte("# x\n\nСтатус: открыт\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := propagatePriority(ticket.path, options{by: "tester"}, &out, &errout); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "20260101-0000-HIGH-x.md")); !os.IsNotExist(err) {
		t.Fatalf("archived neighbor was raised: %v", err)
	}
}

func TestParallelAttachBlocksIsIdempotent(t *testing.T) {
	root, dir := blockTestProject(t)
	x := blockTestWrite(t, dir, "x", "MED", "# x\n\nСтатус: открыт\n")
	blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: открыт\n")
	var wg sync.WaitGroup
	errs := make(chan string, 12)
	for range 12 {
		wg.Go(func() {
			var out, errout bytes.Buffer
			if code := attachBlocks(x, options{baseRoot: root, mainRoot: root, by: "tester"}, &out, &errout, []string{"t"}); code != 0 {
				errs <- errout.String()
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("attach failed: %s", e)
	}
	ticket, _ := os.ReadFile(filepath.Join(dir, "20260101-0000-MED-t.md"))
	source, _ := os.ReadFile(x.path)
	if strings.Count(string(ticket), blockedPrefix+"x") != 1 || strings.Count(string(source), blocksPrefix+"t") != 1 {
		t.Fatalf("duplicate links:\nT=%s\nX=%s", ticket, source)
	}
	if strings.Count(string(ticket), "note — tester: блокирует x") != 1 || strings.Count(string(source), "note — tester: заблокирован t") != 1 {
		t.Fatalf("duplicate journal rows:\nT=%s\nX=%s", ticket, source)
	}
}

func TestParallelSetBlocksPreservesBothEdges(t *testing.T) {
	root, dir := blockTestProject(t)
	blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: открыт\n")
	blockTestWrite(t, dir, "x1", "MED", "# x1\n\nСтатус: открыт\n")
	blockTestWrite(t, dir, "x2", "MED", "# x2\n\nСтатус: открыт\n")
	var wg sync.WaitGroup
	codes, errs := make([]int, 2), make([]string, 2)
	for i, slug := range []string{"x1", "x2"} {
		wg.Go(func() {
			o, err := parseArgs([]string{"set", slug, "--blocks", "t", "--by", "tester"})
			if err != nil {
				codes[i], errs[i] = -1, err.Error()
				return
			}
			var out, errout bytes.Buffer
			codes[i] = runAtBase(root, dir, o, &out, &errout)
			errs[i] = errout.String()
		})
	}
	wg.Wait()
	for i, code := range codes {
		if code != 0 {
			t.Errorf("set %d: code=%d err=%q", i, code, errs[i])
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "20260101-0000-MED-t.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"x1", "x2"} {
		if !strings.Contains(string(got), blockedPrefix+slug) {
			t.Errorf("lost %s edge:\n%s", slug, got)
		}
	}
}

func TestRaiseKeepsConcurrentNeighborNotes(t *testing.T) {
	_, dir := blockTestProject(t)
	ticket := blockTestWrite(t, dir, "t", "HIGH", "# t\n\nСтатус: открыт\nЗаблокирован: x\n")
	blockTestWrite(t, dir, "x", "LOW", "# x\n\nСтатус: открыт\n")
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			_, err := mutateTask(dir, "x", func(current task) (string, error) {
				return journal(current.body, fmt.Sprintf("- %s note — tester: parallel-%02d", timeStamp(), i)), nil
			})
			errs <- err
		})
	}
	var out, errout bytes.Buffer
	wg.Go(func() {
		if code := propagatePriority(ticket.path, options{by: "tester"}, &out, &errout); code != 0 {
			t.Errorf("propagate code=%d stderr=%s", code, errout.String())
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*-x.md"))
	if err != nil || len(files) != 1 {
		t.Fatalf("raised files=%v err=%v", files, err)
	}
	got, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		if !strings.Contains(string(got), fmt.Sprintf("parallel-%02d", i)) {
			t.Errorf("note %02d lost", i)
		}
	}
	if !strings.Contains(filepath.Base(files[0]), "-HIGH-") {
		t.Fatalf("priority not raised: %s", files[0])
	}
}

func TestTransferUpdatesSameFolderBlockerAfterUnlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "git", "sample")
	dir := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(home, "git", ".tk-transfer")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"key\":\"EXT-1\"}'\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	xBody := "# x\n\nСтатус: открыт\nБлокирует: t\n\n## Журнал\n"
	tBody := "# t\n\nСтатус: открыт\nЗаблокирован: x\n\n## Журнал\n"
	xPath := filepath.Join(dir, "20260101-0000-MED-x.md")
	tPath := filepath.Join(dir, "20260101-0000-MED-t.md")
	if err := os.WriteFile(xPath, []byte(xBody), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tPath, []byte(tBody), 0644); err != nil {
		t.Fatal(err)
	}
	o := options{cmd: "transfer", id: "t", baseRoot: project, mainRoot: project, by: "tester"}
	var out, errout bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runLockedCommand(project, dir, o, &out, &errout) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("transfer code=%d stderr=%q", code, errout.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("transfer blocked while updating same-folder blocker")
	}
	got, err := os.ReadFile(xPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), blocksPrefix+"t") || !strings.Contains(string(got), "заблокированный t перенесён в EXT-1") {
		t.Fatalf("blocker not updated:\n%s", got)
	}
	if code := runLockedCommand(project, dir, o, &out, &errout); code != 0 {
		t.Fatalf("repeat transfer code=%d stderr=%q", code, errout.String())
	}
	got, err = os.ReadFile(xPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), "заблокированный t перенесён в EXT-1") != 1 {
		t.Fatalf("repeat transfer duplicated relation note:\n%s", got)
	}
}

func TestNextOrdersLocalBlockerAndRanksJSON(t *testing.T) {
	root, dir := blockTestProject(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	gateDir := filepath.Join(home, ".local", "state", "queue")
	if err := os.MkdirAll(gateDir, 0755); err != nil {
		t.Fatal(err)
	}
	gate := fmt.Sprintf(`{"at":%q,"open_by_root":{}}`, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(gateDir, "gate.json"), []byte(gate), 0644); err != nil {
		t.Fatal(err)
	}
	blocked := blockTestWrite(t, dir, "blocked", "MED", "# blocked\n\nСтатус: открыт\nЗаблокирован: blocker\n")
	blocker := blockTestWrite(t, dir, "blocker", "MED", "# blocker\n\nСтатус: открыт\nБлокирует: blocked\n")
	other := blockTestWrite(t, dir, "other", "MED", "# other\n\nСтатус: открыт\n")
	ordered := orderedNextCandidates([]task{blocked, blocker, other}, "", queuegate.Scope{Valid: true})
	if len(ordered) != 3 || ordered[0].slug != "blocker" || ordered[1].slug != "other" || ordered[2].slug != "blocked" {
		t.Fatalf("order = %v", taskSlugs(ordered))
	}
	var marked bytes.Buffer
	printTask(&marked, blocked, "", &bytes.Buffer{})
	if !strings.Contains(marked.String(), "заблокирован: blocker") {
		t.Fatalf("blocked marker missing: %q", marked.String())
	}
	var out, errout bytes.Buffer
	o := options{cmd: "next", by: "tester", json: true, mainRoot: root}
	if code := runAtBase(root, dir, o, &out, &errout); code != 0 {
		t.Fatalf("next code=%d stderr=%q", code, errout.String())
	}
	var next map[string]any
	if err := json.Unmarshal(out.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if next["slug"] != "blocker" {
		t.Fatalf("next JSON = %v", next)
	}
	out.Reset()
	if code := list(root, options{json: true, by: "tester"}, &out, &errout); code != 0 {
		t.Fatalf("ls code=%d stderr=%q", code, errout.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"blocker": 1, "other": 2, "blocked": 3}
	for _, row := range rows {
		if row["next_rank"] != want[row["slug"].(string)] {
			t.Fatalf("row rank = %v, want %v", row, want[row["slug"].(string)])
		}
	}
	if !strings.Contains(next["title"].(string), "blocker") {
		t.Fatalf("next output missing blocker: %v", next)
	}
}

// A closed or archived blocker whose link is still in the header blocks nothing;
// tickets created at the same time go by slug.
func TestNextIgnoresClosedLocalBlockerAndBreaksTiesBySlug(t *testing.T) {
	at := time.Unix(10, 0)
	tasks := []task{
		{slug: "b-held", status: "открыт", prio: "MED", body: "Заблокирован: done", created: at},
		{slug: "a-free", status: "открыт", prio: "MED", created: at},
		{slug: "c-free", status: "открыт", prio: "MED", created: at},
		{slug: "done", status: "готово", prio: "MED", created: time.Unix(1, 0)},
	}
	for _, input := range [][]task{tasks, {tasks[2], tasks[0], tasks[3], tasks[1]}} {
		if got := strings.Join(taskSlugs(orderedNextCandidates(input, "", queuegate.Scope{Valid: true})), ","); got != "a-free,b-held,c-free" {
			t.Fatalf("order = %s", got)
		}
	}
}

// A blocker open in another project or moved to an external tracker puts the
// blocked ticket after the clear ones; a link to a ticket that is gone does not.
func TestNextDemotesForeignAndJiraBlockedTickets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	otherDir := filepath.Join(home, "git", "other", "docs", "tasks")
	if err := os.MkdirAll(otherDir, 0755); err != nil {
		t.Fatal(err)
	}
	blockTestWrite(t, otherDir, "holder", "MED", "# holder\n\nСтатус: открыт\n")
	_, dir := blockTestProject(t)
	ledger := `{"at":"2026-01-01T00:00:00Z","slug":"moved","stem":"20260101-0000-MED-moved","key":"EXT-123","url":"https://tracker.example/EXT-123"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".transferred.jsonl"), []byte(ledger), 0644); err != nil {
		t.Fatal(err)
	}
	// Slugs sort against the expected order, so only blocking can explain it.
	foreign := blockTestWrite(t, dir, "a-foreign", "MED", "# a-foreign\n\nСтатус: открыт\nЗаблокирован: git/other/holder\n")
	jira := blockTestWrite(t, dir, "b-jira", "MED", "# b-jira\n\nСтатус: открыт\nЗаблокирован: moved\n")
	gone := blockTestWrite(t, dir, "y-gone", "MED", "# y-gone\n\nСтатус: открыт\nЗаблокирован: git/other/missing\n")
	clear := blockTestWrite(t, dir, "z-clear", "MED", "# z-clear\n\nСтатус: открыт\n")
	ordered := orderedNextCandidates([]task{foreign, jira, gone, clear}, "", queuegate.Scope{Valid: true})
	if got := strings.Join(taskSlugs(ordered), ","); got != "y-gone,z-clear,a-foreign,b-jira" {
		t.Fatalf("order = %s", got)
	}
}

func TestNextOrderingUsesBlockDepthAndIgnoresInputOrder(t *testing.T) {
	tasks := []task{
		{slug: "x", status: "открыт", prio: "MED", body: "Заблокирован: y", created: time.Unix(1, 0)},
		{slug: "y", status: "открыт", prio: "MED", body: "Заблокирован: z", created: time.Unix(3, 0)},
		{slug: "z", status: "открыт", prio: "MED", created: time.Unix(4, 0)},
		{slug: "w", status: "открыт", prio: "MED", body: "Заблокирован: v", created: time.Unix(2, 0)},
	}
	// v is in progress with someone else: not a candidate, yet it still blocks w.
	v := task{slug: "v", status: "в работе", prio: "MED", body: "Исполнитель: someone-else", created: time.Unix(5, 0)}
	want := "z,w,y,x"
	for i := range tasks {
		for j := range tasks {
			for k := range tasks {
				for l := range tasks {
					if i == j || i == k || i == l || j == k || j == l || k == l {
						continue
					}
					input := []task{tasks[i], tasks[j], tasks[k], tasks[l], v}
					got := strings.Join(taskSlugs(orderedNextCandidates(input, "", queuegate.Scope{Valid: true})), ",")
					if got != want {
						t.Fatalf("order for permutation %v = %s, want %s", taskSlugs(input), got, want)
					}
				}
			}
		}
	}
}

func TestNextBlockDepthHandlesCyclesDeterministically(t *testing.T) {
	tasks := []task{
		{slug: "a", status: "открыт", prio: "MED", body: "Заблокирован: b", created: time.Unix(1, 0)},
		{slug: "b", status: "открыт", prio: "MED", body: "Заблокирован: a", created: time.Unix(2, 0)},
	}
	first := strings.Join(taskSlugs(orderedNextCandidates(tasks, "", queuegate.Scope{Valid: true})), ",")
	second := strings.Join(taskSlugs(orderedNextCandidates([]task{tasks[1], tasks[0]}, "", queuegate.Scope{Valid: true})), ",")
	if first != second {
		t.Fatalf("cycle order depends on input: %s vs %s", first, second)
	}
}

func TestListNextRankDefaultsToProjectMaster(t *testing.T) {
	root, dir := blockTestProject(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	gateDir := filepath.Join(home, ".local", "state", "queue")
	if err := os.MkdirAll(gateDir, 0755); err != nil {
		t.Fatal(err)
	}
	gate := fmt.Sprintf(`{"at":%q,"open_by_root":{}}`, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(gateDir, "gate.json"), []byte(gate), 0644); err != nil {
		t.Fatal(err)
	}
	master := filepath.Base(root) + "-master"
	blockTestWrite(t, dir, "working", "MED", fmt.Sprintf("# working\n\nСтатус: в работе\nИсполнитель: %s\n", master))
	blockTestWrite(t, dir, "open", "MED", "# open\n\nСтатус: открыт\n")
	var nextOut, errout bytes.Buffer
	if code := runAtBase(root, dir, options{cmd: "next", by: master}, &nextOut, &errout); code != 0 {
		t.Fatalf("next code=%d stderr=%q", code, errout.String())
	}
	want := strings.Fields(nextOut.String())[0]
	var listOut bytes.Buffer
	if code := list(root, options{json: true}, &listOut, &errout); code != 0 {
		t.Fatalf("ls code=%d stderr=%q", code, errout.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(listOut.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["next_rank"] == float64(1) && row["slug"] == want {
			return
		}
	}
	t.Fatalf("rank 1 does not match tk next --by %s (%s): %s", master, want, listOut.String())
}

func taskSlugs(tasks []task) []string {
	slugs := make([]string, len(tasks))
	for i := range tasks {
		slugs[i] = tasks[i].slug
	}
	return slugs
}

func blockTestProject(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

func blockTestWrite(t *testing.T, dir, slug, prio, body string) task {
	t.Helper()
	path := filepath.Join(dir, "20260101-0000-"+prio+"-"+slug+".md")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	ticket, err := find(dir, slug)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestAttachBlocksRejectsClosedTargetWithoutWrites(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	xPath := filepath.Join(dir, "20260101-0000-MED-x.md")
	tPath := filepath.Join(dir, "20260101-0000-MED-t.md")
	xBody := "# x\n\nСтатус: открыт\n"
	if err := os.WriteFile(xPath, []byte(xBody), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tPath, []byte("# t\n\nСтатус: готово\n"), 0644); err != nil {
		t.Fatal(err)
	}
	x, err := find(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := attachBlocks(x, options{baseRoot: root, mainRoot: root, by: "tester"}, &out, &errout, []string{"t"}); code == 0 {
		t.Fatal("closed target accepted")
	}
	got, err := os.ReadFile(xPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != xBody {
		t.Fatalf("blocker changed on rejected link:\n%s", got)
	}
}

func TestAttachBlocksAddsHeaderToLegacyTicket(t *testing.T) {
	root, dir := blockTestProject(t)
	blocker := blockTestWrite(t, dir, "x", "MED", "# x\n\nСтатус: открыт\n")
	legacyPath := filepath.Join(dir, "20260101-0000-MED-legacy.md")
	if err := os.WriteFile(legacyPath, []byte("# legacy\n\nТело\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := attachBlocks(blocker, options{baseRoot: root, by: "tester"}, &out, &errout, []string{"legacy"}); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errout.String())
	}
	got, err := os.ReadFile(legacyPath)
	if err != nil || !strings.Contains(string(got), "# legacy\nЗаблокирован: x") {
		t.Fatalf("legacy header not updated: %s err=%v", got, err)
	}
}

func TestRemoveBlocksUsesSourceOfTruthAndMissingTarget(t *testing.T) {
	root, dir := blockTestProject(t)
	x := blockTestWrite(t, dir, "x", "MED", "# x\n\nСтатус: открыт\n")
	ticket := blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: открыт\nЗаблокирован: x\n")
	var errout bytes.Buffer
	if code := removeBlocks(x, options{baseRoot: root, by: "tester"}, &errout, []string{"t"}); code != 0 {
		t.Fatalf("source truth removal code=%d stderr=%q", code, errout.String())
	}
	got, _ := os.ReadFile(ticket.path)
	if strings.Contains(string(got), blockedPrefix+"x") {
		t.Fatalf("truth link remains without reverse reference: %s", got)
	}
	if err := os.Remove(ticket.path); err != nil {
		t.Fatal(err)
	}
	if code := removeBlocks(x, options{baseRoot: root, by: "tester"}, &errout, []string{"t"}); code != 0 {
		t.Fatalf("missing target code=%d stderr=%q", code, errout.String())
	}
	got, _ = os.ReadFile(x.path)
	if strings.Contains(string(got), blocksPrefix+"t") || !strings.Contains(errout.String(), "warning") {
		t.Fatalf("stale help reference not removed or no warning: %s / %s", got, errout.String())
	}
}

func TestRemoveBlocksWriteFailureKeepsReverseReference(t *testing.T) {
	root, dir := blockTestProject(t)
	x := blockTestWrite(t, dir, "x", "MED", "# x\n\nСтатус: открыт\nБлокирует: t\n")
	ticket := blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: открыт\nЗаблокирован: x\n")
	alias := filepath.Join(dir, "alias")
	if err := os.Link(ticket.path, alias); err != nil {
		t.Fatal(err)
	}
	var errout bytes.Buffer
	if code := removeBlocks(x, options{baseRoot: root, by: "tester"}, &errout, []string{"t"}); code == 0 {
		t.Fatal("write failure was reported as success")
	}
	got, err := os.ReadFile(x.path)
	if err != nil || !strings.Contains(string(got), blocksPrefix+"t") {
		t.Fatalf("reverse reference removed after truth write failed: %s err=%v", got, err)
	}
}

func TestSetPreflightsAllTargetsBeforePriorityChanges(t *testing.T) {
	root, dir := blockTestProject(t)
	path := filepath.Join(dir, "20260101-0000-MED-x.md")
	body := "# x\n\nСтатус: открыт\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	blockTestWrite(t, dir, "open", "MED", "# open\n\nСтатус: открыт\n")
	blockTestWrite(t, dir, "closed", "MED", "# closed\n\nСтатус: готово\n")
	o, err := parseArgs([]string{"set", "x", "--prio", "HIGH", "--why", "synthetic", "--blocks", "open", "--blocks", "closed", "--by", "tester"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := runAtBase(root, dir, o, &out, &errout); code == 0 {
		t.Fatal("closed second target accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body {
		t.Fatalf("source changed before all targets were checked: %s err=%v", got, err)
	}
}

func TestNewPreflightsClosedTargetBeforeCreatingFiles(t *testing.T) {
	root, dir := blockTestProject(t)
	blockTestWrite(t, dir, "closed", "MED", "# closed\n\nСтатус: отклонён\n")
	o, err := parseArgs([]string{"new", "fresh", "--prio", "MED", "--blocks", "closed", "--by", "tester"})
	if err != nil {
		t.Fatal(err)
	}
	o.baseRoot = root
	var out, errout bytes.Buffer
	if code := createAtBase(root, dir, o, &out, &errout); code == 0 {
		t.Fatal("closed target accepted")
	}
	if matches, err := filepath.Glob(filepath.Join(dir, "*-fresh.md")); err != nil || len(matches) != 0 {
		t.Fatalf("new ticket created before target validation: %v err=%v", matches, err)
	}
}

func TestWorktreeProjectUsesTicketFolderAndRejectsCrossProjectRefs(t *testing.T) {
	worktree, dir := blockTestProject(t)
	main := t.TempDir()
	x := blockTestWrite(t, dir, "x", "MED", "# x\n\nСтатус: открыт\n")
	blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: открыт\n")
	o := options{baseRoot: worktree, mainRoot: main, by: "tester"}
	var out, errout bytes.Buffer
	if code := attachBlocks(x, o, &out, &errout, []string{"t"}); code != 0 {
		t.Fatalf("same worktree project link failed: %d %s", code, errout.String())
	}
	if _, err := ticketRef(worktree, main, "elsewhere"); err == nil {
		t.Fatal("cross-project link from unaddressable worktree was accepted")
	}
}

func TestCloseBlockRelationsRepeatIsSilent(t *testing.T) {
	root, dir := blockTestProject(t)
	closed := blockTestWrite(t, dir, "x", "MED", "# x\n\nСтатус: готово\nБлокирует: t\n")
	blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: открыт\nЗаблокирован: x\n")
	var out, errout bytes.Buffer
	closeBlockRelations(closed, options{by: "tester"}, &out, &errout, 0)
	first := out.String()
	out.Reset()
	closeBlockRelations(closed, options{by: "tester"}, &out, &errout, 0)
	if out.Len() != 0 || !strings.Contains(first, "разблокирован") {
		t.Fatalf("repeat close output first=%q second=%q root=%s", first, out.String(), root)
	}
}

func TestManualPriorityFloorAndSameCommandUnblock(t *testing.T) {
	root, dir := blockTestProject(t)
	path := filepath.Join(dir, "20260101-0000-HIGH-x.md")
	if err := os.WriteFile(path, []byte("# x\n\nСтатус: открыт\nБлокирует: t\nПричина: r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	blockTestWrite(t, dir, "t", "HIGH", "# t\n\nСтатус: открыт\n")
	for _, args := range [][]string{{"set", "x", "--prio", "LOW", "--why", "lower"}, {"start", "x", "--prio", "LOW", "--why", "lower"}} {
		o, err := parseArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		o.by = "tester"
		var out, errout bytes.Buffer
		if code := runAtBase(root, dir, o, &out, &errout); code == 0 || !strings.Contains(errout.String(), "t") || !strings.Contains(errout.String(), "HIGH") {
			t.Fatalf("downgrade not rejected for %v: code=%d stderr=%q", args, code, errout.String())
		}
	}
	o, err := parseArgs([]string{"set", "x", "--prio", "LOW", "--why", "lower", "--unblocks", "t", "--by", "tester"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := runAtBase(root, dir, o, &out, &errout); code != 0 {
		t.Fatalf("lowering with same-command unblock failed: code=%d stderr=%q", code, errout.String())
	}
}

func TestManualPriorityFloorAllowsClosedArchivedAndMissingBlockers(t *testing.T) {
	root, dir := blockTestProject(t)
	for _, slug := range []string{"closed", "archived", "missing"} {
		if slug == "closed" {
			blockTestWrite(t, dir, slug, "HIGH", "# closed\n\nСтатус: готово\n")
		}
		if slug == "archived" {
			archive, err := archiveDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(archive, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(archive, "20260101-0000-HIGH-archived.md"), []byte("# archived\n\nСтатус: открыт\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		ticket := blockTestWrite(t, dir, "x-"+slug, "HIGH", "# x\n\nСтатус: открыт\nБлокирует: "+slug+"\n")
		var errout bytes.Buffer
		if code := validatePriorityFloor(ticket, options{prio: "LOW", baseRoot: root}, &errout); code != 0 {
			t.Errorf("allowed %s blocker: code=%d stderr=%q", slug, code, errout.String())
		}
	}
}

func TestRepeatedTransferFinishesNeighborAfterWriteFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "git", "sample")
	dir := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(home, "git", ".tk-transfer")
	if err := os.WriteFile(command, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"key\":\"EXT-2\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	xPath := filepath.Join(dir, "20260101-0000-MED-x.md")
	tPath := filepath.Join(dir, "20260101-0000-MED-t.md")
	if err := os.WriteFile(xPath, []byte("# x\n\nСтатус: открыт\nБлокирует: t\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tPath, []byte("# t\n\nСтатус: открыт\nЗаблокирован: x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Link(xPath, alias); err != nil {
		t.Fatal(err)
	}
	o := options{cmd: "transfer", id: "t", baseRoot: project, mainRoot: project, by: "tester"}
	var out, errout bytes.Buffer
	if code := runLockedCommand(project, dir, o, &out, &errout); code != 0 {
		t.Fatalf("transfer code=%d stderr=%q", code, errout.String())
	}
	if !strings.Contains(errout.String(), "hard links") {
		t.Fatalf("expected neighbor write warning: %q", errout.String())
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	errout.Reset()
	if code := runLockedCommand(project, dir, o, &out, &errout); code != 0 {
		t.Fatalf("repeat transfer code=%d stderr=%q", code, errout.String())
	}
	got, err := os.ReadFile(xPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), blocksPrefix+"t") || !strings.Contains(string(got), "заблокированный t перенесён в EXT-2") {
		t.Fatalf("repeat transfer did not finish neighbor update:\n%s", got)
	}
}

func TestAttachmentPreflightsReverseReferenceForSetAndNew(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	targetRoot := filepath.Join(home, "work", "a")
	targetDir := filepath.Join(targetRoot, "docs", "tasks")
	worktree, sourceDir := blockTestProject(t)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	blockTestWrite(t, targetDir, "t", "HIGH", "# t\n\nСтатус: открыт\n")
	xPath := filepath.Join(sourceDir, "20260101-0000-LOW-x.md")
	xBody := "# x\n\nСтатус: открыт\nПричина: synthetic\n"
	if err := os.WriteFile(xPath, []byte(xBody), 0644); err != nil {
		t.Fatal(err)
	}
	o, err := parseArgs([]string{"set", "x", "--prio", "HIGH", "--why", "synthetic", "--blocks", "work/a/t", "--by", "tester"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := runAtBase(worktree, sourceDir, o, &out, &errout); code == 0 {
		t.Fatal("set accepted an unaddressable reverse project reference")
	}
	if got, err := os.ReadFile(xPath); err != nil || string(got) != xBody {
		t.Fatalf("set changed source before reverse-reference validation: %s err=%v", got, err)
	}
	o, err = parseArgs([]string{"new", "fresh", "--prio", "LOW", "--blocks", "work/a/t", "--by", "tester"})
	if err != nil {
		t.Fatal(err)
	}
	o.baseRoot = worktree
	out.Reset()
	errout.Reset()
	if code := createAtBase(worktree, sourceDir, o, &out, &errout); code == 0 {
		t.Fatal("new accepted an unaddressable reverse project reference")
	}
	if matches, err := filepath.Glob(filepath.Join(sourceDir, "*-fresh.md")); err != nil || len(matches) != 0 {
		t.Fatalf("new created a ticket before reverse-reference validation: %v err=%v", matches, err)
	}
}

func TestAttachBlocksPreflightsSourceHeaderBeforeWritingTarget(t *testing.T) {
	root, dir := blockTestProject(t)
	sourcePath := filepath.Join(dir, "20260101-0000-MED-x.md")
	sourceBody := "plain legacy content\n"
	if err := os.WriteFile(sourcePath, []byte(sourceBody), 0644); err != nil {
		t.Fatal(err)
	}
	target := blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: открыт\n")
	source, err := find(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := attachBlocks(source, options{baseRoot: root, by: "tester"}, &out, &errout, []string{"t"}); code == 0 {
		t.Fatal("source without insertion point accepted")
	}
	got, err := os.ReadFile(target.path)
	if err != nil || strings.Contains(string(got), blockedPrefix) || strings.Contains(string(got), "блокирует x") {
		t.Fatalf("target changed despite source insertion preflight: %s err=%v", got, err)
	}
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != sourceBody {
		t.Fatalf("source changed: %s err=%v", got, err)
	}
}

func TestSetBlocksRefreshesPathAndPrintsFinalName(t *testing.T) {
	root, dir := blockTestProject(t)
	blockTestWrite(t, dir, "x", "LOW", "# x\n\nСтатус: открыт\nПричина: synthetic\n")
	blockTestWrite(t, dir, "t", "HIGH", "# t\n\nСтатус: открыт\n")
	o, err := parseArgs([]string{"set", "x", "--prio", "MED", "--blocks", "t", "--by", "tester"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := runAtBase(root, dir, o, &out, &errout); code != 0 {
		t.Fatalf("set code=%d stderr=%q", code, errout.String())
	}
	if !strings.Contains(out.String(), "-HIGH-x.md") || strings.Contains(out.String(), "-MED-x.md") {
		t.Fatalf("stdout did not name final ticket file: %q", out.String())
	}
	if matches, err := filepath.Glob(filepath.Join(dir, "*-HIGH-x.md")); err != nil || len(matches) != 1 {
		t.Fatalf("raised source missing: %v err=%v", matches, err)
	}
}

func TestNewBlocksPrintsFinalRaisedName(t *testing.T) {
	root, dir := blockTestProject(t)
	blockTestWrite(t, dir, "t", "HIGH", "# t\n\nСтатус: открыт\n")
	o, err := parseArgs([]string{"new", "fresh", "--prio", "LOW", "--blocks", "t", "--by", "tester"})
	if err != nil {
		t.Fatal(err)
	}
	o.baseRoot = root
	var out, errout bytes.Buffer
	if code := createAtBase(root, dir, o, &out, &errout); code != 0 {
		t.Fatalf("new code=%d stderr=%q", code, errout.String())
	}
	if !strings.Contains(out.String(), "-HIGH-fresh.md") || strings.Contains(out.String(), "-LOW-fresh.md") {
		t.Fatalf("new stdout did not name final ticket file: %q", out.String())
	}
	if !strings.Contains(errout.String(), "-HIGH-fresh.md") || strings.Contains(errout.String(), "-LOW-fresh.md") {
		t.Fatalf("new hint did not name final ticket file: %q", errout.String())
	}
}

func TestStartHintUsesRetaggedName(t *testing.T) {
	root, dir := blockTestProject(t)
	blockTestWrite(t, dir, "x", "LOW", "# x\n\nСтатус: открыт\nПричина: synthetic\n\n## Что сделать\n\n## Приёмка\n- check\n\n## Журнал\n")
	o, err := parseArgs([]string{"start", "x", "--prio", "HIGH", "--why", "synthetic", "--by", "tester"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := runAtBase(root, dir, o, &out, &errout); code != 0 {
		t.Fatalf("start code=%d stderr=%q", code, errout.String())
	}
	if !strings.Contains(errout.String(), "HIGH-x.md") || strings.Contains(errout.String(), "LOW-x.md") {
		t.Fatalf("start hint did not name final ticket: %q", errout.String())
	}
}

func TestLintDoesNotRequireReverseReferenceForClosedBlocker(t *testing.T) {
	_, dir := blockTestProject(t)
	ticket := blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: готово\nЗаблокирован: x\n")
	blockTestWrite(t, dir, "x", "LOW", "# x\n\nСтатус: открыт\n")
	issues, err := lintTicket(ticket.path, "t.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		if strings.Contains(issue, "has no reverse") {
			t.Fatalf("closed blocker still requires reverse reference: %v", issues)
		}
	}
}

func TestLintReportsUnresolvedBlockingReference(t *testing.T) {
	_, dir := blockTestProject(t)
	ticket := blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: открыт\nЗаблокирован: foreign/project/ticket\n")
	issues, err := lintTicket(ticket.path, "t.md")
	if err != nil {
		t.Fatal(err)
	}
	want := "blocking reference foreign/project/ticket cannot be resolved"
	if !strings.Contains(strings.Join(issues, "\n"), want) {
		t.Fatalf("lint issues = %v, want %q", issues, want)
	}
}

func TestSetBlocksPrintsTheNameBeforeRaises(t *testing.T) {
	root, dir := blockTestProject(t)
	blockTestWrite(t, dir, "x", "LOW", "# x\n\nСтатус: открыт\nПричина: synthetic\n")
	blockTestWrite(t, dir, "t", "HIGH", "# t\n\nСтатус: открыт\n")
	o, err := parseArgs([]string{"set", "x", "--blocks", "t", "--by", "tester"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := runAtBase(root, dir, o, &out, &errout); code != 0 {
		t.Fatalf("set code=%d stderr=%q", code, errout.String())
	}
	first, rest, _ := strings.Cut(out.String(), "\n")
	if !strings.HasSuffix(first, "-HIGH-x.md") || !strings.Contains(rest, "поднят") {
		t.Fatalf("stdout does not start with the final name: %q", out.String())
	}
}

func TestNewBlocksKeepsTheCreateError(t *testing.T) {
	root, dir := blockTestProject(t)
	blockTestWrite(t, dir, "x", "MED", "# x\n\nСтатус: открыт\n")
	blockTestWrite(t, dir, "t", "MED", "# t\n\nСтатус: открыт\n")
	o, err := parseArgs([]string{"new", "x", "--blocks", "t", "--by", "tester"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	if code := runAtBase(root, dir, o, &out, &errout); code == 0 {
		t.Fatal("a repeated new was accepted")
	}
	if !strings.Contains(errout.String(), "already exists") {
		t.Fatalf("create error lost: stderr=%q", errout.String())
	}
}

func TestTransferredRecordTakesTheLatestBySlugAndExactByStem(t *testing.T) {
	rows := []transferRecord{{Slug: "x", Stem: "20260101-0000-MED-x", Key: "old"}, {Slug: "x", Stem: "20260202-0000-MED-x", Key: "new"}}
	if row, ok := transferredRecord(rows, "x"); !ok || row.Key != "new" {
		t.Fatalf("by slug: %+v %v", row, ok)
	}
	if row, ok := transferredRecord(rows, "20260101-0000-MED-x.md"); !ok || row.Key != "old" {
		t.Fatalf("by stem: %+v %v", row, ok)
	}
}

func TestFailedLinkStillNamesTheRaisedFile(t *testing.T) {
	root, dir := blockTestProject(t)
	blockTestWrite(t, dir, "x", "LOW", "# x\n\nСтатус: открыт\nПричина: synthetic\n")
	blockTestWrite(t, dir, "t1", "HIGH", "# t1\n\nСтатус: открыт\n")
	blockTestWrite(t, dir, "t2", "MED", "# t2\n\nСтатус: открыт\nЗаблокирован: nosuch\n")
	for _, args := range [][]string{
		{"set", "x", "--blocks", "t1", "--blocks", "t2", "--by", "tester"},
		{"new", "y", "--prio", "LOW", "--blocks", "t1", "--blocks", "t2", "--by", "tester"},
	} {
		o, err := parseArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		var out, errout bytes.Buffer
		code := runAtBase(root, dir, o, &out, &errout)
		first, _, _ := strings.Cut(out.String(), "\n")
		if _, err := os.Stat(filepath.Join(dir, filepath.Base(first))); err != nil {
			t.Fatalf("%v: code=%d stdout=%q names no file: %v", args, code, out.String(), err)
		}
	}
}
