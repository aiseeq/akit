package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTaskSourceLaunchAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tk-next-source")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '[{\"key\":\"K1\",\"title\":\"one\\nline\",\"status\":\"open\",\"prio\":\"HIGH\",\"url\":\"https://example.test\",\"extra\":1},{\"key\":\"bad\",\"title\":\"bad\",\"prio\":\"WRONG\"}]'\nprintf '%s' \"$1\" > \"$TMPDIR/source-arg\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	items, bad, reason, failed := runTaskSource(path, "project")
	if failed || bad != 1 || reason == "" || len(items) != 1 || items[0].Key != "K1" || strings.ContainsAny(items[0].Title, "\n\r") {
		t.Fatalf("source result = %#v, bad %d, reason %q, failed %v", items, bad, reason, failed)
	}
}

func TestTaskSourceRejectsUntrustedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tk-next-source")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho []\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, _, reason, failed := runTaskSource(path, "project"); !failed || reason != "writable by group or others" {
		t.Fatalf("untrusted source = failed %v, reason %q", failed, reason)
	}
}

func TestTaskSourceOutputLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tk-next-source")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nhead -c 1100000 /dev/zero | tr '\\000' ' '; echo '[]'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, reason, failed := runTaskSource(path, "project")
	if !failed || reason != "output over 1 MiB" {
		t.Fatalf("oversized source = failed %v, reason %q", failed, reason)
	}
}

func TestExternalCandidateGroups(t *testing.T) {
	items := []externalTask{{Key: "med", Prio: "MED"}, {Key: "crit", Prio: "CRIT"}, {Key: "high", Prio: "HIGH"}}
	sortExternal(items)
	if got := []string{items[0].Key, items[1].Key, items[2].Key}; strings.Join(got, ",") != "crit,high,med" {
		t.Fatalf("source order = %v", got)
	}
	if nextGroup(task{prio: "CRIT", status: "открыт"}, "", gateState{}, false) != 0 || nextGroup(task{prio: "HIGH", status: "открыт"}, "", gateState{}, false) != 2 || nextGroup(task{prio: "MED", status: "открыт"}, "", gateState{}, false) != 4 || nextGroup(task{prio: "LOW", status: "открыт"}, "", gateState{}, false) != 5 {
		t.Fatal("unexpected external candidate groups")
	}
}

func TestParseExternalTasksPartialAndFailures(t *testing.T) {
	items, bad, _, failed := parseExternalTasks([]byte("[{\"key\":\"K\\n1\",\"title\":\"line\\ttitle\",\"prio\":\"LOW\"},{\"key\":\"bad\",\"title\":\"x\",\"prio\":\"MEDIUM\"}]"))
	if failed || bad != 1 || len(items) != 1 || items[0].Key != "K 1" || items[0].Title != "line title" {
		t.Fatalf("partial parse = %#v, bad %d, failed %v", items, bad, failed)
	}
	for _, input := range []string{"null", "{}", "[] trailing", "[{\"key\":\"x\",\"title\":\"x\",\"prio\":\"MEDIUM\"}]"} {
		if _, _, _, failed = parseExternalTasks([]byte(input)); !failed {
			t.Errorf("parseExternalTasks(%q) should fail", input)
		}
	}
}

func TestExternalCandidateGate(t *testing.T) {
	closed := task{prio: "MED", status: "открыт", diff: "DO"}
	if gateVisible(closed, gateState{}, false) {
		t.Fatal("unknown gate should hide HIGH")
	}
	if !gateVisible(closed, gateState{valid: true}, false) {
		t.Fatal("non-live project should not require an open pair")
	}
	if gateVisible(closed, gateState{valid: true, open: []string{"OTHER"}}, true) {
		t.Fatal("closed pair should hide candidate in live project")
	}
}

func TestJournalEligibilityAndGateRoots(t *testing.T) {
	now := time.Now().Add(-time.Minute).Format(time.RFC3339)
	journal := task{status: "открыт", prio: "CRIT", body: "Вид: журнал\n"}
	if eligibleForNext(journal, "") {
		t.Fatal("open journal ticket is eligible")
	}
	journal.status = "ждёт"
	journal.body += "Ждёт: " + now + "\n"
	if !eligibleForNext(journal, "") {
		t.Fatal("due journal wait should be eligible")
	}

	base := t.TempDir()
	root := filepath.Join(base, "work")
	project := filepath.Join(root, "group", "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	state := gateState{valid: true, data: taskGate{Open: []string{"MED-HUNT"}, OpenByRoot: func() *map[string][]string {
		m := map[string][]string{root: {"MED-DO"}, filepath.Join(root, "group"): {}}
		return &m
	}()}}
	got, err := gatePairs(state.data, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("most specific root gate = %#v", got)
	}
	workshop := filepath.Join(base, "workshop", "project")
	if err := os.MkdirAll(workshop, 0o755); err != nil {
		t.Fatal(err)
	}
	boundary, err := gatePairs(state.data, workshop)
	if err != nil {
		t.Fatal(err)
	}
	if len(boundary) != 1 || boundary[0] != "MED-HUNT" {
		t.Fatalf("fallback gate at component boundary = %#v", boundary)
	}
	broken := gateState{problem: "gate.json has no open_by_root"}
	if gateVisible(task{status: "открыт", prio: "MED"}, broken, true) || !gateVisible(task{status: "открыт", prio: "HIGH"}, broken, true) {
		t.Fatal("missing root map should expose only CRIT/HIGH")
	}
	if !gateVisible(task{status: "в работе", prio: "MED"}, broken, true) {
		t.Fatal("started ticket must remain visible with a broken gate")
	}
}

func TestDestinationProjectRootsAndAmbiguity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gitProject, workProject := filepath.Join(home, "git/sample"), filepath.Join(home, "work/sample")
	if err := os.MkdirAll(gitProject, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := destinationProject("sample"); err != nil || got != gitProject {
		t.Fatalf("destination = %q, %v", got, err)
	}
	if err := os.MkdirAll(workProject, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := destinationProject("sample"); err == nil || !strings.Contains(err.Error(), gitProject) || !strings.Contains(err.Error(), workProject) {
		t.Fatalf("ambiguous destination error = %v", err)
	}
}

func TestPrepareTaskDirectoryCreatesSharedLink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "git/sample")
	if err := os.MkdirAll(filepath.Join(home, "git/tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", project, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := prepareTaskDirectory(project, ""); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(project, "docs/tasks")
	got, err := os.Readlink(link)
	if err != nil || got != "../../tasks/sample" {
		t.Fatalf("docs/tasks = %q, %v", got, err)
	}
	if _, err = os.Stat(filepath.Join(home, "git/tasks/sample")); err != nil {
		t.Fatalf("shared store: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(project, ".git/info/exclude"))
	if err != nil || !strings.Contains(string(content), "/docs/tasks\n") {
		t.Fatalf("exclude = %q, %v", content, err)
	}
}

func TestTaskSourceRootMatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, root := range []string{"git", "work"} {
		project := filepath.Join(home, root, "sample")
		if err := os.MkdirAll(project, 0o755); err != nil {
			t.Fatal(err)
		}
		got, ok, err := taskSourcePath(project)
		if err != nil || !ok || got != filepath.Join(home, root, ".tk-next-source") {
			t.Fatalf("taskSourcePath(%q) = %q, %v", project, got, ok)
		}
	}
	if _, ok, err := taskSourcePath(filepath.Join(home, "other", "sample")); err != nil || ok {
		t.Fatal("unexpected source outside supported roots")
	}
}

func gitInTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// linkedProject makes HOME/work/sample, its store and a linked worktree. The link
// is neither ignored nor excluded, as in a project linked by hand.
func linkedProject(t *testing.T) (project, worktree string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	project = filepath.Join(home, "work", "sample")
	for _, path := range []string{filepath.Join(home, "work", "tasks", "sample"), filepath.Join(project, "docs")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitInTest(t, project, "init", "-q", "-b", "main")
	gitInTest(t, project, "commit", "-q", "--allow-empty", "-m", "init")
	if err := os.Symlink("../../tasks/sample", filepath.Join(project, "docs", "tasks")); err != nil {
		t.Fatal(err)
	}
	worktree = filepath.Join(home, "trees", "feature")
	gitInTest(t, project, "worktree", "add", "-q", "-b", "feature", worktree)
	return project, worktree
}

func TestWorktreeUsesLinkedStoreWithoutIgnore(t *testing.T) {
	project, worktree := linkedProject(t)
	shared, err := sharedTaskRoot(worktree, project)
	if err != nil || shared != project {
		t.Fatalf("sharedTaskRoot = %q, %v, want %q", shared, err, project)
	}
}

func TestNewToWorktreeWritesToMainTree(t *testing.T) {
	project, worktree := linkedProject(t)
	if err := os.Remove(filepath.Join(project, "docs", "tasks")); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "20261001-1000-MED-old.md"), []byte("# Old\n\nСтатус: открыт\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInTest(t, project, "add", "docs/tasks")
	gitInTest(t, project, "commit", "-q", "-m", "tickets")
	gitInTest(t, worktree, "merge", "-q", "--ff-only", "main")
	o, err := parseArgs([]string{"new", "fresh", "--to", worktree, "--by", "me"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := createAtBase(worktree, filepath.Join(worktree, "docs", "tasks"), o, &out, &errOut); code != 0 {
		t.Fatalf("createAtBase = %d: %s", code, errOut.String())
	}
	found, err := filepath.Glob(filepath.Join(real, "*-fresh.md"))
	if err != nil || len(found) != 1 {
		t.Fatalf("ticket in main tree = %v, %v", found, err)
	}
	stray, err := filepath.Glob(filepath.Join(worktree, "docs", "tasks", "*-fresh.md"))
	if err != nil || len(stray) != 0 {
		t.Fatalf("ticket in worktree = %v, %v", stray, err)
	}
}

func TestMissingStoreKeepsEmptyTaskDirectory(t *testing.T) {
	project, _ := linkedProject(t)
	link := filepath.Join(project, "docs", "tasks")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(link, 0o755); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(filepath.Dir(project), "tasks")
	if err := os.RemoveAll(store); err != nil {
		t.Fatal(err)
	}
	err := prepareTaskDirectory(project, project)
	if err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("prepareTaskDirectory = %v", err)
	}
	if info, statErr := os.Lstat(link); statErr != nil || !info.IsDir() {
		t.Fatalf("empty directory is gone: %v", statErr)
	}
}

func runExternalIn(t *testing.T, dir string) (code int, stdout, stderr string) {
	t.Helper()
	t.Chdir(dir)
	o, err := parseArgs([]string{"external", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code = run(o, &out, &errOut)
	return code, out.String(), errOut.String()
}

func writeSource(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".tk-next-source"), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestExternalList(t *testing.T) {
	const good = `{"key":"K1","title":"one","status":"open","prio":"LOW","url":"u1"},{"key":"K2","title":"two","status":"open","prio":"CRIT","url":"u2"}`
	want := `[{"key":"K2","title":"two","status":"open","prio":"CRIT","url":"u2"},{"key":"K1","title":"one","status":"open","prio":"LOW","url":"u1"}]` + "\n"
	cases := []struct {
		name, source, stdout, stderr string
		failed                       bool
	}{
		{name: "all records, gates and local tickets aside", source: "[ \"$1\" = sample ] || exit 3\necho '[" + good + "]'", stdout: want},
		{name: "no file", stdout: "[]\n"},
		{name: "empty source", source: "echo '[]'", stdout: "[]\n"},
		{name: "bad record next to good", source: "echo '[" + good + `,{"key":"K3"}]'`, stdout: want, stderr: "skipped 1 bad records"},
		{name: "nonzero exit", source: "echo boom >&2; exit 3", failed: true, stderr: "boom"},
		{name: "not JSON", source: "echo nope", failed: true, stderr: "invalid JSON array"},
		{name: "only bad records", source: `echo '[{"key":"K3"}]'`, failed: true, stderr: "no valid records"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			project, _ := linkedProject(t)
			if err := os.WriteFile(filepath.Join(project, "docs", "tasks", "20261001-1000-HIGH-local.md"), []byte("# Local\n\nСтатус: открыт\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if c.source != "" {
				writeSource(t, filepath.Dir(project), c.source)
			}
			code, stdout, stderr := runExternalIn(t, project)
			if (code != 0) != c.failed || stdout != c.stdout || !strings.Contains(stderr, c.stderr) {
				t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
			}
		})
	}
}

func TestExternalListUntrustedSourceFails(t *testing.T) {
	project, _ := linkedProject(t)
	root := filepath.Dir(project)
	writeSource(t, root, "echo '[]'")
	if err := os.Chmod(filepath.Join(root, ".tk-next-source"), 0o777); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runExternalIn(t, project)
	if code == 0 || stdout != "" || !strings.Contains(stderr, "writable by group or others") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestExternalListFollowsMainTreeFromWorktree(t *testing.T) {
	project, worktree := linkedProject(t)
	writeSource(t, filepath.Dir(project), `[ "$1" = sample ] && echo '[{"key":"K1","title":"one","status":"open","prio":"HIGH","url":"u"}]'`)
	code, stdout, stderr := runExternalIn(t, worktree)
	if code != 0 || !strings.Contains(stdout, `"K1"`) {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestExternalListOutsideProjectRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{filepath.Join(home, "other", "repo"), filepath.Join(home, "other", "plain")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitInTest(t, filepath.Join(home, "other", "repo"), "init", "-q")
	for _, name := range []string{"repo", "plain"} {
		code, stdout, stderr := runExternalIn(t, filepath.Join(home, "other", name))
		if code != 0 || stdout != "[]\n" {
			t.Fatalf("%s: code %d, stdout %q, stderr %q", name, code, stdout, stderr)
		}
	}
}

func TestExternalNeedsJSONFlag(t *testing.T) {
	if _, err := parseArgs([]string{"external"}); err == nil {
		t.Fatal("external without --json accepted")
	}
}
