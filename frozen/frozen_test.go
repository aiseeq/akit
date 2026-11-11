package frozen

import (
	"os"
	"path/filepath"
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
			result, err := NewList(tc.row+"\n", home, Path(home)).Frozen(project)
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
	result, err := Frozen(project)
	if err != nil || !result.Frozen || result.Reason != "why" {
		t.Fatalf("Frozen() = (%+v, %v)", result, err)
	}
	list, err := Read(home)
	if err != nil {
		t.Fatal(err)
	}
	if list.Path() != listPath {
		t.Fatalf("list path = %q, want %q", list.Path(), listPath)
	}
}

func TestNewListKeepsSuppliedPath(t *testing.T) {
	home, source := t.TempDir(), filepath.Join(t.TempDir(), "custom-list")
	list := NewList("contents", home, source)
	if list.Path() != source {
		t.Fatalf("list path = %q, want supplied path %q", list.Path(), source)
	}
}

func TestFrozenIncompleteResultHasLine(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	result, err := NewList("# comment\n"+project+"\n", home, Path(home)).Frozen(project)
	if err != nil || !result.Frozen || result.IncompleteLine != 2 {
		t.Fatalf("Frozen() = (%+v, %v), want incomplete line 2", result, err)
	}
}

func TestFrozenFirstMatchingLineWins(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "work", "sample")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	content := project + "\n" + project + "\towner\tdate\tlater reason\n"
	result, err := NewList(content, home, Path(home)).Frozen(project)
	if err != nil || !result.Frozen || result.Line != 1 || result.IncompleteLine != 1 || result.Reason == "later reason" {
		t.Fatalf("Frozen = %+v, %v; want the first, incomplete line", result, err)
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
	result, err := Frozen("sample")
	if err != nil || !result.Frozen || result.Reason != "reason" {
		t.Fatalf("Frozen() = (%+v, %v)", result, err)
	}
}

func TestFrozenCollectsLineProblems(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	invalid := "relative\towner\tdate\treason"
	missing := filepath.Join(home, "not-there") + "\towner\tdate\treason"
	result, err := NewList(invalid+"\n"+missing+"\n", home, Path(home)).Frozen(project)
	if err != nil {
		t.Fatal(err)
	}
	if result.Frozen || len(result.Problems) != 1 || result.Problems[0].Number != 1 || result.Problems[0].Text != invalid || result.Problems[0].Err == nil {
		t.Fatalf("result=%+v, want one problem on line 1", result)
	}
}

func TestFrozenCommentWithValidPathDoesNotReportProblem(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	line := "#" + project + "\towner\tdate\treason"
	result, err := NewList(line+"\n", home, Path(home)).Frozen(project)
	if err != nil || result.Frozen || len(result.Problems) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestFrozenPathWithSpacesAndHomeShortcut(t *testing.T) {
	home := t.TempDir()
	withSpaces := filepath.Join(home, "project with spaces")
	if err := os.Mkdir(withSpaces, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := NewList(withSpaces+"\towner\tdate\tspace reason\n", home, Path(home)).Frozen(withSpaces)
	if err != nil || !result.Frozen || result.Reason != "space reason" {
		t.Fatalf("space path result=%+v err=%v", result, err)
	}
	result, err = NewList("~\towner\tdate\thome reason\n", home, Path(home)).Frozen(home)
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
	result, err := Frozen(project)
	if err != nil || result.Frozen || len(result.Problems) != 1 || result.Problems[0].Number != 1 || result.Problems[0].Text != "relative\towner\tdate\treason" {
		t.Fatalf("Frozen() = (%+v, %v), want one invalid row and no match", result, err)
	}
}

func TestFrozenMissingListIsEmpty(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	result, err := Frozen(project)
	if err != nil || result.Frozen || result.Reason != "" || len(result.Problems) != 0 {
		t.Fatalf("Frozen() = (%+v, %v), want empty result", result, err)
	}
}

func TestFrozenReadFailureIsFatal(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(Path(home), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Frozen(project)
	if err == nil || result.Frozen || len(result.Problems) != 0 {
		t.Fatalf("Frozen() = (%+v, %v), want fatal list read error", result, err)
	}
}

func TestFrozenSurfacesBadRowsWithoutLosingMatch(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "work", "sample")
	other := filepath.Join(home, "work", "other")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	listPath := Path(home)
	if err := os.MkdirAll(filepath.Dir(listPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "work/typo\towner\tdate\tbad path\n~/work/sample\towner\tdate\twhy\nrelative-again\towner\tdate\tbad path\n"
	if err := os.WriteFile(listPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Frozen(project)
	if err != nil || !result.Frozen || result.Reason != "why" || result.Line != 2 || len(result.Problems) != 2 || result.Problems[0].Number != 1 || result.Problems[1].Number != 3 {
		t.Errorf("Frozen(sample) = (%+v, %v), want match at line 2 and problems at 1 and 3", result, err)
	}
	result, err = Frozen(other)
	if err != nil || result.Frozen || len(result.Problems) != 2 || result.Problems[0].Number != 1 || result.Problems[1].Number != 3 {
		t.Errorf("Frozen(other) = (%+v, %v), want no match and both problems", result, err)
	}
}

func TestFrozenKeepsMatchAfterSymlinkLoop(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "work", "sample")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	loopA, loopB := filepath.Join(home, "loop-a"), filepath.Join(home, "loop-b")
	if err := os.Symlink(loopB, loopA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loopA, loopB); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	listPath := Path(home)
	if err := os.MkdirAll(filepath.Dir(listPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := loopA + "\towner\tdate\tloop\n~/work/sample\towner\tdate\twhy\n"
	if err := os.WriteFile(listPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Frozen(project)
	if err != nil || !result.Frozen || result.Reason != "why" || len(result.Problems) != 1 || result.Problems[0].Number != 1 {
		t.Fatalf("Frozen(sample) = (%+v, %v), want frozen with loop problem", result, err)
	}
}

func TestLineNamesSkipsComment(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	if matches, err := LineNames("#"+project+"\towner\tdate\treason", home, project); err != nil || matches {
		t.Fatalf("LineNames(comment) = %v, %v", matches, err)
	}
}
