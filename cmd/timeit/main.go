package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultRuns   = 5
	defaultBudget = 45 * time.Second
	defaultLimit  = 60 * time.Second
	exitError     = 2
)

const usage = `timeit — measure a command's median runtime within a time budget.

Usage:
  timeit [--runs N] [--budget D] [--limit D] [--ok-exit CODES]
         [--fail-on-stderr TEXT] [--stdin FILE] -- CMD [ARG...]

Flags:
  --runs N              maximum runs (default 5; at least 1)
  --budget D            time from the first run to start more runs (default 45s)
  --limit D             total allowance, adjusted after run one (default 60s)
  --ok-exit CODES       comma-separated successful exit codes (default 0)
  --fail-on-stderr TEXT fail if a run writes TEXT to stderr
  --stdin FILE          reopen FILE for every run (default: empty stdin)
  -h, --help            show this help

Example:
  timeit --runs 5 --budget 45s --limit 60s -- ./bin/reference --case small

The first run always starts. Later runs start only while elapsed time is below
the effective budget. A failed run prints at most the last 64 KiB of stderr,
with a truncation note when earlier output is omitted. The median runtime is
printed in milliseconds.

Exit: 0 median written, 1 stderr match with exit 0, command exit code on a failed run,
2 usage or launch error, 128+signal when the command is killed by a signal
(reported as signal NAME), or when timeit is interrupted (SIGINT, SIGTERM, SIGHUP).
`

type options struct {
	runs         int
	budget       time.Duration
	limit        time.Duration
	okExit       map[int]bool
	failOnStderr string
	stdinPath    string
	command      []string
}

var errHelp = errors.New("help")

func main() {
	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "timeit: %v\n", err)
		os.Exit(exitError)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go watchSignals(signals)
	code := run(opts, os.Stdout, os.Stderr, time.Now, func(opts options) (result, time.Duration, error) {
		return runCommand(opts, nil)
	})
	signal.Stop(signals)
	os.Exit(code)
}

var activeGroup atomic.Int64

func watchSignals(signals <-chan os.Signal) {
	for sig := range signals {
		s, ok := sig.(syscall.Signal)
		if !ok {
			continue
		}
		pid := activeGroup.Load()
		for pid == -1 {
			time.Sleep(time.Millisecond)
			pid = activeGroup.Load()
		}
		if pid > 0 {
			if err := syscall.Kill(-int(pid), syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				fmt.Fprintf(os.Stderr, "timeit: kill command group: %v\n", err)
			}
		}
		os.Exit(128 + int(s))
	}
}

func parseArgs(args []string) (options, error) {
	opts := options{runs: defaultRuns, budget: defaultBudget, limit: defaultLimit, okExit: map[int]bool{0: true}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 == len(args) {
				return options{}, errors.New("missing command after --")
			}
			opts.command = append([]string(nil), args[i+1:]...)
			if opts.command[0] == "" {
				return options{}, errors.New("missing command")
			}
			return opts, nil
		}
		if a == "-h" || a == "--help" {
			return options{}, errHelp
		}
		if !strings.HasPrefix(a, "--") {
			return options{}, fmt.Errorf("expected -- before command, got %q", a)
		}
		if i+1 == len(args) {
			return options{}, fmt.Errorf("%s needs a value", a)
		}
		i++
		v := args[i]
		switch a {
		case "--runs":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return options{}, fmt.Errorf("--runs %q: want an integer of at least 1", v)
			}
			opts.runs = n
		case "--budget", "--limit":
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return options{}, fmt.Errorf("%s %q: want a non-negative duration like 45s", a, v)
			}
			if a == "--budget" {
				opts.budget = d
			} else {
				opts.limit = d
			}
		case "--ok-exit":
			codes, err := parseExitCodes(v)
			if err != nil {
				return options{}, err
			}
			opts.okExit = codes
		case "--fail-on-stderr":
			opts.failOnStderr = v
		case "--stdin":
			opts.stdinPath = v
		default:
			return options{}, fmt.Errorf("unknown flag %q", a)
		}
	}
	return options{}, errors.New("missing -- and command")
}

func parseExitCodes(s string) (map[int]bool, error) {
	codes := make(map[int]bool)
	for part := range strings.SplitSeq(s, ",") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 255 {
			return nil, fmt.Errorf("--ok-exit %q: want comma-separated exit codes from 0 to 255", s)
		}
		codes[n] = true
	}
	if len(codes) == 0 {
		return nil, fmt.Errorf("--ok-exit %q: list must not be empty", s)
	}
	return codes, nil
}

const maxStderrBytes = 64 * 1024

type result struct {
	code        int
	stderrFile  *os.File
	stderrData  []byte
	interrupted int
	signalName  string
}
type commandRunner func(options) (result, time.Duration, error)

func run(opts options, stdout, stderr io.Writer, now func() time.Time, startCommand commandRunner) int {
	if err := validateStdin(opts.stdinPath); err != nil {
		fmt.Fprintf(stderr, "timeit: %v\n", err)
		return exitError
	}
	var durations []time.Duration
	var firstStart time.Time
	budget := opts.budget
	for runNumber := 1; runNumber <= opts.runs; runNumber++ {
		if runNumber > 1 && now().Sub(firstStart) >= budget {
			break
		}
		if runNumber == 1 {
			firstStart = now()
		}
		res, duration, err := startCommand(opts)
		if err != nil {
			fmt.Fprintf(stderr, "timeit: %v\n", err)
			return exitError
		}
		if res.interrupted != 0 {
			cleanupResult(res)
			return 128 + res.interrupted
		}
		durations = append(durations, duration)
		if runNumber == 1 {
			remaining := max(opts.limit-duration, 0)
			if remaining < budget {
				budget = remaining
			}
		}
		failedText := ""
		if opts.failOnStderr != "" {
			found, err := false, error(nil)
			if res.stderrFile == nil {
				found = bytes.Contains(res.stderrData, []byte(opts.failOnStderr))
			} else {
				found, err = stderrContains(res.stderrFile, []byte(opts.failOnStderr))
			}
			if err != nil {
				cleanupResult(res)
				fmt.Fprintf(stderr, "timeit: read captured stderr: %v\n", err)
				return exitError
			}
			if found {
				failedText = opts.failOnStderr
			}
		}
		if !opts.okExit[res.code] || failedText != "" {
			return failedRun(stderr, res, runNumber, opts.runs, failedText)
		}
		cleanupResult(res)
	}
	fmt.Fprintln(stdout, medianMillis(durations))
	return 0
}

func validateStdin(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stdin %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("stdin %q: want a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("stdin %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("stdin %q: %w", path, err)
	}
	return nil
}

func failedRun(stderr io.Writer, res result, runNumber, runs int, text string) int {
	defer cleanupResult(res)
	data, truncated, err := stderrTail(res)
	if err != nil {
		fmt.Fprintf(stderr, "timeit: read captured stderr: %v\n", err)
		return exitError
	}
	if truncated {
		fmt.Fprintln(stderr, "timeit: stderr beginning truncated; showing last 65536 bytes")
	}
	if _, err := stderr.Write(data); err != nil {
		return exitError
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Fprintln(stderr)
	}
	if text != "" {
		fmt.Fprintf(stderr, "timeit: run %d of %d: stderr has %q\n", runNumber, runs, text)
		if res.code == 0 {
			return 1
		}
		return res.code
	}
	if res.signalName != "" {
		fmt.Fprintf(stderr, "timeit: run %d of %d: signal %s\n", runNumber, runs, res.signalName)
		return res.code
	}
	fmt.Fprintf(stderr, "timeit: run %d of %d: exit %d\n", runNumber, runs, res.code)
	return res.code
}

func stderrTail(res result) ([]byte, bool, error) {
	if res.stderrFile == nil {
		if len(res.stderrData) > maxStderrBytes {
			return res.stderrData[len(res.stderrData)-maxStderrBytes:], true, nil
		}
		return res.stderrData, false, nil
	}
	f := res.stderrFile
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, false, fmt.Errorf("seek captured stderr: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("captured stderr: %w", err)
	}
	truncated := info.Size() > maxStderrBytes
	if truncated {
		if _, err := f.Seek(info.Size()-maxStderrBytes, io.SeekStart); err != nil {
			return nil, false, fmt.Errorf("captured stderr: %w", err)
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false, fmt.Errorf("read captured stderr: %w", err)
	}
	return data, truncated, nil
}

func stderrContains(f *os.File, needle []byte) (bool, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("seek captured stderr: %w", err)
	}
	const chunkSize = 32 * 1024
	buf := make([]byte, chunkSize)
	var overlap []byte
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			chunk := append(append([]byte(nil), overlap...), buf[:n]...)
			if bytes.Contains(chunk, needle) {
				return true, nil
			}
			keep := min(len(needle)-1, len(chunk))
			if keep > 0 {
				overlap = append(overlap[:0], chunk[len(chunk)-keep:]...)
			}
		}
		if readErr == io.EOF {
			return false, nil
		}
		if readErr != nil {
			return false, fmt.Errorf("read captured stderr: %w", readErr)
		}
	}
}

func cleanupResult(res result) {
	if res.stderrFile != nil {
		if err := res.stderrFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "timeit: close stderr capture: %v\n", err)
		}
	}
}

func medianMillis(durations []time.Duration) int64 {
	ordered := append([]time.Duration(nil), durations...)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j] < ordered[j-1]; j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	middle := len(ordered) / 2
	var median time.Duration
	if len(ordered)%2 == 1 {
		median = ordered[middle]
	} else {
		median = (ordered[middle-1] + ordered[middle]) / 2
	}
	return int64((median + 500*time.Microsecond) / time.Millisecond)
}

func runCommand(opts options, signals <-chan os.Signal) (result, time.Duration, error) {
	stderrFile, err := os.CreateTemp("", "timeit-stderr-*")
	if err != nil {
		return result{}, 0, fmt.Errorf("create stderr capture: %w", err)
	}
	if err := os.Remove(stderrFile.Name()); err != nil {
		_ = stderrFile.Close()
		return result{}, 0, fmt.Errorf("unlink stderr capture: %w", err)
	}
	cleanup := func() {
		if err := stderrFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "timeit: close stderr capture: %v\n", err)
		}
	}
	stdoutFile, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		cleanup()
		return result{}, 0, fmt.Errorf("open null output: %w", err)
	}
	cmd := exec.Command(opts.command[0], opts.command[1:]...)
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if opts.stdinPath == "" {
		cmd.Stdin = strings.NewReader("")
	} else {
		f, err := os.Open(opts.stdinPath)
		if err != nil {
			cleanup()
			return result{}, 0, fmt.Errorf("stdin %q: %w", opts.stdinPath, err)
		}
		defer f.Close()
		cmd.Stdin = f
	}
	started := time.Now()
	activeGroup.Store(-1)
	if err := cmd.Start(); err != nil {
		activeGroup.Store(0)
		cleanup()
		closeErr := stdoutFile.Close()
		return result{}, 0, errors.Join(fmt.Errorf("%s: %w", opts.command[0], err), closeErr)
	}
	activeGroup.Store(int64(cmd.Process.Pid))
	if err := stdoutFile.Close(); err != nil {
		stopCommand(cmd)
		cleanup()
		return result{}, 0, fmt.Errorf("close null output: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case sig := <-signals:
		if signalNumber, ok := sig.(syscall.Signal); ok {
			if killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
				fmt.Fprintf(os.Stderr, "timeit: kill command group: %v\n", killErr)
			}
			<-wait
			activeGroup.CompareAndSwap(int64(cmd.Process.Pid), 0)
			cleanup()
			return result{interrupted: int(signalNumber)}, time.Since(started), nil
		}
		if killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			fmt.Fprintf(os.Stderr, "timeit: kill command group: %v\n", killErr)
		}
		<-wait
		activeGroup.CompareAndSwap(int64(cmd.Process.Pid), 0)
		cleanup()
		return result{interrupted: int(syscall.SIGTERM)}, time.Since(started), nil
	case err := <-wait:
		activeGroup.CompareAndSwap(int64(cmd.Process.Pid), 0)
		duration := time.Since(started)
		res := result{stderrFile: stderrFile}
		if err == nil {
			return res, duration, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.code = exitErr.ExitCode()
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				res.code = 128 + int(status.Signal())
				res.signalName = signalLabel(status.Signal())
			}
			return res, duration, nil
		}
		cleanup()
		return result{}, 0, fmt.Errorf("%s: %w", opts.command[0], err)
	}
}

func stopCommand(cmd *exec.Cmd) {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		fmt.Fprintf(os.Stderr, "timeit: kill command group: %v\n", err)
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			fmt.Fprintf(os.Stderr, "timeit: wait for command: %v\n", err)
		}
	}
}

func signalLabel(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGABRT:
		return "SIGABRT"
	default:
		return fmt.Sprintf("SIG%d", sig)
	}
}
