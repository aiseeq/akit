// Package session — сессии Claude Code: реестр живых сессий (~/.claude/sessions/<pid>.json)
// и транскрипт сессии по её id (~/.claude/projects/<папка проекта>/<id>.jsonl).
// Сессия жива, если pid существует и время его старта совпадает с procStart
// из реестра (иначе pid переиспользован другим процессом).
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aiseeq/akit/proc"
)

// Info — запись реестра сессии.
type Info struct {
	Pid       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	ProcStart string `json:"procStart"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	// StatusUpdatedAt — мс Unix последней смены Status: от неё считается простой.
	StatusUpdatedAt int64 `json:"statusUpdatedAt"`
	// Kind — interactive у сессии в терминале; другие значения в реестре пока не встречались.
	Kind       string `json:"kind"`
	StartedAt  int64  `json:"startedAt"`
	SocketPath string `json:"messagingSocketPath"`
}

// Registry — папка реестра одной учётки и корень /proc (подменяется в тестах).
type Registry struct {
	Dir      string
	ProcRoot string
}

// ForHome — реестр в домашней папке пользователя.
func ForHome(home string) Registry {
	return Registry{Dir: filepath.Join(home, ".claude", "sessions"), ProcRoot: "/proc"}
}

// Get читает запись реестра по pid; нет файла — (nil, nil).
func (r Registry) Get(pid int) (*Info, error) {
	b, err := os.ReadFile(filepath.Join(r.Dir, strconv.Itoa(pid)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("реестр сессий: %w", err)
	}
	var in Info
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("реестр сессий, pid %d: %w", pid, err)
	}
	return &in, nil
}

// List читает все записи реестра (живые и мёртвые). Нечитаемые записи
// пропускаются и возвращаются ошибкой вместе с прочитанными.
func (r Registry) List() ([]Info, error) {
	ents, err := os.ReadDir(r.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("реестр сессий: %w", err)
	}
	var out []Info
	var errs []error
	for _, e := range ents {
		pid, ok := registryPid(e.Name())
		if !ok {
			continue // не запись сессии: <pid>.<sha>.key и прочее
		}
		in, err := r.Get(pid)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err)) // файл мог писаться прямо сейчас
			continue
		}
		if in != nil {
			out = append(out, *in)
		}
	}
	return out, errors.Join(errs...)
}

// IsAlive — процесс pid жив и стартовал в procStart.
func (r Registry) IsAlive(pid int, procStart string) bool {
	if pid <= 0 || procStart == "" {
		return false
	}
	st, err := proc.ReadStat(r.ProcRoot, pid)
	return err == nil && st.Start == procStart
}

// Alive — живые сессии по имени; при совпадении имён берётся самая свежая.
// Ошибка чтения части реестра не мешает вернуть остальное.
func (r Registry) Alive() (map[string]Info, error) {
	all, err := r.List() // при ошибке — что прочиталось, ошибка уходит вызывающему
	out := map[string]Info{}
	for i := range all {
		in := &all[i]
		if in.Name == "" || !r.IsAlive(in.Pid, in.ProcStart) {
			continue
		}
		if old, ok := out[in.Name]; ok && old.StartedAt > in.StartedAt {
			continue
		}
		out[in.Name] = *in
	}
	return out, err
}

// Token — peerToken из <pid>.<sha256>.key; нет файла — пустая строка без ошибки.
func (r Registry) Token(pid int) (string, error) {
	m, err := filepath.Glob(filepath.Join(r.Dir, strconv.Itoa(pid)+".*.key"))
	if err != nil {
		return "", fmt.Errorf("ключ сессии %d: %w", pid, err)
	}
	for _, p := range m {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("ключ сессии: %w", err)
		}
		var k struct {
			PeerToken string `json:"peerToken"`
		}
		if err := json.Unmarshal(b, &k); err != nil {
			return "", fmt.Errorf("%s: %w", p, err)
		}
		if k.PeerToken != "" {
			return k.PeerToken, nil
		}
	}
	return "", nil
}

// registryPid — pid из имени записи реестра "<pid>.json".
func registryPid(name string) (int, bool) {
	s, ok := strings.CutSuffix(name, ".json")
	if !ok || s == "" || len(s) > 9 {
		return 0, false
	}
	pid := 0
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		pid = pid*10 + int(s[i]-'0')
	}
	return pid, true
}
