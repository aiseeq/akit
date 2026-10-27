package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// scanRecords — сколько первых записей транскрипта смотреть в поисках cwd: первые
// записи (название, режим) cwd не несут, он появляется у первой реплики.
const scanRecords = 200

// Transcript — где лежит транскрипт сессии и в какой папке она шла.
type Transcript struct {
	ID   string
	Path string // <projects>/<папка проекта>/<id>.jsonl
	Cwd  string // рабочая папка сессии из первой записи с cwd; "" — записи с cwd ещё нет
}

// ProjectsDir — папка транскриптов в домашней папке пользователя.
func ProjectsDir(home string) string { return filepath.Join(home, ".claude", "projects") }

// Describe находит транскрипт сессии id в папке транскриптов projects и читает
// его cwd. Транскрипта нет — ошибка с fs.ErrNotExist. Один id в двух папках
// проектов (сессию продолжили из другой папки) — берётся последний изменённый файл.
func Describe(projects, id string) (Transcript, error) {
	if id == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return Transcript{}, fmt.Errorf("id сессии %q: ожидается имя файла без папок", id)
	}
	paths, err := filepath.Glob(filepath.Join(projects, "*", id+".jsonl"))
	if err != nil {
		return Transcript{}, fmt.Errorf("транскрипт %s: %w", id, err)
	}
	t := Transcript{ID: id}
	var newest int64
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return Transcript{}, fmt.Errorf("транскрипт %s: %w", id, err)
		}
		if mt := fi.ModTime().UnixNano(); t.Path == "" || mt > newest {
			t.Path, newest = p, mt
		}
	}
	if t.Path == "" {
		return Transcript{}, fmt.Errorf("транскрипт %s в %s: %w", id, projects, os.ErrNotExist)
	}
	if t.Cwd, err = transcriptCwd(t.Path); err != nil {
		return Transcript{}, err
	}
	return t, nil
}

// transcriptCwd — cwd первой записи транскрипта, где он есть. Записи бывают
// большими (вложения инструкций), поэтому разбор потоковый.
func transcriptCwd(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("транскрипт: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for range scanRecords {
		var rec struct {
			Cwd string `json:"cwd"`
		}
		err := dec.Decode(&rec)
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("транскрипт %s: %w", path, err)
		}
		if rec.Cwd != "" {
			return rec.Cwd, nil
		}
	}
	return "", nil
}
