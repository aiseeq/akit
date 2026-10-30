package pressure

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Правило «машина занята» — одно на всех: graft пускает по нему тяжёлые задачи, команда
// busy показывает агентам. Процессор — по числу задач, готовых к работе прямо сейчас, одним чтением:
// нужно не точное число, а актуальное, а минутный load запаздывает. Обмен со свопом сам по себе
// не занятость: он идёт и при запасе памяти, а вредит, когда задачи ждут память, — поэтому своп
// судится только вместе с давлением памяти.

// Мера, по которой машина занята.
const (
	CPU   = "cpu"
	IO    = "io"
	Swap  = "swap"  // обмен со свопом при давлении памяти
	Quiet = "quiet" // тихий режим: срок в файле QuietPath ещё не прошёл
)

// Меры, которых нет в системе (не Linux, ядро без PSI): потребитель пропускает меру и говорит об этом.
var (
	ErrNoRunnable    = errors.New("no /proc/stat to count runnable tasks from")
	ErrNoIOPressure  = errors.New("no /proc/pressure/io to read the IO pressure from")
	ErrNoMemPressure = errors.New("no /proc/pressure/memory to read the memory pressure from")
	ErrNoVMStat      = errors.New("no /proc/vmstat to count swapping from")
)

// Limits — пороги правила. MaxIOFull 100 и выше выключает проверку диска. Runnable 0 — число потоков машины, у которой procRoot: так машины с разным
// числом потоков судятся одинаково. MaxMemSome 0 — DefaultMemSome: так потребитель, не знающий этого
// порога, получает правило с ним.
type Limits struct {
	Runnable   float64       // занята, когда готовых к работе задач столько или больше
	MaxIOFull  float64       // занята на пороге: % времени, когда все задачи ждали диск за окно 5 с
	MaxMemSome float64       // память: % последних 10 с, когда хоть одна задача ждала память (some avg10)
	MaxSwapKBs float64       // и при этом обмен со свопом выше, KB/s; один обмен, без давления, — не занята
	SwapWindow time.Duration // окно замера обмена при свежем замере
}

// DefaultMemSome — порог давления памяти по умолчанию, %. По замерам машины за неделю: some avg10
// выше 10 — в 0,4% замеров, все — в эпизодах нехватки памяти; обычный фон — доли процента.
const DefaultMemSome = 10

// DefaultLimits — пороги правила по умолчанию: IO 9,6%, память 10%, своп 1024 KB/s за 1 с.
var DefaultLimits = Limits{MaxIOFull: 9.6, MaxMemSome: DefaultMemSome, MaxSwapKBs: 1024, SwapWindow: time.Second}

// Load — замеры машины. Непрочитанная мера — 0, её сентинел в Verdict.Skipped.
type Load struct {
	Runnable       int           // готовых к работе задач без читающего процесса: procs_running из /proc/stat − 1
	Threads        int           // потоков машины: строки cpuN в /proc/stat
	CPUSome        float64       // % последних 10 с, когда хоть одна задача ждала процессор (some avg10), для справки
	IOFull         float64       // % времени, когда все задачи ждали диск; источник диска — окно 5 с, fallback — с прошлого Check
	IOWindow       time.Duration // окно замера IOFull
	Disk           *DiskNow
	DiskFault      error
	ioFullExact    float64
	hasIOFullExact bool
	MemSome        float64 // % последних 10 с, когда хоть одна задача ждала память (some avg10)
	SwapKBs        float64 // обмен со свопом, KB/s
	// QuietUntil — срок тихого режима, пока он не прошёл; нулевое — режима нет или срок прошёл.
	QuietUntil time.Time
}

// Verdict — ответ правила: Measure пусто — свободна; иначе мера, её значение и порог.
type Verdict struct {
	Measure string
	Value   float64
	Limit   float64
	Load    Load
	Skipped []error // отсутствующие меры и источник состояния диска
}

// Busy — занята ли машина.
func (v Verdict) Busy() bool { return v.Measure != "" }

// Why — почему занята, одной фразой; у свободной пусто.
func (v Verdict) Why() string {
	switch v.Measure {
	case CPU:
		return fmt.Sprintf("runnable %.0f is at or above %v", v.Value, v.Limit)
	case IO:
		if v.Load.Disk != nil && v.Load.Disk.Open {
			return fmt.Sprintf("disk freeze in progress for %d ms", v.Load.Disk.OpenMs)
		}
		return fmt.Sprintf("IO pressure %.1f%% of the last %s is at or above %v%%", v.Value, v.Load.IOWindow.String(), v.Limit)
	case Swap:
		return fmt.Sprintf("memory pressure %.1f%% is above %v%% while swapping %.0f KB/s", v.Value, v.Limit, v.Load.SwapKBs)
	case Quiet:
		return "quiet mode until " + quietClock(v.Load.QuietUntil, time.Now())
	}
	return ""
}

// quietClock — срок тихого режима местным временем: часы, а не сегодня — и день.
func quietClock(until, now time.Time) string {
	until, now = until.Local(), now.Local()
	if y, m, d := until.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return until.Format("15:04")
	}
	return until.Format("2006-01-02 15:04")
}

// Rule применяет пороги к замерам: сначала тихий режим, потом процессор, диск, память со свопом;
// пропущенная мера не судится. Value и Limit у Swap — давление памяти и его порог: обмен лишь
// подтверждает его; у Quiet их нет, срок — в Load.QuietUntil.
func Rule(m Load, l Limits, skipped []error) Verdict {
	v := Verdict{Load: m, Skipped: skipped}
	maxIOFull := l.MaxIOFull
	if maxIOFull <= 0 {
		maxIOFull = DefaultLimits.MaxIOFull
	}
	runnable := l.Runnable
	if runnable <= 0 {
		runnable = float64(m.Threads)
	}
	memSome := l.MaxMemSome
	if memSome <= 0 {
		memSome = DefaultMemSome
	}
	ioFull := m.IOFull
	if m.hasIOFullExact {
		ioFull = m.ioFullExact
	}
	memory := !skippedAny(skipped, ErrNoMemPressure) && !skippedAny(skipped, ErrNoVMStat)
	switch {
	case !m.QuietUntil.IsZero():
		v.Measure = Quiet
	case !skippedAny(skipped, ErrNoRunnable) && runnable > 0 && float64(m.Runnable) >= runnable:
		v.Measure, v.Value, v.Limit = CPU, float64(m.Runnable), runnable
	case maxIOFull < 100 && (m.Disk != nil || !skippedAny(skipped, ErrNoIOPressure)) &&
		((m.Disk != nil && m.Disk.Open) || ioFull >= maxIOFull):
		v.Measure, v.Value, v.Limit = IO, m.IOFull, maxIOFull
	case memory && m.MemSome > memSome && m.SwapKBs > l.MaxSwapKBs:
		v.Measure, v.Value, v.Limit = Swap, m.MemSome, memSome
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
	quiet    string // файл срока тихого режима; пусто — QuietPath
	pages    int64
	at       time.Time
	ioTotal  int64
	ioAt     time.Time
	ioValid  bool
	disk     string
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

// NewMeter — замерщик машины procRoot.
func NewMeter(procRoot string) *Meter {
	return &Meter{procRoot: procRoot, now: time.Now, sleep: sleepCtx}
}

// Check замеряет машину и применяет правило. fresh — обмен со свопом за l.SwapWindow (ждёт окно);
// иначе — с прошлого Check, а у первого — тоже за окно. Файл тихого режима читается первым, затем
// меры из procRoot: и в тихом режиме вердикт несёт все замеры. Ошибка — без вердикта: файл тихого
// режима не читается или не разбирается, мера есть, но не читается.
func (m *Meter) Check(ctx context.Context, l Limits, kind string, fresh bool) (Verdict, error) {
	var load Load
	var skipped []error
	quiet := m.quiet
	var err error
	if quiet == "" {
		if quiet, err = QuietPath(); err != nil {
			return Verdict{}, err
		}
	}
	if load.QuietUntil, err = quietUntil(quiet, m.now()); err != nil {
		return Verdict{}, err
	}
	// measure кладёт замер или, когда меры нет в системе, её сентинел в skipped.
	measure := func(none error, read func() error) error {
		err := read()
		if errors.Is(err, none) {
			skipped = append(skipped, none)
			return nil
		}
		return err
	}
	err = errors.Join(
		measure(ErrNoRunnable, func() error {
			stat, err := m.read("stat", ErrNoRunnable)
			if err != nil {
				return err
			}
			load.Runnable, load.Threads, err = ParseStat(string(stat))
			return err
		}),
		measure(ErrNoMemPressure, func() error {
			psi, err := m.read(filepath.Join("pressure", "memory"), ErrNoMemPressure)
			if err != nil {
				return err
			}
			load.MemSome, err = psiField(string(psi), "some", "avg10")
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
	if err := m.measureSwapAndIO(ctx, l, fresh, &load, &skipped); err != nil {
		return Verdict{}, err
	}
	load.Disk, load.DiskFault = readDisk(ctx, m.socketPath(), kind, m.now())
	if load.Disk != nil {
		load.ioFullExact = load.Disk.Five.IOFullMS / 50
		load.IOFull, load.IOWindow = math.Round(load.ioFullExact*10)/10, 5*time.Second
		load.hasIOFullExact = true
	} else {
		if load.IOWindow == 0 {
			load.IOWindow = time.Second
		}
		switch {
		case errors.Is(load.DiskFault, ErrNoIOSource):
			skipped = append(skipped, ErrNoIOSource)
		case load.DiskFault != nil:
			skipped = append(skipped, ErrIOSourceFailed)
		}
	}
	return Rule(load, l, skipped), nil
}

func (m *Meter) measureSwapAndIO(ctx context.Context, l Limits, fresh bool, load *Load, skipped *[]error) error {
	var ioStart int64
	ioStartValid := m.ioValid
	ioStartAt := m.ioAt
	if fresh || m.at.IsZero() {
		var err error
		ioStart, ioStartValid, err = m.readIOTotal()
		if err != nil {
			return err
		}
		ioStartAt = m.now()
	}
	swap, err := m.swapping(ctx, l.SwapWindow, fresh)
	if errors.Is(err, ErrNoVMStat) {
		*skipped = append(*skipped, ErrNoVMStat)
	} else if err != nil {
		return err
	} else {
		load.SwapKBs = swap
	}
	ioEnd, ioEndValid, err := m.readIOTotal()
	if err != nil {
		return err
	}
	ioAt := m.now()
	if ioEndValid {
		baseAt, baseTotal, baseValid := m.ioAt, m.ioTotal, m.ioValid
		if fresh || baseAt.IsZero() {
			baseAt, baseTotal, baseValid = ioStartAt, ioStart, ioStartValid
		}
		if baseValid && !baseAt.IsZero() && ioAt.After(baseAt) {
			load.IOWindow = ioAt.Sub(baseAt).Round(100 * time.Millisecond)
			load.ioFullExact = float64(ioEnd-baseTotal) / ioAt.Sub(baseAt).Seconds() / 10000
			load.IOFull = math.Round(load.ioFullExact*10) / 10
			load.hasIOFullExact = true
		} else if !baseValid {
			*skipped = append(*skipped, ErrNoIOPressure)
		}
		m.ioTotal, m.ioAt, m.ioValid = ioEnd, ioAt, true
	} else {
		m.ioTotal, m.ioAt, m.ioValid = 0, ioAt, false
		*skipped = append(*skipped, ErrNoIOPressure)
	}
	return nil
}

// Check — один свежий замер машины procRoot по правилу.
func Check(ctx context.Context, procRoot string, l Limits, kind string) (Verdict, error) {
	return NewMeter(procRoot).Check(ctx, l, kind, true)
}

func (m *Meter) socketPath() string {
	if m.disk != "" {
		return m.disk
	}
	return diskPath()
}

// read читает файл procRoot; файла нет — мера none.
func (m *Meter) read(name string, none error) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(m.procRoot, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, none
	}
	return data, err
}

// swapping — обмен со свопом в KB/s за окно или с прошлого чтения.
func (m *Meter) swapping(ctx context.Context, window time.Duration, fresh bool) (float64, error) {
	first := fresh || m.at.IsZero()
	var before int64
	var beforeAt time.Time
	if !first {
		before, beforeAt = m.pages, m.at
	} else if err := m.readSwap(); err != nil && !errors.Is(err, ErrNoVMStat) {
		return 0, err
	} else if err == nil {
		before, beforeAt = m.pages, m.at
	}
	if first {
		if err := m.sleep(ctx, window); err != nil {
			return 0, err
		}
	}
	if err := m.readSwap(); err != nil {
		return 0, err
	}
	elapsed := m.at.Sub(beforeAt).Seconds()
	if elapsed <= 0 || beforeAt.IsZero() {
		return 0, nil
	}
	return float64(m.pages-before) * float64(os.Getpagesize()) / 1024 / elapsed, nil
}

func (m *Meter) readIOTotal() (int64, bool, error) {
	data, err := m.read(filepath.Join("pressure", "io"), ErrNoIOPressure)
	if errors.Is(err, ErrNoIOPressure) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("IO pressure: %w", err)
	}
	total, err := psiTotal(string(data), "full")
	if err != nil {
		return 0, false, fmt.Errorf("IO pressure: %w", err)
	}
	return total, true, nil
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

func psiTotal(s, kind string) (int64, error) {
	for line := range strings.SplitSeq(s, "\n") {
		if !strings.HasPrefix(line, kind+" ") {
			continue
		}
		for field := range strings.FieldsSeq(line) {
			if value, ok := strings.CutPrefix(field, "total="); ok {
				return strconv.ParseInt(value, 10, 64)
			}
		}
	}
	return 0, fmt.Errorf("PSI: нет строки %s с total", kind)
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

// ErrNoLoadAvg — нет /proc/loadavg (не Linux).
var ErrNoLoadAvg = errors.New("no /proc/loadavg to read the load average from")

// LoadAvg — строка /proc/loadavg: средние за 1, 5, 15 минут и сущности планировщика (выполняются / всего).
// Для наблюдения за длительной перегрузкой; решение «пускать ли сейчас» — по Check.
type LoadAvg struct {
	Avg1, Avg5, Avg15 float64
	Running, Total    int
}

// ParseLoadAvg разбирает текст /proc/loadavg.
func ParseLoadAvg(s string) (LoadAvg, error) {
	var la LoadAvg
	f := strings.Fields(s)
	if len(f) < 4 {
		return la, fmt.Errorf("loadavg %q: want at least 4 fields", strings.TrimSpace(s))
	}
	for i, p := range []*float64{&la.Avg1, &la.Avg5, &la.Avg15} {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return la, fmt.Errorf("loadavg %q: %w", f[i], err)
		}
		*p = v
	}
	running, total, ok := strings.Cut(f[3], "/")
	if !ok {
		return la, fmt.Errorf("loadavg %q: want running/total", f[3])
	}
	var err error
	if la.Running, err = strconv.Atoi(running); err != nil {
		return la, fmt.Errorf("loadavg running %q: %w", running, err)
	}
	if la.Total, err = strconv.Atoi(total); err != nil {
		return la, fmt.Errorf("loadavg total %q: %w", total, err)
	}
	return la, nil
}

// ReadLoadAvg читает procRoot/loadavg; файла нет — ErrNoLoadAvg.
func ReadLoadAvg(procRoot string) (LoadAvg, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, "loadavg"))
	if errors.Is(err, os.ErrNotExist) {
		return LoadAvg{}, ErrNoLoadAvg
	}
	if err != nil {
		return LoadAvg{}, fmt.Errorf("loadavg: %w", err)
	}
	return ParseLoadAvg(string(data))
}
