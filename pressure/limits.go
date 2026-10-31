package pressure

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Пороги пользователя — одно место на всех потребителей правила (busy, очередь graft, сторожа): файл
// LimitsPath поверх DefaultLimits. Формат — плоские строки YAML `ключ: значение` и комментарии `#`;
// вложенности и списков у порогов нет, и разбор своего подмножества строже чужого парсера: опечатка в
// ключе или значении — ошибка с путём и строкой, а не тихое умолчание.

// LimitsPath — файл порогов пользователя: <каталог настроек пользователя>/akit/pressure.yaml
// ($XDG_CONFIG_HOME, без него — ~/.config).
func LimitsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("pressure limits file: %w", err)
	}
	return filepath.Join(dir, "akit", "pressure.yaml"), nil
}

// UserLimits — DefaultLimits поверх файла LimitsPath; файла нет — DefaultLimits.
func UserLimits() (Limits, error) {
	path, err := LimitsPath()
	if err != nil {
		return Limits{}, err
	}
	return ReadLimits(path)
}

// ReadLimits — DefaultLimits поверх файла path; файла нет — DefaultLimits. Неизвестный или повторный
// ключ, значение не того вида или не больше нуля — ошибка с путём, строкой и ключом.
func ReadLimits(path string) (Limits, error) {
	l := DefaultLimits
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return Limits{}, fmt.Errorf("pressure limits: %w", err)
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return Limits{}, fmt.Errorf("%s:%d: want `key: value`, got %q", path, n, line)
		}
		if seen[key] {
			return Limits{}, fmt.Errorf("%s:%d: %s set twice", path, n, key)
		}
		seen[key] = true
		if err := l.Set(key, value); err != nil {
			return Limits{}, fmt.Errorf("%s:%d: %s: %w", path, n, key, err)
		}
	}
	if err := sc.Err(); err != nil {
		return Limits{}, fmt.Errorf("pressure limits %s: %w", path, err)
	}
	return l, nil
}

// LimitKeys — ключи файла порогов.
var LimitKeys = []string{"runnable", "max_io_full", "max_mem_some", "max_swap_kbs", "swap_window"}

// Set — порог key (один из LimitKeys) из текстового значения: одна проверка для файла порогов и флагов
// потребителей. Число — конечное и больше нуля (NaN и Inf молча выключили бы проверку), окно — длительность
// больше нуля.
func (l *Limits) Set(key, value string) error {
	if key == "swap_window" {
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("duration: %w", err)
		}
		if d <= 0 {
			return errors.New("must be positive")
		}
		l.SwapWindow = d
		return nil
	}
	fields := map[string]*float64{"runnable": &l.Runnable, "max_io_full": &l.MaxIOFull, "max_mem_some": &l.MaxMemSome, "max_swap_kbs": &l.MaxSwapKBs}
	field, ok := fields[key]
	if !ok {
		return fmt.Errorf("unknown key (keys: %s)", strings.Join(LimitKeys, ", "))
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("number: %w", err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return errors.New("must be a finite number")
	}
	if v <= 0 {
		return errors.New("must be positive")
	}
	*field = v
	return nil
}

// stripComment отрезает комментарий `#` до конца строки: в значениях порогов `#` не бывает.
func stripComment(s string) string {
	if before, _, ok := strings.Cut(s, "#"); ok {
		return before
	}
	return s
}
