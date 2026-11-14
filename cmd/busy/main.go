// busy — занята ли машина по общему правилу akit/pressure, одной строкой: то же число, по которому
// graft пускает тяжёлые задачи: моментальное число готовых задач и load average за минуту против числа
// потоков, диск, память со свопом.
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
  busy [--runnable N] [--mem-some P] [--io-full P] [--dirty-mb M] [--window D] [--proc DIR] [--ignore-quiet]
  busy (--half-until TIME | --half D) [--why TEXT] | --half-off
  busy (--quiet-until TIME | --quiet D) [--why TEXT] | --quiet-off

Busy when quiet mode is on, the tasks ready to run now (procs_running, one reading, without busy
itself) or the 1-minute load average are at or above the machine's threads, disk IO pressure is at or above 9.6%
or the disk state source reports a disk freeze in progress, dirty and writeback memory (Dirty+Writeback
in /proc/meminfo, one reading) is at or above the machine's background write-out threshold
(vm.dirty_background_bytes, or vm.dirty_background_ratio of free and file memory when the bytes are 0) — a
write-out to disk in progress — or the memory pressure (some avg10) is above 10% while swapping over
the window is above 1024 KB/s. With a fresh check, a responding disk state source and memory pressure at or below
the limit, swapping is not measured because it cannot affect the verdict.
Swapping alone is not busy. The same rule graft uses to queue heavy tasks. The limits below are the
defaults; the user's own are flat "key: value" lines in $XDG_CONFIG_HOME/akit/pressure.yaml
(~/.config/akit/pressure.yaml; keys runnable, max_io_full, max_mem_some, max_swap_kbs, swap_window,
max_dirty_mb),
which graft reads too; flags override both for one check.

Half mode lowers the CPU threshold to half the machine's threads, with a minimum of one; a lower
configured runnable limit takes precedence. Disk, writeback and memory checks keep their usual limits.
Half mode lasts until its deadline. The time is stored as one RFC3339 line in
$XDG_STATE_HOME/akit/half-until (~/.local/state/akit/half-until). The adjacent half-until.note records
who set it and why, and the busy line and graft wait explain CPU busy verdicts as "half until 14:45 (planner: call)".
graft also gives a local heavy task started in half mode a CPU quota and GOMAXPROCS of half the threads;
tasks already running keep theirs. Neither half nor quiet mode silences the watchdog's messages to
sessions: that is "aguard quiet on".

Quiet mode keeps the machine busy until a time, whatever it measures: heavy work waits while
someone needs a responsive machine. The time is one RFC3339 line in $XDG_STATE_HOME/akit/quiet-until
(~/.local/state/akit/quiet-until); a past time does nothing and needs no cleanup. Who set it (the
Claude Code session, from its registry entry) and --why go to quiet-until.note next to it and show in
the busy line and in graft's wait: "quiet mode until 14:45 (planner: call)".

Flags:
  --runnable N    busy at N runnable tasks or 1-minute load (default: the machine's threads)
  --mem-some P    memory pressure limit, % (default 10)
  --io-full P     disk IO pressure limit, % (default 9.6; 100 or more disables disk checks)
  --dirty-mb M    write-out limit: dirty and writeback memory, MiB (default: the background write-out threshold)
  --window D      swapping measured over D (default 1s)
  --proc DIR      read DIR instead of /proc
  --half-until T half mode until T: 14:45 today, "30.10 09:00", or RFC3339
  --half D       half mode for D from now: 2h, 90m
  --half-off     end half mode
  --quiet-until T quiet mode until T: 14:45 today, "30.10 09:00", or RFC3339
  --quiet D       quiet mode for D from now: 2h, 90m
  --why TEXT      why the selected mode is set, shown to everyone it holds back; over ssh the
                  remote shell splits it again: ssh HOST "busy --quiet 2h --why 'a long reason'"
  --quiet-off     end quiet mode
  --ignore-quiet  judge by the measurements alone, as if quiet mode were off (for a measurement
                  that runs inside quiet mode); the line still names the quiet mode

The three CPU policies are ordinary (the machine's thread count), half, and quiet. Half changes the CPU
threshold only; quiet makes the machine busy regardless of its measurements.

Exit: 0 free, 1 busy, 2 usage error or a failed reading. Setting either mode exits 0.
`

const (
	exitBusy  = 1
	exitUsage = 2
)

type options struct {
	limits      pressure.Limits
	overrides   [][2]string // флаги порогов (ключ файла, значение) — поверх файла порогов
	procRoot    string
	quiet       time.Time // срок тихого режима, который поставить
	half        time.Time // срок режима половины машины, который поставить
	why         string    // зачем активный режим
	quietOff    bool      // снять тихий режим
	halfOff     bool      // снять режим половины машины
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
	opts := options{procRoot: "/proc"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" {
			return opts, errHelp
		}
		if opts.setSwitch(a) {
			continue
		}
		if !strings.HasPrefix(a, "--") {
			if opts.why != "" {
				return opts, fmt.Errorf("unexpected argument %q: a --why of several words needs quotes, over ssh a second layer of them", a)
			}
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
	if err := opts.checkModes(); err != nil {
		return opts, err
	}
	if opts.quietOff || !opts.quiet.IsZero() || opts.halfOff || !opts.half.IsZero() {
		return opts, nil
	}
	err := opts.loadLimits()
	return opts, err
}

// setSwitch ставит ключ без значения; false — a не такой ключ.
func (o *options) setSwitch(a string) bool {
	switch a {
	case "--quiet-off":
		o.quietOff = true
	case "--half-off":
		o.halfOff = true
	case "--ignore-quiet":
		o.ignoreQuiet = true
	default:
		return false
	}
	return true
}

// checkModes отклоняет несовместимые ключи режимов: за вызов — один режим и одно действие с ним.
func (o *options) checkModes() error {
	opts := *o
	if opts.quietOff && !opts.quiet.IsZero() {
		return errors.New("--quiet-off together with a quiet time")
	}
	if opts.halfOff && !opts.half.IsZero() {
		return errors.New("--half-off together with a half time")
	}
	if opts.quietOff && (opts.halfOff || !opts.half.IsZero()) ||
		opts.halfOff && !opts.quiet.IsZero() ||
		!opts.quiet.IsZero() && !opts.half.IsZero() {
		return errors.New("choose one mode per invocation")
	}
	if opts.why != "" && opts.quiet.IsZero() && opts.half.IsZero() {
		return errors.New("--why needs --quiet, --quiet-until, --half, or --half-until")
	}
	if opts.ignoreQuiet && (opts.quietOff || !opts.quiet.IsZero() || opts.halfOff || !opts.half.IsZero()) {
		return errors.New("--ignore-quiet is for a check, not for setting quiet mode")
	}
	return nil
}

// loadLimits — пороги файла пользователя и поверх них флаги. Файл нужен только замеру: справку и тихий режим
// битый файл не блокирует.
func (opts *options) loadLimits() error {
	limits, err := pressure.UserLimits()
	if err != nil {
		return err
	}
	for _, o := range opts.overrides {
		if err := limits.Set(o[0], o[1]); err != nil {
			return err
		}
	}
	opts.limits = limits
	return nil
}

// limitFlags — флаги порогов и их ключи в файле порогов: значение проверяет тот же разбор, что и файл.
var limitFlags = map[string]string{"--runnable": "runnable", "--mem-some": "max_mem_some", "--io-full": "max_io_full", "--window": "swap_window", "--dirty-mb": "max_dirty_mb"}

// set разбирает значение флага name.
func (opts *options) set(name, value string, now time.Time) error {
	var err error
	switch name {
	case "--runnable", "--mem-some", "--io-full", "--window", "--dirty-mb":
		// Значение проверяется сразу, ошибка — с именем флага; ложится поверх файла в конце parseArgs.
		probe := pressure.DefaultLimits
		if err = probe.Set(limitFlags[name], value); err == nil {
			opts.overrides = append(opts.overrides, [2]string{limitFlags[name], value})
		}
	case "--proc":
		opts.procRoot = value
	case "--quiet-until", "--quiet", "--why":
		err = opts.setQuiet(name, value, now)
	case "--half-until", "--half":
		opts.half, err = parseModeUntil(name, value, now)
	default:
		return fmt.Errorf("unknown flag %s", name)
	}
	if err != nil {
		return fmt.Errorf("%s %q: %w", name, value, err)
	}
	return nil
}

// parseModeUntil разбирает срок режима: длительность или время окончания.
func parseModeUntil(name, value string, now time.Time) (time.Time, error) {
	if name == "--half-until" || name == "--quiet-until" {
		until, err := timearg.Parse(value, now)
		if err != nil {
			return time.Time{}, err
		}
		if !until.After(now) {
			return time.Time{}, errors.New("is not in the future")
		}
		return until, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("duration: %w", err)
	}
	if d <= 0 {
		if name == "--quiet" {
			return time.Time{}, errors.New("must be positive; --quiet-off ends quiet mode")
		}
		return time.Time{}, errors.New("must be positive; --half-off ends half mode")
	}
	return now.Add(d), nil
}

// setQuiet разбирает срок тихого режима и общий флаг --why.
func (opts *options) setQuiet(name, value string, now time.Time) error {
	switch name {
	case "--quiet-until", "--quiet":
		var err error
		opts.quiet, err = parseModeUntil(name, value, now)
		if err != nil {
			return err
		}
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
		return setMode(opts.quiet, opts.why, opts.quietOff, "quiet", pressure.QuietPath, stdout, stderr)
	}
	if opts.halfOff || !opts.half.IsZero() {
		return setMode(opts.half, opts.why, opts.halfOff, "half", pressure.HalfPath, stdout, stderr)
	}
	v, err := pressure.Check(ctx, opts.procRoot, opts.limits, "")
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

// setMode ставит или снимает режим с общей записью срока и пометки.
func setMode(until time.Time, why string, off bool, name string, pathFn func() (string, error), stdout, stderr io.Writer) int {
	path, err := pathFn()
	if err == nil {
		if off {
			err = pressure.ClearQuiet(path)
		} else {
			var by string
			if by, err = sessionName(); err == nil {
				err = pressure.SetQuiet(path, pressure.QuietNote{Until: until, By: by, Why: why})
			}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "busy: %v\n", err)
		return exitUsage
	}
	if off {
		fmt.Fprintf(stdout, "%s mode off (%s removed)\n", name, path)
		return 0
	}
	effect := "busy answers busy until then"
	if name == "half" {
		effect = "busy counts half the machine threads until then"
	}
	fmt.Fprintf(stdout, "%s mode until %s (%s): %s; busy --%s-off ends it\n",
		name, until.Local().Format("2006-01-02 15:04 MST"), path, effect, name)
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

// sourceParts — замеры диска от источника состояния: заморозка, давление окна 5 с и признание
// ожидания под потолком io.max, если источник его даёт.
func sourceParts(m pressure.Load, l pressure.Limits) []string {
	var parts []string
	if m.Disk.Open {
		freeze := fmt.Sprintf("disk freeze for %d ms", m.Disk.OpenMs)
		if m.Disk.OpenCapped {
			freeze += " under io.max caps, not counted"
		}
		parts = append(parts, freeze)
	}
	pressureText := fmt.Sprintf("IO pressure %.1f%% of 5s from the disk state source (limit %v%%)", m.IOFull, l.MaxIOFull)
	if c := m.Disk.Five.Capped; c != nil {
		if c.Capped {
			pressureText += " under io.max caps, not counted"
		}
		pressureText += fmt.Sprintf(": waits outside caps %.0f ms, at caps %.0f ms", c.OutsideMS, c.CappedMS)
	}
	return append(parts, pressureText)
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
		limit := l.RunnableLimit(m)
		parts = append(parts, fmt.Sprintf("runnable %d (busy at %v, threads %d)", m.Runnable, limit, m.Threads))
		if !skipped(pressure.ErrNoLoadAvg) {
			parts = append(parts, fmt.Sprintf("load average 1m %.2f (busy at %v)", m.Avg1, limit))
		}
		parts = append(parts, fmt.Sprintf("CPU pressure %.0f%% (some avg10)", m.CPUSome))
	}
	if l.MaxIOFull < 100 && (m.Disk != nil || !skipped(pressure.ErrNoIOPressure)) {
		if m.Disk != nil {
			parts = append(parts, sourceParts(m, l)...)
		} else {
			parts = append(parts, fmt.Sprintf("IO pressure %.1f%% of %s, /proc/pressure/io total (limit %v%%)", m.IOFull, m.IOWindow.String(), l.MaxIOFull))
		}
	}
	if !skipped(pressure.ErrNoMemInfo) && l.WritebackLimit(m) > 0 {
		parts = append(parts, fmt.Sprintf("dirty+writeback %.0f MiB (busy at %.0f)", m.DirtyMB, l.WritebackLimit(m)))
	}
	if !skipped(pressure.ErrNoMemPressure) {
		parts = append(parts, fmt.Sprintf("memory pressure %.1f%% (some avg10, limit %v%%)", m.MemSome, l.MaxMemSome))
	}
	if m.SwapUnmeasured {
		parts = append(parts, fmt.Sprintf("swapping not measured: memory pressure %.1f%% at or below %v%%", m.MemSome, l.MemSomeLimit()))
	} else if !skipped(pressure.ErrNoVMStat) {
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
