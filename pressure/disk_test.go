package pressure

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func socket(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ios")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "io-state.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			serve(conn)
		}
	}()
	return path
}

func response(at time.Time, ioFullMS float64) string {
	return fmt.Sprintf(`{"v":1,"at":%q,"5s":{"io_full_ms":%v},"profile": { "mode" : "x" }}`+"\n", at.Format(time.RFC3339Nano), ioFullMS)
}

func TestReadDiskRequestAndProfile(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	var request diskRequest
	path := socket(t, func(conn net.Conn) {
		defer conn.Close()
		_ = json.NewDecoder(bufio.NewReader(conn)).Decode(&request)
		_, _ = conn.Write([]byte(response(now, 480)))
	})
	disk, err := readDisk(context.Background(), path, "worker:x", now)
	if err != nil {
		t.Fatal(err)
	}
	if request.V != 1 || request.Q != "now" || request.Kind != "worker:x" {
		t.Errorf("запрос = %+v", request)
	}
	if disk.Five.IOFullMS != 480 || string(disk.Profile) != `{ "mode" : "x" }` || !disk.At.Equal(now) {
		t.Errorf("снимок = %+v, profile %s", disk, disk.Profile)
	}
}

func TestReadDiskOmitsEmptyKind(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	now := time.Now()
	var request map[string]json.RawMessage
	path := socket(t, func(conn net.Conn) {
		defer conn.Close()
		_ = json.NewDecoder(bufio.NewReader(conn)).Decode(&request)
		_, _ = conn.Write([]byte(response(now, 0)))
	})
	if _, err := readDisk(context.Background(), path, "", now); err != nil {
		t.Fatal(err)
	}
	if _, ok := request["kind"]; ok {
		t.Errorf("пустой kind попал в запрос: %s", request["kind"])
	}
}

func TestReadDiskFailures(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	now := time.Now()
	cases := []struct {
		name, body string
	}{
		{"version", `{"v":2,"at":"` + now.Format(time.RFC3339Nano) + `"}`},
		{"error", `{"v":1,"error":"failed","at":"` + now.Format(time.RFC3339Nano) + `"}`},
		{"stale", `{"v":1,"at":"` + now.Add(-3*time.Second).Format(time.RFC3339Nano) + `"}`},
		{"invalid JSON", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := socket(t, func(conn net.Conn) {
				defer conn.Close()
				_, _ = conn.Write([]byte(tc.body + "\n"))
			})
			if _, err := readDisk(context.Background(), path, "", now); !errors.Is(err, ErrIOSourceFailed) {
				t.Errorf("ошибка источника = %v", err)
			}
		})
	}
}

func TestReadDiskMissingDefaultRuntime(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := readDisk(context.Background(), diskPath(), "", time.Now()); !errors.Is(err, ErrNoIOSource) {
		t.Errorf("путь по умолчанию без XDG_RUNTIME_DIR: %v", err)
	}
}

func TestMeterDiskWindowAndKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		ms   float64
		want string
	}{
		{"busy at threshold", 480, IO},
		{"free below threshold", 479, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", "")
			m, now := fakeProc(t, map[string]string{
				"stat": stat, "pressure/io": "full avg10=0 avg60=0 avg300=0 total=1\n",
				"pressure/memory": "some avg10=0 avg60=0 avg300=0 total=1\n",
				"vmstat":          "pswpin 1\npswpout 1\n",
			})
			var request diskRequest
			m.disk = socket(t, func(conn net.Conn) {
				defer conn.Close()
				_ = json.NewDecoder(bufio.NewReader(conn)).Decode(&request)
				_, _ = conn.Write([]byte(response(*now, tc.ms)))
			})
			v, err := m.Check(context.Background(), Limits{Runnable: 100, MaxIOFull: 9.6, MaxMemSome: 10}, "worker:x", true)
			if err != nil {
				t.Fatal(err)
			}
			if v.Measure != tc.want || v.Load.IOWindow != 5*time.Second {
				t.Errorf("Check = %+v, ждали меру %q и окно 5s", v, tc.want)
			}
			if tc.ms == 480 && v.Why() != "IO pressure 9.6% of the last 5s is at or above 9.6%" {
				t.Errorf("Why = %q", v.Why())
			}
			if request.Kind != "worker:x" {
				t.Errorf("kind = %q", request.Kind)
			}
			if string(v.Load.Disk.Profile) != `{ "mode" : "x" }` {
				t.Errorf("profile = %s", v.Load.Disk.Profile)
			}
		})
	}
}

func TestMeterOmitsEmptyKind(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	m, now := fakeProc(t, map[string]string{
		"stat": stat, "pressure/io": "full avg10=0 avg60=0 avg300=0 total=1\n",
		"pressure/memory": "some avg10=0 avg60=0 avg300=0 total=1\n",
		"vmstat":          "pswpin 1\npswpout 1\n",
	})
	var request map[string]json.RawMessage
	m.disk = socket(t, func(conn net.Conn) {
		defer conn.Close()
		_ = json.NewDecoder(bufio.NewReader(conn)).Decode(&request)
		_, _ = conn.Write([]byte(response(*now, 0)))
	})
	v, err := m.Check(context.Background(), Limits{Runnable: 100, MaxIOFull: 9.6}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := request["kind"]; exists {
		t.Errorf("пустой kind попал в запрос: %s", request["kind"])
	}
	if string(v.Load.Disk.Profile) != `{ "mode" : "x" }` {
		t.Errorf("profile = %s", v.Load.Disk.Profile)
	}
}

func TestMeterDiskOpenRule(t *testing.T) {
	open := &DiskNow{Open: true, OpenMs: 37}
	limits := Limits{MaxIOFull: 9.6}
	if v := Rule(Load{Disk: open}, limits, nil); v.Measure != IO || v.Why() != "disk freeze in progress for 37 ms" {
		t.Errorf("Open = %+v", v)
	}
	limits.MaxIOFull = 100
	if v := Rule(Load{Disk: open, IOFull: 50}, limits, nil); v.Busy() {
		t.Errorf("проверка выключена, verdict = %+v", v)
	}
}

func TestMeterDiskFallbackWithoutVMStat(t *testing.T) {
	m, _ := fakeProc(t, map[string]string{
		"stat": stat, "pressure/io": "full avg10=0.5 avg60=12.5 avg300=1 total=1\n",
		"pressure/memory": "some avg10=0 avg60=0 avg300=0 total=1\n",
	})
	v, err := m.Check(context.Background(), Limits{Runnable: 100, MaxIOFull: 9.6, SwapWindow: time.Second}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Load.IOFull != 9.6 || v.Load.IOWindow != time.Second || v.Measure != IO || !containsError(v.Skipped, ErrNoIOSource) {
		t.Errorf("fallback без vmstat = %+v", v)
	}
}

func TestMeterMissingSourceSentinelAndFallback(t *testing.T) {
	m, _ := fakeProc(t, map[string]string{
		"stat": stat, "pressure/io": "full avg10=0 avg60=0 avg300=0 total=1\n",
		"pressure/memory": "some avg10=0 avg60=0 avg300=0 total=1\n",
	})
	m.disk = filepath.Join(t.TempDir(), "missing.sock")
	for i := range 2 {
		v, err := m.Check(context.Background(), Limits{Runnable: 100, MaxIOFull: 9.6}, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if !containsError(v.Skipped, ErrNoIOSource) {
			t.Fatalf("Skipped = %v", v.Skipped)
		}
		var sentinel error
		for _, skipped := range v.Skipped {
			if errors.Is(skipped, ErrNoIOSource) {
				sentinel = skipped
				if skipped != ErrNoIOSource {
					t.Errorf("Skipped содержит обёртку: %v", skipped)
				}
			}
		}
		if i == 1 && sentinel != ErrNoIOSource {
			t.Errorf("сентинел второго замера = %v", sentinel)
		}
	}
}

func TestMeterSilentSourceDeadline(t *testing.T) {
	m, _ := fakeProc(t, map[string]string{
		"stat": stat, "pressure/io": "full avg10=0 avg60=0 avg300=0 total=1\n",
		"pressure/memory": "some avg10=0 avg60=0 avg300=0 total=1\n",
	})
	writeProc(t, m.procRoot, "pressure/io", "full avg10=0.5 avg60=12.5 avg300=1 total=1\n")
	m.disk = socket(t, func(conn net.Conn) { defer conn.Close(); _, _ = io.Copy(io.Discard, conn) })
	started := time.Now()
	// Окно свопа подменённый sleep проходит мгновенно и двигает total на 96 мс: откат даёт 9.6% за 1 с.
	v, err := m.Check(context.Background(), Limits{Runnable: 100, MaxIOFull: 9.6, SwapWindow: time.Second}, "", true)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(v.Skipped, ErrIOSourceFailed) || elapsed > 300*time.Millisecond {
		t.Errorf("таймаут: elapsed=%s skipped=%v (нужно само значение ErrIOSourceFailed)", elapsed, v.Skipped)
	}
	if v.Load.Disk != nil || v.Load.IOFull != 9.6 || v.Load.IOWindow != time.Second || v.Measure != IO {
		t.Errorf("откат при молчащем источнике = %+v", v)
	}
}

func TestMeterDiskOpenFromSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit float64
		want  string
	}{
		{"freeze busy", 9.6, IO},
		{"disk checks off", 100, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, now := fakeProc(t, map[string]string{
				"stat": stat, "pressure/io": "full avg10=0 avg60=0 avg300=0 total=1\n",
				"pressure/memory": "some avg10=0 avg60=0 avg300=0 total=1\n",
				"vmstat":          "pswpin 1\npswpout 1\n",
			})
			m.disk = socket(t, func(conn net.Conn) {
				defer conn.Close()
				_, _ = bufio.NewReader(conn).ReadBytes('\n')
				_, _ = fmt.Fprintf(conn, `{"v":1,"at":%q,"5s":{"io_full_ms":10},"open":{"open":true,"ms":340}}`+"\n", now.Format(time.RFC3339Nano))
			})
			v, err := m.Check(context.Background(), Limits{Runnable: 100, MaxIOFull: tc.limit}, "", true)
			if err != nil {
				t.Fatal(err)
			}
			if v.Load.Disk == nil || !v.Load.Disk.Open || v.Load.Disk.OpenMs != 340 || v.Measure != tc.want {
				t.Fatalf("Check = %+v, Disk %+v", v, v.Load.Disk)
			}
			if tc.want == IO && v.Why() != "disk freeze in progress for 340 ms" {
				t.Errorf("Why = %q", v.Why())
			}
		})
	}
}

func TestMeterIOErrorsAreCheckErrors(t *testing.T) {
	for _, ioBody := range []string{"full avg10=0 avg60=0 avg300=0 total=bad\n", "full avg10=0 avg60=0 avg300=0\n"} {
		m, _ := fakeProc(t, map[string]string{"stat": stat, "pressure/memory": "some avg10=0\n", "vmstat": "pswpin 1\npswpout 1\n"})
		writeProc(t, m.procRoot, "pressure/io", ioBody)
		m.sleep = func(context.Context, time.Duration) error { return nil }
		if _, err := m.Check(context.Background(), DefaultLimits, "", true); err == nil {
			t.Errorf("битый PSI total %q не вернул ошибку Check", ioBody)
		}
	}
	m, _ := fakeProc(t, map[string]string{"stat": stat, "pressure/memory": "some avg10=0\n", "vmstat": "pswpin 1\npswpout 1\n"})
	if err := os.Mkdir(filepath.Join(m.procRoot, "pressure", "io"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := m.Check(context.Background(), DefaultLimits, "", true); err == nil {
		t.Error("ошибка чтения PSI не вернула ошибку Check")
	}
}

func containsError(errs []error, target error) bool {
	for _, err := range errs {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
