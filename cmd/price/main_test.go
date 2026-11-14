package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRates(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rates.tsv")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	opts, err := parseArgs(args)
	if err != nil {
		return exitUsage, "", err.Error()
	}
	var out, errOut bytes.Buffer
	code := run(opts, env{}, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestWeekPercentageMatchesUsageCourse(t *testing.T) {
	path := writeRates(t, "# synthetic rates\nvalid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n2026-01-01\tanthropic\tmax\t200\t1000\n")
	code, out, errOut := invoke(t, "--usd", "10", "--provider", "anthropic", "--rates", path)
	if code != 0 || !strings.Contains(out, "$10 API = 2%") || !strings.Contains(out, "plan=max valid_from=2026-01-01") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestLatestDateAndSelectedPlan(t *testing.T) {
	path := writeRates(t, "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n"+
		"2026-01-01\tanthropic\tmax\t200\t1000\n"+
		"2026-02-01\tanthropic\tpro\t100\t500\n"+
		"2026-02-01\tanthropic\tpro\t200\t1000\n")
	code, out, errOut := invoke(t, "--usd=10", "--provider=anthropic", "--plan=pro", "--rates="+path)
	if code != 0 || !strings.Contains(out, "API = 2%") || !strings.Contains(out, "plan=pro valid_from=2026-02-01") {
		t.Fatalf("last tied pro row not selected: code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = invoke(t, "--usd", "10", "--provider", "anthropic", "--rates", path)
	if code != 0 || !strings.Contains(out, "API = 2%") || !strings.Contains(out, "plan=pro") {
		t.Fatalf("latest provider row not selected: code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestNonNumericRateReportsFileAndLine(t *testing.T) {
	path := writeRates(t, "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n2026-01-01\tanthropic\tmax\t200\tbad\n")
	code, _, errOut := invoke(t, "--usd", "1", "--provider", "anthropic", "--rates", path)
	if code != exitData || !strings.Contains(errOut, path+":2:api_usd_week:") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestUnitUSDUsesCaution(t *testing.T) {
	path := writeRates(t, "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\tcents_per_api_usd\tcaution\n"+
		"2026-01-01\tanthropic\tmax\t200\t1000\t2.5\t2\n")
	code, out, errOut := invoke(t, "--usd", "10", "--provider", "anthropic", "--unit", "usd", "--rates", path)
	if code != 0 || !strings.Contains(out, "$10 API = $0.5 subscription") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestSmallAmountKeepsSignificantDigits(t *testing.T) {
	path := writeRates(t, "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n2026-01-01\tanthropic\tmax\t200\t1000\n")
	code, out, errOut := invoke(t, "--usd", "0.004", "--provider", "anthropic", "--rates", path)
	if code != 0 || !strings.HasPrefix(out, "$0.004 API = 0.0008% of one $100 anthropic subscription week\n") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestMultipleLatestPlansAreNamed(t *testing.T) {
	path := writeRates(t, "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n"+
		"2026-02-01\tanthropic\tmax\t200\t1000\n"+
		"2026-02-01\tanthropic\tpro\t200\t1000\n")
	code, out, errOut := invoke(t, "--usd", "10", "--provider", "anthropic", "--rates", path)
	if code != 0 || !strings.Contains(out, "plan=pro") || !strings.Contains(out, "multiple plans on 2026-02-01: max,pro; use --plan to select one") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestUnitUSDDoesNotRoundSmallAmount(t *testing.T) {
	path := writeRates(t, "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\tcents_per_api_usd\tcaution\n"+
		"2026-01-01\tanthropic\tmax\t200\t1000\t3.03\t1\n")
	code, out, errOut := invoke(t, "--usd=1.000", "--provider=anthropic", "--unit=usd", "--rates="+path)
	if code != 0 || !strings.HasPrefix(out, "$1.000 API = $0.0303 subscription\n") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestMissingRatesAndInvalidInvocationCodes(t *testing.T) {
	if code, _, errOut := invoke(t, "--usd", "1", "--provider", "anthropic", "--rates", filepath.Join(t.TempDir(), "missing")); code != exitData || !strings.Contains(errOut, "missing") {
		t.Fatalf("missing file: code=%d err=%q", code, errOut)
	}
	path := writeRates(t, "valid_from\tprovider\tplan\tplan_usd_month\tapi_usd_week\n2026-01-01\tanthropic\tmax\t200\t1000\n")
	if code, _, errOut := invoke(t, "--usd", "1", "--provider", "nosuch", "--rates", path); code != exitData || !strings.Contains(errOut, "nosuch") {
		t.Fatalf("missing provider: code=%d err=%q", code, errOut)
	}
	if code, _, _ := invoke(t, "--usd", "1", "--provider", "anthropic", "--plan", "other", "--rates", path); code != exitData {
		t.Fatalf("missing plan code=%d, want %d", code, exitData)
	}
	if _, err := parseArgs([]string{"--usd", "1", "--provider", "anthropic", "--unit", "wat"}); err == nil {
		t.Fatal("invalid unit was accepted")
	}
	if _, err := parseArgs([]string{"--usd", "1", "--provider", "anthropic", "extra"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("unexpected positional argument error = %v", err)
	}
}

func TestResultKeepsThreeSignificantDigitsWithoutExponent(t *testing.T) {
	for v, want := range map[float64]string{
		2:            "2",
		0.12995:      "0.13",
		0.0303:       "0.0303",
		0.000519818:  "0.00052",
		0.0000519818: "0.000052",
		1234.5:       "1230",
	} {
		if got := significant(v); got != want {
			t.Errorf("significant(%v) = %q, want %q", v, got, want)
		}
	}
}
