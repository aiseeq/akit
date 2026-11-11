package frozen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrozenListCases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "work", "sample")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "work", "linked")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, row  string
		wantFrozen bool
		wantReason string
	}{
		{name: "home shortcut", row: "~/work/sample\towner\tdate\treason", wantFrozen: true, wantReason: "reason"},
		{name: "symlink", row: link + "\towner\tdate\tlinked reason", wantFrozen: true, wantReason: "linked reason"},
		{name: "incomplete", row: project, wantFrozen: true, wantReason: "incomplete line in the frozen list"},
		{name: "missing directory", row: filepath.Join(home, "missing") + "\towner\tdate\treason"},
		{name: "comment", row: "#" + project + "\towner\tdate\treason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := NewList(tc.row+"\n", home).Frozen(project, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Frozen != tc.wantFrozen || result.Reason != tc.wantReason {
				t.Fatalf("Frozen() = (%+v), want frozen=%v reason=%q", result, tc.wantFrozen, tc.wantReason)
			}
		})
	}
}

func TestFrozenReadsDefaultList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	listPath := filepath.Join(home, ".config", "queue", "frozen")
	if err := os.MkdirAll(filepath.Dir(listPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(listPath, []byte(project+"\towner\tdate\twhy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reason, isFrozen, err := Frozen(project)
	if err != nil || !isFrozen || reason != "why" {
		t.Fatalf("Frozen() = (%q, %v, %v)", reason, isFrozen, err)
	}
	list, err := Read(home)
	if err != nil {
		t.Fatal(err)
	}
	if list.Path() != listPath {
		t.Fatalf("list path = %q, want %q", list.Path(), listPath)
	}
}

func TestFrozenIncompleteResultHasLine(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	result, err := NewList("# comment\n"+project+"\n", home).Frozen(project, nil)
	if err != nil || !result.Frozen || !result.Incomplete || result.Line != 2 {
		t.Fatalf("Frozen() = (%+v, %v), want incomplete line 2", result, err)
	}
}

func TestFrozenRelativeProject(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(work, "sample")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	listPath := Path(home)
	if err := os.MkdirAll(filepath.Dir(listPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(listPath, []byte(project+"\towner\tdate\treason\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	reason, isFrozen, err := Frozen("sample")
	if err != nil || !isFrozen || reason != "reason" {
		t.Fatalf("Frozen() = (%q, %v, %v)", reason, isFrozen, err)
	}
}

func TestFrozenRowCallback(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	invalid := "relative\towner\tdate\treason"
	missing := filepath.Join(home, "not-there") + "\towner\tdate\treason"
	var gotNumber int
	var gotLine string
	calls := 0
	result, err := NewList(invalid+"\n"+missing+"\n", home).Frozen(project, func(number int, line string, err error) {
		calls++
		gotNumber, gotLine = number, line
		if err == nil {
			t.Error("callback error is nil")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Frozen || calls != 1 || gotNumber != 1 || gotLine != invalid {
		t.Fatalf("result=%+v callback=(%d,%d,%q), want no freeze and one callback at line 1", result, calls, gotNumber, gotLine)
	}
}

func TestFrozenCommentWithValidPathDoesNotCallBack(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	line := "#" + project + "\towner\tdate\treason"
	calls := 0
	result, err := NewList(line+"\n", home).Frozen(project, func(int, string, error) { calls++ })
	if err != nil || result.Frozen || calls != 0 {
		t.Fatalf("result=%+v callbacks=%d err=%v", result, calls, err)
	}
}

func TestFrozenPathWithSpacesAndHomeShortcut(t *testing.T) {
	home := t.TempDir()
	withSpaces := filepath.Join(home, "project with spaces")
	if err := os.Mkdir(withSpaces, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := NewList(withSpaces+"\towner\tdate\tspace reason\n", home).Frozen(withSpaces, nil)
	if err != nil || !result.Frozen || result.Reason != "space reason" {
		t.Fatalf("space path result=%+v err=%v", result, err)
	}
	result, err = NewList("~\towner\tdate\thome reason\n", home).Frozen(home, nil)
	if err != nil || !result.Frozen || result.Reason != "home reason" {
		t.Fatalf("home path result=%+v err=%v", result, err)
	}
	if matches, err := LineNames("~\towner\tdate\thome reason", home, home); err != nil || !matches {
		t.Fatalf("LineNames(~) = %v, %v", matches, err)
	}
}

func TestFrozenReportsInvalidRows(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	listPath := Path(home)
	if err := os.MkdirAll(filepath.Dir(listPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(listPath, []byte("relative\towner\tdate\treason\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Frozen(project)
	if err == nil || !strings.Contains(err.Error(), listPath+":1:") {
		t.Fatalf("Frozen() error = %v, want list path and line", err)
	}
}

func TestFrozenMissingListIsEmpty(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	reason, isFrozen, err := Frozen(project)
	if err != nil || isFrozen || reason != "" {
		t.Fatalf("Frozen() = (%q, %v, %v), want empty result", reason, isFrozen, err)
	}
}

func TestLineNamesSkipsComment(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	if matches, err := LineNames("#"+project+"\towner\tdate\treason", home, project); err != nil || matches {
		t.Fatalf("LineNames(comment) = %v, %v", matches, err)
	}
}
