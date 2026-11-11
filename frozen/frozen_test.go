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
			reason, frozen, err := NewList(tc.row+"\n", home).Frozen(project, nil)
			if err != nil {
				t.Fatal(err)
			}
			if frozen != tc.wantFrozen || reason != tc.wantReason {
				t.Fatalf("Frozen() = (%q, %v), want (%q, %v)", reason, frozen, tc.wantReason, tc.wantFrozen)
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
}
