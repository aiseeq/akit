package pressure

import (
	"encoding/json"
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

// QuietNote — тихий режим: срок и пометка, кто его поставил и зачем. Пометка лежит рядом со сроком,
// в QuietNotePath, а не в его строке: читатели срока, не знающие пометки, разбирают файл как раньше.
type QuietNote struct {
	Until time.Time `json:"until"`
	By    string    `json:"by,omitempty"`  // сессия, поставившая режим; пусто — не из сессии
	Why   string    `json:"why,omitempty"` // зачем
}

// QuietNotePath — файл пометки тихого режима со сроком в path.
func QuietNotePath(path string) string { return path + ".note" }

// SetQuiet пишет срок в path и пометку рядом; без By и Why пометку убирает. Каждый файл — временный
// рядом и переименование поверх, чтобы читающий не застал половину строки.
func SetQuiet(path string, note QuietNote) error {
	// Срок в файле — с точностью до секунды; пометка хранит тот же срок, иначе не узнает свой.
	note.Until = note.Until.Truncate(time.Second)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("quiet mode: %w", err)
	}
	if err := writeReplace(path, []byte(note.Until.Format(time.RFC3339)+"\n")); err != nil {
		return err
	}
	if note.By == "" && note.Why == "" {
		return removeQuiet(QuietNotePath(path))
	}
	data, err := json.Marshal(note)
	if err != nil {
		return fmt.Errorf("quiet mode note: %w", err)
	}
	return writeReplace(QuietNotePath(path), append(data, '\n'))
}

func writeReplace(path string, data []byte) error {
	t, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("quiet mode: %w", err)
	}
	_, werr := t.Write(data)
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

// ClearQuiet снимает тихий режим: удаляет срок в path и пометку; файла нет — режим уже снят.
func ClearQuiet(path string) error {
	return errors.Join(removeQuiet(path), removeQuiet(QuietNotePath(path)))
}

func removeQuiet(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("quiet mode: %w", err)
	}
	return nil
}

// readQuietNote — пометка срока until из файла рядом с path; нет файла или пометка от другого срока
// (срок переставил тот, кто пометок не пишет) — пустая.
func readQuietNote(path string, until time.Time) (QuietNote, error) {
	data, err := os.ReadFile(QuietNotePath(path))
	if errors.Is(err, os.ErrNotExist) {
		return QuietNote{Until: until}, nil
	}
	if err != nil {
		return QuietNote{}, fmt.Errorf("quiet mode note: %w", err)
	}
	var note QuietNote
	if err := json.Unmarshal(data, &note); err != nil {
		return QuietNote{}, fmt.Errorf("quiet mode note %s: %w", QuietNotePath(path), err)
	}
	if !note.Until.Equal(until) {
		return QuietNote{Until: until}, nil
	}
	return note, nil
}

// activeQuiet — тихий режим из path с пометкой, пока срок не прошёл к now; иначе пустой.
func activeQuiet(path string, now time.Time) (QuietNote, error) {
	until, err := ReadQuiet(path)
	if err != nil || !now.Before(until) {
		return QuietNote{}, err
	}
	return readQuietNote(path, until)
}
