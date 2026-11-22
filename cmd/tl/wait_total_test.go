package main

import (
	"strings"
	"testing"
	"time"
)

func TestWaitAllProgramTotalRow(t *testing.T) {
	day := time.Date(2026, 10, 25, 12, 0, 0, 0, time.Local)
	var rows []waitSample
	// 19 short calls without a subcommand and one long call with a positional argument:
	// only the total row sees the long call next to the short ones.
	for i := 1; i <= 19; i++ {
		rows = append(rows, waitSample{At: day, Program: "prior", Sub: "-", Duration: time.Duration(i) * time.Millisecond})
	}
	rows = append(rows, waitSample{At: day, Program: "prior", Sub: "fable", Duration: 900 * time.Millisecond})
	rows = append(rows, waitSample{At: day.Add(24 * time.Hour), Program: "prior", Sub: "-", Duration: 5 * time.Millisecond})
	rows = append(rows, waitSample{At: day, Program: "tk", Sub: "done", Duration: 7 * time.Millisecond})
	var out strings.Builder
	reportWaitAll(result{waitRows: rows}, &out)
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	d1, d2 := day.Format(time.DateOnly), day.Add(24*time.Hour).Format(time.DateOnly)
	want := []string{
		d1 + "\tprior\t*\t20\t1090\t19\t900",
		d1 + "\tprior\t-\t19\t190\t19\t19",
		d1 + "\tprior\tfable\t1\t900\t900\t900",
		d1 + "\ttk\t*\t1\t7\t7\t7",
		d1 + "\ttk\tdone\t1\t7\t7\t7",
		d2 + "\tprior\t*\t1\t5\t5\t5",
		d2 + "\tprior\t-\t1\t5\t5\t5",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("wait-all rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
