package pressure

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Правило «машина занята» — одно на всех: graft пускает по нему тяжёлые задачи, команда
// busy показывает агентам. Процессор — по числу задач, готовых к работе прямо сейчас, одним чтением, и по
// load average за минуту, оба с одним порогом — числом потоков: моментальное число ловит всплеск сразу, а
// минутное не пускает новое в машину, которая уже минуту перегружена и лишь на миг притихла. Обмен со свопом сам по себе
// не занятость: он идёт и при запасе памяти, а вредит, когда задачи ждут память, — поэтому своп
// судится только вместе с давлением памяти.

// Мера, по которой машина занята.
const (
	CPU       = "cpu" // готовых к работе задач сейчас или load average за минуту — не меньше порога Runnable
	IO        = "io"
	Writeback = "writeback" // выброс записи: грязных и пишущихся страниц сейчас не меньше порога фонового сброса
	Swap      = "swap"      // обмен со свопом при давлении памяти
	Quiet     = "quiet"     // тихий режим: срок в файле QuietPath ещё не прошёл
)

// Меры, которых нет в системе (не Linux, ядро без PSI): потребитель пропускает меру и говорит об этом.
var (
	ErrNoRunnable    = errors.New("no /proc/stat to count runnable tasks from")
	ErrNoIOPressure  = errors.New("no /proc/pressure/io to read the IO pressure from")
	ErrNoMemPressure = errors.New("no /proc/pressure/memory to read the memory pressure from")
	ErrNoVMStat      = errors.New("no /proc/vmstat to count swapping from")
	ErrNoMemInfo     = errors.New("no /proc/meminfo to read dirty and writeback memory from")
	ErrNoDirtyLimit  = errors.New("no background write-out threshold in /proc/sys/vm (dirty_background_bytes or dirty_background_ratio)")
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
	MaxDirtyMB float64       // выброс записи: Dirty+Writeback из /proc/meminfo, MiB; 0 — порог фонового сброса машины (Load.DirtyLimitMB)
}

// Выброс записи. Порог по умолчанию — порог фонового сброса самой машины: с него ядро начинает писать
// грязные страницы на диск, ниже — копит их в памяти. Так машина, настроенная сбрасывать рано (малый
// vm.dirty_background_bytes ради отзывчивости диска), занята при малом выбросе, а машина с настройками
// ядра по умолчанию (10% доступной памяти) — только при большом. Мера моментальная: выброс длится
// секунды, и новое тяжёлое, начатое посреди него, добавляет запись в очередь, которая уже держит диск.
// По замерам машины с dirty_background_bytes 64 MiB выше порога — около 10% времени, а за 3 с до
// заморозки диска — в 43% случаев, в 4,5 раза чаще, чем в случайный момент.

// DefaultMemSome — порог давления памяти по умолчанию, %. По замерам машины за неделю: some avg10
// выше 10 — в 0,4% замеров, все — в эпизодах нехватки памяти; обычный фон — доли процента.
const DefaultMemSome = 10

// DefaultLimits — пороги правила по умолчанию: IO 9,6%, память 10%, своп 1024 KB/s за 1 с.
var DefaultLimits = Limits{MaxIOFull: 9.6, MaxMemSome: DefaultMemSome, MaxSwapKBs: 1024, SwapWindow: time.Second}

// Load — замеры машины. Непрочитанная мера — 0, её сентинел в Verdict.Skipped.
type Load struct {
	Runnable       int           // готовых к работе задач без читающего процесса: procs_running из /proc/stat − 1
	Threads        int           // потоков машины: строки cpuN в /proc/stat
	Avg1           float64       // load average за минуту из /proc/loadavg
	CPUSome        float64       // % последних 10 с, когда хоть одна задача ждала процессор (some avg10), для справки
	IOFull         float64       // % IOWindow, когда все задачи ждали диск, до 0,1; при источнике — его окно 5 с, без него — дельта total PSI io за окно свопа (fresh) или с прошлого Check
	IOWindow       time.Duration // окно замера IOFull
	Disk           *DiskNow      // снимок источника состояния диска; nil — источника нет или он сбоит
	DiskFault      error         // почему источника нет или чем он сбоит; сентинел той же ошибки — в Verdict.Skipped
	ioFullExact    float64
	hasIOFullExact bool
	MemSome        float64 // % последних 10 с, когда хоть одна задача ждала память (some avg10)
	SwapKBs        float64 // обмен со свопом, KB/s
	SwapUnmeasured bool    // свежий замер свопа пропущен: источник диска ответил, давление памяти не выше порога
	DirtyMB        float64 // грязных и пишущихся на диск страниц сейчас (Dirty+Writeback из /proc/meminfo), MiB
	// DirtyLimitMB — порог фонового сброса машины, MiB, приближённо: vm.dirty_background_bytes, а если
	// он 0 — vm.dirty_background_ratio % от свободной и файловой памяти (см. dirtyLimit).
	DirtyLimitMB float64
	// QuietUntil — срок тихого режима, пока он не прошёл; нулевое — режима нет или срок прошёл.
	QuietUntil time.Time
	QuietBy    string    // кто поставил тихий режим, из его пометки
	QuietWhy   string    // зачем
	HalfUntil  time.Time // срок режима половины машины, пока не прошёл
	HalfBy     string    // кто поставил режим половины машины
	HalfWhy    string    // зачем
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
		if float64(v.Load.Runnable) < v.Limit {
			return withHalfWhy(v, fmt.Sprintf("load average 1m %.2f is at or above %v", v.Value, v.Limit))
		}
		return withHalfWhy(v, fmt.Sprintf("runnable %.0f is at or above %v", v.Value, v.Limit))
	case IO:
		if v.Load.Disk != nil && v.Load.Disk.Open && !v.Load.Disk.OpenCapped {
			return fmt.Sprintf("disk freeze in progress for %d ms", v.Load.Disk.OpenMs)
		}
		return fmt.Sprintf("IO pressure %.1f%% of the last %s is at or above %v%%", v.Value, v.Load.IOWindow.String(), v.Limit)
	case Writeback:
		return fmt.Sprintf("write-out in progress: dirty and writeback memory %.0f MiB is at or above %v MiB", v.Value, v.Limit)
	case Swap:
		return fmt.Sprintf("memory pressure %.1f%% is above %v%% while swapping %.0f KB/s", v.Value, v.Limit, v.Load.SwapKBs)
	case Quiet:
		return "quiet mode until " + quietClock(v.Load.QuietUntil, time.Now()) + quietNoteText(v.Load.QuietBy, v.Load.QuietWhy)
	}
	return ""
}

func withHalfWhy(v Verdict, why string) string {
	if v.Load.HalfUntil.IsZero() {
		return why
	}
	return why + " (half until " + quietClock(v.Load.HalfUntil, time.Now()) + quietNoteText(v.Load.HalfBy, v.Load.HalfWhy) + ")"
}

// quietNoteText — пометка тихого режима в скобках: «(planner: созвон)», «(planner)», «(созвон)».
func quietNoteText(by, why string) string {
	switch {
	case by != "" && why != "":
		return " (" + by + ": " + why + ")"
	case by != "" || why != "":
		return " (" + by + why + ")"
	}
	return ""
}

// WithoutQuiet — вердикт тех же замеров и порогов l без тихого режима: что машина показывает сама.
func (v Verdict) WithoutQuiet(l Limits) Verdict {
	m := v.Load
	m.QuietUntil, m.QuietBy, m.QuietWhy = time.Time{}, "", ""
	return Rule(m, l, v.Skipped)
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
	maxIOFull, runnable, memSome := effectiveLimits(m, l)
	memory := !skippedAny(skipped, ErrNoMemPressure) && !skippedAny(skipped, ErrNoVMStat)
	switch {
	case !m.QuietUntil.IsZero():
		v.Measure = Quiet
	case !skippedAny(skipped, ErrNoRunnable) && runnable > 0 && float64(m.Runnable) >= runnable:
		v.Measure, v.Value, v.Limit = CPU, float64(m.Runnable), runnable
	case !skippedAny(skipped, ErrNoLoadAvg) && runnable > 0 && m.Avg1 >= runnable:
		v.Measure, v.Value, v.Limit = CPU, m.Avg1, runnable
	case diskBusy(m, maxIOFull, skipped):
		v.Measure, v.Value, v.Limit = IO, m.IOFull, maxIOFull
	case !skippedAny(skipped, ErrNoMemInfo) && l.WritebackLimit(m) > 0 && m.DirtyMB >= l.WritebackLimit(m):
		v.Measure, v.Value, v.Limit = Writeback, m.DirtyMB, l.WritebackLimit(m)
	case memory && m.MemSome > memSome && m.SwapKBs > l.MaxSwapKBs:
		v.Measure, v.Value, v.Limit = Swap, m.MemSome, memSome
	}
	return v
}

// diskBusy — занята ли машина по диску: заморозка от источника или давление IO на пороге maxIOFull;
// порог 100 и выше выключает проверку, пропущенная мера без источника не судится. Ожидание, которое
// источник признал ожиданием под потолком io.max (задача ждёт свой предел, а не диск), не занятость:
// ни заморозка, ни давление окна 5 с. Без источника потолки не различаются.
func diskBusy(m Load, maxIOFull float64, skipped []error) bool {
	if maxIOFull >= 100 || (m.Disk == nil && skippedAny(skipped, ErrNoIOPressure)) {
		return false
	}
	if m.Disk != nil {
		if m.Disk.Open && !m.Disk.OpenCapped {
			return true
		}
		if m.Disk.Five.Capped != nil && m.Disk.Five.Capped.Capped {
			return false
		}
	}
	ioFull := m.IOFull
	if m.hasIOFullExact {
		ioFull = m.ioFullExact
	}
	return ioFull >= maxIOFull
}

// effectiveLimits — пороги правила с умолчаниями на месте нулевых: диск, процессор (число потоков машины),
// память.
func effectiveLimits(m Load, l Limits) (maxIOFull, runnable, memSome float64) {
	maxIOFull, runnable = l.MaxIOFull, l.Runnable
	if maxIOFull <= 0 {
		maxIOFull = DefaultLimits.MaxIOFull
	}
	runnable = l.RunnableLimit(m)
	return maxIOFull, runnable, l.MemSomeLimit()
}

// RunnableLimit — порог CPU после умолчаний и активного режима половины машины.
func (l Limits) RunnableLimit(m Load) float64 {
	runnable := l.Runnable
	if runnable <= 0 {
		runnable = float64(m.Threads)
	}
	if !m.HalfUntil.IsZero() {
		half := math.Max(1, float64(m.Threads)/2)
		if runnable > half {
			runnable = half
		}
	}
	return runnable
}

// MemSomeLimit — порог давления памяти: MaxMemSome, а 0 — DefaultMemSome.
func (l Limits) MemSomeLimit() float64 {
	if l.MaxMemSome <= 0 {
		return DefaultMemSome
	}
	return l.MaxMemSome
}

// WritebackLimit — порог выброса записи, MiB: свой MaxDirtyMB или порог фонового сброса машины m;
// 0 — порога нет (не задан и не прочитан), мера не судится.
func (l Limits) WritebackLimit(m Load) float64 {
	if l.MaxDirtyMB > 0 {
		return l.MaxDirtyMB
	}
	return m.DirtyLimitMB
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
	half     string // файл срока режима половины машины; пусто — HalfPath
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

// Check замеряет машину и применяет правило. fresh — обмен со свопом за l.SwapWindow (ждёт окно),
// кроме случая, когда источник диска ответил и давление памяти не выше порога: тогда своп не влияет
// на правило и не измеряется. Иначе обмен — с прошлого Check, а у первого — тоже за окно. Файл тихого
// режима читается первым, затем
// меры из procRoot: и в тихом режиме вердикт несёт все замеры. Ошибка — без вердикта: файл тихого
// режима не читается или не разбирается, мера есть, но не читается.
func (m *Meter) Check(ctx context.Context, l Limits, kind string, fresh bool) (Verdict, error) {
	var load Load
	var meminfo string // текст /proc/meminfo: из него и порог фонового сброса по доле
	var skipped []error
	quiet := m.quiet
	var err error
	if quiet == "" {
		if quiet, err = QuietPath(); err != nil {
			return Verdict{}, err
		}
	}
	note, err := activeQuiet(quiet, m.now())
	if err != nil {
		return Verdict{}, err
	}
	load.QuietUntil, load.QuietBy, load.QuietWhy = note.Until, note.By, note.Why
	halfPath := m.half
	if halfPath == "" {
		halfPath, err = HalfPath()
		if err != nil {
			return Verdict{}, err
		}
	}
	halfNote, err := activeQuiet(halfPath, m.now())
	if err != nil {
		return Verdict{}, err
	}
	load.HalfUntil, load.HalfBy, load.HalfWhy = halfNote.Until, halfNote.By, halfNote.Why
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
		measure(ErrNoLoadAvg, func() error {
			avg, err := ReadLoadAvg(m.procRoot)
			load.Avg1 = avg.Avg1
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
		measure(ErrNoMemInfo, func() error {
			info, err := m.read("meminfo", ErrNoMemInfo)
			if err != nil {
				return err
			}
			meminfo = string(info)
			load.DirtyMB, err = ParseDirty(meminfo)
			return err
		}),
		measure(ErrNoDirtyLimit, func() error {
			if l.MaxDirtyMB > 0 || meminfo == "" { // свой порог или нечего судить: порог машины не нужен
				return nil
			}
			var err error
			load.DirtyLimitMB, err = m.dirtyLimit(meminfo)
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
	if err := m.measureSwapAndDisk(ctx, l, kind, fresh, &load, &skipped); err != nil {
		return Verdict{}, err
	}
	return Rule(load, l, skipped), nil
}

func (m *Meter) measureSwapAndDisk(ctx context.Context, l Limits, kind string, fresh bool, load *Load, skipped *[]error) error {
	load.Disk, load.DiskFault = readDisk(ctx, m.socketPath(), kind, m.now())
	if fresh && load.Disk != nil && !skippedAny(*skipped, ErrNoMemPressure) && load.MemSome <= l.MemSomeLimit() {
		load.SwapUnmeasured = true
		if err := m.markBaseline(skipped); err != nil {
			return err
		}
	} else if err := m.measureSwapAndIO(ctx, l, fresh, load, skipped); err != nil {
		return err
	}
	if load.Disk != nil {
		load.ioFullExact = load.Disk.Five.IOFullMS / 50
		load.IOFull, load.IOWindow = math.Round(load.ioFullExact*10)/10, 5*time.Second
		load.hasIOFullExact = true
		if load.Disk.Five.Capped == nil {
			*skipped = append(*skipped, ErrNoCapJudgement)
		}
	} else {
		if load.IOWindow == 0 {
			load.IOWindow = time.Second
		}
		switch {
		case errors.Is(load.DiskFault, ErrNoIOSource):
			*skipped = append(*skipped, ErrNoIOSource)
		case load.DiskFault != nil:
			*skipped = append(*skipped, ErrIOSourceFailed)
		}
	}
	return nil
}

// markBaseline reads swapping and IO pressure counters without a window, so the next Check that is
// not fresh measures them since now, as it would after a measured window.
func (m *Meter) markBaseline(skipped *[]error) error {
	if err := m.readSwap(); errors.Is(err, ErrNoVMStat) {
		*skipped = append(*skipped, ErrNoVMStat)
	} else if err != nil {
		return err
	}
	total, valid, err := m.readIOTotal()
	if err != nil {
		return err
	}
	m.ioTotal, m.ioAt, m.ioValid = total, m.now(), valid
	if !valid {
		*skipped = append(*skipped, ErrNoIOPressure)
	}
	return nil
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

// meminfoKB — поля names из текста /proc/meminfo, kB. Нет поля, поле дважды или не целое число kB — ошибка.
func meminfoKB(meminfo string, names ...string) (map[string]uint64, error) {
	kb := map[string]uint64{}
	for line := range strings.SplitSeq(meminfo, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || !slices.Contains(names, name) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return nil, fmt.Errorf("meminfo %s: want `<number> kB`, got %q", name, strings.TrimSpace(rest))
		}
		if _, twice := kb[name]; twice {
			return nil, fmt.Errorf("meminfo: %s twice", name)
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("meminfo %s: %w", name, err)
		}
		kb[name] = v
	}
	for _, name := range names {
		if _, ok := kb[name]; !ok {
			return nil, fmt.Errorf("meminfo: no %s line", name)
		}
	}
	return kb, nil
}

// ParseDirty — грязные и пишущиеся на диск страницы из текста /proc/meminfo, MiB: Dirty+Writeback.
func ParseDirty(meminfo string) (float64, error) {
	kb, err := meminfoKB(meminfo, "Dirty", "Writeback")
	return float64(kb["Dirty"]+kb["Writeback"]) / 1024, err
}

// dirtyableMB — память, от которой ядро берёт долю dirty_background_ratio (global_dirtyable_memory в
// mm/page-writeback.c): свободная и файловая, MiB. Резерва ядра (totalreserve_pages) в meminfo нет, и он
// не вычтен: порог выходит выше ядерного на его долю.
func dirtyableMB(meminfo string) (float64, error) {
	kb, err := meminfoKB(meminfo, "MemFree", "Active(file)", "Inactive(file)")
	return float64(kb["MemFree"]+kb["Active(file)"]+kb["Inactive(file)"]) / 1024, err
}

// dirtyLimit — порог фонового сброса машины, MiB (Load.DirtyLimitMB): dirty_background_bytes, при 0 —
// dirty_background_ratio % от памяти dirtyableMB текста meminfo. Нет файла или оба 0 — ErrNoDirtyLimit.
// Это приближение порога ядра: резерв страниц не вычтен (dirtyableMB), и не повторено ядерное снижение
// фонового порога до половины общего, когда фоновый не ниже общего (dirty_bytes, dirty_ratio) —
// такая настройка ошибочна сама по себе.
func (m *Meter) dirtyLimit(meminfo string) (float64, error) {
	read := func(name string) (uint64, error) {
		data, err := m.read(filepath.Join("sys", "vm", name), ErrNoDirtyLimit)
		if err != nil {
			return 0, err
		}
		v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("vm.%s: %w", name, err)
		}
		return v, nil
	}
	bytes, err := read("dirty_background_bytes")
	if err != nil || bytes > 0 {
		return float64(bytes) / (1 << 20), err
	}
	ratio, err := read("dirty_background_ratio")
	if err != nil {
		return 0, err
	}
	if ratio == 0 {
		return 0, ErrNoDirtyLimit
	}
	dirtyable, err := dirtyableMB(meminfo)
	if err != nil {
		return 0, err
	}
	if dirtyable == 0 {
		return 0, ErrNoDirtyLimit
	}
	return float64(ratio) / 100 * dirtyable, nil
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
