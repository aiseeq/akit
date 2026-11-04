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
