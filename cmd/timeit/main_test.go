package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
)

func TestMain(m *testing.M) {
	if err := proc.BecomeChildSubreaper(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestParseArgs(t *testing.T) {
	opts, err := parseArgs([]string{"--runs", "3", "--budget", "12s", "--limit", "20s", "--ok-exit", "0,3", "--fail-on-stderr", "bad", "--stdin", "in", "--", "echo", "-n"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.runs != 3 || opts.budget != 12*time.Second || opts.limit != 20*time.Second || !opts.okExit[3] || opts.command[0] != "echo" || opts.stdinPath != "in" {
		t.Fatalf("%+v", opts)
	}
	defaults, err := parseArgs([]string{"--", "true"})
	if err != nil || defaults.runs != 5 || defaults.budget != 45*time.Second || defaults.limit != 60*time.Second || !defaults.okExit[0] {
		t.Fatalf("defaults %+v, %v", defaults, err)
	}
}

func TestParseArgsErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"true"}, {"--"}, {"--runs", "0", "--", "true"}, {"--runs", "x", "--", "true"}, {"--budget", "wat", "--", "true"}, {"--limit", "-1s", "--", "true"}, {"--ok-exit", "0,x", "--", "true"}, {"--ok-exit", "", "--", "true"}, {"--wat", "x", "--", "true"}, {"--runs"}, {"--", ""}} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	if _, err := parseArgs([]string{"--help"}); err != errHelp {
		t.Errorf("help: %v", err)
	}
}

func TestMedianMillis(t *testing.T) {
	for _, test := range []struct {
		name  string
		input []time.Duration
		want  int64
	}{
		{"one", []time.Duration{1500 * time.Microsecond}, 2},
		{"odd", []time.Duration{9 * time.Millisecond, time.Millisecond, 5 * time.Millisecond}, 5},
		{"even", []time.Duration{time.Millisecond, 4 * time.Millisecond}, 3},
		{"round half up", []time.Duration{time.Millisecond, 2 * time.Millisecond}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := medianMillis(test.input); got != test.want {
				t.Errorf("got %d, want %d", got, test.want)
			}
		})
	}
}

func TestBudgetAndLimit(t *testing.T) {
	for _, test := range []struct {
		name                 string
		budget, limit, first time.Duration
		wantRuns             int
	}{
		{"budget", 10 * time.Second, time.Minute, time.Second, 5},
		{"budget boundary", time.Second, time.Minute, time.Second, 1},
		{"limit remainder", time.Minute, 4 * time.Second, time.Second, 3},
		{"limit exhausted", time.Minute, time.Second, time.Second, 1},
		{"slow first", time.Minute, 2 * time.Second, 3 * time.Second, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := time.Unix(0, 0)
			opts := options{runs: 5, budget: test.budget, limit: test.limit, okExit: map[int]bool{0: true}}
			count := 0
			var out, stderr bytes.Buffer
			code := run(opts, &out, &stderr, func() time.Time { return clock }, func(options) (result, time.Duration, error) {
				count++
				d := test.first
				if count > 1 {
					d = time.Second
				}
				clock = clock.Add(d)
				return result{}, d, nil
			})
			if code != 0 || count != test.wantRuns || stderr.Len() != 0 {
				t.Errorf("code %d, runs %d, stderr %q", code, count, stderr.String())
			}
		})
	}
}

func TestFailureStopsAtRun(t *testing.T) {
	opts := options{runs: 4, budget: time.Minute, limit: time.Minute, okExit: map[int]bool{0: true}}
	count := 0
	var out, stderr bytes.Buffer
	code := run(opts, &out, &stderr, time.Now, func(options) (result, time.Duration, error) {
		count++
		if count == 3 {
			return result{code: 7, stderrData: []byte("diagnostic")}, 0, nil
		}
		return result{}, 0, nil
	})
	if code != 7 || count != 3 || out.Len() != 0 || stderr.String() != "diagnostic\ntimeit: run 3 of 4: exit 7\n" {
		t.Fatalf("code %d runs %d stdout %q stderr %q", code, count, out.String(), stderr.String())
	}
}

func TestOKExitAndFailOnStderr(t *testing.T) {
	opts := options{runs: 1, okExit: map[int]bool{0: true, 3: true}, failOnStderr: "warning"}
	var out, stderr bytes.Buffer
	code := run(opts, &out, &stderr, time.Now, func(options) (result, time.Duration, error) { return result{code: 3}, 0, nil })
	if code != 0 {
		t.Fatalf("allowed exit code: %d %q", code, stderr.String())
	}
	out.Reset()
	stderr.Reset()
	code = run(opts, &out, &stderr, time.Now, func(options) (result, time.Duration, error) {
		return result{stderrData: []byte("a warning here")}, 0, nil
	})
	if code != 1 || out.Len() != 0 || !strings.Contains(stderr.String(), "timeit: run 1 of 1: stderr has \"warning\"") {
		t.Fatalf("code %d stdout %q stderr %q", code, out.String(), stderr.String())
	}
}

func TestStdinReopenedForEachRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := options{command: []string{"sh", "-c", "test \"$(cat)\" = payload"}, stdinPath: path, runs: 2, budget: time.Second, limit: time.Second, okExit: map[int]bool{0: true}}
	var out, stderr bytes.Buffer
	code := run(opts, &out, &stderr, time.Now, func(o options) (result, time.Duration, error) { return runCommand(o, nil) })
	if _, err := strconv.Atoi(strings.TrimSpace(out.String())); code != 0 || err != nil || stderr.Len() != 0 {
		t.Fatalf("code %d stdout %q stderr %q", code, out.String(), stderr.String())
	}
}

func TestStdinDirectoryRejectedBeforeRun(t *testing.T) {
	dir := t.TempDir()
	opts := options{command: []string{"true"}, stdinPath: dir, runs: 1, okExit: map[int]bool{0: true}}
	called := false
	var out, stderr bytes.Buffer
	code := run(opts, &out, &stderr, time.Now, func(options) (result, time.Duration, error) {
		called = true
		return result{}, 0, nil
	})
	if code != 2 || called || out.Len() != 0 || !strings.Contains(stderr.String(), "regular file") {
		t.Fatalf("code %d called %t stdout %q stderr %q", code, called, out.String(), stderr.String())
	}
}

func TestStderrCaptureUnlinkedWhileCommandRuns(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	opts := options{command: []string{"sh", "-c", "sleep 0.3"}, runs: 1, okExit: map[int]bool{0: true}}
	done := make(chan error, 1)
	go func() {
		res, _, err := runCommand(opts, nil)
		cleanupResult(res)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
				t.Fatalf("temporary stderr entry remained: %v, %v", entries, err)
			}
			return
		default:
			if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
				t.Fatalf("temporary stderr entry remained while running: %v, %v", entries, err)
			}
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatal("command did not finish")
}

func TestBackgroundChildDoesNotHoldOutputPipes(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "background.pid")
	opts := options{command: []string{"sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $! > \"$1\"", "sh", pidPath}, runs: 1, okExit: map[int]bool{0: true}}
	started := time.Now()
	res, duration, err := runCommand(opts, nil)
	elapsed := time.Since(started)
	if err != nil || duration > time.Second || elapsed > time.Second {
		t.Fatalf("background child delayed completion: duration %v elapsed %v err %v", duration, elapsed, err)
	}
	defer cleanupResult(res)
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if stat, err := proc.ReadStat("/proc", pid); err != nil || stat.State == "Z" || stat.State == "X" {
		t.Fatalf("ordinary completion stopped background child: stat=%+v err=%v", stat, err)
	}
	if _, err := proc.StopChildGroup(res.processGroup, 0, syscall.SIGKILL); err != nil {
		t.Fatalf("clean test command group: %v", err)
	}
}

func TestChildSignalExit(t *testing.T) {
	opts := options{command: []string{"sh", "-c", "kill -9 $$"}, runs: 1, okExit: map[int]bool{0: true}}
	var out, stderr bytes.Buffer
	code := run(opts, &out, &stderr, time.Now, func(o options) (result, time.Duration, error) { return runCommand(o, nil) })
	if code != 128+int(syscall.SIGKILL) || out.Len() != 0 || !strings.Contains(stderr.String(), "signal SIGKILL") {
		t.Fatalf("code %d stdout %q stderr %q", code, out.String(), stderr.String())
	}
}

func TestSignalDuringCommandPublication(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "member.pid")
	started := make(chan *exec.Cmd, 1)
	release := make(chan struct{})
	var testCmd *exec.Cmd
	oldAfterStart := afterCommandStartBeforePublish
	afterCommandStartBeforePublish = func(cmd *exec.Cmd) {
		started <- cmd
		<-release
	}
	signalEntered := make(chan struct{})
	oldSignalLookup := beforeCommandSignalLookup
	beforeCommandSignalLookup = func() { close(signalEntered) }
	t.Cleanup(func() {
		afterCommandStartBeforePublish = oldAfterStart
		beforeCommandSignalLookup = oldSignalLookup
		select {
		case <-release:
		default:
			close(release)
		}
		if testCmd == nil {
			select {
			case testCmd = <-started:
			default:
			}
		}
		if testCmd != nil {
			if _, err := proc.StopChildGroup(testCmd, 0, syscall.SIGKILL); err != nil {
				t.Errorf("cleanup test command group: %v", err)
			}
		}
	})

	opts := options{command: []string{"sh", "-c", `sleep 30 </dev/null >/dev/null 2>&1 & echo $! > "$1"; exec sleep 30`, "sh", pidFile}, runs: 1, okExit: map[int]bool{0: true}}
	type commandResult struct {
		result  result
		elapsed time.Duration
		err     error
	}
	done := make(chan commandResult, 1)
	go func() {
		res, elapsed, err := runCommand(opts, nil)
		done <- commandResult{result: res, elapsed: elapsed, err: err}
	}()
	cmd := <-started
	testCmd = cmd
	waitTimeitFile(t, pidFile)

	handled := make(chan int, 1)
	go func() { handled <- signalExitCode(syscall.SIGTERM) }()
	select {
	case <-signalEntered:
	case <-time.After(time.Second):
		t.Fatal("signal handler did not enter command lookup")
	}
	early := false
	select {
	case code := <-handled:
		early = true
		t.Errorf("signal handler returned %d before command publication", code)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	code := <-handled
	if early {
		if _, err := proc.StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
			t.Errorf("clean command group after failed window check: %v", err)
		}
	}
	got := <-done
	if code != 128+int(syscall.SIGTERM) {
		t.Errorf("signal handler code = %d, want 143", code)
	}
	if got.err != nil {
		t.Fatalf("runCommand: %v", got.err)
	}
	cleanupResult(got.result)
	if got.result.code != 128+int(syscall.SIGKILL) {
		t.Errorf("command exit code = %d, want 137 after group stop", got.result.code)
	}
	if cmd.ProcessState == nil {
		t.Error("test command was not reaped by runCommand")
	}
	member := readTimeitPID(t, pidFile)
	if !timeitProcessGone(member, time.Second) {
		t.Errorf("command group member %d survived signal", member)
		if _, err := proc.StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
			t.Errorf("clean surviving test group: %v", err)
		}
	}
}

func readTimeitPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid PID in %s: %q (%v)", path, b, err)
	}
	return pid
}

func waitTimeitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("file %s was not created", path)
}

func timeitProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		stat, err := proc.ReadStat("/proc", pid)
		if err != nil || stat.State == "Z" || stat.State == "X" {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestForwardSignalToCommandGroup(t *testing.T) {
	if os.Getenv("TIMEIT_SIGNAL_HELPER") == "1" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		if err := os.WriteFile(os.Getenv("TIMEIT_SIGNAL_READY"), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		opts := options{command: []string{"sh", "-c", "sleep 60"}, runs: 1, okExit: map[int]bool{0: true}}
		code := run(opts, io.Discard, io.Discard, time.Now, func(o options) (result, time.Duration, error) { return runCommand(o, signals) })
		signal.Stop(signals)
		os.Exit(code)
	}
	for _, forwarded := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(forwarded.String(), func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			cmd := exec.Command(os.Args[0], "-test.run=^TestForwardSignalToCommandGroup$")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Env = append(os.Environ(), "TIMEIT_SIGNAL_HELPER=1", "TIMEIT_SIGNAL_READY="+ready)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := proc.StopChildGroup(cmd, 0, syscall.SIGKILL); err != nil {
					t.Errorf("stop signal helper group: %v", err)
				}
			})
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("signal helper did not become ready")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if _, err := proc.StopChild(cmd.Process.Pid, time.Second, func() error { return proc.SignalChild(cmd.Process.Pid, forwarded) }); err != nil {
				t.Fatal(err)
			}
			err := cmd.Wait()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128+int(forwarded) {
				t.Fatalf("helper exit: %v", err)
			}
		})
	}
}

type blockingWriter struct{ entered string }

func (w blockingWriter) Write(p []byte) (int, error) {
	if err := os.WriteFile(w.entered, []byte("blocked"), 0o600); err != nil {
		return 0, fmt.Errorf("write blocking marker: %w", err)
	}
	select {}
}

func TestSignalDuringStderrWrite(t *testing.T) {
	if os.Getenv("TIMEIT_BLOCKING_HELPER") == "1" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		go watchSignals(signals)
		marker := os.Getenv("TIMEIT_BLOCKING_MARKER")
		if err := os.WriteFile(os.Getenv("TIMEIT_SIGNAL_READY"), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		opts := options{command: []string{"sh", "-c", "echo diagnostic >&2; exit 3"}, runs: 1, okExit: map[int]bool{0: true}}
		_ = run(opts, io.Discard, blockingWriter{entered: marker}, time.Now, func(o options) (result, time.Duration, error) { return runCommand(o, nil) })
		t.Fatal("run unexpectedly returned")
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	marker := filepath.Join(dir, "blocked")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalDuringStderrWrite$")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "TIMEIT_BLOCKING_HELPER=1", "TIMEIT_SIGNAL_READY="+ready, "TIMEIT_BLOCKING_MARKER="+marker)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_, _ = proc.StopChildGroup(cmd, 0, syscall.SIGKILL)
			t.Fatal("helper did not block writing stderr")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := proc.StopChild(cmd.Process.Pid, time.Second, func() error { return proc.SignalChild(cmd.Process.Pid, syscall.SIGTERM) }); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128+int(syscall.SIGTERM) {
		t.Fatalf("helper exit: %v", err)
	}
}

func TestCommandNotFound(t *testing.T) {
	opts := options{command: []string{"timeit-command-that-does-not-exist"}, runs: 1, okExit: map[int]bool{0: true}}
	var out, stderr bytes.Buffer
	code := run(opts, &out, &stderr, time.Now, func(o options) (result, time.Duration, error) { return runCommand(o, nil) })
	if code != 2 || out.Len() != 0 || !strings.Contains(stderr.String(), "timeit-command-that-does-not-exist") {
		t.Fatalf("code %d stdout %q stderr %q", code, out.String(), stderr.String())
	}
}

// firstWriteFails fails its first write and records the later ones.
type firstWriteFails struct {
	failed bool
	bytes.Buffer
}

func (w *firstWriteFails) Write(p []byte) (int, error) {
	if !w.failed {
		w.failed = true
		return 0, errors.New("disk full")
	}
	return w.Buffer.Write(p)
}

func TestFailedRunReportsStderrWriteError(t *testing.T) {
	stderr := &firstWriteFails{}
	code := failedRun(stderr, result{code: 3, stderrData: []byte("diagnostic\n")}, 1, 1, "")
	if code != exitError {
		t.Errorf("code %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr.String(), "disk full") {
		t.Errorf("write error not reported: %q", stderr.String())
	}
}
