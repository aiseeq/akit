package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// clearDir создаёт папку или очищает её, кроме .git: история выгрузки остаётся.
func clearDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create OUT_DIR: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list OUT_DIR: %w", err)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("clear OUT_DIR: %w", err)
		}
	}
	return nil
}

// copyTree копирует папку как cp -R: ссылки остаются ссылками, права файлов сохраняются.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return fmt.Errorf("copy %s: %w", p, err)
		}
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return mkdir(to)
		case d.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return fmt.Errorf("copy link: %w", err)
			}
			if err := os.Symlink(link, to); err != nil {
				return fmt.Errorf("copy link: %w", err)
			}
			return nil
		}
		return copyFile(p, to)
	})
}

func copyFile(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return writeFile(dst, data, st.Mode().Perm())
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := mkdir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

func mkdir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create folder: %w", err)
	}
	return nil
}

// textFiles — файлы выгрузки без .git; двоичные (с нулевым байтом) не правятся и не проверяются, как grep -I.
func textFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read export: %w", err)
		}
		if !bytes.ContainsRune(data, 0) {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}
