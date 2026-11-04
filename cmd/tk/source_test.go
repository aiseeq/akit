package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	if _, _, reason, failed := runTaskSource(path, "project"); !failed || reason != "source is not trusted" {
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
	if externalGroup(externalTask{Prio: "CRIT"}) != 0 || externalGroup(externalTask{Prio: "HIGH"}) != 2 || externalGroup(externalTask{Prio: "MED"}) != 3 || externalGroup(externalTask{Prio: "LOW"}) != 4 {
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
	if gateVisible(closed, gateState{valid: true, data: taskGate{Open: []string{"OTHER"}}}, true) {
		t.Fatal("closed pair should hide candidate in live project")
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
