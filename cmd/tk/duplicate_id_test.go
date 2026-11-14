package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func duplicateIDTicket(t *testing.T, repo, slug, status, stamp string) string {
	t.Helper()
	dir := filepath.Join(repo, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-KEY-%s.md", stamp, slug))
	body := "# " + slug + "\n\nСтатус: " + status + "\n\nОписание\n\n## Журнал\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnqualifiedTicketStaysInCurrentRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "work", "current")
	alpha := filepath.Join(home, "git", "alpha")
	beta := filepath.Join(home, "git", "beta")
	stamp := time.Now().Format("20060102-1504")
	duplicateIDTicket(t, alpha, "shared-key", "открыт", stamp)
	neighbor := duplicateIDTicket(t, beta, "shared-key", "готово", stamp)
	duplicateIDTicket(t, current, "source-key", "открыт", stamp)
	t.Chdir(current)
	before, err := os.ReadFile(neighbor)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"show", "shared-key"},
		{"note", "shared-key", "progress", "--by", "actor"},
		{"set", "shared-key", "--title", "changed", "--by", "actor"},
		{"start", "shared-key", "--by", "actor"},
		{"done", "shared-key", "--check", "checked", "--by", "actor"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runMain(args, time.Now(), &stdout, &stderr); code != exitNone || stdout.Len() != 0 || !strings.Contains(stderr.String(), `task "shared-key" not found`) {
			t.Errorf("%v = code %d, stdout %q, stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runMain([]string{"set", "source-key", "--title", "changed", "--blocks", "shared-key", "--by", "actor"}, time.Now(), &stdout, &stderr); code != exitUsage || strings.Contains(stdout.String(), "Статус: готово") {
		t.Errorf("unqualified --blocks = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	after, err := os.ReadFile(neighbor)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("neighbor ticket changed: read error %v", err)
	}
}

func TestQualifiedShowChoosesNamedProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "work", "current")
	stamp := time.Now().Format("20060102-1504")
	duplicateIDTicket(t, filepath.Join(home, "git", "alpha"), "shared-key", "открыт", stamp)
	duplicateIDTicket(t, filepath.Join(home, "git", "beta"), "shared-key", "готово", stamp)
	duplicateIDTicket(t, filepath.Join(home, "work", "gamma"), "shared-key", "ждёт", stamp)
	if err := os.MkdirAll(filepath.Join(current, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(current)
	for _, tc := range []struct {
		args   []string
		status string
	}{
		{[]string{"show", "git/alpha/shared-key"}, "Статус: открыт"},
		{[]string{"show", "git/beta/shared-key"}, "Статус: готово"},
		{[]string{"show", "work/gamma/shared-key"}, "Статус: ждёт"},
		{[]string{"show", "git/alpha/shared-key", "--head"}, "Статус: открыт"},
		{[]string{"show", "git/beta/shared-key", "--json"}, `"status":"готово"`},
	} {
		var stdout, stderr bytes.Buffer
		if code := runMain(tc.args, time.Now(), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), tc.status) {
			t.Errorf("%v = code %d, stdout %q, stderr %q", tc.args, code, stdout.String(), stderr.String())
		}
	}
}

func TestQualifiedShowIncludesArchivedTickets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "work", "current")
	beta := filepath.Join(home, "git", "beta")
	stamp := time.Now().Format("20060102-1504")
	archived := duplicateIDTicket(t, beta, "shared-key", "готово", stamp)
	archive, err := archiveDir(beta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(archived, filepath.Join(archive, filepath.Base(archived))); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(current, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(current)
	var stdout, stderr bytes.Buffer
	if code := runMain([]string{"show", "git/beta/shared-key", "--head"}, time.Now(), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Статус: готово") {
		t.Fatalf("archived show = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestQualifiedShowAcceptsFullFilenameIDAndNamesProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "work", "current")
	alpha := filepath.Join(home, "git", "alpha")
	beta := filepath.Join(home, "git", "beta")
	stamp := time.Now().Format("20060102-1504")
	alphaPath := duplicateIDTicket(t, alpha, "shared-key", "открыт", stamp)
	betaPath := duplicateIDTicket(t, beta, "shared-key", "готово", stamp)
	if err := os.MkdirAll(filepath.Join(current, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(current)
	for _, tc := range []struct {
		path, status string
	}{
		{alphaPath, "Статус: открыт"},
		{betaPath, "Статус: готово"},
	} {
		ref := "git/" + filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(tc.path)))) + "/" + strings.TrimSuffix(filepath.Base(tc.path), ".md")
		var stdout, stderr bytes.Buffer
		if code := runMain([]string{"show", ref}, time.Now(), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), relative(current, tc.path)) || !strings.Contains(stdout.String(), tc.status) {
			t.Errorf("show %q = code %d, stdout %q, stderr %q; want named path %q and %q", ref, code, stdout.String(), stderr.String(), relative(current, tc.path), tc.status)
		}
	}
}

func TestQualifiedShowAcceptsArchivedFullFilenameID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "work", "current")
	beta := filepath.Join(home, "git", "beta")
	archived := duplicateIDTicket(t, beta, "shared-key", "готово", time.Now().Format("20060102-1504"))
	archive, err := archiveDir(beta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	archived = filepath.Join(archive, filepath.Base(archived))
	if err := os.Rename(filepath.Join(beta, "docs/tasks", filepath.Base(archived)), archived); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(current, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(current)
	ref := "git/beta/" + strings.TrimSuffix(filepath.Base(archived), ".md")
	var stdout, stderr bytes.Buffer
	if code := runMain([]string{"show", ref, "--head"}, time.Now(), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), relative(current, archived)) || !strings.Contains(stdout.String(), "Статус: готово") {
		t.Fatalf("archived show %q = code %d, stdout %q, stderr %q", ref, code, stdout.String(), stderr.String())
	}
}

func TestQualifiedShowAllowsSharedStoreInManagedRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "work", "current")
	project := filepath.Join(home, "git", "alpha")
	store := filepath.Join(home, "work", "task-store")
	duplicateIDTicket(t, store, "shared-key", "открыт", time.Now().Format("20060102-1504"))
	if err := os.MkdirAll(filepath.Join(project, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store, "docs/tasks"), filepath.Join(project, "docs/tasks")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(current, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(current)
	var stdout, stderr bytes.Buffer
	if code := runMain([]string{"show", "git/alpha/shared-key"}, time.Now(), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Статус: открыт") {
		t.Fatalf("shared-store show = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestQualifiedShowUsesProjectSymlinkAndReportsMissingRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "work", "current")
	outside := filepath.Join(home, "outside")
	if err := os.MkdirAll(filepath.Join(current, "docs", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	duplicateIDTicket(t, home, "shared-key", "готово", time.Now().Format("20060102-1504"))
	outsidePath := duplicateIDTicket(t, outside, "shared-key", "готово", time.Now().Format("20060102-1504"))
	if err := os.MkdirAll(filepath.Join(home, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "git", "escape")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(current)
	for _, ref := range []string{"git/../shared-key", "git/a/b/c"} {
		var stdout, stderr bytes.Buffer
		if code := runMain([]string{"show", ref}, time.Now(), &stdout, &stderr); code != exitUsage || stdout.Len() != 0 {
			t.Errorf("show %q = code %d, stdout %q, stderr %q", ref, code, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runMain([]string{"show", "git/missing/shared-key"}, time.Now(), &stdout, &stderr); code != exitNone || stdout.Len() != 0 || !strings.Contains(stderr.String(), filepath.Join(home, "git/missing/docs/tasks")) {
		t.Errorf("missing repository = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runMain([]string{"show", "git/escape/shared-key"}, time.Now(), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Статус: готово") {
		t.Errorf("project symlink = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{
		{"note", "git/alpha/shared-key", "progress", "--by", "actor"},
		{"set", "git/alpha/shared-key", "--title", "changed", "--by", "actor"},
		{"start", "git/alpha/shared-key", "--by", "actor"},
		{"done", "git/alpha/shared-key", "--check", "checked", "--by", "actor"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runMain(args, time.Now(), &stdout, &stderr); code != exitUsage || stdout.Len() != 0 {
			t.Errorf("qualified action %v = code %d, stdout %q, stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
	if strings.Contains(outsidePath, filepath.Join(home, "git")) {
		t.Fatal("invalid test fixture: outside ticket lies in the project root")
	}
}

func TestBlockingLinkFromRepositoryWithSpaceInNameStaysResolvable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join(home, "git", "odd name")
	target := filepath.Join(home, "work", "current")
	stamp := time.Now().Format("20060102-1504")
	duplicateIDTicket(t, source, "source-key", "открыт", stamp)
	duplicateIDTicket(t, target, "target-key", "открыт", stamp)
	t.Chdir(source)
	var stdout, stderr bytes.Buffer
	if code := runMain([]string{"set", "source-key", "--blocks", "work/current/target-key", "--by", "actor"}, time.Now(), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("--blocks from a repository with a space = code %d, stderr %q", code, stderr.String())
	}
	t.Chdir(target)
	stdout.Reset()
	stderr.Reset()
	if runMain([]string{"lint"}, time.Now(), &stdout, &stderr); strings.Contains(stdout.String()+stderr.String(), "blocking reference") {
		t.Fatalf("lint of the blocked ticket cannot resolve the link: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestQualifiedShowJournalHintKeepsProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "work", "current")
	alpha := filepath.Join(home, "git", "alpha")
	stamp := time.Now().Format("20060102-1504")
	duplicateIDTicket(t, current, "shared-key", "открыт", stamp)
	remote := duplicateIDTicket(t, alpha, "shared-key", "готово", stamp)
	var journal strings.Builder
	for i := range 35 {
		fmt.Fprintf(&journal, "- 2026-01-01 10:%02d note — actor: %s\n", i, strings.Repeat("x", 700))
	}
	body, err := os.ReadFile(remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remote, append(body, journal.String()...), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(current)
	var stdout, stderr bytes.Buffer
	if code := runMain([]string{"show", "git/alpha/shared-key"}, time.Now(), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "tk show git/alpha/shared-key --tail 0") {
		t.Fatalf("qualified show = code %d, stderr %q, output tail %q", code, stderr.String(), stdout.String()[max(0, stdout.Len()-200):])
	}
	spaced := filepath.Join(home, "git", "odd name")
	if err := os.MkdirAll(filepath.Join(spaced, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(alpha, "docs", "tasks"), filepath.Join(spaced, "docs", "tasks")); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runMain([]string{"show", "git/odd name/shared-key"}, time.Now(), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "tk show 'git/odd name/shared-key' --tail 0") {
		t.Fatalf("qualified show with a space = code %d, stderr %q, output tail %q", code, stderr.String(), stdout.String()[max(0, stdout.Len()-200):])
	}
}

func TestQualifiedReferenceRefusesPathRepositoryNames(t *testing.T) {
	for _, ref := range []string{"git//shared-key", "git/./shared-key", "git/../shared-key"} {
		if _, _, err := referenceLocation("", ref); err == nil {
			t.Errorf("referenceLocation(%q) accepted a path form", ref)
		}
	}
}
