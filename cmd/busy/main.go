// busy — занята ли машина по общему правилу akit/pressure, одной строкой: то же число, по которому
// graft пускает тяжёлые задачи, вместо `cat /proc/loadavg`: минутный load запаздывает.
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
	"github.com/aiseeq/akit/timearg"
)

const usage = `busy — is the machine busy by the shared rule (akit/pressure), in one line.

Usage:
  busy [--runnable N] [--mem-some P] [--io-full P] [--kind K] [--window D] [--proc DIR]
  busy --quiet-until TIME | --quiet D | --quiet-off

Busy when quiet mode is on, the tasks ready to run now (procs_running, one reading, without busy
itself) are at or above the machine's threads, disk IO pressure is at or above 9.6%, or the
memory pressure (some avg10) is above 10% while swapping over the window is above 1024 KB/s.
Swapping alone is not busy. The same rule graft uses to queue heavy tasks.

Quiet mode keeps the machine busy until a time, whatever it measures: heavy work waits while
someone needs a responsive machine. The time is one RFC3339 line in $XDG_STATE_HOME/akit/quiet-until
(~/.local/state/akit/quiet-until); a past time does nothing and needs no cleanup.

Flags:
  --runnable N    busy at N runnable tasks (default: the machine's threads)
  --mem-some P    memory pressure limit, % (default 10)
  --io-full P     disk IO pressure limit, % (default 9.6; 100 or more disables disk checks)
  --kind K        disk state profile kind to request
  --window D      swapping measured over D (default 1s)
  --proc DIR      read DIR instead of /proc
  --quiet-until T quiet mode until T: 14:45 today, "30.10 09:00", or RFC3339
  --quiet D       quiet mode for D from now: 2h, 90m
  --quiet-off     end quiet mode

Exit: 0 free, 1 busy, 2 usage error or a failed reading. Setting quiet mode exits 0.
`

const (
	exitBusy  = 1
	exitUsage = 2
)

type options struct {
	limits   pressure.Limits
	procRoot string
	kind     string
	quiet    time.Time // срок тихого режима, который поставить
	quietOff bool      // снять тихий режим
}

var errHelp = errors.New("help")

func main() {
	opts, err := parseArgs(os.Args[1:], time.Now())
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

func parseArgs(args []string, now time.Time) (options, error) {
	opts := options{limits: pressure.DefaultLimits, procRoot: "/proc"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" {
			return opts, errHelp
		}
		if a == "--quiet-off" {
			opts.quietOff = true
			continue
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
		if err := opts.set(name, value, now); err != nil {
			return opts, err
		}
	}
	if opts.quietOff && !opts.quiet.IsZero() {
		return opts, errors.New("--quiet-off together with a quiet time")
	}
	return opts, nil
}

// set разбирает значение флага name.
func (opts *options) set(name, value string, now time.Time) error {
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
	case "--io-full":
		opts.limits.MaxIOFull, err = strconv.ParseFloat(value, 64)
		if err == nil && opts.limits.MaxIOFull <= 0 {
			err = errors.New("must be positive")
		}
	case "--kind":
		opts.kind = value
	case "--window":
		opts.limits.SwapWindow, err = time.ParseDuration(value)
	case "--proc":
		opts.procRoot = value
	case "--quiet-until":
		opts.quiet, err = timearg.Parse(value, now)
		if err == nil && !opts.quiet.After(now) {
			err = errors.New("is not in the future")
		}
	case "--quiet":
		var d time.Duration
		d, err = time.ParseDuration(value)
		if err == nil && d <= 0 {
			err = errors.New("must be positive")
		}
		opts.quiet = now.Add(d)
	default:
		return fmt.Errorf("unknown flag %s", name)
	}
	if err != nil {
		return fmt.Errorf("%s %q: %w", name, value, err)
	}
	return nil
}

func run(ctx context.Context, opts options, stdout, stderr io.Writer) int {
	if opts.quietOff || !opts.quiet.IsZero() {
		return setQuiet(opts, stdout, stderr)
	}
	if opts.limits.MaxIOFull <= 0 {
		opts.limits.MaxIOFull = pressure.DefaultLimits.MaxIOFull
	}
	v, err := pressure.Check(ctx, opts.procRoot, opts.limits, opts.kind)
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

// setQuiet ставит или снимает тихий режим и говорит, где его срок.
func setQuiet(opts options, stdout, stderr io.Writer) int {
	path, err := pressure.QuietPath()
	if err == nil {
		if opts.quietOff {
			err = pressure.ClearQuiet(path)
		} else {
			err = pressure.SetQuiet(path, opts.quiet)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "busy: %v\n", err)
		return exitUsage
	}
	if opts.quietOff {
		fmt.Fprintf(stdout, "quiet mode off (%s removed)\n", path)
		return 0
	}
	fmt.Fprintf(stdout, "quiet mode until %s (%s): busy answers busy until then; busy --quiet-off ends it\n",
		opts.quiet.Local().Format("2006-01-02 15:04 MST"), path)
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
	if l.MaxIOFull < 100 && (m.Disk != nil || !skipped(pressure.ErrNoIOPressure)) {
		if m.Disk != nil {
			if m.Disk.Open {
				parts = append(parts, fmt.Sprintf("disk freeze for %d ms", m.Disk.OpenMs))
			}
			parts = append(parts, fmt.Sprintf("IO pressure %.1f%% of 5s from the disk state source (limit %v%%)", m.IOFull, l.MaxIOFull))
		} else {
			parts = append(parts, fmt.Sprintf("IO pressure %.1f%% of %s, /proc/pressure/io total (limit %v%%)", m.IOFull, m.IOWindow.String(), l.MaxIOFull))
		}
	}
	if !skipped(pressure.ErrNoMemPressure) {
		parts = append(parts, fmt.Sprintf("memory pressure %.1f%% (some avg10, limit %v%%)", m.MemSome, l.MaxMemSome))
	}
	if !skipped(pressure.ErrNoVMStat) {
		parts = append(parts, fmt.Sprintf("swapping %.0f KB/s (counts above %v only with memory pressure)", m.SwapKBs, l.MaxSwapKBs))
	}
	if errors.Is(errors.Join(v.Skipped...), pressure.ErrNoIOSource) || errors.Is(errors.Join(v.Skipped...), pressure.ErrIOSourceFailed) {
		parts = append(parts, "not checked: no disk state source, /proc/pressure/io total used: "+m.DiskFault.Error())
	}
	for _, e := range v.Skipped {
		if errors.Is(e, pressure.ErrNoIOSource) || errors.Is(e, pressure.ErrIOSourceFailed) {
			continue
		}
		parts = append(parts, "not checked: "+e.Error())
	}
	return head + "; " + strings.Join(parts, "; ")
}
