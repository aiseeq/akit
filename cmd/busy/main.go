// busy — занята ли машина по общему правилу akit/pressure, одной строкой: то же число, по которому
// graft пускает тяжёлые задачи и следит aguard, вместо `cat /proc/loadavg`: минутный load запаздывает.
//
// Коды выхода: 0 — свободна, 1 — занята, 2 — ошибка вызова или замера.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/pressure"
)

const usage = `busy — is the machine busy by the shared rule (akit/pressure), in one line.

Usage:
  busy [--runnable N] [--mem-some P] [--window D] [--proc DIR]

Busy when the tasks ready to run now (procs_running, one reading, without busy itself) are at or
above the machine's threads, the IO pressure (full avg60) is above 10%, or the memory pressure
(some avg10) is above 10% while swapping over the window is above 1024 KB/s. Swapping alone is
not busy. The same rule graft queues heavy tasks by and aguard watches.

Flags:
  --runnable N    busy at N runnable tasks (default: the machine's threads)
  --mem-some P    memory pressure limit, % (default 10)
  --window D      swapping measured over D (default 1s)
  --proc DIR      read DIR instead of /proc

Exit: 0 free, 1 busy, 2 usage error or a failed reading.
`

const (
	exitBusy  = 1
	exitUsage = 2
)

type options struct {
	limits   pressure.Limits
	procRoot string
}

var errHelp = errors.New("help")

func main() {
	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "busy: %v\n(busy -h for usage)\n", err)
		os.Exit(exitUsage)
	}
	os.Exit(run(context.Background(), opts, os.Stdout, os.Stderr))
}

func parseArgs(args []string) (options, error) {
	opts := options{limits: pressure.DefaultLimits, procRoot: "/proc"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" {
			return opts, errHelp
		}
		if !strings.HasPrefix(a, "--") {
			return opts, fmt.Errorf("unexpected argument %q", a)
		}
		name, value, ok := strings.Cut(a, "=")
		if !ok {
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s needs a value", a)
			}
			i++
			value = args[i]
		}
		var err error
		switch name {
		case "--runnable":
			var n int
			n, err = strconv.Atoi(value)
			if err == nil && n <= 0 {
				err = errors.New("must be positive")
			}
			opts.limits.Runnable = float64(n)
		case "--mem-some":
			opts.limits.MaxMemSome, err = strconv.ParseFloat(value, 64)
			if err == nil && opts.limits.MaxMemSome <= 0 {
				err = errors.New("must be positive")
			}
		case "--window":
			opts.limits.SwapWindow, err = time.ParseDuration(value)
		case "--proc":
			opts.procRoot = value
		default:
			return opts, fmt.Errorf("unknown flag %s", name)
		}
		if err != nil {
			return opts, fmt.Errorf("%s %q: %w", name, value, err)
		}
	}
	return opts, nil
}

func run(ctx context.Context, opts options, stdout, stderr io.Writer) int {
	v, err := pressure.Check(ctx, opts.procRoot, opts.limits)
	if err != nil {
		fmt.Fprintf(stderr, "busy: %v\n", err)
		return exitUsage
	}
	fmt.Fprintln(stdout, line(v, opts.limits))
	if v.Busy() {
		return exitBusy
	}
	return 0
}

// line — ответ одной строкой: занята или свободна, почему и все замеры.
func line(v pressure.Verdict, l pressure.Limits) string {
	head := "free"
	if v.Busy() {
		head = "busy: " + v.Why()
	}
	m := v.Load
	var parts []string
	skipped := func(none error) bool { return errors.Is(errors.Join(v.Skipped...), none) }
	if !skipped(pressure.ErrNoRunnable) {
		limit := l.Runnable
		if limit <= 0 {
			limit = float64(m.Threads)
		}
		parts = append(parts, fmt.Sprintf("runnable %d (busy at %v, threads %d)", m.Runnable, limit, m.Threads))
		parts = append(parts, fmt.Sprintf("CPU pressure %.0f%% (some avg10)", m.CPUSome))
	}
	if !skipped(pressure.ErrNoIOPressure) {
		parts = append(parts, fmt.Sprintf("IO pressure %.1f%% (limit %v%%)", m.IOFull, l.MaxIOFull))
	}
	if !skipped(pressure.ErrNoMemPressure) {
		parts = append(parts, fmt.Sprintf("memory pressure %.1f%% (some avg10, limit %v%%)", m.MemSome, l.MaxMemSome))
	}
	if !skipped(pressure.ErrNoVMStat) {
		parts = append(parts, fmt.Sprintf("swapping %.0f KB/s (counts above %v only with memory pressure)", m.SwapKBs, l.MaxSwapKBs))
	}
	for _, e := range v.Skipped {
		parts = append(parts, "not checked: "+e.Error())
	}
	return head + "; " + strings.Join(parts, "; ")
}
