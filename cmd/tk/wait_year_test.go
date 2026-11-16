package main

import (
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/timearg"
)

// A new wait written as «DD.MM» is stored with the nearest future year.
func TestWaitWriteStoresYear(t *testing.T) {
	for _, c := range []struct {
		wait string
		now  time.Time
		want string
	}{
		{"05.01 10:00: x", time.Date(2026, 12, 20, 9, 0, 0, 0, time.Local), "2027-01-05 10:00: x"},
		{"10.11 12:00: x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-10 12:00: x"},
		{"10.11: x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-10: x"},
		{"12:00 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-09 12:00 x"},
		{"06:40: x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-09 06:40: x"},
		{"07:30+03:00 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-09 07:30+03:00 x"},
		{"07:30Z x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-09 07:30Z x"},
		{"07:30z x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-09 07:30z x"},
		{"08:30, x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-09 08:30, x"},
		{"08:30; x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-09 08:30; x"},
		{"07:30. x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-09 07:30. x"},
		{"17.11 07:30+3 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-17 07:30+3 x"},
		{"17.11 07:30Z x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-17 07:30Z x"},
		{"17.11 07:30 -0500 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-17 07:30 -0500 x"},
		{"31.02 10:00-11:00 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "31.02 10:00-11:00 x"},
		{"31.02\u00a010:00 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "31.02\u00a010:00 x"},
		{"17.13 10:00-11:00 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "17.13 10:00-11:00 x"},
		{"  7.11 10:00-11:00 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2027-11-07 10:00-11:00 x"},
		{"\t17.1 10:00-11:00 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2027-01-17 10:00-11:00 x"},
		{"\u00a017.11 10:00-11:00 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-17 10:00-11:00 x"},
		{"17.11 10:00-11:00Z x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-17 10:00-11:00Z x"},
		{"17.11 7:30-8:00Z x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-17 7:30-8:00Z x"},
		{"17.11 10:00-11:00+3 x", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "2026-11-17 10:00-11:00+3 x"},
		{"после ответа", time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local), "после ответа"},
	} {
		if got := datedWait(c.wait, c.now); got != c.want {
			t.Errorf("datedWait(%q) at %v = %q, want %q", c.wait, c.now, got, c.want)
		}
	}
}

func TestClockOnlyWaitPreservesWrittenText(t *testing.T) {
	now := time.Date(2026, 11, 16, 15, 0, 0, 0, time.Local)
	for _, input := range []string{"16:00 x", "16:00+03 x", "16:00 +03:00 x", "16:00Z x", "16:00, x", "16:00; x", "16:00. x"} {
		stored := datedWait(input, now)
		got, ok := timearg.Deadline(stored, now)
		wantStored := "2026-11-16 " + strings.TrimSpace(input)
		want := time.Date(2026, 11, 16, 16, 0, 0, 0, time.Local)
		if strings.Contains(input, "+03") {
			want = time.Date(2026, 11, 16, 16, 0, 0, 0, time.FixedZone("+03:00", 3*3600))
		}
		if strings.Contains(input, "Z") {
			want = time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC)
		}
		if stored != wantStored || !ok || !got.Equal(want) {
			t.Errorf("%q stored as %q gives %v, %v; want %q and deadline %v", input, stored, got, ok, wantStored, want)
		}
	}
	for _, input := range []string{"  10:00 x", "\u00a0\u00a010:00 x", "10:00 x   ", "\t10:00 x\t"} {
		want := "2026-11-16 10:00 x"
		if got := datedWait(input, now); got != want {
			t.Errorf("edge spaces in %q stored as %q; want %q", input, got, want)
		}
	}
}

func TestWaitFractionalSecondsErrorNamesCause(t *testing.T) {
	for _, input := range []string{"2026-11-17 07:30:00.5 x", "2026-11-17 07:30:00,5 x"} {
		var errout strings.Builder
		_, err := transitionBody("# t\n\nСтатус: открыт\n", &options{cmd: "wait", until: input}, "ждёт", &errout)
		if err == nil || !strings.Contains(err.Error(), "fractional seconds") || strings.Contains(err.Error(), "time zone") {
			t.Errorf("fractional clock error for %q = %v; want a fractional-seconds explanation", input, err)
		}
	}
}

func TestWaitTimeRangesUseOnlyTheirStart(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.Local)
	for _, input := range []string{"17.11", "2026-11-17"} {
		got, isRange, problem := waitDeadlineText(input)
		if got != input || isRange || problem {
			t.Errorf("waitDeadlineText(%q) = %q, %v, %v; date without a clock is not a range", input, got, isRange, problem)
		}
	}
	for _, tc := range []struct {
		input, deadline, stored string
		want                    time.Time
	}{
		{"10:00-11:00 созвон", "10:00 созвон", "2026-11-16 10:00-11:00 созвон", time.Date(2026, 11, 16, 10, 0, 0, 0, time.Local)},
		{"7:30-8:00 x", "7:30 x", "2026-11-16 7:30-8:00 x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.Local)},
		{"7:30-08:00 x", "7:30 x", "2026-11-16 7:30-08:00 x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.Local)},
		{"2026-11-17 10:00-11:00 созвон", "2026-11-17 10:00 созвон", "2026-11-17 10:00-11:00 созвон", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)},
		{"2026-11-17,10:00-11:00 x", "2026-11-17,10:00 x", "2026-11-17,10:00-11:00 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)},
		{"17.11 7:30-8:00 x", "17.11 7:30 x", "2026-11-17 7:30-8:00 x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.Local)},
		{"7.11 10:00-11:00 x", "7.11 10:00 x", "2027-11-07 10:00-11:00 x", time.Date(2027, 11, 7, 10, 0, 0, 0, time.Local)},
		{"17.1 10:00-11:00 x", "17.1 10:00 x", "2027-01-17 10:00-11:00 x", time.Date(2027, 1, 17, 10, 0, 0, 0, time.Local)},
		{"1.12 09:00-10:00 созвон", "1.12 09:00 созвон", "2026-12-01 09:00-10:00 созвон", time.Date(2026, 12, 1, 9, 0, 0, 0, time.Local)},
		{"7.11;10:00-11:00 x", "7.11;10:00 x", "2027-11-07;10:00-11:00 x", time.Date(2027, 11, 7, 10, 0, 0, 0, time.Local)},
		{"17.11 10:00-12:00 x", "17.11 10:00 x", "2026-11-17 10:00-12:00 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)},
		{"2026-11-17;10:00-11:00 x", "2026-11-17;10:00 x", "2026-11-17;10:00-11:00 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)},
		{"2026-11-17 10:00-11:00Z x", "2026-11-17 10:00Z x", "2026-11-17 10:00-11:00Z x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)},
		{"2026-11-17 10:00-11:00+3 x", "2026-11-17 10:00+3 x", "2026-11-17 10:00-11:00+3 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.FixedZone("+03:00", 3*3600))},
		{"2026-11-17 10:00-11:00-05:00 x", "2026-11-17 10:00-05:00 x", "2026-11-17 10:00-11:00-05:00 x", time.Date(2026, 11, 17, 16, 0, 0, 0, time.Local)},
		{"2026-11-17 10:00-11:00-12:00 x", "2026-11-17 10:00-12:00 x", "2026-11-17 10:00-11:00-12:00 x", time.Date(2026, 11, 17, 23, 0, 0, 0, time.Local)},
		{"2026-11-17 10:00-11:00:30 x", "2026-11-17 10:00 :30 x", "2026-11-17 10:00-11:00:30 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)},
		{"  2026-11-17 10:00-11:00 x", "  2026-11-17 10:00 x", "2026-11-17 10:00-11:00 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)},
		{"\t2026-11-17 10:00-11:00 x", " 2026-11-17 10:00 x", "2026-11-17 10:00-11:00 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)},
		{"\u00a02026-11-17 10:00-11:00 x", " 2026-11-17 10:00 x", "2026-11-17 10:00-11:00 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)},
		{"2026-11-17\u00a010:00-11:00 x", "2026-11-17 10:00 x", "2026-11-17\u00a010:00-11:00 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)},
	} {
		deadlineText, isRange, ambiguous := waitDeadlineText(tc.input)
		if !isRange || ambiguous || deadlineText != tc.deadline {
			t.Errorf("waitDeadlineText(%q) = %q, %v, %v; want %q, true, false", tc.input, deadlineText, isRange, ambiguous, tc.deadline)
		}
		stored := datedWait(tc.input, now)
		if stored != tc.stored {
			t.Errorf("datedWait(%q) = %q; want %q", tc.input, stored, tc.stored)
		}
		got, ok := waitTimeDeadline(stored, now)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("waitTimeDeadline(%q) = %v, %v; want %v", stored, got, ok, tc.want)
		}
	}
	for _, tc := range []struct {
		input string
		want  time.Time
	}{
		{"2026-11-17T07:30-05:00", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("-05:00", -5*3600))},
		{"2026-11-17T07:30-05:00 x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("-05:00", -5*3600))},
		{"2026-11-17t07:30-05:00", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("-05:00", -5*3600))},
		{"2026-11-17T07:30:00-05:00", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("-05:00", -5*3600))},
		{"2026-11-17T07:30-0500", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("-05:00", -5*3600))},
		{"2026-11-17T07:30+05:00", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("+05:00", 5*3600))},
		{"2026-11-17T10:00-11:00", time.Date(2026, 11, 17, 10, 0, 0, 0, time.FixedZone("-11:00", -11*3600))},
		{"17.11T10:00-11:00", time.Date(2026, 11, 17, 10, 0, 0, 0, time.FixedZone("-11:00", -11*3600))},
	} {
		if _, isRange, problem := waitDeadlineText(tc.input); isRange || problem {
			t.Errorf("waitDeadlineText(%q) = range %v problem %v; time after T must stay ISO", tc.input, isRange, problem)
		}
		got, ok := waitTimeDeadline(tc.input, now)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("waitTimeDeadline(%q) = %v, %v; want %v", tc.input, got, ok, tc.want)
		}
	}
	for _, input := range []string{"10:00-12", "10:00-5", "10:00-5:", "10:00-5,", "10:00-5.", "10:00-12:: x", "10:00-5:: x", "2026-11-17 10:00-12 x", "2026-11-17 10:00-12."} {
		_, isRange, ambiguous := waitDeadlineText(input)
		if isRange || !ambiguous {
			t.Errorf("waitDeadlineText(%q) = isRange %v ambiguous %v; want ambiguous", input, isRange, ambiguous)
		}
		var errout strings.Builder
		_, err := transitionBody("# t\n\nСтатус: открыт\n", &options{cmd: "wait", until: input}, "ждёт", &errout)
		if err == nil || !strings.Contains(err.Error(), "смещение пиши -05:00, диапазон — 10:00-12:00") {
			t.Errorf("transitionBody(%q) error = %v; want ambiguity hint", input, err)
		}
	}
	for _, input := range []string{"10:00 - 11:00 x", "2026-11-17 10:00 - 11:00 x"} {
		_, isRange, ambiguous := waitDeadlineText(input)
		if isRange || ambiguous {
			t.Errorf("waitDeadlineText(%q) = isRange %v ambiguous %v; spaced dash is text", input, isRange, ambiguous)
		}
	}
	for _, tc := range []struct {
		input string
		want  time.Time
		err   bool
	}{
		{"2026-11-17 10:00 - 11:00 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local), false},
		{"2026-11-17 10:00 – 11:00 x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local), false},
		{"2026-11-17 10:00—11:00 x", time.Time{}, true},
		{"2026-11-17 10:00–11:00 x", time.Time{}, true},
	} {
		var errout strings.Builder
		_, err := transitionBody("# t\n\nСтатус: открыт\n", &options{cmd: "wait", until: tc.input}, "ждёт", &errout)
		if tc.err {
			if err == nil || !strings.Contains(err.Error(), "unsupported or invalid time zone") {
				t.Errorf("transitionBody(%q) error = %v; want code-2 attached-dash error", tc.input, err)
			}
			continue
		}
		got, ok := waitTimeDeadline(tc.input, now)
		if err != nil || !ok || !got.Equal(tc.want) {
			t.Errorf("%q => %v, %v, %v; want %v", tc.input, got, ok, err, tc.want)
		}
	}

	// Reducing a range to its start preserves the exact semantics of the start's suffix.
	for _, tc := range []struct{ rangeForm, start string }{
		{"10:00-11:00 +3 задачи", "10:00 +3 задачи"},
		{"10:00-11:00 UTC +3 x", "10:00 UTC +3 x"},
		{"2026-11-17;10:00-11:00 x", "2026-11-17;10:00 x"},
		{"17.11;10:00-11:00 x", "17.11;10:00 x"},
		{"10:00-11:00x", "10:00x"},
		{"10:00-11:00 UTC, +3 x", "10:00 UTC, +3 x"},
		{"10:00-11:00 (UTC) x", "10:00 (UTC) x"},
		{"10:00-11:00 +7a x", "10:00 +7a x"},
	} {
		for _, form := range []string{tc.rangeForm, tc.start} {
			text, _, _ := waitDeadlineText(form)
			problem := timearg.ZoneError(text)
			deadline, ok := timearg.Deadline(text, now)
			if form == tc.rangeForm {
				rangeProblem, rangeOK := problem, ok
				rangeDeadline := deadline
				startText, _, _ := waitDeadlineText(tc.start)
				startProblem := timearg.ZoneError(startText)
				startDeadline, startOK := timearg.Deadline(startText, now)
				if rangeProblem != startProblem || rangeOK != startOK || rangeOK && !rangeDeadline.Equal(startDeadline) {
					t.Errorf("range %q => %v/%v/%q; start %q => %v/%v/%q", tc.rangeForm, rangeDeadline, rangeOK, rangeProblem, tc.start, startDeadline, startOK, startProblem)
				}
			}
		}
	}
	for _, input := range []string{"2026-11-17 10:00-24:00 x", "2026-11-17 10:00-24:00", "2026-11-17 10:00-11:60 x"} {
		_, isRange, invalid := waitDeadlineText(input)
		if isRange || !invalid {
			t.Errorf("waitDeadlineText(%q) = range %v invalid %v; want invalid-end error", input, isRange, invalid)
		}
		_, err := transitionBody("# t\n\nСтатус: открыт\n", &options{cmd: "wait", until: input}, "ждёт", &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "invalid range end") {
			t.Errorf("transitionBody(%q) error = %v; want specific invalid range-end error", input, err)
		}
	}
	for _, input := range []string{"10:00-11:60 x", "2026-11-17 10:00-11:60 x"} {
		if got, ok := waitTimeDeadline(input, now); ok {
			t.Errorf("waitTimeDeadline(%q) = %v, true; invalid range end must have no deadline", input, got)
		}
	}
	for _, input := range []string{"2026-11-17T10:00-24:00", "2026-11-17t10:00:00-24:00", "2026-11-17 10:00:00-24:00"} {
		_, err := transitionBody("# t\n\nСтатус: открыт\n", &options{cmd: "wait", until: input}, "ждёт", &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "unsupported or invalid time zone") {
			t.Errorf("transitionBody(%q) error = %v; ISO T offset must be handled as a zone, not a range", input, err)
		}
	}
	_, err := transitionBody("# t\n\nСтатус: открыт\n", &options{cmd: "wait", until: "17.11\u00a010:00-25:00 x"}, "ждёт", &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "invalid range end") {
		t.Errorf("NBSP range-end error = %v; want invalid range end", err)
	}
	spacedOffset := time.Date(2026, 11, 17, 10, 0, 0, 0, time.FixedZone("-05:00", -5*3600))
	for _, input := range []string{"2026-11-17 10:00 -05:00", "2026-11-17 10:00 -0500", "2026-11-17 10:00 -5:00", "2026-11-17 10:00 -05:00,"} {
		got, ok := waitTimeDeadline(input, now)
		if !ok || !got.Equal(spacedOffset) {
			t.Errorf("waitTimeDeadline(%q) = %v, %v; want %v", input, got, ok, spacedOffset)
		}
	}
	secondsOffset := time.Date(2026, 11, 17, 10, 0, 0, 0, time.FixedZone("-11:00", -11*3600))
	if got, ok := waitTimeDeadline("2026-11-17 10:00:00-11:00 x", now); !ok || !got.Equal(secondsOffset) {
		t.Errorf("waitTimeDeadline(seconds offset) = %v, %v; want %v", got, ok, secondsOffset)
	}
}

// An old «Ждёт:» line without a year keeps the date its author meant across New Year.
func TestWaitDueOldLineWithoutYear(t *testing.T) {
	for _, c := range []struct {
		wait     string
		now, due time.Time
	}{
		{"31.12 23:59: x", time.Date(2027, 1, 2, 9, 0, 0, 0, time.Local), time.Date(2026, 12, 31, 23, 59, 0, 0, time.Local)},
		{"10.01 12:00: x", time.Date(2026, 12, 20, 9, 0, 0, 0, time.Local), time.Date(2027, 1, 10, 12, 0, 0, 0, time.Local)},
		{"31.10 12:00 после замера", time.Date(2026, 10, 31, 15, 0, 0, 0, time.Local), time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)},
	} {
		got, ok := timearg.Deadline(c.wait, c.now)
		if !ok || !got.Equal(c.due) {
			t.Errorf("timearg.Deadline(%q) at %v = %v, %v; want %v", c.wait, c.now, got, ok, c.due)
		}
	}
}
