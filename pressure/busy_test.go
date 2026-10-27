package pressure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
		{"диск на пороге", Load{Runnable: 1, Threads: 22, IOFull: 10}, l, nil, ""},
		{"обмен без давления памяти", Load{Runnable: 1, Threads: 22, MemSome: 0.4, SwapKBs: 5000}, l, nil, ""},
		{"давление памяти с обменом", Load{Runnable: 1, Threads: 22, MemSome: 12, SwapKBs: 3000}, l, nil, Swap},
		{"давление памяти без обмена", Load{Runnable: 1, Threads: 22, MemSome: 12}, l, nil, ""},
		{"порог памяти 0 — по умолчанию", Load{Runnable: 1, Threads: 22, MemSome: 9, SwapKBs: 3000}, Limits{MaxSwapKBs: 1024}, nil, ""},
		{"свой порог памяти", Load{Runnable: 1, Threads: 22, MemSome: 6, SwapKBs: 3000}, Limits{MaxMemSome: 5, MaxSwapKBs: 1024}, nil, Swap},
		{"нет PSI памяти", Load{Runnable: 1, Threads: 22, MemSome: 50, SwapKBs: 3000}, l, []error{ErrNoMemPressure}, ""},
		{"процессор раньше диска", Load{Runnable: 30, Threads: 22, IOFull: 50}, l, nil, CPU},
		{"пропущенная мера не судится", Load{Threads: 22, IOFull: 50}, l, []error{ErrNoIOPressure}, ""},
		{"без потоков и порога", Load{Runnable: 5}, l, []error{ErrNoRunnable}, ""},
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
	if why := Rule(Load{Threads: 22, MemSome: 12.5, SwapKBs: 3000}, l, nil).Why(); why != "memory pressure 12.5% is above 10% while swapping 3000 KB/s" {
		t.Errorf("Why = %q", why)
	}
}

// fakeProc — /proc в папке, часы и сон без ожидания.
func fakeProc(t *testing.T, files map[string]string) (*Meter, *time.Time) {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		writeProc(t, root, name, body)
	}
	now := time.Unix(1000, 0)
	m := NewMeter(root)
	m.now = func() time.Time { return now }
	m.sleep = func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
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
	v, err := m.Check(context.Background(), DefaultLimits, true)
	if err != nil {
		t.Fatal(err)
	}
	want := Load{Runnable: 3, Threads: 3, CPUSome: 40, IOFull: 12.5, MemSome: 12.5}
	if v.Measure != CPU || v.Load != want || v.Skipped != nil {
		t.Errorf("Check = %+v; ждали занята по cpu, %+v", v, want)
	}
	// Своп с прошлого замера, без ожидания окна: 512 страниц за 1 с.
	writeProc(t, m.procRoot, "vmstat", "pswpin 10\npswpout 532\n")
	*now = now.Add(time.Second)
	v, err = m.Check(context.Background(), Limits{Runnable: 100, MaxIOFull: 50, MaxSwapKBs: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if kbs := float64(512*os.Getpagesize()) / 1024; v.Measure != Swap || v.Load.SwapKBs != kbs {
		t.Errorf("своп = %+v; ждали %v KB/s", v, kbs)
	}
}

func TestCheckNoProc(t *testing.T) {
	m, _ := fakeProc(t, nil)
	v, err := m.Check(context.Background(), DefaultLimits, true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Busy() || !slices.Equal(v.Skipped, []error{ErrNoRunnable, ErrNoIOPressure, ErrNoMemPressure, ErrNoVMStat}) {
		t.Errorf("без /proc = %+v; ждали свободна и все меры пропущены", v)
	}
}

func TestCheckBroken(t *testing.T) {
	m, _ := fakeProc(t, map[string]string{"stat": "cpu0 1\n", "vmstat": "pswpin 1\npswpout 1\n"})
	if _, err := m.Check(context.Background(), DefaultLimits, true); err == nil {
		t.Error("stat без procs_running — ждали ошибку, а не пропуск меры")
	}
	writeProc(t, m.procRoot, "stat", stat)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Check(ctx, m.procRoot, DefaultLimits); !errors.Is(err, context.Canceled) {
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
