// Package rates reads subscription capacity and conversion rates from rates.tsv.
package rates

import (
	"bufio"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Rate is one rates.tsv row. Invalid numeric fields retain their source cell
// and line so callers can explain a bad selected rate; a numeric column the
// header lacks (only valid_from and provider are required) is such an error too.
// ValidFrom of a date-only cell is midnight in the zone of the now given to Read. A row carries its cells
// for Cell, so rows of two Read calls are not == even for the same file; copies
// of one row are.
type Rate struct {
	Provider, Plan               string
	PlanUSDMonth, APIUSDWeek     float64
	CentsPerAPIUSD, Caution      float64
	ValidFrom                    time.Time
	Line                         int
	InvalidPlan, InvalidWeek     error
	InvalidCents, InvalidCaution error
	cells                        *rowCells // the row as written, for columns Rate has no field for; nil outside Read
}

// rowCells is one row's cells and the header that names them.
type rowCells struct {
	fields  []string
	columns map[string]int
}

// Cell returns the row's cell in column name as written in the file. ok is
// false when the file's header has no such column or the row did not come
// from Read; an empty cell is "", true.
func (r Rate) Cell(name string) (value string, ok bool) {
	if r.cells == nil {
		return "", false
	}
	i, ok := r.cells.columns[name]
	if !ok {
		return "", false
	}
	return r.cells.fields[i], true
}

// Table contains rows at or before now. Later rows are ignored.
type Table struct {
	Path      string
	Rows      []Rate
	Providers []string
	columns   map[string]int // header name → cell index
}

// HasColumn reports whether the header names column name, so callers can tell
// a missing column from an empty cell.
func (t Table) HasColumn(name string) bool {
	_, ok := t.columns[name]
	return ok
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
	providerSeen := map[string]bool{}
	width := 0
	table := Table{Path: path, columns: columns}
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
			for _, name := range []string{"valid_from", "provider"} {
				if _, ok := columns[name]; !ok {
					return Table{}, fmt.Errorf("%s: header missing %s", path, name)
				}
			}
			continue
		}
		if len(fields) != width {
			return Table{}, fmt.Errorf("%s:%d: wrong number of columns", path, lineNo)
		}
		provider := fields[columns["provider"]]
		if !providerSeen[provider] {
			providerSeen[provider] = true
			table.Providers = append(table.Providers, provider)
		}
		row, include, err := parseRow(path, lineNo, fields, columns, now)
		if err != nil {
			return Table{}, err
		}
		if !include {
			continue
		}
		row.cells = &rowCells{fields: fields, columns: columns}
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
	valid, err := parseDate(fields[columns["valid_from"]], now.Location())
	if err != nil {
		return Rate{}, false, fmt.Errorf("%s:%d:valid_from: invalid date", path, lineNo)
	}
	if valid.After(now) {
		return Rate{}, false, nil
	}
	row := Rate{Provider: fields[columns["provider"]], ValidFrom: valid, Line: lineNo}
	row.PlanUSDMonth, row.InvalidPlan = rateColumn(path, lineNo, "plan_usd_month", fields, columns)
	row.APIUSDWeek, row.InvalidWeek = rateColumn(path, lineNo, "api_usd_week", fields, columns)
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

// PlansAtLatest returns the distinct plans represented by the provider's
// selected latest date, in sorted order.
func (t Table) PlansAtLatest(provider string) []string {
	latest, ok := t.Latest(provider, "")
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for i := range t.Rows {
		row := &t.Rows[i]
		if row.Provider == provider && row.ValidFrom.Equal(latest.ValidFrom) {
			seen[row.Plan] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// rateColumn parses a numeric column the header may lack: a missing column is
// an error only for the caller that reads the value.
func rateColumn(path string, line int, column string, fields []string, columns map[string]int) (float64, error) {
	i, ok := columns[column]
	if !ok {
		return 0, fmt.Errorf("%s:%d:%s: header missing %s", path, line, column, column)
	}
	return rateNumber(path, line, column, fields[i])
}

func rateNumber(path string, line int, column, value string) (float64, error) {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return n, fmt.Errorf("%s:%d:%s: invalid value %q", path, line, column, value)
	}
	if n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return n, fmt.Errorf("%s:%d:%s: invalid value %q", path, line, column, value)
	}
	return n, nil
}

// parseDate reads a date in loc, the zone of the caller's now: a process serving
// requests from other zones compares the date in the requester's zone.
func parseDate(value string, loc *time.Location) (time.Time, error) {
	if date, err := time.ParseInLocation(time.DateOnly, value, loc); err == nil {
		return date, nil
	}
	return time.Parse(time.RFC3339, value)
}
