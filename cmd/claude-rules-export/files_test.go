package main

import (
	"os"
	"path/filepath"
	"testing"
)

// На OUT_DIR без учёта регистра скиллы A и a сходятся в одной папке, а второй скилл пишет сквозь ссылки первого.
// Здесь та же встреча путей: вторая копия в ту же папку и запись поверх ссылки отказывают, цель ссылки цела.
func TestCopyRefusesExistingPath(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	write(t, filepath.Join(victim, "f"), "keep\n", 0o644)
	upper, lower := filepath.Join(dir, "A"), filepath.Join(dir, "a")
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(victim, "f"), filepath.Join(upper, "f")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(upper, "d")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(lower, "f"), "overwritten\n", 0o644)
	write(t, filepath.Join(lower, "d", "new"), "planted\n", 0o644)
	dst := filepath.Join(dir, "out", "skills", "s")
	if err := copyTree(upper, dst); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(lower, dst); err == nil {
		t.Error("second copy into the same folder: no error")
	}
	if err := writeFile(filepath.Join(dst, "f"), []byte("overwritten\n"), 0o644); err == nil {
		t.Error("write over a link: no error")
	}
	if err := writeFile(filepath.Join(lower, "f"), []byte("again\n"), 0o644); err == nil {
		t.Error("write over an existing file: no error")
	}
	if err := putFile(filepath.Join(dst, "f"), []byte("overwritten\n"), 0o644, os.O_TRUNC); err == nil {
		t.Error("rewrite through a link: no error")
	}
	if err := copyTree(filepath.Join(lower, "d"), filepath.Join(dst, "d")); err == nil {
		t.Error("copy into a linked folder: no error")
	}
	if _, err := os.Lstat(filepath.Join(victim, "new")); err == nil {
		t.Error("file planted through a linked folder")
	}
	if b, err := os.ReadFile(filepath.Join(victim, "f")); err != nil || string(b) != "keep\n" {
		t.Errorf("link target changed: %q %v", b, err)
	}
}
