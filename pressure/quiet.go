package pressure

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Тихий режим — срок, до которого машина занята, что бы ни показывали замеры: человеку нужна отзывчивая
// машина (созвон, демонстрация экрана), и тяжёлое ждёт заранее, а не после фриза. Срок — одна строка
// RFC3339 в файле состояния; истёкший срок не действует сам, файл никто не убирает.

// QuietPath — файл срока тихого режима: $XDG_STATE_HOME/akit/quiet-until, без XDG_STATE_HOME —
// ~/.local/state/akit/quiet-until.
func QuietPath() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("quiet mode file: %w", err)
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "akit", "quiet-until"), nil
}

// ReadQuiet — срок тихого режима из path; файла нет — нулевое время. Срок может быть и прошедшим.
func ReadQuiet(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("quiet mode: %w", err)
	}
	until, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, fmt.Errorf("quiet mode file %s: %w", path, err)
	}
	return until, nil
}

// SetQuiet пишет срок в path: временный файл рядом и переименование поверх, чтобы читающий не застал
// половину строки.
func SetQuiet(path string, until time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("quiet mode: %w", err)
	}
	t, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("quiet mode: %w", err)
	}
	_, werr := t.WriteString(until.Format(time.RFC3339) + "\n")
	if cerr := t.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(t.Name(), path)
	}
	if werr != nil {
		werr = fmt.Errorf("quiet mode: %w", werr)
		if rerr := os.Remove(t.Name()); rerr != nil {
			werr = errors.Join(werr, fmt.Errorf("temp file left: %w", rerr))
		}
	}
	return werr
}

// ClearQuiet снимает тихий режим: удаляет path; файла нет — режим уже снят.
func ClearQuiet(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("quiet mode: %w", err)
	}
	return nil
}

// quietUntil — срок тихого режима из path, пока он не прошёл к now; иначе нулевое время.
func quietUntil(path string, now time.Time) (time.Time, error) {
	until, err := ReadQuiet(path)
	if err != nil || !now.Before(until) {
		return time.Time{}, err
	}
	return until, nil
}
