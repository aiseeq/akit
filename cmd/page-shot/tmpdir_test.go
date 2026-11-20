package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runInTmpdir runs page-shot on a small page with TMPDIR set to a fresh folder whose path is n bytes long and
// returns what is left in that folder after the run.
func runInTmpdir(t *testing.T, n int) []string {
	t.Helper()
	page := filepath.Join(t.TempDir(), "p.html")
	if err := os.WriteFile(page, []byte("<!doctype html><p>x</p>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	base, err := os.MkdirTemp("/tmp", "pt") // short, so that n bytes are reachable whatever TMPDIR is
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if len(base)+2 > n {
		t.Fatalf("temp root %s is longer than %d bytes", base, n)
	}
	tmp := base + "/" + strings.Repeat("d", n-len(base)-1)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	os.Args = []string{"page-shot", "--element", "p", "--out", out, page}
	if err := run(); err != nil {
		t.Fatalf("TMPDIR of %d bytes: %v", n, err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	return left
}

// Chrome and its crash handler write com.google.Chrome.* folders to TMPDIR; page-shot gives Chrome a folder of
// its own and removes it.
func TestRunLeavesTmpdirEmpty(t *testing.T) {
	if left := runInTmpdir(t, 45); len(left) != 0 {
		t.Fatalf("left in TMPDIR: %v", left)
	}
}

// Under a TMPDIR too long for one more level (the singleton socket path of Chrome would exceed 107 bytes) Chrome
// still starts, and nothing is left in TMPDIR.
func TestRunWithLongTmpdir(t *testing.T) {
	short, err := os.MkdirTemp("/tmp", "ps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	old := shortTmpRoot
	shortTmpRoot = short
	t.Cleanup(func() { shortTmpRoot = old })
	if left := runInTmpdir(t, 60); len(left) != 0 {
		t.Fatalf("left in TMPDIR: %v", left)
	}
	if left, err := os.ReadDir(short); err != nil || len(left) != 0 {
		t.Fatalf("left in the short temp root: %v %v", left, err)
	}
}
