package pressure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const stat = "cpu  1 2 3\ncpu0 1 2 3\ncpu1 1 2 3\ncpu2 1 2 3\nintr 1 2 3\nprocs_running 4\nprocs_blocked 0\n"

func TestParseStat(t *testing.T) {
	if r, n, err := ParseStat(stat); err != nil || r != 3 || n != 3 {
		t.Errorf("ParseStat = %d, %d, %v; ждали 3 готовых без себя и 3 потока", r, n, err)
	}
	if r, _, err := ParseStat("cpu0 1\nprocs_running 0\n"); err != nil || r != 0 {
		t.Errorf("procs_running 0 = %d, %v", r, err)
	}
	if _, _, err := ParseStat("cpu0 1\n"); err == nil {
		t.Error("нет procs_running — ждали ошибку")
	}
}

func TestRule(t *testing.T) {
	l := DefaultLimits
	cases := []struct {
		name    string
		m       Load
		l       Limits
		skipped []error
		want    string
	}{
		{"порог — число потоков", Load{Runnable: 22, Threads: 22}, l, nil, CPU},
		{"ниже потоков", Load{Runnable: 21, Threads: 22}, l, nil, ""},
		{"свой порог", Load{Runnable: 10, Threads: 22}, Limits{Runnable: 10, MaxIOFull: 10, MaxSwapKBs: 1024}, nil, CPU},
		{"диск", Load{Runnable: 1, Threads: 22, IOFull: 10.5}, l, nil, IO},
		{"диск на пороге", Load{Runnable: 1, Threads: 22, IOFull: 9.6}, l, nil, IO},
		{"диск ниже порога", Load{Runnable: 1, Threads: 22, IOFull: 9.5}, l, nil, ""},
		{"замороженный диск", Load{Runnable: 1, Threads: 22, Disk: &DiskNow{Open: true}}, l, nil, IO},
		{"диск выключен при заморозке", Load{Runnable: 1, Threads: 22, IOFull: 50, Disk: &DiskNow{Open: true}}, Limits{MaxIOFull: 100}, nil, ""},
		{"диск выключен по давлению", Load{Runnable: 1, Threads: 22, IOFull: 50}, Limits{MaxIOFull: 100}, nil, ""},
		{"обмен без давления памяти", Load{Runnable: 1, Threads: 22, MemSome: 0.4, SwapKBs: 5000}, l, nil, ""},
		{"давление памяти с обменом", Load{Runnable: 1, Threads: 22, MemSome: 12, SwapKBs: 3000}, l, nil, Swap},
		{"давление памяти без обмена", Load{Runnable: 1, Threads: 22, MemSome: 12}, l, nil, ""},
		{"порог памяти 0 — по умолчанию", Load{Runnable: 1, Threads: 22, MemSome: 9, SwapKBs: 3000}, Limits{MaxSwapKBs: 1024}, nil, ""},
		{"свой порог памяти", Load{Runnable: 1, Threads: 22, MemSome: 6, SwapKBs: 3000}, Limits{MaxMemSome: 5, MaxSwapKBs: 1024}, nil, Swap},
		{"нет PSI памяти", Load{Runnable: 1, Threads: 22, MemSome: 50, SwapKBs: 3000}, l, []error{ErrNoMemPressure}, ""},
		{"процессор раньше диска", Load{Runnable: 30, Threads: 22, IOFull: 50}, l, nil, CPU},
		{"пропущенная мера не судится", Load{Threads: 22, IOFull: 50}, l, []error{ErrNoIOPressure}, ""},
		{"без потоков и порога", Load{Runnable: 5}, l, []error{ErrNoRunnable}, ""},
		{"LA за минуту на пороге при моментальном ниже", Load{Runnable: 10, Threads: 22, Avg1: 22}, l, nil, CPU},
		{"LA за минуту и моментальное ниже", Load{Runnable: 21, Threads: 22, Avg1: 21.99}, l, nil, ""},
		{"LA за минуту — свой порог", Load{Runnable: 1, Threads: 22, Avg1: 10}, Limits{Runnable: 10, MaxIOFull: 10, MaxSwapKBs: 1024}, nil, CPU},
		{"нет loadavg — LA не судится", Load{Runnable: 1, Threads: 22, Avg1: 90}, l, []error{ErrNoLoadAvg}, ""},
		{"выброс записи на пороге машины", Load{Runnable: 1, Threads: 22, DirtyMB: 64, DirtyLimitMB: 64}, l, nil, Writeback},
		{"запись ниже порога машины", Load{Runnable: 1, Threads: 22, DirtyMB: 63.9, DirtyLimitMB: 64}, l, nil, ""},
		{"большой порог машины", Load{Runnable: 1, Threads: 22, DirtyMB: 250, DirtyLimitMB: 2000}, l, nil, ""},
		{"свой порог записи выше машины", Load{Runnable: 1, Threads: 22, DirtyMB: 100, DirtyLimitMB: 64}, Limits{MaxDirtyMB: 128, MaxSwapKBs: 1024}, nil, ""},
		{"свой порог записи без порога машины", Load{Runnable: 1, Threads: 22, DirtyMB: 130}, Limits{MaxDirtyMB: 128, MaxSwapKBs: 1024}, nil, Writeback},
		{"порога нет — запись не судится", Load{Runnable: 1, Threads: 22, DirtyMB: 500}, l, []error{ErrNoDirtyLimit}, ""},
		{"нет meminfo — запись не судится", Load{Runnable: 1, Threads: 22, DirtyMB: 500, DirtyLimitMB: 64}, l, []error{ErrNoMemInfo}, ""},
		{"диск раньше записи", Load{Runnable: 1, Threads: 22, IOFull: 50, DirtyMB: 500, DirtyLimitMB: 64}, l, nil, IO},
		{"запись раньше свопа", Load{Runnable: 1, Threads: 22, DirtyMB: 500, DirtyLimitMB: 64, MemSome: 12, SwapKBs: 3000}, l, nil, Writeback},
	}
	for _, c := range cases {
		v := Rule(c.m, c.l, c.skipped)
		if v.Measure != c.want || v.Busy() != (c.want != "") {
			t.Errorf("%s: мера %q, ждали %q", c.name, v.Measure, c.want)
		}
	}
	if why := Rule(Load{Runnable: 31, Threads: 22}, l, nil).Why(); why != "runnable 31 is at or above 22" {
		t.Errorf("Why = %q", why)
	}
	if why := Rule(Load{Runnable: 31, Threads: 22, Avg1: 40}, l, nil).Why(); why != "runnable 31 is at or above 22" {
		t.Errorf("Why при обоих выше = %q; ждали моментальное", why)
	}
	if why := Rule(Load{Runnable: 10, Threads: 22, Avg1: 26.6}, l, nil).Why(); why != "load average 1m 26.60 is at or above 22" {
		t.Errorf("Why = %q", why)
	}
	if why := Rule(Load{Threads: 22, MemSome: 12.5, SwapKBs: 3000}, l, nil).Why(); why != "memory pressure 12.5% is above 10% while swapping 3000 KB/s" {
		t.Errorf("Why = %q", why)
	}
	if why := Rule(Load{Threads: 22, DirtyMB: 130.4, DirtyLimitMB: 64}, l, nil).Why(); why != "write-out in progress: dirty and writeback memory 130 MiB is at or above 64 MiB" {
		t.Errorf("Why = %q", why)
	}
}

func TestHalfRule(t *testing.T) {
	until := time.Date(2026, 11, 12, 21, 30, 0, 0, time.Local)
	cases := []struct {
		name   string
		load   Load
		limits Limits
		want   string
	}{
		{"above half is busy", Load{Runnable: 5, Threads: 8, HalfUntil: until, HalfBy: "lead", HalfWhy: "игра"}, DefaultLimits, CPU},
		{"below half is free", Load{Runnable: 3, Threads: 8, HalfUntil: until}, Limits{MaxIOFull: 100}, ""},
		{"half is at least one", Load{Runnable: 1, Threads: 1, HalfUntil: until}, Limits{MaxIOFull: 100}, CPU},
		{"zero tasks remains below minimum half threshold", Load{Runnable: 0, Threads: 1, HalfUntil: until}, Limits{MaxIOFull: 100}, ""},
		{"lower configured threshold wins", Load{Runnable: 2, Threads: 8, HalfUntil: until}, Limits{Runnable: 2, MaxIOFull: 100}, CPU},
		{"higher configured threshold is capped", Load{Runnable: 5, Threads: 8, HalfUntil: until}, Limits{Runnable: 6, MaxIOFull: 100}, CPU},
	}
	for _, c := range cases {
		v := Rule(c.load, c.limits, nil)
		if v.Measure != c.want {
			t.Errorf("%s: measure %q, want %q", c.name, v.Measure, c.want)
		}
	}
	v := Rule(cases[0].load, DefaultLimits, nil)
	if got := v.Why(); !strings.Contains(got, "half until 21:30 (lead: игра)") {
		t.Errorf("half reason = %q", got)
	}
	// Load average использует тот же порог half.
	if v := Rule(Load{Runnable: 1, Threads: 8, Avg1: 4, HalfUntil: until}, Limits{MaxIOFull: 100}, nil); v.Measure != CPU || !strings.Contains(v.Why(), "half until") {
		t.Errorf("half load average = %+v, %q", v, v.Why())
	}
	// Дробный load average проверяет минимум в один поток на однопоточной машине.
	if v := Rule(Load{Threads: 1, Avg1: 0.75, HalfUntil: until}, Limits{MaxIOFull: 100}, nil); v.Busy() {
		t.Errorf("load average below minimum threshold = %+v", v)
	}
	// При обоих активных сроках тихий режим главнее.
	if v := Rule(Load{Runnable: 5, Threads: 8, QuietUntil: until, HalfUntil: until}, DefaultLimits, nil); v.Measure != Quiet {
		t.Errorf("both modes = %q; want quiet", v.Measure)
	}
}

func TestParseDirty(t *testing.T) {
	if mb, err := ParseDirty("MemTotal:  100 kB\nDirty:     65536 kB\nWriteback:  1024 kB\nWritebackTmp: 9999 kB\n"); err != nil || mb != 65 {
		t.Errorf("ParseDirty = %v, %v; ждали 65 MiB (WritebackTmp не в счёт)", mb, err)
	}
	for _, bad := range []string{"Dirty: 1 kB\n", "Writeback: 1 kB\n", "Dirty: x kB\nWriteback: 1 kB\n", "Dirty: 1 MB\nWriteback: 1 kB\n", "",
		"Dirty: NaN kB\nWriteback: 1 kB\n", "Dirty: Inf kB\nWriteback: 1 kB\n", "Dirty: -5 kB\nWriteback: 1 kB\n", "Dirty: 1.5 kB\nWriteback: 1 kB\n",
		"Dirty: 1 kB\nWriteback: 1 kB\nDirty: 1 kB\n"} {
		if _, err := ParseDirty(bad); err == nil {
			t.Errorf("ParseDirty(%q) — ждали ошибку", bad)
		}
	}
	// Доля порога — от свободной и файловой памяти, как у ядра; MemAvailable и анонимная не в счёт.
	if mb, err := dirtyableMB("MemFree: 1024 kB\nMemAvailable: 999999 kB\nActive(anon): 999999 kB\nActive(file): 2048 kB\nInactive(file): 1024 kB\n"); err != nil || mb != 4 {
		t.Errorf("dirtyableMB = %v, %v; ждали 4 MiB", mb, err)
	}
	if _, err := dirtyableMB("MemFree: 1 kB\nActive(file): 1 kB\n"); err == nil {
		t.Error("нет Inactive(file) — ждали ошибку")
	}
}

// fakeProc — /proc в папке, часы и сон без ожидания.
func fakeProc(t *testing.T, files map[string]string) (*Meter, *time.Time) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := t.TempDir()
	for name, body := range files {
		writeProc(t, root, name, body)
	}
	now := time.Unix(1000, 0)
	m := NewMeter(root)
	m.quiet = filepath.Join(root, "quiet-until")
	m.half = filepath.Join(root, "half-until")
	m.now = func() time.Time { return now }
	m.sleep = func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		path := filepath.Join(root, "pressure", "io")
		if data, err := os.ReadFile(path); err == nil {
			updated := strings.Replace(string(data), "full avg10=0.5 avg60=12.5 avg300=1 total=1", "full avg10=0.5 avg60=12.5 avg300=1 total=96001", 1)
			_ = os.WriteFile(path, []byte(updated), 0o644)
		}
		return nil
	}
	return m, &now
}

func writeProc(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	m, now := fakeProc(t, map[string]string{
		"stat":            stat,
		"pressure/io":     "some avg10=1 avg60=2 avg300=3 total=1\nfull avg10=0.5 avg60=12.5 avg300=1 total=1\n",
		"pressure/cpu":    "some avg10=40.00 avg60=30 avg300=20 total=1\n",
		"pressure/memory": "some avg10=12.50 avg60=3 avg300=1 total=1\nfull avg10=5 avg60=1 avg300=0 total=1\n",
		"vmstat":          "pswpin 10\npswpout 20\n",
	})
	v, err := m.Check(context.Background(), DefaultLimits, "", true)
	if err != nil {
		t.Fatal(err)
	}
	want := Load{Runnable: 3, Threads: 3, CPUSome: 40, IOFull: 9.6, IOWindow: time.Second, ioFullExact: 9.6, hasIOFullExact: true, MemSome: 12.5}
	got := v.Load
	got.DiskFault = nil
	if v.Measure != CPU || got != want || !slices.ContainsFunc(v.Skipped, func(e error) bool { return errors.Is(e, ErrNoIOSource) }) {
		t.Errorf("Check = %+v; ждали занята по cpu, %+v", v, want)
	}
	// Своп с прошлого замера, без ожидания окна: 512 страниц за 1 с.
	writeProc(t, m.procRoot, "vmstat", "pswpin 10\npswpout 532\n")
	*now = now.Add(time.Second)
	v, err = m.Check(context.Background(), Limits{Runnable: 100, MaxIOFull: 50, MaxSwapKBs: 1}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if kbs := float64(512*os.Getpagesize()) / 1024; v.Measure != Swap || v.Load.SwapKBs != kbs {
		t.Errorf("своп = %+v; ждали %v KB/s", v, kbs)
	}
}

func TestCheckWriteback(t *testing.T) {
	// Ранний сброс байтами: 64 MiB.
	m, _ := fakeProc(t, map[string]string{"stat": stat, "meminfo": "Dirty: 60000 kB\nWriteback: 10000 kB\n",
		"sys/vm/dirty_background_bytes": "67108864\n", "sys/vm/dirty_background_ratio": "0\n"})
	l := Limits{Runnable: 100, MaxIOFull: 100, MaxSwapKBs: 1024}
	v, err := m.Check(context.Background(), l, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Measure != Writeback || v.Load.DirtyMB != 70000.0/1024 || v.Limit != 64 || slices.Contains(v.Skipped, ErrNoMemInfo) {
		t.Errorf("Dirty+Writeback 68 MiB при сбросе с 64 MiB = %+v; ждали занята по writeback", v)
	}
	writeProc(t, m.procRoot, "meminfo", "Dirty: 1000 kB\nWriteback: 0 kB\n")
	if v, err = m.Check(context.Background(), l, "", true); err != nil || v.Busy() {
		t.Errorf("1 MiB = %+v, %v; ждали свободна", v, err)
	}
	// Настройки ядра по умолчанию: 10% от 20000 MiB свободной и файловой — 2000 MiB; 205 MiB не выброс.
	writeProc(t, m.procRoot, "sys/vm/dirty_background_bytes", "0\n")
	writeProc(t, m.procRoot, "sys/vm/dirty_background_ratio", "10\n")
	const dirty205 = "Dirty: 200000 kB\nWriteback: 10000 kB\n"
	writeProc(t, m.procRoot, "meminfo", "MemFree: 4096000 kB\nActive(file): 8192000 kB\nInactive(file): 8192000 kB\n"+dirty205)
	if v, err = m.Check(context.Background(), l, "", true); err != nil || v.Busy() || v.Load.DirtyLimitMB != 2000 {
		t.Errorf("205 MiB при сбросе с 10%% от 20000 MiB = %+v, %v; ждали свободна и порог 2000", v, err)
	}
	writeProc(t, m.procRoot, "meminfo", "MemFree: 1024000 kB\nActive(file): 512000 kB\nInactive(file): 512000 kB\n"+dirty205)
	if v, err = m.Check(context.Background(), l, "", true); err != nil || v.Measure != Writeback || v.Limit != 200 {
		t.Errorf("205 MiB при сбросе с 10%% от 2000 MiB = %+v, %v; ждали занята, порог 200", v, err)
	}
	writeProc(t, m.procRoot, "meminfo", "MemFree: 0 kB\nActive(file): 0 kB\nInactive(file): 0 kB\n"+dirty205)
	if v, err = m.Check(context.Background(), l, "", true); err != nil || v.Busy() || !slices.Contains(v.Skipped, ErrNoDirtyLimit) {
		t.Errorf("доля от нулевой памяти = %+v, %v; ждали свободна, порог пропущен с пометкой", v, err)
	}
	writeProc(t, m.procRoot, "meminfo", "MemFree: 1024000 kB\n"+dirty205)
	if _, err = m.Check(context.Background(), l, "", true); err == nil {
		t.Error("доля без файловой памяти в meminfo — ждали ошибку")
	}
	writeProc(t, m.procRoot, "meminfo", dirty205)
	writeProc(t, m.procRoot, "sys/vm/dirty_background_ratio", "0\n")
	if v, err = m.Check(context.Background(), l, "", true); err != nil || v.Busy() || !slices.Contains(v.Skipped, ErrNoDirtyLimit) {
		t.Errorf("оба порога 0 = %+v, %v; ждали свободна, порог пропущен с пометкой", v, err)
	}
	writeProc(t, m.procRoot, "sys/vm/dirty_background_ratio", "ten\n")
	if _, err = m.Check(context.Background(), l, "", true); err == nil {
		t.Error("нечисловой dirty_background_ratio — ждали ошибку")
	}
	if err := os.Remove(filepath.Join(m.procRoot, "sys", "vm", "dirty_background_ratio")); err != nil {
		t.Fatal(err)
	}
	if v, err = m.Check(context.Background(), l, "", true); err != nil || v.Busy() || !slices.Contains(v.Skipped, ErrNoDirtyLimit) {
		t.Errorf("байты 0 без файла доли = %+v, %v; ждали свободна, порог пропущен с пометкой", v, err)
	}
	writeProc(t, m.procRoot, "sys/vm/dirty_background_bytes", "67108864\n")
	if v, err = m.Check(context.Background(), l, "", true); err != nil || v.Measure != Writeback {
		t.Errorf("байтовый порог без файла доли и без MemFree = %+v, %v; ждали занята: доля не нужна", v, err)
	}
	// Свой порог — без порога машины, даже если его не прочесть.
	if err := os.RemoveAll(filepath.Join(m.procRoot, "sys")); err != nil {
		t.Fatal(err)
	}
	if v, err = m.Check(context.Background(), l, "", true); err != nil || v.Busy() || !slices.Contains(v.Skipped, ErrNoDirtyLimit) {
		t.Errorf("без /proc/sys/vm = %+v, %v; ждали свободна, порог пропущен", v, err)
	}
	own := l
	own.MaxDirtyMB = 100
	if v, err = m.Check(context.Background(), own, "", true); err != nil || v.Measure != Writeback || slices.Contains(v.Skipped, ErrNoDirtyLimit) {
		t.Errorf("свой порог 100 без /proc/sys/vm = %+v, %v; ждали занята, без пометки о пороге машины", v, err)
	}
	writeProc(t, m.procRoot, "sys/vm/dirty_background_bytes", "lots\n")
	if _, err = m.Check(context.Background(), l, "", true); err == nil {
		t.Error("нечисловой dirty_background_bytes — ждали ошибку")
	}
	writeProc(t, m.procRoot, "meminfo", "MemTotal: 1 kB\n")
	if _, err = m.Check(context.Background(), own, "", true); err == nil {
		t.Error("meminfo без Dirty — ждали ошибку, а не пропуск меры")
	}
}

func TestCheckLoadAvg(t *testing.T) {
	m, _ := fakeProc(t, map[string]string{"stat": stat, "loadavg": "6.00 2.00 1.00 2/100 1\n"})
	l := Limits{Runnable: 5, MaxIOFull: 100, MaxSwapKBs: 1024}
	v, err := m.Check(context.Background(), l, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Measure != CPU || v.Value != 6 || v.Load.Avg1 != 6 || v.Load.Runnable >= 5 {
		t.Errorf("LA 6 при пороге 5 и моментальном %d = %+v; ждали занята по cpu с LA", v.Load.Runnable, v)
	}
	writeProc(t, m.procRoot, "loadavg", "4.99 9.00 9.00 2/100 1\n")
	if v, err = m.Check(context.Background(), l, "", true); err != nil || v.Busy() {
		t.Errorf("LA 4.99 при пороге 5 = %+v, %v; ждали свободна (5 и 15 минут не судятся)", v, err)
	}
}

func TestCheckNoProc(t *testing.T) {
	m, _ := fakeProc(t, nil)
	v, err := m.Check(context.Background(), DefaultLimits, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Busy() || !slices.Contains(v.Skipped, ErrNoRunnable) || !slices.Contains(v.Skipped, ErrNoLoadAvg) || !slices.Contains(v.Skipped, ErrNoIOPressure) || !slices.Contains(v.Skipped, ErrNoMemPressure) || !slices.Contains(v.Skipped, ErrNoVMStat) || !slices.Contains(v.Skipped, ErrNoMemInfo) || slices.Contains(v.Skipped, ErrNoDirtyLimit) || !slices.ContainsFunc(v.Skipped, func(e error) bool { return errors.Is(e, ErrNoIOSource) }) {
		t.Errorf("без /proc = %+v; ждали свободна и все меры пропущены, порог записи без meminfo не читается", v)
	}
}

func TestCheckBroken(t *testing.T) {
	m, _ := fakeProc(t, map[string]string{"stat": "cpu0 1\n", "vmstat": "pswpin 1\npswpout 1\n"})
	if _, err := m.Check(context.Background(), DefaultLimits, "", true); err == nil {
		t.Error("stat без procs_running — ждали ошибку, а не пропуск меры")
	}
	writeProc(t, m.procRoot, "stat", stat)
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // Check без Meter читает файл тихого режима из XDG_STATE_HOME
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Check(ctx, m.procRoot, DefaultLimits, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("отменённый контекст: %v", err)
	}
}

func TestLoadAvg(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadLoadAvg(root); !errors.Is(err, ErrNoLoadAvg) {
		t.Errorf("нет файла: %v", err)
	}
	writeProc(t, root, "loadavg", "1.50 2.25 3.00 4/1234 5678\n")
	la, err := ReadLoadAvg(root)
	if want := (LoadAvg{1.5, 2.25, 3, 4, 1234}); err != nil || la != want {
		t.Errorf("ReadLoadAvg = %+v, %v; ждали %+v", la, err, want)
	}
	for _, bad := range []string{"", "1 2 3", "1 2 x 4/5 6", "1 2 3 45 6", "1 2 3 a/5 6", "1 2 3 4/b 6"} {
		if _, err := ParseLoadAvg(bad); err == nil {
			t.Errorf("%q — ждали ошибку", bad)
		}
	}
}
