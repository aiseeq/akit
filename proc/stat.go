// Package proc читает процессы из /proc: поля stat, снимок всех процессов
// и владельца процесса — сессию агента по CLAUDE_PID в окружении и по scope
// сессии в cgroup (agents.slice/agent-<имя>-<pid>.scope, так их запускает cl).
package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Stat — нужные поля /proc/<pid>/stat.
type Stat struct {
	Comm  string
	State string // поле 3: R, S, D, Z (завершился, родитель ещё не прибрал) …
	Ppid  int
	Start string // поле 22, время старта в тиках с загрузки
}

// ReadStat читает /proc/<pid>/stat. Ошибка — процесса нет (или он недоступен).
func ReadStat(procRoot string, pid int) (Stat, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return Stat{}, fmt.Errorf("stat pid %d: %w", pid, err)
	}
	return ParseStat(string(b))
}

// ParseStat разбирает строку /proc/<pid>/stat. comm может содержать пробелы
// и скобки, поэтому режем по последней ')'.
func ParseStat(s string) (Stat, error) {
	l, r := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if l < 0 || r < l || r+2 > len(s) {
		return Stat{}, fmt.Errorf("stat: нет comm в %.40q", s)
	}
	st := Stat{Comm: s[l+1 : r]}
	f := strings.Fields(s[r+2:])
	// после comm идут поля 3.. ; поле 22 (starttime) — f[19]
	if len(f) < 20 {
		return st, fmt.Errorf("stat: полей %d, нужно 20", len(f))
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return st, fmt.Errorf("stat: ppid: %w", err)
	}
	st.State, st.Ppid, st.Start = f[0], ppid, f[19]
	return st, nil
}
