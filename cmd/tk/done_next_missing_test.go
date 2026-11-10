package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A names table line whose folder is gone names nothing: no warning, and the
// <name>-master folder convention still applies.
func TestDoneHintNamesLineWithMissingFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "shared")
	dir := filepath.Join(root, "docs", "tasks")
	scopeTicket(t, dir, "closing", "в работе", "alpha-master")
	scopeTicket(t, dir, "root-open", "открыт", "")
	scopeTicket(t, filepath.Join(root, "alpha", "docs", "tasks"), "alpha-own", "открыт", "")
	namesDir := filepath.Join(home, ".config", "cl")
	if err := os.MkdirAll(namesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(namesDir, "names.tsv"), []byte("~/gone\talpha-master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	o := options{cmd: "done", id: "closing", by: "checker", check: "checked by test", baseRoot: root}
	if code := runLockedCommand(root, dir, o, &out, &errout); code != 0 {
		t.Fatalf("done = %d, stderr %q", code, errout.String())
	}
	if !strings.Contains(out.String(), "следующий в alpha: alpha-own ") || strings.Contains(errout.String(), "gone") {
		t.Errorf("output %q, stderr %q", out.String(), errout.String())
	}
}

// The executor named with --doer at done, an upper-case master name and an
// unreadable names table: the hint still comes from the executor's folder.
func TestDoneHintExecutorForms(t *testing.T) {
	run := func(t *testing.T, executor, doer string, unreadable bool) (string, string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		root := filepath.Join(home, "shared")
		dir := filepath.Join(root, "docs", "tasks")
		scopeTicket(t, dir, "closing", "в работе", executor)
		scopeTicket(t, dir, "root-open", "открыт", "")
		scopeTicket(t, filepath.Join(root, "alpha", "docs", "tasks"), "alpha-own", "открыт", "")
		if unreadable {
			namesDir := filepath.Join(home, ".config", "cl")
			if err := os.MkdirAll(filepath.Join(namesDir, "names.tsv"), 0o755); err != nil { // a folder cannot be read as the table
				t.Fatal(err)
			}
		}
		var out, errout bytes.Buffer
		o := options{cmd: "done", id: "closing", by: "checker", doer: doer, check: "checked by test", baseRoot: root}
		if code := runLockedCommand(root, dir, o, &out, &errout); code != 0 {
			t.Fatalf("done = %d, stderr %q", code, errout.String())
		}
		return out.String(), errout.String()
	}
	t.Run("--doer without an executor line", func(t *testing.T) {
		if out, _ := run(t, "", "alpha-master", false); !strings.Contains(out, "следующий в alpha: alpha-own ") {
			t.Errorf("output %q", out)
		}
	})
	t.Run("upper-case master name", func(t *testing.T) {
		if out, _ := run(t, "ALPHA-MASTER", "", false); !strings.Contains(out, "следующий в alpha: alpha-own ") {
			t.Errorf("output %q", out)
		}
	})
	t.Run("unreadable names table", func(t *testing.T) {
		out, errout := run(t, "alpha-master", "", true)
		if !strings.Contains(out, "следующий в alpha: alpha-own ") || !strings.Contains(errout, "session names unreadable") {
			t.Errorf("output %q, stderr %q", out, errout)
		}
	})
}

// A built binary named like the project lies in the root of its repository: a
// file is no project folder, and the hint comes from the root without an error.
func TestDoneHintFileNamedLikeProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "tool")
	dir := filepath.Join(root, "docs", "tasks")
	scopeTicket(t, dir, "closing", "в работе", "tool-master")
	scopeTicket(t, dir, "root-open", "открыт", "")
	if err := os.WriteFile(filepath.Join(root, "tool"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	o := options{cmd: "done", id: "closing", by: "checker", check: "checked by test", baseRoot: root}
	if code := runLockedCommand(root, dir, o, &out, &errout); code != 0 {
		t.Fatalf("done = %d, stderr %q", code, errout.String())
	}
	if !strings.Contains(out.String(), "следующий: root-open ") || errout.Len() != 0 {
		t.Errorf("output %q, stderr %q", out.String(), errout.String())
	}
}

// A candidate folder whose docs/tasks is a file is no project either.
func TestDoneHintTasksFileIsNoProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "tool")
	dir := filepath.Join(root, "docs", "tasks")
	scopeTicket(t, dir, "closing", "в работе", "tool-master")
	scopeTicket(t, dir, "root-open", "открыт", "")
	if err := os.MkdirAll(filepath.Join(root, "tool", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tool", "docs", "tasks"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	o := options{cmd: "done", id: "closing", by: "checker", check: "checked by test", baseRoot: root}
	if code := runLockedCommand(root, dir, o, &out, &errout); code != 0 {
		t.Fatalf("done = %d, stderr %q", code, errout.String())
	}
	if !strings.Contains(out.String(), "следующий: root-open ") || errout.Len() != 0 {
		t.Errorf("output %q, stderr %q", out.String(), errout.String())
	}
}
