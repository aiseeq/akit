package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
	"golang.org/x/sys/unix"
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

func TestKernelWakeFileConditions(t *testing.T) {
	t.Run("file creation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "target")
		assertWake(t, []string{"file", path, "--every", "10s", "--timeout", "3s"}, func() error {
			return os.WriteFile(path, []byte("ready"), 0o644)
		})
	})
	t.Run("line append", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "log")
		if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertWake(t, []string{"line", "^READY$", path, "--new", "--every", "10s", "--timeout", "3s"}, func() error {
			return appendFile(path, "READY\n")
		})
	})
	t.Run("line replacement", func(t *testing.T) {
		dir := t.TempDir()
		path, replacement := filepath.Join(dir, "log"), filepath.Join(dir, "new")
		if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertWake(t, []string{"line", "^READY$", path, "--every", "10s", "--timeout", "3s"}, func() error {
			if err := os.WriteFile(replacement, []byte("READY\n"), 0o644); err != nil {
				return err
			}
			return os.Rename(replacement, path)
		})
	})
	t.Run("line appears", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "log")
		assertWake(t, []string{"line", "^READY$", path, "--every", "10s", "--timeout", "3s"}, func() error {
			return os.WriteFile(path, []byte("READY\n"), 0o644)
		})
	})
	t.Run("unrelated write", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "watched")
		opts, err := parseArgs([]string{"file", path, "--every", "10s", "--timeout", "2s"}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		written := make(chan time.Time, 1)
		started := time.Now()
		go func() {
			if err := waitForInotifyFD(os.Getpid(), 1500*time.Millisecond); err != nil {
				t.Errorf("wait for watcher: %v", err)
				return
			}
			if err := os.WriteFile(filepath.Join(dir, "other"), []byte("x"), 0o644); err != nil {
				t.Errorf("unrelated write: %v", err)
			}
			written <- time.Now()
		}()
		var out, errOut bytes.Buffer
		if code := run(context.Background(), opts, &out, &errOut); code != exitTimeout {
			t.Fatalf("unrelated event woke condition: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
		}
		select {
		case at := <-written:
			if at.After(started.Add(2 * time.Second)) {
				t.Fatal("unrelated write happened after the timeout")
			}
		case <-time.After(time.Second):
			t.Fatal("unrelated writer did not finish")
		}
	})
}

func TestLineWatcherWakesWhileWriterKeepsFileOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held-open.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	line, err := newLineCond("DONE", path, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events, cleanup, err := watchFiles(ctx, []cond{line})
	if err != nil {
		t.Fatal(err)
	}
	if events == nil {
		t.Fatal("file watcher unavailable")
	}
	defer func() {
		cancel()
		cleanup()
	}()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("DONE\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("IN_MODIFY did not wake the line watcher while the writer held the file open")
	}
	matched, err := line.checkEvent()
	if err != nil || !matched {
		t.Fatalf("line check while writer held file: matched=%v err=%v", matched, err)
	}
}

func TestFileWatcherWakesWhileWriterKeepsFileOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held-open.file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file := &fileCond{path: path}
	ctx, cancel := context.WithCancel(context.Background())
	events, cleanup, err := watchFiles(ctx, []cond{file})
	if err != nil {
		t.Fatal(err)
	}
	if events == nil {
		cancel()
		cleanup()
		t.Fatal("file watcher unavailable")
	}
	defer func() { cancel(); cleanup() }()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("ready"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("IN_MODIFY did not wake the file condition while the writer held the file open")
	}
	matched, err := file.check(context.Background())
	if err != nil || !matched {
		t.Fatalf("file check while writer held file: matched=%v err=%v", matched, err)
	}
}

func TestLineWatcherCoalescesWritesToWatchedFile(t *testing.T) {
	const helperEnv = "AKIT_WFOR_BUSY_LINE_HELPER"
	if watched := os.Getenv(helperEnv); watched != "" {
		opts, err := parseArgs([]string{"line", "NEVER", watched, "--timeout", "3s", "--every", "10s"}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if code := run(context.Background(), opts, io.Discard, io.Discard); code != exitTimeout {
			t.Fatalf("helper run code=%d, want timeout", code)
		}
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "watched.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestLineWatcherCoalescesWritesToWatchedFile$")
	helper.Env = append(os.Environ(), helperEnv+"="+path)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitForInotifyFD(helper.Process.Pid, time.Second); err != nil {
		_, _ = proc.StopChild(helper.Process.Pid, 0, nil)
		_ = helper.Wait()
		t.Fatal(err)
	}
	before := processCPUTicksForPID(t, helper.Process.Pid)
	beforeSwitches := processContextSwitchesForPID(t, helper.Process.Pid)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_, _ = proc.StopChild(helper.Process.Pid, 0, nil)
		_ = helper.Wait()
		t.Fatal(err)
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := f.WriteString("other\n"); err != nil {
			_ = f.Close()
			_, _ = proc.StopChild(helper.Process.Pid, 0, nil)
			_ = helper.Wait()
			t.Fatal(err)
		}
		timer := time.NewTimer(time.Millisecond)
		<-timer.C
	}
	_ = f.Close()
	after := processCPUTicksForPID(t, helper.Process.Pid)
	afterSwitches := processContextSwitchesForPID(t, helper.Process.Pid)
	if err := helper.Wait(); err != nil {
		t.Fatalf("busy line watcher helper: %v", err)
	}
	if ticks := after - before; ticks > 10 {
		t.Fatalf("watched-file writes used %d CPU ticks in 1.5s, want at most 10", ticks)
	}
	if switches := afterSwitches - beforeSwitches; switches > 500 {
		t.Fatalf("watched-file writes caused %d context switches in 1.5s, want at most 500", switches)
	}
}

func processCPUTicksForPID(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 16 {
		t.Fatalf("process %d stat has %d fields", pid, len(fields))
	}
	user, err := strconv.Atoi(fields[13])
	if err != nil {
		t.Fatal(err)
	}
	system, err := strconv.Atoi(fields[14])
	if err != nil {
		t.Fatal(err)
	}
	return user + system
}

func TestFileConditionsShareInotifyFDAndCloseAfterWatcherStops(t *testing.T) {
	dir := t.TempDir()
	conds := make([]cond, 27)
	for i := range conds {
		conds[i] = &lineCond{path: filepath.Join(dir, fmt.Sprintf("log-%d", i))}
	}
	ctx, cancel := context.WithCancel(context.Background())
	events, cleanup, err := watchFiles(ctx, conds)
	if err != nil {
		t.Fatal(err)
	}
	if events == nil {
		t.Fatal("file watcher unavailable")
	}
	if got := countInotifyFDs(t); got != 1 {
		t.Fatalf("inotify descriptors = %d, want one shared descriptor", got)
	}
	cancel()
	cleanup()
	if got := countInotifyFDs(t); got != 0 {
		t.Fatalf("inotify descriptors after cleanup = %d, want zero", got)
	}
}

func TestFileWatcherIgnoresWritesToUnrelatedFiles(t *testing.T) {
	const helperEnv = "AKIT_WFOR_FILE_WATCH_HELPER"
	if watched := os.Getenv(helperEnv); watched != "" {
		opts, err := parseArgs([]string{"line", "NEVER", watched, "--timeout", "2500ms", "--every", "10s"}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if code := run(context.Background(), opts, io.Discard, io.Discard); code != exitTimeout {
			t.Fatalf("helper run code=%d, want timeout", code)
		}
		return
	}
	dir := t.TempDir()
	watched := filepath.Join(dir, "watched")
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(watched, []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestFileWatcherIgnoresWritesToUnrelatedFiles$")
	helper.Env = append(os.Environ(), helperEnv+"="+watched)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitForInotifyFD(helper.Process.Pid, time.Second); err != nil {
		_, _ = proc.StopChild(helper.Process.Pid, 0, nil)
		_ = helper.Wait()
		t.Fatal(err)
	}
	before := processContextSwitchesForPID(t, helper.Process.Pid)
	f, err := os.OpenFile(other, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_, _ = proc.StopChild(helper.Process.Pid, 0, nil)
		_ = helper.Wait()
		t.Fatal(err)
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := f.Write([]byte("x\n")); err != nil {
			_ = f.Close()
			_, _ = proc.StopChild(helper.Process.Pid, 0, nil)
			_ = helper.Wait()
			t.Fatal(err)
		}
		timer := time.NewTimer(time.Millisecond)
		<-timer.C
	}
	_ = f.Close()
	after := processContextSwitchesForPID(t, helper.Process.Pid)
	if err := helper.Wait(); err != nil {
		t.Fatalf("file watcher helper: %v", err)
	}
	if switches := after - before; switches > 500 {
		t.Fatalf("unrelated writes caused %d wfor context switches in 1.5s, want at most 500", switches)
	}
}

func waitForInotifyFD(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			return fmt.Errorf("read helper descriptors: %w", err)
		}
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", entry.Name()))
			if err == nil && strings.HasPrefix(target, "anon_inode:inotify") {
				return nil
			}
		}
		timer := time.NewTimer(5 * time.Millisecond)
		<-timer.C
	}
	return fmt.Errorf("helper %d did not start its inotify watcher within %s", pid, timeout)
}

func TestDirectoryWatchMaskOnlyTracksNames(t *testing.T) {
	if directoryWatchMask&(unix.IN_MODIFY|unix.IN_CLOSE_WRITE|unix.IN_ATTRIB) != 0 {
		t.Fatalf("directory watch mask includes content/metadata events: %#x", directoryWatchMask)
	}
	want := unix.IN_CREATE | unix.IN_MOVED_TO | unix.IN_DELETE | unix.IN_MOVED_FROM
	if directoryWatchMask&want != want {
		t.Fatalf("directory watch mask misses name lifecycle events: %#x", directoryWatchMask)
	}
}

type retryFDCheck struct {
	t     *testing.T
	calls int
}

func (c *retryFDCheck) check(context.Context) (bool, error) {
	c.calls++
	if c.calls == 1 {
		return false, unix.EMFILE
	}
	if got := countInotifyFDs(c.t); got != 0 {
		return false, fmt.Errorf("inotify descriptors after EMFILE retry = %d", got)
	}
	return true, nil
}

func (*retryFDCheck) String() string { return "retry-fd-check" }
func (*retryFDCheck) state() string  { return "waiting" }

func TestRunRetriesEMFILEWithoutWatchers(t *testing.T) {
	dir := t.TempDir()
	graft := filepath.Join(dir, "graft")
	if err := os.WriteFile(graft, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	base, err := parseArgs([]string{"idle", "--timeout", "1s"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	idle := base.conds[0].(*idleCond)
	idle.graft = filepath.Join(dir, "missing")
	if _, err := idle.check(context.Background()); err == nil || idle.done != nil {
		t.Fatalf("failed graft start must leave idle retryable: err=%v done=%v", err, idle.done)
	}
	idle.graft = graft
	if _, err := idle.check(context.Background()); err != nil {
		t.Fatalf("idle retry after failed start: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		ok, err := idle.check(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle graft retry did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	path := filepath.Join(dir, "log")
	base, err = parseArgs([]string{"line", "NEVER", path, "--timeout", "1s"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	check := &retryFDCheck{t: t}
	idle.done, idle.met = nil, false
	base.conds = append([]cond{idle, check}, base.conds...)
	base.any = true
	if code := run(context.Background(), base, io.Discard, io.Discard); code != 0 {
		t.Fatalf("run after EMFILE retry = %d, want 0", code)
	}
	if check.calls != 2 {
		t.Fatalf("condition checks = %d, want retry once", check.calls)
	}
}

func TestEventChecksOnlyChangedLineCondition(t *testing.T) {
	dir := t.TempDir()
	conds := make([]cond, 300)
	met := make([]bool, len(conds))
	for i := range conds {
		path := filepath.Join(dir, fmt.Sprintf("log-%d", i))
		if err := os.WriteFile(path, []byte("initial\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		line, err := newLineCond("NEVER", path, false, 0)
		if err != nil {
			t.Fatal(err)
		}
		conds[i] = line
	}
	if _, err := checkConditions(context.Background(), conds, met, false, nil, false); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(conds); i++ {
		if err := os.Remove(conds[i].(*lineCond).path); err != nil {
			t.Fatal(err)
		}
	}
	path := conds[0].(*lineCond).path
	if err := appendFile(path, "changed\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := checkConditions(context.Background(), conds, met, true, map[string]struct{}{path: {}}, false); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(conds); i++ {
		if conds[i].(*lineCond).absent {
			t.Fatalf("unrelated line condition %d was checked on the file event", i)
		}
	}
}

func TestWatcherFDsCloseOnlyAfterReaderStops(t *testing.T) {
	done := make(chan struct{})
	closed := make(chan struct{})
	go closeAfterWatcher(done, func() { close(closed) })
	select {
	case <-closed:
		t.Fatal("descriptor closed before watcher stopped")
	case <-time.After(20 * time.Millisecond):
	}
	close(done)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("descriptor was not closed after watcher stopped")
	}
}

func TestFileWatcherIdleDoesNotPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiet")
	line, err := newLineCond("NEVER", path, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events, cleanup, err := watchFiles(ctx, []cond{line})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); cleanup() }()
	if events == nil {
		t.Fatal("file watcher unavailable")
	}
	deadline := time.Now().Add(time.Second)
	for countInotifyFDs(t) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if countInotifyFDs(t) == 0 {
		t.Fatal("inotify watcher did not start")
	}
	select {
	case event := <-events:
		t.Fatalf("quiet file produced an event: %#v", event)
	case <-time.After(250 * time.Millisecond):
	}
}

func processContextSwitchesForPID(t *testing.T, pid int) int {
	t.Helper()
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, task := range tasks {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", task.Name(), "status"))
		if err != nil {
			t.Fatalf("read context switches for task %s: %v", task.Name(), err)
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.HasPrefix(line, "voluntary_ctxt_switches:") || strings.HasPrefix(line, "nonvoluntary_ctxt_switches:") {
				fields := strings.Fields(line)
				if len(fields) == 2 {
					count, parseErr := strconv.Atoi(fields[1])
					if parseErr == nil {
						total += count
					}
				}
			}
		}
	}
	return total
}

func countInotifyFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && strings.HasPrefix(target, "anon_inode:inotify") {
			count++
		}
	}
	return count
}

func TestLineEventDoesNotMatchIncompleteLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	opts, err := parseArgs([]string{"line", "^READY$", path, "--every", "2s", "--timeout", "600ms"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	writes := make(chan error, 2)
	go func() { done <- run(context.Background(), opts, &out, &errOut) }()
	writerDone := make(chan time.Time, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		writes <- appendFile(path, "READY")
		time.Sleep(100 * time.Millisecond)
		writes <- appendFile(path, "-not-ready\n")
		writerDone <- time.Now()
	}()
	started := time.Now()
	var code int
	select {
	case code = <-done:
	case <-time.After(time.Second):
		t.Fatal("wfor did not return at its timeout")
	}
	for range 2 {
		if err := <-writes; err != nil {
			t.Fatal(err)
		}
	}
	if writtenAt := <-writerDone; writtenAt.After(started.Add(600 * time.Millisecond)) {
		t.Fatalf("writes happened after the test wait ended: %s", writtenAt.Sub(started))
	}
	if code != exitTimeout || !strings.Contains(out.String(), "still waiting: line") {
		t.Fatalf("incomplete line counted: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestLineTickMatchesIncompleteLiteralAfterEventScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.log")
	if err := os.WriteFile(path, []byte("prefixREADYtail"), 0o600); err != nil {
		t.Fatal(err)
	}
	line, err := newLineCond("READY", path, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := line.checkEvent(); err != nil || matched {
		t.Fatalf("event check matched incomplete line: matched=%v err=%v", matched, err)
	}
	if matched, err := line.check(context.Background()); err != nil || !matched {
		t.Fatalf("tick missed incomplete literal line: matched=%v err=%v", matched, err)
	}
}

func TestLineLiteralFindsSameInodePartialRewrite(t *testing.T) {
	for _, tc := range []struct {
		name, before, after, expr, want string
	}{
		{"same-length", "WAIT", "DONE", "DONE", "DONE"},
		{"longer", "RUNNING", "DONE_OK!", "DONE", "DONE_OK!"},
		{"later-line", "x\nWAIT", "x\nDONE", "DONE", "DONE"},
		{"literal-prefix-regexp", "WAIT", "DONE", "DON[E]", "DONE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log")
			if err := os.WriteFile(path, []byte(tc.before), 0o600); err != nil {
				t.Fatal(err)
			}
			line, err := newLineCond(tc.expr, path, false, 0)
			if err != nil {
				t.Fatal(err)
			}
			if matched, err := line.check(context.Background()); err != nil || matched {
				t.Fatalf("before rewrite: matched=%v err=%v", matched, err)
			}
			if err := os.WriteFile(path, []byte(tc.after), 0o600); err != nil {
				t.Fatal(err)
			}
			if matched, err := line.check(context.Background()); err != nil || !matched || line.hit != tc.want {
				t.Fatalf("after rewrite: matched=%v hit=%q err=%v", matched, line.hit, err)
			}
		})
	}
}

func TestLineTruncateResetsConsumedOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("long old line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	line, err := newLineCond("DONE", path, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := line.check(context.Background()); err != nil || matched {
		t.Fatalf("initial check: matched=%v err=%v", matched, err)
	}
	if err := os.WriteFile(path, []byte("DONE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if matched, err := line.check(context.Background()); err != nil || !matched {
		t.Fatalf("after truncate: matched=%v err=%v", matched, err)
	}
}

func TestLineReplaceDetectsTruncateBeforeEventOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("old partial line"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	line := &lineCond{path: path, off: 0, eventOff: st.Size(), file: st}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if replaced, err := line.replaced(f, st); err != nil || !replaced {
		t.Fatalf("truncate before event offset: replaced=%v err=%v", replaced, err)
	}
}

func TestLineWatcherResubscribesAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	line, err := newLineCond("DONE", path, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events, cleanup, err := watchFiles(ctx, []cond{line})
	if err != nil || events == nil {
		t.Fatalf("watchFiles: events=%v err=%v", events, err)
	}
	defer func() { cancel(); cleanup() }()
	tmp := filepath.Join(dir, "new")
	if err := os.WriteFile(tmp, []byte("pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("rename did not wake watcher")
	}
	if err := appendFile(path, "DONE\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("watcher did not resubscribe to renamed file")
	}
}

func TestInotifyOverflowRebuildsSubscriptions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	dirs := map[string]map[string]uint32{dir: {"log": unix.IN_MODIFY | unix.IN_ATTRIB}}
	byWD, fileMasks, filePaths, paths, dirWDs := addInotifyWatches(fd, dirs)
	var event [unix.SizeofInotifyEvent]byte
	binary.LittleEndian.PutUint32(event[0:4], ^uint32(0))
	binary.LittleEndian.PutUint32(event[4:8], unix.IN_Q_OVERFLOW)
	out := make(chan fileEvent, 1)
	_, _, overflow := emitInotifyEventsForFiles(fd, event[:], byWD, fileMasks, filePaths, paths, dirWDs, out)
	if !overflow {
		t.Fatal("IN_Q_OVERFLOW was not reported")
	}
	select {
	case got := <-out:
		if _, ok := got.paths[path]; !ok {
			t.Fatalf("overflow check paths = %v, missing %s", got.paths, path)
		}
	default:
		t.Fatal("overflow did not request condition check")
	}
	byWD, fileMasks, filePaths, paths, dirWDs = rebuildInotifyWatches(fd, paths)
	if err := appendFile(path, "changed"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := unix.Read(fd, buf)
	if err != nil {
		t.Fatal(err)
	}
	if relevant, _, _ := emitInotifyEventsForFiles(fd, buf[:n], byWD, fileMasks, filePaths, paths, dirWDs, out); !relevant {
		t.Fatal("rebuilt subscription missed write")
	}
}

func TestExitedPIDWatcherDoesNotBurnCPU(t *testing.T) {
	child := exec.Command("bash", "-c", "sleep 0.12")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Wait()
	opts, err := parseArgs([]string{"exit", strconv.Itoa(child.Process.Pid), "file", filepath.Join(t.TempDir(), "missing"), "--timeout", "800ms"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	before := processCPU(t)
	code := run(context.Background(), opts, io.Discard, io.Discard)
	after := processCPU(t)
	if code != exitTimeout {
		t.Fatalf("run code=%d, want timeout", code)
	}
	if used := after - before; used >= 100*time.Millisecond {
		t.Fatalf("process CPU during wait = %s, want < 100ms", used)
	}
}

func TestExitWatcherCancellationWithManyPIDs(t *testing.T) {
	args := []string{"exit"}
	children := make([]*exec.Cmd, 0, 6)
	for range 6 {
		child := exec.Command("bash", "-c", "sleep 0.5")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
		args = append(args, strconv.Itoa(child.Process.Pid))
	}
	defer func() {
		for _, child := range children {
			_ = child.Wait()
		}
	}()
	args = append(args, "file", filepath.Join(t.TempDir(), "missing"), "--timeout", "100ms")
	opts, err := parseArgs(args, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	code := run(context.Background(), opts, io.Discard, io.Discard)
	if code != exitTimeout {
		t.Fatalf("run code=%d, want timeout", code)
	}
}

func TestExitWatcherRLimit(t *testing.T) {
	const helperEnv = "AKIT_WFOR_RLIMIT_HELPER"
	if os.Getenv(helperEnv) == "1" {
		var limit unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
			t.Fatal(err)
		}
		if limit.Cur > 32 {
			limit.Cur = 32
			if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
				t.Fatal(err)
			}
		}
		child := exec.Command("sleep", "2")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		defer child.Wait()
		args := []string{"exit"}
		for range 40 {
			args = append(args, strconv.Itoa(child.Process.Pid))
		}
		args = append(args, "--timeout", "150ms", "--every", "1s")
		opts, err := parseArgs(args, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if code := run(context.Background(), opts, io.Discard, io.Discard); code != exitTimeout {
			t.Fatalf("run code=%d, want timeout with pidfd as an optional watcher", code)
		}

		runtime.LockOSThread()
		tid := unix.Gettid()
		runtime.UnlockOSThread()
		quick := exec.Command("bash", "-c", "sleep 0.15")
		if err := quick.Start(); err != nil {
			t.Fatal(err)
		}
		defer quick.Wait()
		args = []string{"exit", strconv.Itoa(tid), "exit", strconv.Itoa(quick.Process.Pid), "--any", "--timeout", "2s", "--every", "10s"}
		opts, err = parseArgs(args, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if code := run(context.Background(), opts, io.Discard, io.Discard); code != 0 {
			t.Fatalf("run code=%d after one pidfd open failure, want remaining pidfd to wake run", code)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestExitWatcherRLimit$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("RLIMIT helper: %v\n%s", err, output)
	}
}

func processCPU(t *testing.T) time.Duration {
	t.Helper()
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return time.Duration(usage.Utime.Sec+usage.Stime.Sec)*time.Second + time.Duration(usage.Utime.Usec+usage.Stime.Usec)*time.Microsecond
}

func TestKernelWakeExit(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(strconv.Itoa(count)+" process(es)", func(t *testing.T) {
			args := []string{"exit"}
			var children []*exec.Cmd
			for range count {
				cmd := exec.Command("bash", "-c", "sleep 0.2")
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				children = append(children, cmd)
				args = append(args, strconv.Itoa(cmd.Process.Pid))
			}
			defer func() {
				for _, cmd := range children {
					_ = cmd.Wait()
				}
			}()
			args = append(args, "--every", "10s", "--timeout", "3s")
			assertWake(t, args, func() error { return nil })
		})
	}
}

func assertWake(t *testing.T, args []string, change func() error) {
	t.Helper()
	opts, err := parseArgs(args, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(context.Background(), opts, &out, &errOut) }()
	started := time.Now()
	if err := change(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("wake after %s, code=%d stdout=%q stderr=%q", time.Since(started), code, out.String(), errOut.String())
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("condition did not wake promptly")
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
	cmd := exec.Command("sleep", "1")
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
