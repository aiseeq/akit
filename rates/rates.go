// Package rates reads subscription capacity and conversion rates from rates.tsv.
package rates

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Rate is one rates.tsv row. Invalid numeric fields retain their source cell
// and line so callers can explain a bad selected rate.
type Rate struct {
	Provider, Plan               string
	PlanUSDMonth, APIUSDWeek     float64
	CentsPerAPIUSD, Caution      float64
	ValidFrom                    time.Time
	Line                         int
	InvalidPlan, InvalidWeek     error
	InvalidCents, InvalidCaution error
}

// Table contains rows at or before now. Later rows are ignored.
type Table struct {
	Path string
	Rows []Rate
}

// Read reads a tab separated rates file. Row selection is left to Latest,
// which consistently uses the latest date and then the last row in the file.
func Read(path string, now time.Time) (Table, error) {
	f, err := os.Open(path)
	if err != nil {
		return Table{}, fmt.Errorf("open rates %s: %w", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	columns := map[string]int{}
	width := 0
	table := Table{Path: path}
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimRight(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if width == 0 {
			width = len(fields)
			for i, name := range fields {
				columns[name] = i
			}
			for _, name := range []string{"valid_from", "provider", "plan_usd_month", "api_usd_week"} {
				if _, ok := columns[name]; !ok {
					return Table{}, fmt.Errorf("%s: header missing %s", path, name)
				}
			}
			continue
		}
		if len(fields) != width {
			return Table{}, fmt.Errorf("%s:%d: wrong number of columns", path, lineNo)
		}
		row, include, err := parseRow(path, lineNo, fields, columns, now)
		if err != nil {
			return Table{}, err
		}
		if !include {
			continue
		}
		table.Rows = append(table.Rows, row)
	}
	if err := scanner.Err(); err != nil {
		return Table{}, fmt.Errorf("read rates %s: %w", path, err)
	}
	if width == 0 {
		return Table{}, fmt.Errorf("%s: no header", path)
	}
	return table, nil
}

func parseRow(path string, lineNo int, fields []string, columns map[string]int, now time.Time) (Rate, bool, error) {
	valid, err := parseDate(fields[columns["valid_from"]])
	if err != nil {
		return Rate{}, false, fmt.Errorf("%s:%d:valid_from: invalid date: %w", path, lineNo, err)
	}
	if valid.After(now) {
		return Rate{}, false, nil
	}
	row := Rate{Provider: fields[columns["provider"]], ValidFrom: valid, Line: lineNo}
	row.PlanUSDMonth, row.InvalidPlan = rateNumber(path, lineNo, "plan_usd_month", fields[columns["plan_usd_month"]])
	row.APIUSDWeek, row.InvalidWeek = rateNumber(path, lineNo, "api_usd_week", fields[columns["api_usd_week"]])
	if i, ok := columns["plan"]; ok {
		row.Plan = fields[i]
	}
	if i, ok := columns["cents_per_api_usd"]; ok {
		row.CentsPerAPIUSD, row.InvalidCents = rateNumber(path, lineNo, "cents_per_api_usd", fields[i])
	}
	if i, ok := columns["caution"]; ok {
		row.Caution, row.InvalidCaution = rateNumber(path, lineNo, "caution", fields[i])
	}
	return row, true, nil
}

// Latest returns the selected provider row. An empty plan matches every plan.
func (t Table) Latest(provider, plan string) (Rate, bool) {
	var best Rate
	found := false
	for i := range t.Rows {
		row := &t.Rows[i]
		if row.Provider != provider || (plan != "" && row.Plan != plan) {
			continue
		}
		if !found || row.ValidFrom.After(best.ValidFrom) || row.ValidFrom.Equal(best.ValidFrom) {
			best, found = *row, true
		}
	}
	return best, found
}

func rateNumber(path string, line int, column, value string) (float64, error) {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return n, fmt.Errorf("%s:%d:%s: invalid value %q: %w", path, line, column, value, err)
	}
	if n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return n, fmt.Errorf("%s:%d:%s: invalid value %q: number must be positive and finite", path, line, column, value)
	}
	return n, nil
}

func parseDate(value string) (time.Time, error) {
	if date, err := time.ParseInLocation(time.DateOnly, value, time.Local); err == nil {
		return date, nil
	}
	return time.Parse(time.RFC3339, value)
}
