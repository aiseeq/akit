package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
