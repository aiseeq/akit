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

	"github.com/aiseeq/akit/queuegate"
	"github.com/aiseeq/akit/secretmask"
)

type externalTask struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Prio   string `json:"prio"`
	URL    string `json:"url"`
	// Started marks work already begun at the source; the queue keeps it
	// visible under a closed pair, as it does a local ticket in progress.
	Started bool `json:"started,omitempty"`
	// CanTake is whether tk next would show it to the master of the project.
	CanTake bool `json:"can_take"`
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

var normalizedRoot = queuegate.Resolve

func taskSourcePath(main string) (string, bool, error) {
	return taskCommandPath(main, ".tk-next-source")
}

func taskCommandPath(main, filename string) (string, bool, error) {
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
			return filepath.Join(parent, filename), true, nil
		}
	}
	return "", false, nil
}

func runBoundedCommand(path string, args []string, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = filepath.Dir(path)
	cmd.Stdin = bytes.NewReader(stdin)
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
		return nil, stderr.Bytes(), errors.New("output over 1 MiB")
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil, stderr.Bytes(), commandFailure(errors.New("command did not exit"), stderr.String())
	}
	if ctx.Err() != nil {
		return nil, stderr.Bytes(), commandFailure(errors.New("command timed out"), stderr.String())
	}
	if err != nil {
		return nil, stderr.Bytes(), commandFailure(fmt.Errorf("run command: %w", err), stderr.String())
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

func runTaskSource(path, project string) ([]externalTask, int, string, bool) {
	if err := trustedSource(path); err != nil {
		return nil, 0, err.Error(), true
	}
	stdout, _, err := runBoundedCommand(path, []string{project}, nil, 5*time.Second)
	if err != nil {
		return nil, 0, err.Error(), true
	}
	return parseExternalTasks(stdout)
}

func trustedSource(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("cannot inspect command: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return errors.New("not executable")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("writable by group or others")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return errors.New("owned by another user")
	}
	return nil
}

func commandFailure(err error, stderr string) error {
	message := err.Error()
	if line := firstStderrLine(stderr); line != "" {
		message += ": " + line
	}
	return errors.New(message)
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
	masked, _ := secretmask.Mask(line)
	return strings.ReplaceAll(masked, "\n", " ")
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
	fmt.Fprintf(w, "tk: source %s: %s; showing local tickets only\n", path, firstStderrLine(reason))
}
