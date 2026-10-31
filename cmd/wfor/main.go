// wfor — ждать одним вызовом с таймаутом: файл, конец процесса, строку в логе, время, успех команды, разгрузку
// машины. Вместо циклов `while … sleep`, которые агенты пишут каждый раз заново. Ответ — что выполнилось и за
// сколько, а по таймауту — чего ещё ждём и что видно сейчас.
//
// Коды выхода: 0 — дождались, 1 — таймаут, 2 — ошибка вызова или проверки.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/timearg"
)

const usage = `wfor — wait for conditions in one call, with a timeout.

Usage:
  wfor [flags] COND [COND...]     all CONDs (with --any: one of them)

Conditions:
  file PATH       PATH exists and is not empty
  gone PATH       PATH does not exist
  line RE FILE    a line of FILE matches RE (Go regexp); FILE may not exist yet
  exit PID...     the processes have exited (any processes, not only children)
  time T          local time T: 14:45, "26.10 09:00", 2026-10-26T09:00:00Z
  ok CMD          bash -c CMD exits 0 (one string; run every 10s, or --every)
  idle            the machine is calm by graft's rule (graft exec --heavy --local);
                  for a heavy command after the wait run graft exec --heavy -- CMD
                  instead: it waits the same way and keeps its place in the queue

Flags:
  --timeout D     stop after D (default 4m30s) and say what is still missing;
                  to wait longer run the same wfor again
  --every D       check interval (default 1s; ok commands 10s)
  --any           done when one condition is met
  --new           line: only lines appended after the start count
  --after N       line: only lines after line N count (to continue a wait)

Exit: 0 done, 1 not yet (timeout), 2 usage error or a check failed (no access, bad file).
`

const (
	exitTimeout = 1
	exitUsage   = 2
)

const (
	defaultTimeout = 270 * time.Second // меньше 5 минут жизни кэша промптов суб-агента
	defaultEvery   = time.Second
	defaultOkEvery = 10 * time.Second
)

type options struct {
	timeout time.Duration
	every   time.Duration
	any     bool
	conds   []cond
}

var errHelp = errors.New("help")

func main() {
	opts, err := parseArgs(os.Args[1:], time.Now)
	if errors.Is(err, errHelp) {
		fmt.Print(usage)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "wfor: %v\n(wfor -h for usage)\n", err)
		os.Exit(exitUsage)
	}
	os.Exit(run(context.Background(), opts, os.Stdout, os.Stderr))
}

// parsed — аргументы до сборки условий: ok и line зависят от флагов, которые могут стоять после них.
type parsed struct {
	opts     options
	everySet bool
	onlyNew  bool
	after    int
	specs    [][]string
}

func parseArgs(args []string, now func() time.Time) (options, error) {
	p := parsed{opts: options{timeout: defaultTimeout, every: defaultEvery}}
	for i := 0; i < len(args); i++ {
		n, err := p.arg(args, i)
		if err != nil {
			return options{}, err
		}
		i += n
	}
	if len(p.specs) == 0 {
		return options{}, errors.New("no condition to wait for")
	}
	if p.onlyNew && p.after > 0 {
		return options{}, errors.New("--new and --after together; pick one")
	}
	for _, s := range p.specs {
		c, err := p.build(s, now)
		if err != nil {
			return options{}, err
		}
		p.opts.conds = append(p.opts.conds, c)
	}
	return p.opts, nil
}

// arg разбирает аргумент i и возвращает, сколько следующих аргументов он съел.
func (p *parsed) arg(args []string, i int) (int, error) {
	a := args[i]
	value := func() (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", a)
		}
		return args[i+1], nil
	}
	switch a {
	case "-h", "--help":
		return 0, errHelp
	case "--any":
		p.opts.any = true
		return 0, nil
	case "--new":
		p.onlyNew = true
		return 0, nil
	case "--after":
		v, err := value()
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("--after %q: want a line number", v)
		}
		p.after = n
		return 1, nil
	case "--timeout", "--every":
		v, err := value()
		if err != nil {
			return 0, err
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("%s %q: want a duration like 30s, 5m, 1h", a, v)
		}
		if a == "--timeout" {
			p.opts.timeout = d
		} else {
			p.opts.every, p.everySet = d, true
		}
		return 1, nil
	}
	arity := map[string]int{"file": 1, "gone": 1, "line": 2, "time": 1, "ok": 1, "idle": 0}
	if a == "exit" {
		n := 0
		for i+1+n < len(args) && isPID(args[i+1+n]) {
			n++
		}
		if n == 0 {
			return 0, errors.New("exit needs one or more PIDs")
		}
		p.specs = append(p.specs, args[i:i+1+n])
		return n, nil
	}
	n, ok := arity[a]
	if !ok {
		return 0, fmt.Errorf("unknown condition or flag %q", a)
	}
	if i+n >= len(args) {
		return 0, fmt.Errorf("%s needs %d argument(s)", a, n)
	}
	p.specs = append(p.specs, args[i:i+1+n])
	return n, nil
}

func isPID(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0
}

func (p *parsed) build(s []string, now func() time.Time) (cond, error) {
	switch s[0] {
	case "file":
		return &fileCond{path: s[1]}, nil
	case "gone":
		return &goneCond{path: s[1]}, nil
	case "line":
		return newLineCond(s[1], s[2], p.onlyNew, p.after)
	case "exit":
		pids := make([]int, 0, len(s)-1)
		for _, a := range s[1:] {
			pid, err := strconv.Atoi(a)
			if err != nil {
				return nil, fmt.Errorf("exit %q: %w", a, err)
			}
			pids = append(pids, pid)
		}
		return newExitCond(pids)
	case "time":
		at, err := timearg.Parse(s[1], now())
		if err != nil {
			return nil, err
		}
		if !at.After(now()) {
			return nil, fmt.Errorf("time %q is already past (%s); time takes a clock time, for a pause use sleep", s[1],
				at.Format("02.01 15:04:05"))
		}
		return &timeCond{at: at, now: now}, nil
	case "ok":
		every := defaultOkEvery
		if p.everySet {
			every = p.opts.every
		}
		return &okCond{cmd: s[1], every: every, now: now}, nil
	}
	graft, err := exec.LookPath("graft")
	if err != nil {
		return nil, fmt.Errorf("idle needs graft: %w", err)
	}
	return &idleCond{graft: graft}, nil
}

func run(ctx context.Context, opts options, stdout, stderr io.Writer) int {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	met := make([]bool, len(opts.conds))
	tick := time.NewTicker(opts.every)
	defer tick.Stop()
	for {
		for i, c := range opts.conds {
			if met[i] {
				continue
			}
			ok, err := c.check(ctx)
			if err != nil {
				fmt.Fprintf(stderr, "wfor: %v\n", err)
				return exitUsage
			}
			met[i] = ok
		}
		if done(met, opts.any) {
			fmt.Fprintf(stdout, "wfor: done in %s: %s\n", since(start), describe(opts.conds, met, true))
			return 0
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(stdout, timeoutReport(opts.conds, met, start))
			return exitTimeout
		case <-tick.C:
		}
	}
}

// timeoutReport — чего ещё ждём и что видно сейчас; выполненное — следом; как ждать дальше. Таймаут по умолчанию
// короче 5 минут, жизни кэша промптов суб-агента: ждать дольше — повтором вызова, а не одним долгим.
func timeoutReport(conds []cond, met []bool, start time.Time) string {
	msg := fmt.Sprintf("wfor: not yet after %s; still waiting: %s", since(start), describe(conds, met, false))
	if m := describe(conds, met, true); m != "" {
		msg += "; met: " + m
	}
	var skipped []*lineCond
	for i, c := range conds {
		if lc, ok := c.(*lineCond); ok && lc.skipped && !met[i] {
			skipped = append(skipped, lc)
		}
	}
	switch len(skipped) {
	case 0:
		return msg + "; to keep waiting run the same wfor again"
	case 1:
		return msg + fmt.Sprintf("; to keep waiting run it again with --after %d in place of --new/--after",
			skipped[0].lines)
	}
	return msg + "; to keep waiting run each line condition again with its own --after (lines read: " +
		linesRead(skipped) + ")"
}

func linesRead(conds []*lineCond) string {
	parts := make([]string, len(conds))
	for i, c := range conds {
		parts[i] = fmt.Sprintf("%s %d", c.path, c.lines)
	}
	return strings.Join(parts, ", ")
}

func done(met []bool, any bool) bool {
	n := 0
	for _, m := range met {
		if m {
			n++
		}
	}
	if any {
		return n > 0
	}
	return n == len(met)
}

// describe — условия с выполненностью want: «условие (что видно)» через «; ».
func describe(conds []cond, met []bool, want bool) string {
	var parts []string
	for i, c := range conds {
		if met[i] == want {
			parts = append(parts, fmt.Sprintf("%s (%s)", c, c.state()))
		}
	}
	return strings.Join(parts, "; ")
}

func since(start time.Time) string {
	d := time.Since(start)
	if d < 10*time.Second {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}
