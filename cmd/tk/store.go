package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Внешнее хранилище тикетов: отдельный git-репо с папкой <repo>/docs/tasks на
// каждый репо, чьи тикеты не должны лежать в его собственном дереве.
const (
	storeEnv        = "TK_STORE"
	storeLockFile   = "tk.lock"
	storeLockMode   = 0o600
	indexLockTries  = 20
	indexLockPause  = 250 * time.Millisecond
	indexLockMarker = "index.lock"
)

// taskStore — действующее хранилище: root — корень его git-репо, repo — папка
// текущего репо в нём. Пустой root — хранилища нет.
type taskStore struct {
	root, repo string
}

// externalTaskStore находит хранилище для основного дерева main: папка tasks
// рядом с ним или TK_STORE. Хранилище действует, только если в нём есть
// <имя репо>/docs/tasks.
func externalTaskStore(main string) (taskStore, error) {
	if main == "" {
		return taskStore{}, nil
	}
	root := os.Getenv(storeEnv)
	if root == "" {
		root = filepath.Join(filepath.Dir(main), "tasks")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return taskStore{}, fmt.Errorf("task store path %s: %w", root, err)
	}
	repo := filepath.Join(root, filepath.Base(main))
	tasks := filepath.Join(repo, "docs/tasks")
	info, err := os.Stat(tasks)
	if errors.Is(err, os.ErrNotExist) {
		return taskStore{}, nil
	}
	if err != nil {
		return taskStore{}, fmt.Errorf("task store: %w", err)
	}
	if !info.IsDir() {
		return taskStore{}, fmt.Errorf("task path is not a directory: %s", tasks)
	}
	return taskStore{root: root, repo: repo}, nil
}

func storeGitDir(store string) (string, error) {
	b, err := gitWithRetry(store, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ensureStoreRepo отказывает, если хранилище — не корень своего git-репо:
// иначе коммит ушёл бы в объемлющий репо.
func ensureStoreRepo(store string) error {
	b, err := gitWithRetry(store, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("task store is not a git repository: %w", err)
	}
	top, err := filepath.EvalSymlinks(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("task store toplevel: %w", err)
	}
	want, err := filepath.EvalSymlinks(store)
	if err != nil {
		return fmt.Errorf("task store: %w", err)
	}
	if top != want {
		return fmt.Errorf("task store is not its own git repository: %s", store)
	}
	return nil
}

// lockStore берёт эксклюзивный flock в git-каталоге хранилища; возвращённая
// функция закрывает файл, и замок снимается вместе с ним.
func lockStore(store string) (func(), error) {
	if err := ensureStoreRepo(store); err != nil {
		return nil, err
	}
	gitDir, err := storeGitDir(store)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(gitDir, storeLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, storeLockMode)
	if err != nil {
		return nil, fmt.Errorf("task store lock: %w", err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("task store lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// gitWithRetry повторяет вызов git, пока index.lock держит кто-то другой:
// замок tk к этому времени уже взят, значит это ручной git или другой инструмент.
func gitWithRetry(dir string, args ...string) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil {
			return out, nil
		}
		msg := strings.TrimSpace(stderr.String())
		if attempt >= indexLockTries || !strings.Contains(msg, indexLockMarker) {
			return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), msg, err)
		}
		time.Sleep(indexLockPause)
	}
}

// commitStorePaths коммитит в хранилище только пути этого вызова: чужие
// изменения хранилища остаются как были.
func commitStorePaths(store taskStore, paths []string, message string) error {
	last := paths[len(paths)-1]
	rel := make([]string, 0, len(paths))
	for _, path := range paths {
		p, err := filepath.Rel(store.root, path)
		if err != nil {
			return writtenNotCommitted(store, last, err)
		}
		rel = append(rel, p)
	}
	if _, err := gitWithRetry(store.root, append([]string{"add", "-A", "--"}, rel...)...); err != nil {
		return writtenNotCommitted(store, last, err)
	}
	if _, err := gitWithRetry(store.root, append([]string{"commit", "-m", message, "--"}, rel...)...); err != nil {
		return writtenNotCommitted(store, last, err)
	}
	return nil
}

func writtenNotCommitted(store taskStore, path string, err error) error {
	return fmt.Errorf("written, not committed: %s: %w", relative(store.repo, path), err)
}

// writeTicket пишет тикет; в хранилище — и коммитит его.
func writeTicket(path string, store taskStore, body, message string) error {
	if err := os.WriteFile(path, []byte(body), fileMode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if store.root != "" {
		return commitStorePaths(store, []string{path}, message)
	}
	return nil
}
