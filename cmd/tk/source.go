package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

type externalTask struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Prio   string `json:"prio"`
	URL    string `json:"url"`
}

type cappedBuffer struct {
	data     []byte
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := (1 << 20) - len(b.data)
	if len(p) > remaining {
		b.overflow = true
		if remaining > 0 {
			b.data = append(b.data, p[:remaining]...)
		}
		return len(p), nil
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func (b *cappedBuffer) Bytes() []byte  { return b.data }
func (b *cappedBuffer) String() string { return string(b.data) }

func normalizedRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path %s: %w", path, err)
	}
	return filepath.EvalSymlinks(abs)
}

func taskSourcePath(main string) (string, bool, error) {
	if main == "" {
		return "", false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("determine source home: %w", err)
	}
	parent, err := normalizedRoot(filepath.Dir(main))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolve project parent: %w", err)
	}
	for _, name := range []string{"git", "work"} {
		rootPath := filepath.Join(home, name)
		if _, e := os.Stat(rootPath); errors.Is(e, os.ErrNotExist) {
			continue
		} else if e != nil {
			return "", false, fmt.Errorf("inspect projects root %s: %w", rootPath, e)
		}
		root, e := normalizedRoot(rootPath)
		if e != nil {
			return "", false, fmt.Errorf("resolve projects root %s: %w", rootPath, e)
		}
		if root == parent {
			return filepath.Join(parent, ".tk-next-source"), true, nil
		}
	}
	return "", false, nil
}

func runTaskSource(path, project string) ([]externalTask, int, string, bool) {
	if err := trustedSource(path); err != nil {
		return nil, 0, err.Error(), true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, project)
	cmd.Dir = filepath.Dir(path)
	cmd.Stdin = strings.NewReader("")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 500 * time.Millisecond
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.overflow || stderr.overflow {
		return nil, 0, "output over 1 MiB", true
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil, 0, "source did not exit", true
	}
	if ctx.Err() != nil {
		return nil, 0, "source timed out", true
	}
	if err != nil {
		reason := firstStderrLine(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		return nil, 0, reason, true
	}
	return parseExternalTasks(stdout.Bytes())
}

func trustedSource(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("cannot inspect source: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return errors.New("source is not trusted")
	}
	return nil
}

func firstStderrLine(stderr string) string {
	line, _, _ := strings.Cut(stderr, "\n")
	line = strings.TrimSpace(line)
	if len(line) > 200 {
		limit := 200
		for limit > 0 && !utf8.RuneStart(line[limit]) {
			limit--
		}
		line = line[:limit]
	}
	return line
}

func parseExternalTasks(output []byte) ([]externalTask, int, string, bool) {
	var records []json.RawMessage
	d := json.NewDecoder(bytes.NewReader(output))
	if err := d.Decode(&records); err != nil {
		return nil, 0, "invalid JSON array: " + err.Error(), true
	}
	if records == nil {
		return nil, 0, "output is not a JSON array", true
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, 0, "invalid trailing data: " + err.Error(), true
		}
		return nil, 0, "output has trailing data", true
	}
	good := make([]externalTask, 0, len(records))
	bad := 0
	first := ""
	for _, raw := range records {
		item, reason := parseExternalTask(raw)
		if reason != "" {
			bad++
			if first == "" {
				first = reason
			}
			continue
		}
		good = append(good, item)
	}
	if len(records) > 0 && len(good) == 0 {
		return nil, 0, "no valid records", true
	}
	return good, bad, first, false
}

func parseExternalTask(raw json.RawMessage) (externalTask, string) {
	var item externalTask
	if err := json.Unmarshal(raw, &item); err != nil {
		return externalTask{}, "invalid record JSON: " + err.Error()
	}
	if item.Key == "" {
		return externalTask{}, "missing key"
	}
	if item.Title == "" {
		return externalTask{}, "missing title"
	}
	if !validExternalPrio(item.Prio) {
		return externalTask{}, "invalid prio"
	}
	item.Key = safeSourceString(item.Key)
	item.Title = safeSourceString(item.Title)
	item.Status = safeSourceString(item.Status)
	item.URL = safeSourceString(item.URL)
	return item, ""
}

func validExternalPrio(p string) bool { return p == "CRIT" || p == "HIGH" || p == "MED" || p == "LOW" }
func safeSourceString(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
func sourceWarning(w io.Writer, path, reason string) {
	fmt.Fprintf(w, "tk: source %s: %s; showing local tickets only\n", path, maskSourceText(reason))
}
