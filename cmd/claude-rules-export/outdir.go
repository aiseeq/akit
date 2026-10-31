package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Выгрузка удаляет в OUT_DIR только записи своего прошлого прогона, которые с тех пор не менялись: путь, тип,
// права, размер и sha256 файла, текст ссылки и inode — как в списке. Список лежит вне OUT_DIR, в каталоге
// состояния, под ключом физического пути OUT_DIR: под другим путём (ссылка, bind mount, overlay, /proc) его нет, и
// непустая папка без списка — отказ. Чужой файл с тем же содержимым — другой inode, тоже отказ. Папки удаляются
// только пустыми, все операции в OUT_DIR — через os.Root: ни путь из списка, ни ссылка, подменённая на ходу, не
// выводят за OUT_DIR.

// entry — запись выгрузки: f — файл, l — ссылка, d — папка. Ino 0 — запись прерванного прогона, inode ещё не
// записан: сверяется без него.
type entry struct {
	Kind string      `json:"kind"`
	Path string      `json:"path"`
	Mode fs.FileMode `json:"mode,omitempty"`
	Size int64       `json:"size,omitempty"`
	Sum  string      `json:"sha256,omitempty"`
	Link string      `json:"link,omitempty"`
	Ino  uint64      `json:"ino,omitempty"`
}

type exportList struct {
	Out     string  `json:"out"`
	Entries []entry `json:"entries"`
}

// transferStep — точка после шага переноса; nil в работе. Тест останавливает на ней прогон, как обрыв процесса.
var transferStep func(step string) error

// listFile — файл списка для OUT_DIR out: $XDG_STATE_HOME/claude-rules-export/<sha256 пути>.json.
func listFile(out string) (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("state folder: %w", err)
		}
		state = filepath.Join(home, ".local", "state")
	}
	sum := sha256.Sum256([]byte(out))
	return filepath.Join(state, "claude-rules-export", hex.EncodeToString(sum[:16])+".json"), nil
}

func loadList(file, out string) ([]entry, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("export list: %w", err)
	}
	var l exportList
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("export list %s: %w", file, err)
	}
	if l.Out != out {
		return nil, fmt.Errorf("export list %s is for %s, not %s", file, l.Out, out)
	}
	for _, e := range l.Entries {
		if err := checkEntryPath(e.Path); err != nil {
			return nil, fmt.Errorf("export list %s: %w", file, err)
		}
	}
	return l.Entries, nil
}

// saveList пишет список целиком через временный файл и rename: обрыв оставляет прежний или новый, не половину.
func saveList(file, out string, entries []entry) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return fmt.Errorf("export list: %w", err)
	}
	data, err := json.MarshalIndent(exportList{Out: out, Entries: entries}, "", " ")
	if err != nil {
		return fmt.Errorf("export list: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".list-*")
	if err != nil {
		return fmt.Errorf("export list: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("export list: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("export list: %w", err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return fmt.Errorf("export list: %w", err)
	}
	return nil
}

// checkEntryPath — путь записи внутри OUT_DIR и не в .git верхнего уровня (в любом регистре: папка может быть без
// учёта регистра).
func checkEntryPath(p string) error {
	if !filepath.IsLocal(p) || p != path.Clean(p) || p == "." || strings.ContainsRune(p, 0) {
		return fmt.Errorf("bad export path %q", p)
	}
	if first, _, _ := strings.Cut(p, "/"); strings.EqualFold(first, ".git") {
		return fmt.Errorf("export path inside .git: %q", p)
	}
	return nil
}

// scanStage — записи собранной выгрузки в папке dir; два пути, равные без учёта регистра, — ошибка до касания
// OUT_DIR: на папке без учёта регистра они сошлись бы в один.
func scanStage(dir string) ([]entry, error) {
	var entries []entry
	folded := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if other, ok := folded[strings.ToLower(rel)]; ok {
			return fmt.Errorf("export paths differ only in case: %s and %s", other, rel)
		}
		folded[strings.ToLower(rel)] = rel
		e, err := describe(func() (fs.FileInfo, error) { return os.Lstat(p) },
			func() (io.ReadCloser, error) { return os.Open(p) },
			func() (string, error) { return os.Readlink(p) }, rel)
		if err != nil {
			return err
		}
		e.Ino = 0
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	return entries, nil
}

// describe — запись по пути rel: тип, права, размер, sha256 файла, текст ссылки, inode.
func describe(lstat func() (fs.FileInfo, error), open func() (io.ReadCloser, error), readlink func() (string, error), rel string) (entry, error) {
	if err := checkEntryPath(rel); err != nil {
		return entry{}, err
	}
	st, err := lstat()
	if err != nil {
		return entry{}, err
	}
	e := entry{Path: rel, Mode: st.Mode().Perm()}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		e.Ino = sys.Ino
	}
	switch {
	case st.Mode().IsRegular():
		e.Kind, e.Size = "f", st.Size()
		f, err := open()
		if err != nil {
			return entry{}, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return entry{}, err
		}
		e.Sum = hex.EncodeToString(h.Sum(nil))
	case st.Mode()&fs.ModeSymlink != 0:
		e.Kind, e.Mode = "l", 0
		if e.Link, err = readlink(); err != nil {
			return entry{}, err
		}
	case st.IsDir():
		e.Kind = "d"
	default:
		return entry{}, fmt.Errorf("%s: not a file, folder or link", rel)
	}
	return e, nil
}

// same — запись на диске совпадает с записью списка; inode — если он в списке есть. Права папки не сверяются:
// оборванный перенос ставит их последними, а пустая папка — всё, что очистка удаляет.
func same(have, want entry) bool {
	return have.Kind == want.Kind && (have.Mode == want.Mode || want.Kind == "d") && have.Size == want.Size && have.Sum == want.Sum &&
		have.Link == want.Link && (want.Ino == 0 || have.Ino == want.Ino)
}

// verifyOut — каждая запись OUT_DIR, кроме .git верхнего уровня, есть в списке и не менялась. Записи из списка,
// которых нет, допустимы: прошлый прогон оборвался посреди очистки.
func verifyOut(root *os.Root, known []entry) error {
	byPath := map[string][]entry{}
	for _, e := range known {
		byPath[e.Path] = append(byPath[e.Path], e)
	}
	var walk func(dir string) error
	walk = func(dir string) error {
		f, err := root.Open(dir)
		if err != nil {
			return fmt.Errorf("OUT_DIR: %w", err)
		}
		names, err := f.Readdirnames(-1)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("OUT_DIR: %w", err)
		}
		slices.Sort(names)
		for _, name := range names {
			if dir == "." && strings.EqualFold(name, ".git") {
				continue
			}
			rel := path.Join(dir, name)
			have, err := describe(func() (fs.FileInfo, error) { return root.Lstat(rel) },
				func() (io.ReadCloser, error) { return root.Open(rel) },
				func() (string, error) { return root.Readlink(rel) }, rel)
			if err != nil {
				return fmt.Errorf("OUT_DIR: %w", err)
			}
			if !slices.ContainsFunc(byPath[rel], func(want entry) bool { return same(have, want) }) {
				if len(known) == 0 {
					return fmt.Errorf("OUT_DIR holds %s and has no export list: give an empty folder (only .git is kept) or a previous export", rel)
				}
				return fmt.Errorf("OUT_DIR holds %s, which is not the previous export's (changed or foreign): nothing is cleared", rel)
			}
			if have.Kind == "d" {
				if err := walk(rel); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(".")
}

// clearOld удаляет записи прошлого прогона: файлы и ссылки, затем папки от глубоких к мелким, только пустые.
// Записи, которой уже нет, — не ошибка.
func clearOld(root *os.Root, old []entry) error {
	var paths []string
	for _, e := range old {
		if !slices.Contains(paths, e.Path) {
			paths = append(paths, e.Path)
		}
	}
	// глубже — раньше: содержимое папки уходит до неё самой
	slices.SortFunc(paths, func(a, b string) int { return strings.Count(b, "/") - strings.Count(a, "/") })
	for _, p := range paths {
		if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear OUT_DIR: %w", err)
		}
	}
	return nil
}

// writeNew переносит выгрузку из stage в OUT_DIR: каждую папку, файл и ссылку создаёт заново, существующий путь —
// ошибка, не перезапись. Возвращает записи с inode созданных.
func writeNew(root *os.Root, stage string, entries []entry) ([]entry, error) {
	done := make([]entry, 0, len(entries))
	for _, e := range entries {
		var err error
		switch e.Kind {
		case "d":
			err = root.Mkdir(e.Path, 0o700)
		case "l":
			err = root.Symlink(e.Link, e.Path)
		case "f":
			err = copyIn(root, filepath.Join(stage, filepath.FromSlash(e.Path)), e.Path)
		}
		if err == nil && e.Kind == "f" {
			err = root.Chmod(e.Path, e.Mode)
		}
		if err != nil {
			return nil, fmt.Errorf("write OUT_DIR: %w", err)
		}
		st, err := root.Lstat(e.Path)
		if err != nil {
			return nil, fmt.Errorf("write OUT_DIR: %w", err)
		}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			e.Ino = sys.Ino
		}
		done = append(done, e)
		if transferStep != nil {
			if err := transferStep("wrote"); err != nil {
				return nil, err
			}
		}
	}
	// права папок — после их содержимого: папка только для чтения иначе не приняла бы своих файлов
	for i := len(done) - 1; i >= 0; i-- {
		if done[i].Kind == "d" {
			if err := root.Chmod(done[i].Path, done[i].Mode); err != nil {
				return nil, fmt.Errorf("write OUT_DIR: %w", err)
			}
		}
	}
	return done, nil
}

func copyIn(root *os.Root, src, name string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// replaceExport кладёт собранную в stage выгрузку в OUT_DIR out: сверка, список прошлого и нового прогона,
// очистка своего, перенос, список с inode. Обрыв на любом шаге оставляет список, по которому следующий прогон
// узнает и старые записи, и уже перенесённые новые.
func replaceExport(out, stage string) error {
	fresh, err := scanStage(stage)
	if err != nil {
		return err
	}
	file, err := listFile(out)
	if err != nil {
		return err
	}
	old, err := loadList(file, out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("create OUT_DIR: %w", err)
	}
	root, err := os.OpenRoot(out)
	if err != nil {
		return fmt.Errorf("OUT_DIR: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := verifyOut(root, old); err != nil {
		return err
	}
	if err := saveList(file, out, append(slices.Clip(old), fresh...)); err != nil {
		return err
	}
	if err := clearOld(root, old); err != nil {
		return err
	}
	if transferStep != nil {
		if err := transferStep("cleared"); err != nil {
			return err
		}
	}
	done, err := writeNew(root, stage, fresh)
	if err != nil {
		return err
	}
	return saveList(file, out, done)
}
