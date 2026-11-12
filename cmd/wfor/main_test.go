package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
)

func TestOKCondWarnsWhenCancelStopDegrades(t *testing.T) {
	oldCancel, oldStderr := cancelByStopWithObserver, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	cancelByStopWithObserver = func(cmd *exec.Cmd, _ time.Duration, _ syscall.Signal, observer func(bool, error)) error {
		observer(true, nil)
		return nil
	}
	t.Cleanup(func() {
		cancelByStopWithObserver, os.Stderr = oldCancel, oldStderr
		_ = r.Close()
		_ = w.Close()
	})
	cond := &okCond{cmd: "true", every: time.Second, now: time.Now}
	ok, checkErr := cond.check(context.Background())
	_ = w.Close()
	os.Stderr = oldStderr
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || checkErr != nil || !strings.Contains(string(got), "остановлен только прямой потомок: pidfd недоступен") {
		t.Fatalf("check=(%v,%v), stderr=%q", ok, checkErr, got)
	}
}

func TestMain(m *testing.M) {
	if err := proc.BecomeChildSubreaper(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

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
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	past := now.Add(-time.Hour).Format("15:04")
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
	if _, err := parseArgs([]string{"time", past}, func() time.Time { return now }); err == nil {
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
	code, out, _ = wfor(t, "ok", "echo not yet; exit 3", "--every", "10ms", "--timeout", "1s")
	if code != exitTimeout || !strings.Contains(out, "last exit 3, output: not yet") {
		t.Errorf("code %d: %s", code, out)
	}
}

func TestOkTimeoutKillsProcessGroup(t *testing.T) {
	start := time.Now()
	code, out, _ := wfor(t, "ok", "sleep 2 & wait", "--timeout", "100ms")
	if elapsed := time.Since(start); elapsed > time.Second || code != exitTimeout {
		t.Errorf("timeout returned after %s with code %d: %s", elapsed, code, out)
	}
}

func TestOkKillsLeftoverProcesses(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	cond := &okCond{cmd: "sleep 2 & echo $! >" + pidPath + "; exit 0", every: time.Second, now: time.Now}
	ok, err := cond.check(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second || !ok || err != nil {
		t.Errorf("successful command returned after %s, ok=%v err=%v", elapsed, ok, err)
	}
	t.Cleanup(func() {
		if _, err := proc.StopChildGroup(cond.lastCommand, 0, syscall.SIGKILL); err != nil {
			t.Errorf("stop test command group: %v", err)
		}
	})
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		stat, readErr := os.ReadFile(filepath.Join("/proc", itoa(pid), "stat"))
		fields := strings.Fields(string(stat[strings.LastIndex(string(stat), ")")+1:]))
		if readErr != nil || len(fields) > 0 && fields[0] == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("background process %d is still alive", pid)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRepeatedOkChecksReapOrphans(t *testing.T) {
	if os.Getenv("WFOR_ORPHAN_REAPER_HELPER") == "1" {
		groups := make(map[int]bool)
		for range 12 {
			cond := &okCond{cmd: "sleep 0.05 & exit 1", now: time.Now}
			if ok, err := cond.check(context.Background()); ok || err != nil {
				os.Exit(70)
			}
			groups[cond.lastCommand.Process.Pid] = true
		}
		entries, err := os.ReadDir("/proc")
		if err != nil {
			os.Exit(71)
		}
		var zombies []string
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 1 {
				continue
			}
			stat, err := proc.ReadStat("/proc", pid)
			if err == nil && stat.Ppid == os.Getpid() && stat.State == "Z" && groups[stat.Pgid] {
				zombies = append(zombies, strconv.Itoa(pid))
			}
		}
		if len(zombies) > 0 {
			_ = os.WriteFile(os.Getenv("WFOR_ORPHAN_REAPER_ZOMBIES"), []byte(strings.Join(zombies, "\n")), 0o600)
			os.Exit(72)
		}
		os.Exit(0)
	}
	zombies := filepath.Join(t.TempDir(), "zombies")
	helper := exec.Command(os.Args[0], "-test.run=^TestRepeatedOkChecksReapOrphans$")
	helper.Env = append(os.Environ(), "WFOR_ORPHAN_REAPER_HELPER=1", "WFOR_ORPHAN_REAPER_ZOMBIES="+zombies)
	if output, err := helper.CombinedOutput(); err != nil {
		if data, readErr := os.ReadFile(zombies); readErr == nil {
			for field := range strings.FieldsSeq(string(data)) {
				pid, parseErr := strconv.Atoi(field)
				if parseErr == nil {
					var status syscall.WaitStatus
					_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
				}
			}
		}
		t.Fatalf("repeated orphan checks left a zombie or failed: %v; output=%s", err, output)
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

// Неполная последняя строка, бывшая в файле до старта, с --new не в счёт ни сразу, ни дописанная.
func TestNewSkipsPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := appendFile(path, "build\nold DONE"); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := wfor(t, "line", "DONE", path, "--new", "--every", "10ms", "--timeout", "50ms"); code != exitTimeout ||
		!strings.Contains(out, "--after 2 in place of") || !strings.Contains(out, "(1 lines and one begun before the start, last: build)") {
		t.Errorf("прежняя неполная строка засчитана сразу или не в подсказке --after: code %d: %s", code, out)
	}
	// повтор по подсказке, пока строка не дописана, тоже её не засчитывает
	if code, out, _ := wfor(t, "line", "DONE", path, "--after", "2", "--every", "10ms", "--timeout", "50ms"); code != exitTimeout {
		t.Errorf("--after 2 засчитал прежнюю неполную строку: code %d: %s", code, out)
	}
	c, err := newLineCond("DONE", path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendFile(path, " tail"); err != nil {
		t.Fatal(err)
	}
	if hit, err := c.check(t.Context()); hit || err != nil {
		t.Fatalf("недописанная прежняя строка: hit %v %v", hit, err)
	}
	if err := appendFile(path, "\nDONE new\n"); err != nil {
		t.Fatal(err)
	}
	if hit, err := c.check(t.Context()); !hit || err != nil || c.hitLine != 3 || c.hit != "DONE new" {
		t.Errorf("hit %v %v: line %d %q", hit, err, c.hitLine, c.hit)
	}
}

// Файл заменён (новый inode), пока ждали конца прежней неполной строки: новое содержимое — не прежнее, его первая
// строка в счёт.
func TestNewPartialLineThenReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := appendFile(path, "a\nold DONE"); err != nil {
		t.Fatal(err)
	}
	c, err := newLineCond("DONE", path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hit, err := c.check(t.Context()); hit || err != nil {
		t.Fatalf("прежняя неполная строка: hit %v %v", hit, err)
	}
	if err := appendFile(filepath.Join(dir, "new"), "DONE"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "new"), path); err != nil {
		t.Fatal(err)
	}
	if hit, err := c.check(t.Context()); !hit || err != nil || c.hitLine != 1 {
		t.Errorf("новый файл: hit %v %v, line %d", hit, err, c.hitLine)
	}
}

func TestWaitUntilClockTimeNearest(t *testing.T) {
	at := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.Local) }
	for _, c := range []struct {
		arg  string
		now  time.Time
		want time.Time
	}{
		{"00:03", at(2, 23, 56), at(3, 0, 3)},
		{"00:03:30", at(2, 23, 56), time.Date(2026, 11, 3, 0, 3, 30, 0, time.Local)},
		{"14:00", at(2, 12, 0), at(2, 14, 0)},
		{"21:00", at(2, 8, 0), at(2, 21, 0)},
		{"23:59", at(3, 0, 1), at(3, 23, 59)},
		{"07:00", at(2, 20, 0), at(3, 7, 0)},
		{"03.11 00:03", at(2, 23, 56), at(3, 0, 3)},
		{"00:03:00.5", at(2, 23, 56), time.Date(2026, 11, 3, 0, 3, 0, 5e8, time.Local)},
		{" 7:05 ", at(2, 6, 0), at(2, 7, 5)},
	} {
		got, err := waitUntil(c.arg, c.now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("waitUntil(%q, %s) = %s, %v; want %s", c.arg, c.now, got, err, c.want)
		}
	}
	for _, c := range []struct {
		arg  string
		now  time.Time
		want string
	}{
		{"12:00", at(2, 12, 5), "already past"},
		{"07:00", at(2, 8, 0), "already past"},
		{"02.11 00:03", at(2, 23, 56), "already past"},
		{"5m", at(2, 23, 56), "ago"},
		{"200000d", at(2, 23, 56), "ago"},
		{"25:00", at(2, 23, 56), "25:00"},
	} {
		if got, err := waitUntil(c.arg, c.now); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("waitUntil(%q, %s) = %s, %v; want an error with %q", c.arg, c.now, got, err, c.want)
		}
	}
}

func TestWaitUntilAcrossDaylightSaving(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal("zone data:", err)
	}
	// The clocks go back at 03:00 on 2026-10-25, between today's 00:30 and tomorrow's:
	// tomorrow's 00:30 is 25 hours on, and a plain 24 hours would give 23:30, already past.
	now := time.Date(2026, 10, 25, 23, 50, 0, 0, loc)
	got, err := waitUntil("00:30", now)
	if want := time.Date(2026, 10, 26, 0, 30, 0, 0, loc); err != nil || !got.Equal(want) {
		t.Fatalf("waitUntil = %s, %v; want %s", got, err, want)
	}
	// The clocks go forward at 02:00 on 2026-03-29: today's 02:30 does not exist,
	// and tomorrow's 02:30 must stay 02:30.
	got, err = waitUntil("02:30", time.Date(2026, 3, 29, 23, 50, 0, 0, loc))
	if want := time.Date(2026, 3, 30, 2, 30, 0, 0, loc); err != nil || !got.Equal(want) {
		t.Fatalf("waitUntil after the clocks go forward = %s, %v; want %s", got, err, want)
	}
}

func TestWaitUntilParseErrorNamesOnlyMoments(t *testing.T) {
	_, err := waitUntil("tomorrow", time.Date(2026, 11, 2, 12, 0, 0, 0, time.Local))
	if err == nil || strings.Contains(err.Error(), "30m") || !strings.Contains(err.Error(), "14:45") {
		t.Fatalf("parse error = %v; want the moment forms without ago forms", err)
	}
}
