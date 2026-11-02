package main

import (
	"bufio"
	"errors"
	"fmt"
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

// hookMessagePrefix — начало сообщений хука webtext; строки других хуков в том же логе — не наши, их формат не проверяется.
const hookMessagePrefix = `msg="webfetch fallback`

func (u *run) readWebtextHookLog(path string, since, until time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open hook log: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for n := 1; scanner.Scan(); n++ {
		line := scanner.Text()
		ours := strings.Contains(line, hookMessagePrefix)
		if !ours && u.hookSummary.hasCoverage {
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
			continue // чужая строка в непривычном формате: покрытие ищется дальше
		}
		if !u.hookSummary.hasCoverage {
			u.hookSummary.coverage = at
			u.hookSummary.hasCoverage = true
		}
		msg := fields["msg"]
		if msg != "webfetch fallback" && msg != "webfetch fallback skipped" {
			continue
		}
		if at.Before(since) || !at.Before(until) {
			continue
		}
		record := hookRecord{day: at.Format("2006-01-02")}
		switch msg {
		case "webfetch fallback skipped":
			record.opened = "skipped"
			u.hookSummary.skipped++
		default:
			if fields["opened"] == "true" {
				record.opened = "true"
				u.hookSummary.opened++
				if fields["source"] == "wayback" {
					record.source = "wayback"
					u.hookSummary.wayback++
				} else {
					record.source = "direct"
					u.hookSummary.direct++
				}
			} else {
				record.opened = "false"
			}
			u.hookSummary.total++
		}
		u.hookCounts[strings.Join([]string{record.day, record.opened, record.source}, "\t")]++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read hook log: %w", err)
	}
	return nil
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
