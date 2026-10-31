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
	"github.com/aiseeq/akit/session"
	"github.com/aiseeq/akit/timearg"
)

const usage = `busy — is the machine busy by the shared rule (akit/pressure), in one line.

Usage:
  busy [--runnable N] [--mem-some P] [--io-full P] [--kind K] [--window D] [--proc DIR] [--ignore-quiet]
  busy (--quiet-until TIME | --quiet D) [--why TEXT] | --quiet-off

Busy when quiet mode is on, the tasks ready to run now (procs_running, one reading, without busy
itself) are at or above the machine's threads, disk IO pressure is at or above 9.6%, or the
memory pressure (some avg10) is above 10% while swapping over the window is above 1024 KB/s.
Swapping alone is not busy. The same rule graft uses to queue heavy tasks. The limits below are the
defaults; the user's own are flat "key: value" lines in $XDG_CONFIG_HOME/akit/pressure.yaml
(~/.config/akit/pressure.yaml; keys runnable, max_io_full, max_mem_some, max_swap_kbs, swap_window),
which graft reads too; flags override both for one check.

Quiet mode keeps the machine busy until a time, whatever it measures: heavy work waits while
someone needs a responsive machine. The time is one RFC3339 line in $XDG_STATE_HOME/akit/quiet-until
(~/.local/state/akit/quiet-until); a past time does nothing and needs no cleanup. Who set it (the
Claude Code session, from its registry entry) and --why go to quiet-until.note next to it and show in
the busy line and in graft's wait: "quiet mode until 14:45 (planner: call)".

Flags:
  --runnable N    busy at N runnable tasks (default: the machine's threads)
  --mem-some P    memory pressure limit, % (default 10)
  --io-full P     disk IO pressure limit, % (default 9.6; 100 or more disables disk checks)
  --kind K        disk state profile kind to request
  --window D      swapping measured over D (default 1s)
  --proc DIR      read DIR instead of /proc
  --quiet-until T quiet mode until T: 14:45 today, "30.10 09:00", or RFC3339
  --quiet D       quiet mode for D from now: 2h, 90m
  --why TEXT      why quiet mode is set, shown to everyone it holds back
  --quiet-off     end quiet mode
  --ignore-quiet  judge by the measurements alone, as if quiet mode were off (for a measurement
                  that runs inside quiet mode); the line still names the quiet mode

Exit: 0 free, 1 busy, 2 usage error or a failed reading. Setting quiet mode exits 0.
`

const (
	exitBusy  = 1
	exitUsage = 2
)

type options struct {
	limits      pressure.Limits
	procRoot    string
	kind        string
	quiet       time.Time // срок тихого режима, который поставить
	why         string    // зачем тихий режим
	quietOff    bool      // снять тихий режим
	ignoreQuiet bool      // судить по замерам без тихого режима
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
	limits, err := pressure.UserLimits()
	if err != nil {
		return options{}, err
	}
	opts := options{limits: limits, procRoot: "/proc"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" {
			return opts, errHelp
		}
		if a == "--quiet-off" {
			opts.quietOff = true
			continue
		}
		if a == "--ignore-quiet" {
			opts.ignoreQuiet = true
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
	if opts.why != "" && opts.quiet.IsZero() {
		return opts, errors.New("--why needs --quiet or --quiet-until")
	}
	if opts.ignoreQuiet && (opts.quietOff || !opts.quiet.IsZero()) {
		return opts, errors.New("--ignore-quiet is for a check, not for setting quiet mode")
	}
	return opts, nil
}

// limitFlags — флаги порогов и их ключи в файле порогов: значение проверяет тот же разбор, что и файл.
var limitFlags = map[string]string{"--runnable": "runnable", "--mem-some": "max_mem_some", "--io-full": "max_io_full", "--window": "swap_window"}

// set разбирает значение флага name.
func (opts *options) set(name, value string, now time.Time) error {
	var err error
	switch name {
	case "--runnable", "--mem-some", "--io-full", "--window":
		err = opts.limits.Set(limitFlags[name], value)
	case "--kind":
		opts.kind = value
	case "--proc":
		opts.procRoot = value
	case "--quiet-until", "--quiet", "--why":
		err = opts.setQuiet(name, value, now)
	default:
		return fmt.Errorf("unknown flag %s", name)
	}
	if err != nil {
		return fmt.Errorf("%s %q: %w", name, value, err)
	}
	return nil
}

// setQuiet разбирает флаги тихого режима: срок и зачем.
func (opts *options) setQuiet(name, value string, now time.Time) error {
	switch name {
	case "--quiet-until":
		var err error
		if opts.quiet, err = timearg.Parse(value, now); err != nil {
			return err
		}
		if !opts.quiet.After(now) {
			return errors.New("is not in the future")
		}
	case "--quiet":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("duration: %w", err)
		}
		if d <= 0 {
			return errors.New("must be positive")
		}
		opts.quiet = now.Add(d)
	default:
		opts.why = strings.TrimSpace(value)
		if opts.why == "" {
			return errors.New("must not be empty")
		}
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
	if opts.ignoreQuiet && v.Measure == pressure.Quiet {
		quiet := v.Why()
		v = v.WithoutQuiet(opts.limits)
		fmt.Fprintln(stdout, line(v, opts.limits)+"; ignored: "+quiet)
	} else {
		fmt.Fprintln(stdout, line(v, opts.limits))
	}
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
			var by string
			if by, err = sessionName(); err == nil {
				err = pressure.SetQuiet(path, pressure.QuietNote{Until: opts.quiet, By: by, Why: opts.why})
			}
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

// sessionName — имя сессии Claude Code, из которой вызван busy, по её записи в реестре (CLAUDE_PID);
// вне сессии или без имени в записи — пусто.
func sessionName() (string, error) {
	raw := os.Getenv("CLAUDE_PID")
	if raw == "" {
		return "", nil
	}
	pid, err := strconv.Atoi(raw)
	if err != nil {
		return "", fmt.Errorf("CLAUDE_PID %q: %w", raw, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("session registry: %w", err)
	}
	info, err := session.ForHome(home).Get(pid)
	if err != nil || info == nil {
		return "", err
	}
	return info.Name, nil
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
