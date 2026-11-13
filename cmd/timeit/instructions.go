package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// perfArgs wraps the command in perf stat counting user-space instructions,
// written to file descriptor 3 (the first of exec.Cmd.ExtraFiles).
// --no-scale keeps raw counts: on a hybrid CPU each core type has its own
// counter, and the default scaling extrapolates a counter that barely ran
// into an invented number. The real total is the plain sum over core types.
func perfArgs(command []string) []string {
	return append([]string{"perf", "stat", "--no-scale", "-x,", "-e", "instructions:u", "--log-fd", "3", "--"}, command...)
}

// instructionsEnv sets GOMAXPROCS=1 unless the caller chose a value: idle
// threads of a Go scheduler spin and add instructions that vary run to run.
func instructionsEnv(env []string) []string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "GOMAXPROCS=") {
			return env
		}
	}
	return append(slices.Clone(env), "GOMAXPROCS=1")
}

// commandArgv is the argv to start; with --instructions it runs under perf
// stat, whose counts go to the returned file. The file is unlinked at once,
// so an interrupted run leaves nothing behind.
func commandArgv(opts options) ([]string, *os.File, error) {
	if !opts.instructions {
		return opts.command, nil, nil
	}
	out, err := os.CreateTemp("", "timeit-perf-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create perf output: %w", err)
	}
	if err := os.Remove(out.Name()); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("unlink perf output: %w", err), out.Close())
	}
	return perfArgs(opts.command), out, nil
}

// parsePerfInstructions sums the instructions:u lines of perf stat -x, output.
// A core type the command never ran on reports "<not counted>" and adds nothing.
func parsePerfInstructions(data []byte) (int64, error) {
	var total int64
	counted := false
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 3 || !strings.Contains(fields[2], "instructions") {
			continue
		}
		value := strings.TrimSpace(fields[0])
		if value == "<not counted>" {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("perf stat: instructions value %q: %s", value, perfHint)
		}
		total += n
		counted = true
	}
	if !counted {
		return 0, fmt.Errorf("perf stat counted no instructions: %s", perfHint)
	}
	return total, nil
}

const perfHint = "needs perf (dnf install perf) and kernel.perf_event_paranoid <= 2"

func readPerfInstructions(f *os.File) (int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("perf output: %w", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return 0, fmt.Errorf("perf output: %w", err)
	}
	return parsePerfInstructions(data)
}

func medianCount(counts []int64) int64 {
	ordered := slices.Clone(counts)
	slices.Sort(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return ordered[middle-1] + (ordered[middle]-ordered[middle-1])/2
}
