package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// copyTree копирует папку как cp -R: ссылки остаются ссылками, права файлов сохраняются. Каждую папку, файл и
// ссылку создаёт заново и отказывает, если путь уже есть: на OUT_DIR без учёта регистра два скилла или два имени
// внутри скилла иначе сошлись бы в одном пути и запись пошла бы сквозь ссылку, скопированную раньше.
func copyTree(src, dst string) error {
	if err := mkdir(filepath.Dir(dst)); err != nil {
		return err
	}
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
			if err := os.Mkdir(to, 0o755); err != nil {
				return fmt.Errorf("copy folder: %w", err)
			}
			return nil
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

// writeFile создаёт новый файл: существующий путь или ссылка на его месте — ошибка, не перезапись.
func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := mkdir(filepath.Dir(path)); err != nil {
		return err
	}
	return putFile(path, data, perm, os.O_CREATE|os.O_EXCL)
}

// putFile пишет файл, не проходя по ссылке на его месте.
func putFile(path string, data []byte, perm os.FileMode, flag int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW|flag, perm)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Close(); err != nil {
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
		// .git очистка не трогает, и правка его не трогает: файл .git (worktree, жёсткая ссылка) — не выгрузка
		if d.Name() == ".git" && (d.IsDir() || p == filepath.Join(root, ".git")) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
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
