package main

import (
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/timearg"
)

// Wait refuses inputs without a parseable deadline, including impossible dates.
func TestWaitRejectsInputsWithoutDeadline(t *testing.T) {
	for _, tc := range []struct{ until, want string }{
		{"12.11, 25:00 build done", "ГГГГ-ММ-ДД ЧЧ:ММ: чего ждём"},
		{"12.11 9:5 build done", "ГГГГ-ММ-ДД ЧЧ:ММ: чего ждём"},
		{"2026-11-12 25:00 build done", "ГГГГ-ММ-ДД ЧЧ:ММ: чего ждём"},
		{"2026-11-12; 25:00 build done", "ГГГГ-ММ-ДД ЧЧ:ММ: чего ждём"},
		{"31.11 9:00 build done", "ГГГГ-ММ-ДД ЧЧ:ММ: чего ждём"},
		{"31.02\u00a010:00 build done", "ГГГГ-ММ-ДД ЧЧ:ММ: чего ждём"},
		{"12.11;25:00 build done", "ГГГГ-ММ-ДД ЧЧ:ММ: чего ждём"},
		{"2026-11-12,25:00 build done", "ГГГГ-ММ-ДД ЧЧ:ММ: чего ждём"},
		{"build done", "ГГГГ-ММ-ДД ЧЧ:ММ: чего ждём"},
		{"2026-11-12,09:00 build done", ""},
	} {
		var errout strings.Builder
		o := &options{cmd: "wait", until: tc.until, by: "someone"}
		_, err := transitionBody("# t\n\nСтатус: открыт\n", o, "ждёт", &errout)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%q: error %v, want %q", tc.until, err, tc.want)
		}
	}
}

func TestWaitRejectsAlphabeticZone(t *testing.T) {
	_, dir := testRunMainRoot(t)
	writeRunMainTask(t, dir, "20200101-1000-MED-zone.md", "# Zone\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	for _, until := range []string{"07:30MSK", "07:30abc", "07:30+25:00", "07:30 МСК", "07:30 мск", "07:30 Мск", "07:30 Europe/Moscow", "07:30 (MSK)", "2026-11-17 07:30 UTC +3", "07:30 UTC -05:00", "07:30 GMT +3", "07:30 WEST"} {
		var out, errout strings.Builder
		code := runMain([]string{"wait", "zone", "--until", until, "--by", "tester"}, time.Now(), &out, &errout)
		wantHint := "supported zones are Z, UTC, GMT, CET, CEST"
		if strings.Contains(until, " UTC ") || strings.Contains(until, " GMT ") {
			wantHint = "UTC+3 without a space"
		}
		if code != 2 || !strings.Contains(errout.String(), wantHint) {
			t.Errorf("wait with %q: code=%d stdout=%q stderr=%q", until, code, out.String(), errout.String())
		}
		if wantHint != "UTC+3 without a space" && strings.Contains(errout.String(), "UTC+3 without a space") {
			t.Errorf("wait with %q got the UTC/GMT spacing hint: %q", until, errout.String())
		}
	}
}

func TestWaitRejectsAmbiguousAttachedMinus(t *testing.T) {
	for _, until := range []string{"10:00-12", "10:00-5", "2026-11-17 10:00-12 x"} {
		var errout strings.Builder
		_, err := transitionBody("# t\n\nСтатус: открыт\n", &options{cmd: "wait", until: until}, "ждёт", &errout)
		if err == nil || !strings.Contains(err.Error(), "смещение пиши -05:00, диапазон — 10:00-12:00") {
			t.Errorf("transitionBody(%q) error = %v; want ambiguous-minus hint", until, err)
		}
	}
}

func TestWaitRejectsInvalidRangeEndWithSpecificError(t *testing.T) {
	for _, until := range []string{"10:00-24:00", "10:00-11:60"} {
		var errout strings.Builder
		_, err := transitionBody("# t\n\nСтатус: открыт\n", &options{cmd: "wait", until: until}, "ждёт", &errout)
		if err == nil || !strings.Contains(err.Error(), "invalid range end") || strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("transitionBody(%q) error = %v; want invalid range end", until, err)
		}
	}
	for _, until := range []string{"17.11\u00a010:00-25:00 x", "\u00a02026-11-17 10:00-25:00 x", "17.11,\u00a010:00-25:00 x"} {
		var errout strings.Builder
		_, err := transitionBody("# t\n\nСтатус: открыт\n", &options{cmd: "wait", until: until}, "ждёт", &errout)
		if err == nil || !strings.Contains(err.Error(), "invalid range end") || strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("transitionBody(%q) error = %v; want invalid range end with Unicode spaces", until, err)
		}
	}
}

// One wait gives one deadline: the text as written and the text tk wait
// stores after dating it agree for every separator after the date.
func TestDatedWaitKeepsDeadline(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.FixedZone("CET", 3600))
	for _, wait := range []string{"12.11, 9:00 x", "12.11: 9:00 x", "12.11; 9:00 x", "12.11,9:00 x", "12.11:9:00 x", "12.11;9:00 x", "12.11; 25:00 x", "12.11; текст", "12.11;9:00 UTC x", "12.11;9:00Z x", "2026-11-12,09:00 x", "2026-11-12;собрать"} {
		before, okBefore := timearg.Deadline(wait, now)
		after, okAfter := timearg.Deadline(datedWait(wait, now), now)
		if okBefore != okAfter || !before.Equal(after) {
			t.Errorf("%q: deadline %v %v before dating, %v %v after (%q)", wait, before, okBefore, after, okAfter, datedWait(wait, now))
		}
	}
}
