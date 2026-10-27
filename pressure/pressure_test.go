package pressure

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	psi     = "some avg10=1.00 avg60=6.50 avg300=2.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	meminfo = "MemTotal: 100 kB\nSwapTotal:       1000 kB\nSwapFree:         250 kB\n"
)

func TestParse(t *testing.T) {
	if v, err := ParsePSI(psi); err != nil || v != 6.5 {
		t.Errorf("ParsePSI = %v, %v", v, err)
	}
	if _, err := ParsePSI("full avg60=1\n"); err == nil {
		t.Error("нет строки some — ждали ошибку")
	}
	if v, err := ParseSwapUsedPct(meminfo); err != nil || v != 75 {
		t.Errorf("ParseSwapUsedPct = %v, %v", v, err)
	}
	if v, err := ParseSwapUsedPct("MemTotal: 100 kB\n"); err != nil || v != 0 {
		t.Errorf("без свопа = %v, %v", v, err)
	}
}

func TestReadMemory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pressure"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pressure", "memory"), []byte(psi), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMemory(root)
	if err == nil || m.PSISomeAvg60 != 6.5 {
		t.Errorf("без meminfo: PSI прочитан, ошибка есть: %+v %v", m, err)
	}
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte(meminfo), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, err := ReadMemory(root); err != nil || m != (Memory{PSISomeAvg60: 6.5, SwapUsedPct: 75}) {
		t.Errorf("ReadMemory = %+v, %v", m, err)
	}
}
