package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type hookSummary struct {
	opened, total, direct, wayback, skipped int
	coverage                                time.Time
	hasCoverage                             bool
}

type hookRecord struct {
	day, opened, source string
}

func (u *run) readWebtextHookLogs(since, until time.Time) {
	logs := []string{
		filepath.Join(u.home, ".claude", "logs", "chooks.log.1"),
		filepath.Join(u.home, ".claude", "logs", "chooks.log"),
	}
	// Файлы — от старого к новому: первая разобранная строка по порядку и есть начало покрытия лога.
	for _, path := range logs {
		if err := u.readWebtextHookLog(path, since, until); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				u.hookMissing = append(u.hookMissing, filepath.Base(path))
				continue
			}
			u.hookReadErrors = append(u.hookReadErrors, fmt.Sprintf("%s: %v", path, err))
		}
	}
}

// hookMessage — сообщения хука webtext целиком; строки других хуков в том же логе — не наши, их формат не проверяется.
const hookMessageStart = `msg="webfetch fallback`

// leadingHookTime — время строки лога хуков из её начала: time="ГГГГ-ММ-ДД чч:мм:сс".
func leadingHookTime(line string) (time.Time, bool) {
	const prefix, layout = `time="`, "2006-01-02 15:04:05"
	if !strings.HasPrefix(line, prefix) || len(line) < len(prefix)+len(layout)+1 || line[len(prefix)+len(layout)] != '"' {
		return time.Time{}, false
	}
	at, err := time.ParseInLocation(layout, line[len(prefix):len(prefix)+len(layout)], time.Local)
	return at, err == nil
}

// isHookLine — строка сообщения хука webtext: имя целиком в кавычках («webfetch fallback» или «… skipped») или
// строка, обрезанная внутри имени, — та повреждённая, а не чужая. «webfetch fallback statistics» — чужое сообщение.
func isHookLine(line string) bool {
	i := strings.Index(line, hookMessageStart)
	if i < 0 || i > 0 && line[i-1] != ' ' {
		return false
	}
	rest := line[i+len(hookMessageStart):]
	// Имя целиком — строка наша, даже если дальше формат сломан: её разбор и скажет, что она повреждена.
	return strings.HasPrefix(rest, `"`) || strings.HasPrefix(rest, ` skipped"`) || strings.HasPrefix(` skipped"`, rest)
}

func (u *run) readWebtextHookLog(path string, since, until time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open hook log: %w", err)
	}
	defer f.Close()
	// Reader, не Scanner: длинная чужая строка не должна обрывать чтение файла.
	reader := bufio.NewReader(f)
	for n := 1; ; n++ {
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) && line == "" {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read hook log: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		ours := isHookLine(line)
		if !u.hookSummary.hasCoverage {
			// Покрытие — по времени в начале строки любого хука, даже если остальная строка не logfmt.
			if at, ok := leadingHookTime(line); ok {
				u.hookSummary.coverage, u.hookSummary.hasCoverage = at, true
			}
		}
		if !ours {
			continue
		}
		fields, err := parseHookFields(line)
		var at time.Time
		if err == nil {
			at, err = time.ParseInLocation("2006-01-02 15:04:05", fields["time"], time.Local)
		}
		if err != nil {
			if ours {
				u.recordBadHookLine(path, n, err)
			}
			continue
		}
		msg := fields["msg"]
		if msg != "webfetch fallback" && msg != "webfetch fallback skipped" {
			continue
		}
		record, err := hookOutcome(msg, fields)
		if err != nil {
			u.recordBadHookLine(path, n, err)
			continue
		}
		if at.Before(since) || !at.Before(until) {
			continue
		}
		record.day = at.Format("2006-01-02")
		u.countHookRecord(record)
	}
	return nil
}

// hookOutcome — исход строки хука: skipped, false или true с источником direct|wayback; иное значение — ошибка
// формата, а не исход.
func hookOutcome(msg string, fields map[string]string) (hookRecord, error) {
	if msg == "webfetch fallback skipped" {
		return hookRecord{opened: "skipped"}, nil
	}
	source, hasSource := fields["source"]
	switch opened := fields["opened"]; {
	case !hasSource:
		return hookRecord{}, fmt.Errorf("no source field")
	case opened == "false" && source == "":
		return hookRecord{opened: "false"}, nil
	case opened == "false":
		return hookRecord{}, fmt.Errorf("source %q for an unopened page", source)
	case opened == "true" && (source == "direct" || source == "wayback"):
		return hookRecord{opened: "true", source: source}, nil
	case opened == "true":
		return hookRecord{}, fmt.Errorf("unknown source %q", source)
	default:
		return hookRecord{}, fmt.Errorf("unknown opened %q", opened)
	}
}

func (u *run) countHookRecord(record hookRecord) {
	switch record.opened {
	case "skipped":
		u.hookSummary.skipped++
	case "true":
		u.hookSummary.opened++
		u.hookSummary.total++
		if record.source == "wayback" {
			u.hookSummary.wayback++
		} else {
			u.hookSummary.direct++
		}
	default:
		u.hookSummary.total++
	}
	u.hookCounts[strings.Join([]string{record.day, record.opened, record.source}, "\t")]++
}

func (u *run) recordBadHookLine(path string, line int, err error) {
	u.hookBadLines++
	if u.firstHookBad == "" {
		u.firstHookBad = fmt.Sprintf("%s:%d: %v", path, line, err)
	}
}

func parseHookFields(line string) (map[string]string, error) {
	fields := make(map[string]string)
	line = strings.TrimSpace(line)
	for line != "" {
		equal := strings.IndexByte(line, '=')
		if equal <= 0 || strings.IndexByte(line[:equal], ' ') >= 0 {
			return nil, fmt.Errorf("malformed logfmt key")
		}
		key := line[:equal]
		line = line[equal+1:]
		value, rest, err := parseHookValue(line)
		if err != nil {
			return nil, fmt.Errorf("logfmt field %s: %w", key, err)
		}
		fields[key] = value
		line = strings.TrimLeft(rest, " ")
	}
	return fields, nil
}

func parseHookValue(input string) (string, string, error) {
	if input == "" {
		return "", "", fmt.Errorf("missing value")
	}
	if input[0] == '"' {
		quoted, err := strconv.QuotedPrefix(input)
		if err != nil {
			return "", "", fmt.Errorf("quoted value: %w", err)
		}
		value, err := strconv.Unquote(quoted)
		if err != nil {
			return "", "", fmt.Errorf("unquote value: %w", err)
		}
		rest := input[len(quoted):]
		if rest != "" && rest[0] != ' ' {
			return "", "", fmt.Errorf("expected field separator")
		}
		return value, rest, nil
	}
	separator := strings.IndexByte(input, ' ')
	if separator < 0 {
		return input, "", nil
	}
	return input[:separator], input[separator:], nil
}

func (u *run) writeWebtextHook() error {
	path := filepath.Join(u.outputDir, "webtext-hook.tsv")
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create webtext-hook.tsv: %w", err)
	}
	w := bufio.NewWriter(f)
	if _, err := w.WriteString("day\topened\tsource\tcount\n"); err != nil {
		f.Close()
		return fmt.Errorf("write webtext-hook.tsv: %w", err)
	}
	keys := make([]string, 0, len(u.hookCounts))
	for key := range u.hookCounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := fmt.Fprintf(w, "%s\t%d\n", key, u.hookCounts[key]); err != nil {
			f.Close()
			return fmt.Errorf("write webtext-hook.tsv: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return fmt.Errorf("flush webtext-hook.tsv: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close webtext-hook.tsv: %w", err)
	}
	return nil
}
