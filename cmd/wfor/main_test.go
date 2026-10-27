package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func wfor(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	opts, err := parseArgs(args, time.Now)
	if err != nil {
		t.Fatalf("parseArgs %q: %v", args, err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), opts, &out, &errOut)
	return code, out.String(), errOut.String()
}

func later(t *testing.T, d time.Duration, fn func() error) {
	t.Helper()
	errs := make(chan error, 1)
	go func() {
		time.Sleep(d)
		errs <- fn()
	}()
	t.Cleanup(func() {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	})
}

func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	if _, err := f.WriteString(text); err != nil {
		return errors.Join(fmt.Errorf("append %s: %w", path, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	return nil
}

func TestParseArgsErrors(t *testing.T) {
	past := time.Now().Add(-time.Hour).Format("15:04")
	for _, args := range [][]string{
		{}, {"--any"}, {"wait", "x"}, {"exit"}, {"exit", "abc"}, {"line", "x"}, {"line", "(", "f"},
		{"file", "x", "--timeout", "soon"}, {"file", "x", "--every", "-1s"}, {"time", "yesterday"},
		{"time", "2026-01-01T00:00:00Z"}, {"file"}, {"--timeout"}, {"line", "x", "f", "--new", "--after", "3"},
		{"line", "x", "f", "--after", "-1"},
	} {
		if _, err := parseArgs(args, time.Now); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
	if _, err := parseArgs([]string{"time", past}, time.Now); err == nil && past != "00:00" {
		t.Errorf("past %s accepted", past)
	}
}

func TestParseArgs(t *testing.T) {
	opts, err := parseArgs([]string{"exit", "1", "2", "file", "x", "ok", "true", "--every", "50ms", "--any"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.conds) != 3 || opts.conds[0].String() != "exit 1 2" || !opts.any || opts.timeout != defaultTimeout {
		t.Fatalf("%+v", opts)
	}
	// --every после ok всё равно задаёт ему интервал
	if ok := opts.conds[2].(*okCond); ok.every != 50*time.Millisecond {
		t.Errorf("ok every %s", ok.every)
	}
	opts, err = parseArgs([]string{"ok", "true"}, time.Now)
	if err != nil || opts.conds[0].(*okCond).every != defaultOkEvery {
		t.Errorf("default ok every: %v", err)
	}
	if _, err := parseArgs([]string{"line", "-v", "f"}, time.Now); err != nil {
		t.Errorf("regexp starting with a dash: %v", err)
	}
}

func TestFileAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	later(t, 50*time.Millisecond, func() error { return appendFile(path, "x") })
	code, out, _ := wfor(t, "file", path, "--every", "10ms", "--timeout", "5s")
	if code != 0 || !strings.Contains(out, "wfor: done in") || !strings.Contains(out, "(1 bytes)") {
		t.Errorf("code %d: %s", code, out)
	}
}

func TestTimeoutSaysWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	have := filepath.Join(dir, "have")
	if err := appendFile(have, "x"); err != nil {
		t.Fatal(err)
	}
	code, out, _ := wfor(t, "file", filepath.Join(dir, "none"), "file", have, "--every", "10ms", "--timeout", "100ms")
	if code != exitTimeout || !strings.Contains(out, "still waiting: file "+dir+"/none (absent); met: file "+have) ||
		!strings.HasSuffix(out, "; to keep waiting run the same wfor again\n") {
		t.Errorf("code %d: %s", code, out)
	}
	code, out, _ = wfor(t, "file", filepath.Join(dir, "none"), "file", have, "--any", "--timeout", "100ms")
	if code != 0 || !strings.Contains(out, "done in 0s: file "+have) {
		t.Errorf("--any: code %d: %s", code, out)
	}
}

func TestLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := appendFile(path, "PASS old\n"); err != nil {
		t.Fatal(err)
	}
	code, out, _ := wfor(t, "line", "^PASS", path, "--timeout", "1s")
	if code != 0 || !strings.Contains(out, "(line 1: PASS old)") {
		t.Errorf("code %d: %s", code, out)
	}
	// --new: старое не в счёт; недописанная строка тоже находится
	later(t, 50*time.Millisecond, func() error { return appendFile(path, "build\nPASS new") })
	code, out, _ = wfor(t, "line", "^PASS", path, "--new", "--every", "10ms", "--timeout", "5s")
	if code != 0 || !strings.Contains(out, "(line 3: PASS new)") {
		t.Errorf("--new: code %d: %s", code, out)
	}
	code, out, _ = wfor(t, "line", "FAIL", path, "--every", "10ms", "--timeout", "50ms")
	if code != exitTimeout || !strings.Contains(out, "2 lines, last: PASS new") {
		t.Errorf("timeout: code %d: %s", code, out)
	}
}

func TestLineFileRewritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := appendFile(path, "a long first line\nanother line\n"); err != nil {
		t.Fatal(err)
	}
	opts, err := parseArgs([]string{"line", "^ok", path}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	c := opts.conds[0]
	if ok, err := c.check(context.Background()); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if err := os.WriteFile(path, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.check(context.Background()); !ok || err != nil || c.state() != "line 1: ok" {
		t.Errorf("rewritten file: %v %v %s", ok, err, c.state())
	}
}

func TestCheckErrorStops(t *testing.T) {
	code, _, errOut := wfor(t, "line", "x", t.TempDir(), "--timeout", "1s")
	if code != exitUsage || !strings.Contains(errOut, "is a directory") {
		t.Errorf("code %d: %s", code, errOut)
	}
}

func TestExit(t *testing.T) {
	cmd := exec.Command("sleep", "0.2")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// родитель не прибирает процесс: он станет зомби, а это тоже конец
	t.Cleanup(func() { _ = cmd.Wait() })
	code, out, _ := wfor(t, "exit", itoa(pid), "--every", "10ms", "--timeout", "100ms")
	if code != exitTimeout || !strings.Contains(out, "running: "+itoa(pid)+" (sleep)") {
		t.Errorf("code %d: %s", code, out)
	}
	code, out, _ = wfor(t, "exit", itoa(pid), "--every", "10ms", "--timeout", "5s")
	if code != 0 || !strings.Contains(out, "(exited)") {
		t.Errorf("code %d: %s", code, out)
	}
	code, out, _ = wfor(t, "exit", itoa(pid), "--timeout", "1s")
	if code != 0 || !strings.Contains(out, "not running at start: "+itoa(pid)) {
		t.Errorf("zombie at start: code %d: %s", code, out)
	}
}

func TestOk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flag")
	later(t, 50*time.Millisecond, func() error { return appendFile(path, "x") })
	code, out, _ := wfor(t, "ok", "test -e "+path, "--every", "10ms", "--timeout", "5s")
	if code != 0 || !strings.Contains(out, "last exit 0") {
		t.Errorf("code %d: %s", code, out)
	}
	code, out, _ = wfor(t, "ok", "echo not yet; exit 3", "--every", "10ms", "--timeout", "100ms")
	if code != exitTimeout || !strings.Contains(out, "last exit 3, output: not yet") {
		t.Errorf("code %d: %s", code, out)
	}
}

func TestTime(t *testing.T) {
	at := time.Now().Add(1500 * time.Millisecond).Format(time.RFC3339)
	code, out, _ := wfor(t, "time", at, "--every", "50ms", "--timeout", "5s")
	if code != 0 || !strings.Contains(out, "(reached)") {
		t.Errorf("code %d: %s", code, out)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// Файл заменяют целиком: mv нового поверх (другой inode) или перезапись на месте более длинным текстом. Обе замены
// читаются сначала, а не с прежнего смещения (было: ждали до таймаута при готовой версии).
func TestLineFileReplaced(t *testing.T) {
	for name, replace := range map[string]func(path, text string) error{
		"mv": func(path, text string) error {
			if err := os.WriteFile(path+".tmp", []byte(text), 0o644); err != nil {
				return fmt.Errorf("write tmp: %w", err)
			}
			return os.Rename(path+".tmp", path)
		},
		"in place": func(path, text string) error { return os.WriteFile(path, []byte(text), 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state")
			if err := os.WriteFile(path, []byte("pending\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"line", "^ready v", path}, {"line", "^ready v", path, "--new"}} {
				opts, err := parseArgs(args, time.Now)
				if err != nil {
					t.Fatal(err)
				}
				c := opts.conds[0]
				if ok, err := c.check(context.Background()); ok || err != nil {
					t.Fatalf("%q before: %v %v", args, ok, err)
				}
				if err := replace(path, "ready v1.23.110\n"); err != nil {
					t.Fatal(err)
				}
				if ok, err := c.check(context.Background()); !ok || err != nil || c.state() != "line 1: ready v1.23.110" {
					t.Errorf("%q: %v %v %s", args, ok, err, c.state())
				}
				if err := os.WriteFile(path, []byte("pending\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Дописывание в тот же файл не сбивает смещение: старые строки не читаются второй раз.
func TestLineAppendKeepsOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := appendFile(path, "one\n"); err != nil {
		t.Fatal(err)
	}
	opts, err := parseArgs([]string{"line", "^three", path}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	c := opts.conds[0]
	for _, more := range []string{"two\n", "three\n"} {
		if _, err := c.check(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := appendFile(path, more); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := c.check(context.Background()); !ok || err != nil || c.state() != "line 3: three" {
		t.Errorf("%v %v %s", ok, err, c.state())
	}
}

// Ожидание шагами: таймаут по умолчанию меньше 5 минут, повтор с --after продолжает с места прошлого вызова и не
// теряет строки, дописанные между вызовами.
func TestRerunContinues(t *testing.T) {
	if defaultTimeout >= 5*time.Minute {
		t.Errorf("default timeout %s: a sub-agent's prompt cache lives 5m", defaultTimeout)
	}
	path := filepath.Join(t.TempDir(), "log")
	if err := appendFile(path, "PASS old\nbuild\n"); err != nil {
		t.Fatal(err)
	}
	code, out, _ := wfor(t, "line", "^PASS", path, "--new", "--every", "10ms", "--timeout", "50ms")
	if code != exitTimeout || !strings.Contains(out, "run it again with --after 2 in place of --new/--after") ||
		!strings.Contains(out, "(2 lines, last: build)") {
		t.Fatalf("code %d: %s", code, out)
	}
	// между вызовами дописали итог: --new его бы пропустил, --after 2 — находит
	if err := appendFile(path, "PASS new\n"); err != nil {
		t.Fatal(err)
	}
	code, out, _ = wfor(t, "line", "^PASS", path, "--after", "2", "--timeout", "1s")
	if code != 0 || !strings.Contains(out, "(line 3: PASS new)") {
		t.Errorf("--after: code %d: %s", code, out)
	}
	code, out, _ = wfor(t, "line", "^PASS", path, "line", "^FAIL", path, "--new", "--every", "10ms", "--timeout", "50ms")
	if code != exitTimeout || !strings.Contains(out, "own --after (lines read: "+path+" 3, "+path+" 3)") {
		t.Errorf("two line conditions: code %d: %s", code, out)
	}
}
