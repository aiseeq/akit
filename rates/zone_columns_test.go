package rates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRates(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rates.tsv")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A date is compared in the zone of now, not in the process zone: a server in UTC
// answering a request at 2026-03-01 00:30 UTC+9 selects the row valid from 2026-03-01.
func TestReadComparesDateInZoneOfNow(t *testing.T) {
	saved := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = saved })
	path := writeRates(t, "valid_from\tprovider\tplan_usd_month\tapi_usd_week\n"+
		"2026-02-01\tanthropic\t100\t1000\n"+
		"2026-03-01\tanthropic\t200\t2000\n")
	now := time.Date(2026, 3, 1, 0, 30, 0, 0, time.FixedZone("UTC+9", 9*3600))
	table, err := Read(path, now)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := table.Latest("anthropic", "")
	if !ok || row.PlanUSDMonth != 200 {
		t.Fatalf("latest = %+v, %v; want the 2026-03-01 row ($200)", row, ok)
	}
	if row.ValidFrom.Location() != now.Location() {
		t.Fatalf("ValidFrom zone = %v, want the zone of now", row.ValidFrom.Location())
	}
}

// Only valid_from and provider are required; a missing numeric column is an error
// for the reader of that value only.
func TestReadWithoutAPIUSDWeek(t *testing.T) {
	path := writeRates(t, "valid_from\tprovider\tplan\tplan_usd_month\n"+
		"2026-02-01\tanthropic\tmax\t200\n")
	table, err := Read(path, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := table.Latest("anthropic", "max")
	if !ok || row.PlanUSDMonth != 200 || row.InvalidPlan != nil {
		t.Fatalf("latest = %+v, %v; want plan_usd_month 200", row, ok)
	}
	if row.InvalidWeek == nil || !strings.Contains(row.InvalidWeek.Error(), "header missing api_usd_week") {
		t.Fatalf("InvalidWeek = %v, want header missing api_usd_week", row.InvalidWeek)
	}
	if table.HasColumn("api_usd_week") {
		t.Fatal("HasColumn(api_usd_week) = true for a file without it")
	}
	if _, err := Read(writeRates(t, "valid_from\tplan_usd_month\n2026-02-01\t200\n"), time.Now()); err == nil || !strings.Contains(err.Error(), "header missing provider") {
		t.Fatalf("Read without provider: err = %v, want header missing provider", err)
	}
}
