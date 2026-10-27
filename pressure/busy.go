package pressure

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Правило «машина занята» — одно на всех: graft пускает по нему тяжёлые задачи, aguard следит, команда
// busy показывает агентам. Процессор — по числу задач, готовых к работе прямо сейчас, одним чтением:
// нужно не точное число, а актуальное, а минутный load запаздывает.

// Мера, по которой машина занята.
const (
	CPU  = "cpu"
	IO   = "io"
	Swap = "swap"
)

// Меры, которых нет в системе (не Linux, ядро без PSI): потребитель пропускает меру и говорит об этом.
var (
	ErrNoRunnable   = errors.New("no /proc/stat to count runnable tasks from")
	ErrNoIOPressure = errors.New("no /proc/pressure/io to read the IO pressure from")
	ErrNoVMStat     = errors.New("no /proc/vmstat to count swapping from")
)

// Limits — пороги правила. Runnable 0 — число потоков машины, у которой procRoot: так машины с разным
// числом потоков судятся одинаково.
type Limits struct {
	Runnable   float64       // занята, когда готовых к работе задач столько или больше
	MaxIOFull  float64       // занята выше: % последней минуты, когда все задачи ждали диск (full avg60)
	MaxSwapKBs float64       // занята выше: обмен со свопом, KB/s; занятый своп не вредит, вредит обмен
	SwapWindow time.Duration // окно замера обмена при свежем замере
}

// DefaultLimits — пороги graft по умолчанию (load.max_io_full 10, load.max_swap_kbs 1024, окно 1 с).
var DefaultLimits = Limits{MaxIOFull: 10, MaxSwapKBs: 1024, SwapWindow: time.Second}

// Load — замеры машины. Непрочитанная мера — 0, её сентинел в Verdict.Skipped.
type Load struct {
	Runnable int     // готовых к работе задач без читающего процесса: procs_running из /proc/stat − 1
	Threads  int     // потоков машины: строки cpuN в /proc/stat
	CPUSome  float64 // % последних 10 с, когда хоть одна задача ждала процессор (some avg10), для справки
	IOFull   float64 // % последней минуты, когда все задачи ждали диск (full avg60)
	SwapKBs  float64 // обмен со свопом, KB/s
}

// Verdict — ответ правила: Measure пусто — свободна; иначе мера, её значение и порог.
type Verdict struct {
	Measure string
	Value   float64
	Limit   float64
	Load    Load
	Skipped []error // меры, которых нет в системе: ErrNoRunnable, ErrNoIOPressure, ErrNoVMStat
}

// Busy — занята ли машина.
func (v Verdict) Busy() bool { return v.Measure != "" }

// Why — почему занята, одной фразой; у свободной пусто.
func (v Verdict) Why() string {
	switch v.Measure {
	case CPU:
		return fmt.Sprintf("runnable %.0f is at or above %v", v.Value, v.Limit)
	case IO:
		return fmt.Sprintf("IO pressure %.1f%% is above %v%%", v.Value, v.Limit)
	case Swap:
		return fmt.Sprintf("swapping %.0f KB/s is above %v", v.Value, v.Limit)
	}
	return ""
}

// Rule применяет пороги к замерам: процессор, потом диск, потом своп; пропущенная мера не судится.
func Rule(m Load, l Limits, skipped []error) Verdict {
	v := Verdict{Load: m, Skipped: skipped}
	runnable := l.Runnable
	if runnable <= 0 {
		runnable = float64(m.Threads)
	}
	switch {
	case !skippedAny(skipped, ErrNoRunnable) && runnable > 0 && float64(m.Runnable) >= runnable:
		v.Measure, v.Value, v.Limit = CPU, float64(m.Runnable), runnable
	case !skippedAny(skipped, ErrNoIOPressure) && m.IOFull > l.MaxIOFull:
		v.Measure, v.Value, v.Limit = IO, m.IOFull, l.MaxIOFull
	case !skippedAny(skipped, ErrNoVMStat) && m.SwapKBs > l.MaxSwapKBs:
		v.Measure, v.Value, v.Limit = Swap, m.SwapKBs, l.MaxSwapKBs
	}
	return v
}

func skippedAny(skipped []error, target error) bool {
	for _, e := range skipped {
		if errors.Is(e, target) {
			return true
		}
	}
	return false
}

// Meter замеряет машину procRoot (/proc) раз за разом и помнит прошлое чтение свопа.
type Meter struct {
	procRoot string
	pages    int64
	at       time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

// NewMeter — замерщик машины procRoot.
func NewMeter(procRoot string) *Meter {
	return &Meter{procRoot: procRoot, now: time.Now, sleep: sleepCtx}
}

// Check замеряет машину и применяет правило. fresh — обмен со свопом за l.SwapWindow (ждёт окно);
// иначе — с прошлого Check, а у первого — тоже за окно. Ошибка — мера есть, но не читается.
func (m *Meter) Check(ctx context.Context, l Limits, fresh bool) (Verdict, error) {
	var load Load
	var skipped []error
	// measure кладёт замер или, когда меры нет в системе, её сентинел в skipped.
	measure := func(none error, read func() error) error {
		err := read()
		if errors.Is(err, none) {
			skipped = append(skipped, none)
			return nil
		}
		return err
	}
	err := errors.Join(
		measure(ErrNoRunnable, func() error {
			stat, err := m.read("stat", ErrNoRunnable)
			if err != nil {
				return err
			}
			load.Runnable, load.Threads, err = ParseStat(string(stat))
			return err
		}),
		measure(ErrNoIOPressure, func() error {
			psi, err := m.read(filepath.Join("pressure", "io"), ErrNoIOPressure)
			if err != nil {
				return err
			}
			load.IOFull, err = psiField(string(psi), "full", "avg60")
			return err
		}),
	)
	if err != nil {
		return Verdict{}, err
	}
	// Давление на процессор — для справки, правило его не судит: файла нет — 0.
	if cpu, err := os.ReadFile(filepath.Join(m.procRoot, "pressure", "cpu")); err == nil {
		if load.CPUSome, err = psiField(string(cpu), "some", "avg10"); err != nil {
			return Verdict{}, fmt.Errorf("CPU pressure: %w", err)
		}
	}
	if err := measure(ErrNoVMStat, func() (err error) {
		load.SwapKBs, err = m.swapping(ctx, l.SwapWindow, fresh)
		return err
	}); err != nil {
		return Verdict{}, err
	}
	return Rule(load, l, skipped), nil
}

// Check — один свежий замер машины procRoot по правилу.
func Check(ctx context.Context, procRoot string, l Limits) (Verdict, error) {
	return NewMeter(procRoot).Check(ctx, l, true)
}

// read читает файл procRoot; файла нет — мера none.
func (m *Meter) read(name string, none error) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(m.procRoot, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, none
	}
	return data, err
}

// swapping — обмен со свопом в KB/s: за окно, когда fresh или прошлого чтения нет, иначе с прошлого чтения.
func (m *Meter) swapping(ctx context.Context, window time.Duration, fresh bool) (float64, error) {
	if fresh || m.at.IsZero() {
		if err := m.readSwap(); err != nil {
			return 0, err
		}
		if err := m.sleep(ctx, window); err != nil {
			return 0, err
		}
	}
	pages, at := m.pages, m.at
	if err := m.readSwap(); err != nil {
		return 0, err
	}
	elapsed := m.at.Sub(at).Seconds()
	if elapsed <= 0 {
		return 0, nil
	}
	return float64(m.pages-pages) * float64(os.Getpagesize()) / 1024 / elapsed, nil
}

func (m *Meter) readSwap() error {
	data, err := m.read("vmstat", ErrNoVMStat)
	if err != nil {
		return err
	}
	pages, err := ParseSwapPages(string(data))
	if err != nil {
		return err
	}
	m.pages, m.at = pages, m.now()
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ParseStat достаёт из /proc/stat число готовых к работе задач без читающего процесса
// (procs_running − 1) и число потоков (строки cpuN).
func ParseStat(s string) (runnable, threads int, err error) {
	seen := false
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64*1024), 1024*1024) // строка intr длинная
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch {
		case f[0] == "procs_running":
			n, err := strconv.Atoi(f[1])
			if err != nil {
				return 0, 0, fmt.Errorf("stat procs_running %q: %w", f[1], err)
			}
			runnable, seen = max(n-1, 0), true
		case strings.HasPrefix(f[0], "cpu") && len(f[0]) > 3:
			threads++
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, fmt.Errorf("stat: %w", err)
	}
	if !seen {
		return 0, 0, errors.New("stat: no procs_running")
	}
	return runnable, threads, nil
}

// ParseSwapPages — страниц, ушедших в своп и вернувшихся с загрузки: pswpin + pswpout из /proc/vmstat.
func ParseSwapPages(s string) (int64, error) {
	var pages int64
	seen := 0
	for line := range strings.SplitSeq(s, "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || (name != "pswpin" && name != "pswpout") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("vmstat %s %q: %w", name, value, err)
		}
		pages += n
		seen++
	}
	if seen != 2 {
		return 0, errors.New("vmstat: no pswpin and pswpout")
	}
	return pages, nil
}
