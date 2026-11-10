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
