// Package pressure замеряет давление на машину и держит правило «машина занята» (busy.go):
// готовые к работе задачи, давление на диск, давление памяти с обменом со свопом. Memory (some avg60
// и занятый своп) — замеры для потребителя (aguard), правило их не судит.
package pressure

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Memory — давление памяти: PSI some avg60 и доля занятого свопа.
type Memory struct {
	PSISomeAvg60 float64 // /proc/pressure/memory, %
	SwapUsedPct  float64 // занятый своп, % от размера
}

// ParsePSI достаёт «some avg60» из /proc/pressure/<ресурс>.
func ParsePSI(s string) (float64, error) {
	return psiField(s, "some", "avg60")
}

// psiField достаёт поле key (avg10, avg60) строки kind (some, full) файла PSI.
func psiField(s, kind, key string) (float64, error) {
	for line := range strings.SplitSeq(s, "\n") {
		if !strings.HasPrefix(line, kind+" ") {
			continue
		}
		for f := range strings.FieldsSeq(line) {
			if v, ok := strings.CutPrefix(f, key+"="); ok {
				x, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return 0, fmt.Errorf("PSI %s %s: %w", kind, key, err)
				}
				return x, nil
			}
		}
	}
	return 0, fmt.Errorf("PSI: нет строки %s с %s", kind, key)
}

// ParseSwapUsedPct считает занятый своп в процентах по /proc/meminfo; свопа нет — 0.
func ParseSwapUsedPct(meminfo string) (float64, error) {
	var total, free float64
	sc := bufio.NewScanner(strings.NewReader(meminfo))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || (f[0] != "SwapTotal:" && f[0] != "SwapFree:") {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			return 0, fmt.Errorf("meminfo %s: %w", f[0], err)
		}
		if f[0] == "SwapTotal:" {
			total = v
		} else {
			free = v
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("meminfo: %w", err)
	}
	if total <= 0 {
		return 0, nil
	}
	return (total - free) / total * 100, nil
}

// ReadMemory читает давление памяти из procRoot (/proc). При ошибке отдаёт
// то, что прочиталось (непрочитанное — 0), и ошибку.
func ReadMemory(procRoot string) (Memory, error) {
	var m Memory
	psi, err := os.ReadFile(filepath.Join(procRoot, "pressure", "memory"))
	if err != nil {
		return m, fmt.Errorf("PSI: %w", err)
	}
	if m.PSISomeAvg60, err = ParsePSI(string(psi)); err != nil {
		return m, err
	}
	mi, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return m, fmt.Errorf("meminfo: %w", err)
	}
	m.SwapUsedPct, err = ParseSwapUsedPct(string(mi))
	return m, err
}
