package rates

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readFixture(t *testing.T, body string) Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rates.tsv")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := Read(path, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func TestLatestUsesDateThenLastRow(t *testing.T) {
	table := readFixture(t, "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n"+
		"2026-01-01\tanthropic\told\t200\t1000\n"+
		"2026-02-01\tanthropic\tfirst\t200\t1500\n"+
		"2026-02-01\tanthropic\tlast\t200\t2000\n")
	row, ok := table.Latest("anthropic", "")
	if !ok || row.Plan != "last" || row.Line != 4 || row.APIUSDWeek != 2000 {
		t.Fatalf("latest tied row = %+v, %v; want line 4", row, ok)
	}
	row, ok = table.Latest("anthropic", "first")
	if !ok || row.Line != 3 {
		t.Fatalf("selected plan row = %+v, %v; want line 3", row, ok)
	}
}

func TestLatestIgnoresFutureRowsButRetainsProvider(t *testing.T) {
	table := readFixture(t, "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n"+
		"2026-01-01\tanthropic\tcurrent\t200\t1000\n"+
		"2099-01-01\tanthropic\tfuture\t200\t9000\n"+
		"2099-01-01\tfuture-only\tfuture\t200\t9000\n")
	row, ok := table.Latest("anthropic", "")
	if !ok || row.Plan != "current" || row.Line != 2 {
		t.Fatalf("future row replaced active rate: %+v, %v", row, ok)
	}
	if _, ok := table.Latest("future-only", ""); ok {
		t.Fatal("future-only provider returned an active rate")
	}
	if len(table.Providers) != 2 || table.Providers[0] != "anthropic" || table.Providers[1] != "future-only" {
		t.Fatalf("provider names including future-only row = %q", table.Providers)
	}
}
