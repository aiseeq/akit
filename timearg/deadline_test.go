package timearg

import (
	"strings"
	"testing"
	"time"
)

func TestDeadlineQuirks(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, loc)
	// A day with impossible hours is no deadline, not that day at midnight.
	for _, wait := range []string{
		"12.11 25:00 x", "2026-11-12 25:00 x", "12.11 9:61: x",
		"12.11 06:4x", "2026-11-12 06:4х",
		"12.11 06:4x: итог", "12.11 06:4x, итог", "12.11 06:4x; итог",
		"2026-11-12 06:4x: итог", "12.11 06:4xМСК",
		"12.11 в 06:4x", "12.11 6:4x: итог",
		"12.11: 06:4x итог", "12.11, 06:4x итог",
		"13.11 до 06:4x", "13.11 к 06:4x", "13.11 около 06:4x", "13.11 В 06:4x",
	} {
		if at, ok := Deadline(wait, now); ok {
			t.Errorf("Deadline(%q) = %v, want no deadline", wait, at)
		}
	}
	for _, value := range []string{"06:4x", "06:4х", "06:4xМСК", "06:4x:", "06:4x,"} {
		if got, ok := GuessedTime(value); !ok || !strings.HasPrefix(value, got) {
			t.Errorf("GuessedTime(%q) = %q, %v", value, got, ok)
		}
	}
	if got, ok := GuessedTime("6:4x"); ok || got != "" {
		t.Errorf("GuessedTime one-digit hour = %q, %v, want empty and false", got, ok)
	}
	if got, ok := GuessedTime("шаг 1:4x, упало в 06:4x"); !ok || got != "06:4x" {
		t.Errorf("GuessedTime did not find later double-digit clock: %q, %v", got, ok)
	}
	for _, value := range []string{"13.11 до 06:4x", "13.11 к 06:4x", "13.11 около 06:4x", "13.11 В 06:4x"} {
		if got, ok := DatedGuessedTime(value); !ok || !strings.Contains(value, got) {
			t.Errorf("DatedGuessedTime(%q) = %q, %v", value, got, ok)
		}
	}
	if _, ok := DatedGuessedTime("6:4x"); ok {
		t.Error("DatedGuessedTime accepted a clock without a date")
	}
	if _, ok := GuessedTime("06:41"); ok {
		t.Error("exact clock detected as guessed")
	}
	if at, ok := Deadline("13.11 18:00 06:4x", now); !ok || !at.Equal(time.Date(2026, 11, 13, 18, 0, 0, 0, loc)) {
		t.Errorf("exact deadline before later guessed clock = %v, %v", at, ok)
	}
	// Exact time after an intervening word keeps the existing date-only deadline behavior.
	if at, ok := Deadline("13.11 до 18:00", now); !ok || !at.Equal(time.Date(2026, 11, 13, 0, 0, 0, 0, loc)) {
		t.Errorf("Deadline with exact time after word = %v, %v", at, ok)
	}
	// A one-digit day or month reads as the two-digit one.
	for wait, want := range map[string]time.Time{
		"1.1 x":        time.Date(2027, 1, 1, 0, 0, 0, 0, loc),
		"5.12 9:30: x": time.Date(2026, 12, 5, 9, 30, 0, 0, loc),
	} {
		if at, ok := Deadline(wait, now); !ok || !at.Equal(want) {
			t.Errorf("Deadline(%q) = %v, %v; want %v", wait, at, ok, want)
		}
	}
	// A DayMonth built by the caller works without ParseDayMonth.
	for d, want := range map[DayMonth]time.Time{
		{Date: "31.10"}:                   time.Date(2026, 10, 31, 0, 0, 0, 0, loc),
		{Date: "31.10", Clock: "12:00"}:   time.Date(2026, 10, 31, 12, 0, 0, 0, loc),
		{Date: "31.10", Clock: "9:30:15"}: time.Date(2026, 10, 31, 9, 30, 15, 0, loc),
	} {
		if at, ok := d.At(2026, loc); !ok || !at.Equal(want) {
			t.Errorf("%+v.At = %v, %v; want %v", d, at, ok, want)
		}
	}
}

func TestDeadlineClockZones(t *testing.T) {
	loc := time.FixedZone("Berlin", 3600)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, loc)
	cases := []struct {
		input string
		want  time.Time
	}{
		{"06:40: x", time.Date(2026, 11, 16, 6, 40, 0, 0, loc)},
		{"2026-11-17 07:30 +03:00 x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("+03:00", 3*3600))},
		{"2026-11-17 07:30 -0500 x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("-0500", -5*3600))},
		{"2026-11-17 07:30+03:00 x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("+03:00", 3*3600))},
		{"17.11 07:30+03:00 x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("+03:00", 3*3600))},
		{"07:30+03:00 x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.FixedZone("+03:00", 3*3600))},
		{"2026-11-17T06:40+03:00", time.Date(2026, 11, 17, 6, 40, 0, 0, time.FixedZone("+03:00", 3*3600))},
		{"2026-11-17T07:30Z x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC)},
		{"2026-11-17\u00a010:00 x", time.Date(2026, 11, 17, 10, 0, 0, 0, loc)},
		{"2026-11-17 07:30 UTC x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC)},
		{"2026-11-17T07:30:15 x", time.Date(2026, 11, 17, 7, 30, 15, 0, loc)},
		{"17.11 07:30:15: x", time.Date(2026, 11, 17, 7, 30, 15, 0, loc)},
	}
	for _, tc := range cases {
		got, ok := Deadline(tc.input, now)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("Deadline(%q) = %v, %v; want %v", tc.input, got, ok, tc.want)
		}
	}
	for _, input := range []string{"2026-11-17 07:30 MSK x", "2026-11-17 07:30MSK x", "07:30MSK"} {
		if !UnsupportedZone(input) {
			t.Errorf("UnsupportedZone(%q) = false", input)
		}
	}
}

func TestFractionalRFC3339IsRejected(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.Local)
	for _, input := range []string{"2026-11-17T07:30:00.5Z", "2026-11-17T07:30:00.123+03:00"} {
		if got, ok := Deadline(input, now); ok || ZoneError(input) == "" {
			t.Errorf("Deadline(%q) = %v, %v, ZoneError=%q; want unsupported zone error", input, got, ok, ZoneError(input))
		}
	}
}

func TestDeadlineNormalizesUnicodeSpacesAndTabs(t *testing.T) {
	loc := time.FixedZone("Berlin", 3600)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, loc)
	forms := []string{"\u00a0", "\u2009", "\u202f", "\u3000", "\t"}
	for _, space := range forms {
		for _, input := range []string{
			"17.11" + space + "10:00Z x",
			space + "2026-11-17 10:00 x",
			"17.11 10:00" + space + "x",
			"1.12 09:00" + space + "созвон",
			"2026-11-17" + space + "10:00-11:00 x",
		} {
			got, ok := Deadline(input, now)
			if !ok {
				t.Errorf("Deadline(%q) not parsed", input)
				continue
			}
			baseline := NormalizeSpaces(input)
			want, wantOK := Deadline(baseline, now)
			if !wantOK || !got.Equal(want) {
				t.Errorf("Deadline(%q) = %v, %v; normalized %q = %v, %v", input, got, ok, baseline, want, wantOK)
			}
		}
	}
	for _, space := range forms {
		if got := ZoneError(space + "2026-11-17 10:00 MSK x"); got == "" {
			t.Errorf("ZoneError did not recognize MSK after leading %q", space)
		}
	}
}

func TestDeadlineReviewR4Forms(t *testing.T) {
	loc := time.FixedZone("Berlin", 3600)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, loc)
	zone3 := time.FixedZone("+03:00", 3*3600)
	zoneMinus5 := time.FixedZone("-05:00", -5*3600)
	cases := []struct {
		input     string
		want      time.Time
		wantError bool
	}{
		{"2026-11-17 10:00 CI зелёный", time.Date(2026, 11, 17, 10, 0, 0, 0, loc), false},
		{"2026-11-17 10:00 PR смёржен", time.Date(2026, 11, 17, 10, 0, 0, 0, loc), false},
		{"17.11 10:00 API готов", time.Date(2026, 11, 17, 10, 0, 0, 0, loc), false},
		{"10:00 OK от лида", time.Date(2026, 11, 16, 10, 0, 0, 0, loc), false},
		{"2026-11-17 07:30 UTC+3 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zone3), false},
		{"2026-11-17 07:30 UTC-05:00 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zoneMinus5), false},
		{"2026-11-17 07:30 GMT-05:00 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zoneMinus5), false},
		{"2026-11-17 07:30 utc+3 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zone3), false},
		{"2026-11-17 07:30 gmt-05:00 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zoneMinus5), false},
		{"2026-11-17 07:30UTC+3 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zone3), false},
		{"2026-11-17 07:30GMT-05:00 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zoneMinus5), false},
		{"2026-11-17T07:30:15+0300 x", time.Date(2026, 11, 17, 7, 30, 15, 0, zone3), false},
		{"2026-11-17T07:30+0300 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zone3), false},
		{"2026-11-17T07:30-0500 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zoneMinus5), false},
		{"2026-11-17T07:30:15-05:00 x", time.Date(2026, 11, 17, 7, 30, 15, 0, zoneMinus5), false},
		{"17.11 07:30Z x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC), false},
		{"17.11 07:30 -0500 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zoneMinus5), false},
		{"17.11 07:30 UTC-0500 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zoneMinus5), false},
		{"2026-11-17 07:30 +0530 x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("+05:30", 19800)), false},
		{"2026-11-17 07:30 +03 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zone3), false},
		{"2026-11-17T07:30z x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC), false},
		{"2026-11-17T07:30UTC+3 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zone3), false},
		{"2026-11-17T07:30+3 x", time.Date(2026, 11, 17, 7, 30, 0, 0, zone3), false},
		{"2026-11-17T07:30:15z x", time.Date(2026, 11, 17, 7, 30, 15, 0, time.UTC), false},
		{"07:30Z x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.UTC), false},
		{"07:30z x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.UTC), false},
		{"07:30 UTC+3 x", time.Date(2026, 11, 16, 7, 30, 0, 0, zone3), false},
		{"07:30UTC+3 x", time.Date(2026, 11, 16, 7, 30, 0, 0, zone3), false},
		{"07:30 GMT x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.UTC), false},
		{"07:30+03:00 x", time.Date(2026, 11, 16, 7, 30, 0, 0, zone3), false},
		{"07:30 +03:00 x", time.Date(2026, 11, 16, 7, 30, 0, 0, zone3), false},
		{"07:30 +03 x", time.Date(2026, 11, 16, 7, 30, 0, 0, zone3), false},
		{"07:30+03 x", time.Date(2026, 11, 16, 7, 30, 0, 0, zone3), false},
		{"08:30, x", time.Date(2026, 11, 16, 8, 30, 0, 0, loc), false},
		{"08:30; x", time.Date(2026, 11, 16, 8, 30, 0, 0, loc), false},
		{"2026-11-17 07:30 +14:00 x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("+14:00", 14*3600)), false},
		{"2026-11-17 07:30 +14:30 x", time.Time{}, true},
		{"2026-11-17 07:30 +15:00 x", time.Time{}, true},
		{"2026-11-17 07:30 +23:00 x", time.Time{}, true},
		{"2026-11-17 07:30 +25:00 x", time.Time{}, true},
		{"2026-11-17 07:30 +03:75 x", time.Time{}, true},
		{"2026-11-17 07:30 +999 x", time.Time{}, true},
		{"2026-11-17 07:30 +03:7 x", time.Time{}, true},
		{"2026-11-17 07:30 MSK x", time.Time{}, true},
		{"2026-11-17 07:30 msk x", time.Date(2026, 11, 17, 7, 30, 0, 0, loc), false},
		{"2026-11-17 07:30 Msk x", time.Date(2026, 11, 17, 7, 30, 0, 0, loc), false},
		{"2026-11-17 07:30 CEST x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.FixedZone("CEST", 2*3600)), false},
		{"2026-11-17 07:30 МСК x", time.Time{}, true},
		{"2026-11-17 07:30 мск x", time.Time{}, true},
		{"2026-11-17 07:30 Мск x", time.Time{}, true},
		{"2026-11-17 07:30 Europe/Moscow x", time.Time{}, true},
		{"2026-11-17 07:30 (MSK) x", time.Time{}, true},
		{"2026-11-17 07:30 UTC +3 x", time.Time{}, true},
		{"2026-11-17 07:30 UTC -05:00 x", time.Time{}, true},
		{"2026-11-17 07:30 GMT +3 x", time.Time{}, true},
		{"10:00 МСК: x", time.Time{}, true},
		{"2026-11-17 07:30 GMT x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC), false},
		{"2026-11-17 10:00 +5 задач", time.Date(2026, 11, 17, 10, 0, 0, 0, loc), false},
		{"2026-11-17 10:00 +1 ревьюер", time.Date(2026, 11, 17, 10, 0, 0, 0, loc), false},
		{"10:00 -1 x", time.Date(2026, 11, 16, 10, 0, 0, 0, loc), false},
		{"2026-11-17 10:00 west wing", time.Date(2026, 11, 17, 10, 0, 0, 0, loc), false},
		{"2026-11-17 10:00 wing x", time.Date(2026, 11, 17, 10, 0, 0, 0, loc), false},
		{"2026-11-17 10:00 est. x", time.Date(2026, 11, 17, 10, 0, 0, 0, loc), false},
		{"2026-11-17 10:00 API готов", time.Date(2026, 11, 17, 10, 0, 0, 0, loc), false},
		{"2026-11-17 09:00 CET (выходные)", time.Date(2026, 11, 17, 9, 0, 0, 0, time.FixedZone("CET", 3600)), false},
		{"2026-11-17 09:00 CET (Berlin) x", time.Date(2026, 11, 17, 9, 0, 0, 0, time.FixedZone("CET", 3600)), false},
		{"2026-11-17 09:00 CEST", time.Date(2026, 11, 17, 9, 0, 0, 0, time.FixedZone("CEST", 7200)), false},
		{"2026-11-17 07:30 Utc x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		got, ok := Deadline(tc.input, now)
		if tc.wantError {
			if !UnsupportedZone(tc.input) || ok {
				t.Errorf("%q: deadline=%v, ok=%v, zone error=%v; want error", tc.input, got, ok, UnsupportedZone(tc.input))
			}
			continue
		}
		if UnsupportedZone(tc.input) || !ok || !got.Equal(tc.want) {
			t.Errorf("%q: deadline=%v, ok=%v, zone error=%v; want %v", tc.input, got, ok, UnsupportedZone(tc.input), tc.want)
		}
	}
}

func TestDeadlineSeparatedSingleDigitOffsetRemainsText(t *testing.T) {
	loc := time.FixedZone("Berlin", 3600)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, loc)
	for _, input := range []string{"2026-11-17 10:00 +5 задач", "2026-11-17 10:00 +1 ревьюер", "2026-11-16 10:00 -1 x"} {
		got, ok := Deadline(input, now)
		if !ok || got.Location() != loc || got.Hour() != 10 {
			t.Errorf("Deadline(%q) = %v, %v; want local 10:00", input, got, ok)
		}
	}
}

func TestDeadlineR6ClosedZoneRules(t *testing.T) {
	loc := time.FixedZone("Berlin", 3600)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, loc)
	local := func(day, hour, minute, second int) time.Time {
		return time.Date(2026, 11, day, hour, minute, second, 0, loc)
	}
	zone := func(day, hour, minute, second, offset int, name string) time.Time {
		return time.Date(2026, 11, day, hour, minute, second, 0, time.FixedZone(name, offset*60))
	}
	cases := []struct {
		input string
		want  time.Time
		err   bool
	}{
		{"2026-11-17 07:30 МСК x", time.Time{}, true},
		{"2026-11-17 07:30 мск x", time.Time{}, true},
		{"2026-11-17 07:30 Мск x", time.Time{}, true},
		{"10:00 МСК: x", time.Time{}, true},
		{"2026-11-17 07:30 UTC +3 x", time.Time{}, true},
		{"2026-11-17 07:30 UTC -05:00 x", time.Time{}, true},
		{"2026-11-17 07:30 GMT +3 x", time.Time{}, true},
		{"2026-11-17 10:00 +5 задач", local(17, 10, 0, 0), false},
		{"10:00 -1 x", local(16, 10, 0, 0), false},
		{"2026-11-17 10:00 +1 ревьюер", local(17, 10, 0, 0), false},
		{"2026-11-17 07:30 Europe/Moscow x", time.Time{}, true},
		{"2026-11-17 07:30 (MSK) x", time.Time{}, true},
		{"2026-11-17 10:00 west wing", local(17, 10, 0, 0), false},
		{"2026-11-17 10:00 West wing", local(17, 10, 0, 0), false},
		{"2026-11-17 10:00 WEST x", time.Time{}, true},
		{"2026-11-17 10:00 wing x", local(17, 10, 0, 0), false},
		{"2026-11-17 10:00 est. x", local(17, 10, 0, 0), false},
		{"2026-11-17 10:00 est x", local(17, 10, 0, 0), false},
		{"2026-11-02 09:00 CET (выходные)", zone(2, 9, 0, 0, 60, "CET"), false},
		{"2026-11-17 07:30 CET (Berlin) x", zone(17, 7, 30, 0, 60, "CET"), false},
		{"2026-11-17 07:30 cet x", zone(17, 7, 30, 0, 60, "CET"), false},
		{"2026-11-17 07:30 Cest x", zone(17, 7, 30, 0, 120, "CEST"), false},
		{"2026-11-17 07:30 (CEST) x", zone(17, 7, 30, 0, 120, "CEST"), false},
		{"2026-11-17 07:30 (CET) x", zone(17, 7, 30, 0, 60, "CET"), false},
		{"2026-11-17 07:30 (UTC) x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC), false},
		{"2026-11-17 07:30 (Z) x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC), false},
		{"2026-11-17 07:30 (+03:00) x", zone(17, 7, 30, 0, 180, "+03:00"), false},
		{"2026-11-17 07:30CEST x", zone(17, 7, 30, 0, 120, "CEST"), false},
		{"2026-11-17 07:30CET x", zone(17, 7, 30, 0, 60, "CET"), false},
		{"07:30CET x", zone(16, 7, 30, 0, 60, "CET"), false},
		{"07:30 CEST x", zone(16, 7, 30, 0, 120, "CEST"), false},
		{"17.11 07:30 CEST x", zone(17, 7, 30, 0, 120, "CEST"), false},
		{"17.11 07:30CEST x", zone(17, 7, 30, 0, 120, "CEST"), false},
		{"2026-11-17T07:30 CEST x", zone(17, 7, 30, 0, 120, "CEST"), false},
		{"2026-11-17T07:30CEST x", zone(17, 7, 30, 0, 120, "CEST"), false},
		{"2026-11-17 07:30 CEST: x", zone(17, 7, 30, 0, 120, "CEST"), false},
		{"2026-11-17 10:00 PR/42 x", local(17, 10, 0, 0), false},
		{"2026-11-17 10:00 и/или x", local(17, 10, 0, 0), false},
		{"2026-11-17 10:00 1/2 готово", local(17, 10, 0, 0), false},
		{"2026-11-17 10:00 +05 задач", zone(17, 10, 0, 0, 300, "+05:00"), false},
		{"2026-11-17 10:00 +10 задач", zone(17, 10, 0, 0, 600, "+10:00"), false},
		{"2026-11-17 10:00 +123 x", time.Time{}, true},
		{"2026-11-17 10:00 +3 x", local(17, 10, 0, 0), false},
		{"2026-11-17 07:30+3 x", zone(17, 7, 30, 0, 180, "+03:00"), false},
		{"2026-11-17 07:30 +3:00 x", zone(17, 7, 30, 0, 180, "+03:00"), false},
		{"2026-11-17 07:30 +14:30 x", time.Time{}, true},
		{"2026-11-17 07:30 +15:00 x", time.Time{}, true},
		{"2026-11-17 07:30 UTC+3: x", zone(17, 7, 30, 0, 180, "+03:00"), false},
		{"2026-11-17 07:30 UTC, x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC), false},
		{"2026-11-17 07:30 MSK: x", time.Time{}, true},
		{"2026-11-17 07:30 MSK, x", time.Time{}, true},
		{"2026-11-17 07:30 MSK. x", time.Time{}, true},
		{"2026-11-17 07:30 CEST, x", zone(17, 7, 30, 0, 120, "CEST"), false},
	}
	for _, tc := range cases {
		got, ok := Deadline(tc.input, now)
		if tc.err {
			if ZoneError(tc.input) == "" || ok {
				t.Errorf("Deadline(%q) = %v, %v, ZoneError=%q; want code 2", tc.input, got, ok, ZoneError(tc.input))
			}
			continue
		}
		if ZoneError(tc.input) != "" || !ok || !got.Equal(tc.want) {
			t.Errorf("Deadline(%q) = %v, %v, ZoneError=%q; want %v", tc.input, got, ok, ZoneError(tc.input), tc.want)
		}
	}
}

func TestDeadlineR6GluedZoneErrorsAndOffsets(t *testing.T) {
	loc := time.FixedZone("Berlin", 3600)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, loc)
	for _, input := range []string{"07:30abc x", "07:30MSK x", "07:30+25:00 x", "07:30+03:75 x", "07:30UTC+25 x"} {
		if !UnsupportedZone(input) {
			t.Errorf("UnsupportedZone(%q) = false", input)
		}
		if _, ok := Deadline(input, now); ok {
			t.Errorf("Deadline(%q) unexpectedly succeeded", input)
		}
	}
	for _, tc := range []struct {
		input string
		want  time.Time
	}{
		{"07:30+3 x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.FixedZone("+03:00", 3*3600))},
		{"7:30+3 x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.FixedZone("+03:00", 3*3600))},
		{"07:30:15-0500 x", time.Date(2026, 11, 16, 7, 30, 15, 0, time.FixedZone("-05:00", -5*3600))},
		{"07:30CEST x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.FixedZone("CEST", 7200))},
	} {
		got, ok := Deadline(tc.input, now)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("Deadline(%q) = %v, %v; want %v", tc.input, got, ok, tc.want)
		}
	}
}

func TestDeadlineR7ZonesAndWrappers(t *testing.T) {
	loc := time.FixedZone("Berlin", 3600)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, loc)
	local := func(day, hour, minute, second int) time.Time {
		return time.Date(2026, 11, day, hour, minute, second, 0, loc)
	}
	offset := func(day, hour, minute, second, zoneMinutes int, name string) time.Time {
		return time.Date(2026, 11, day, hour, minute, second, 0, time.FixedZone(name, zoneMinutes*60))
	}
	cases := []struct {
		input string
		want  time.Time
		err   string
	}{
		{"07:30CEST x", offset(16, 7, 30, 0, 120, "CEST"), ""},
		{"07:30UTC x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.UTC), ""},
		{"07:30gmt x", time.Date(2026, 11, 16, 7, 30, 0, 0, time.UTC), ""},
		{"07:30:15CEST x", offset(16, 7, 30, 15, 120, "CEST"), ""},
		{"07:30UTC+3 x", offset(16, 7, 30, 0, 180, "+03:00"), ""},
		{"07:30-0500 x", offset(16, 7, 30, 0, -300, "-05:00"), ""},
		{"07:30-05:00 x", offset(16, 7, 30, 0, -300, "-05:00"), ""},
		{"2026-11-17 07:30 -5:00 x", offset(17, 7, 30, 0, -300, "-05:00"), ""},
		{"2026-11-17 07:30 -05:00, x", offset(17, 7, 30, 0, -300, "-05:00"), ""},
		{"07:30-12:00 x", offset(16, 7, 30, 0, -720, "-12:00"), ""},
		{"07:30..11:00 x", time.Time{}, "unsupported"},
		{"07:30-25:00 x", time.Time{}, "unsupported"},
		{"2026-11-17 07:30+03:00: x", offset(17, 7, 30, 0, 180, "+03:00"), ""},
		{"2026-11-17 07:30Z: x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC), ""},
		{"2026-11-17 07:30CEST: x", offset(17, 7, 30, 0, 120, "CEST"), ""},
		{"2026-11-17 07:30UTC, x", time.Date(2026, 11, 17, 7, 30, 0, 0, time.UTC), ""},
		{"2026-11-17 07:30ч x", time.Time{}, "unsupported"},
		{"2026-11-17 07:30h x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 Design/Review x", local(17, 10, 0, 0), ""},
		{"2026-11-17 10:00 America/New_York x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 Etc/GMT+3 x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 US/Eastern x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 europe/moscow x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 (MSK), x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 ((UTC)) x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17 10:00 [UTC] x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17 10:00 \"UTC\" x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17 10:00 «UTC» x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17 10:00 (UTC x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17 10:00 UTC) x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17 10:00 (UTC] x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17 10:00 +10 задач", offset(17, 10, 0, 0, 600, "+10:00"), ""},
		{"2026-11-17 10:00 +5 задач", local(17, 10, 0, 0), ""},
		{"2026-11-17 10:00 +3:00 x", offset(17, 10, 0, 0, 180, "+03:00"), ""},
		{"2026-11-17 10:00 +0300 x", offset(17, 10, 0, 0, 180, "+03:00"), ""},
		{"2026-11-17 10:00 +14:30 x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 +15:00 x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 +0a x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 +03:0 x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 IST x", time.Time{}, "unsupported"},
		{"2026-11-17 10:00 Ист x", local(17, 10, 0, 0), ""},
		{"2026-11-17 10:00 UTC +3", time.Time{}, "utc-space"},
		{"2026-11-17 10:00 UTC, +3 x", time.Time{}, "utc-space"},
		{"2026-11-17 10:00 UTC , +3 x", time.Time{}, "utc-space"},
		{"2026-11-17 10:00 UTC: +3 x", time.Time{}, "utc-space"},
		{"2026-11-17 10:00 GMT -5 x", time.Time{}, "utc-space"},
		{"2026-11-17 10:00\tUTC x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17  10:00  UTC x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17,10:00 UTC x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17T10:00 (CEST) x", offset(17, 10, 0, 0, 120, "CEST"), ""},
		{"2026-11-17 10:00:00 UTC x", time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), ""},
		{"2026-11-17 10:00:00+3 x", offset(17, 10, 0, 0, 180, "+03:00"), ""},
		{"2026-11-17 10:00 UTC+25 x", time.Time{}, "unsupported"},
	}
	for _, tc := range cases {
		got, ok := Deadline(tc.input, now)
		problem := ZoneError(tc.input)
		if tc.err != "" {
			if problem != tc.err || ok {
				t.Errorf("Deadline(%q) = %v, %v, ZoneError=%q; want code 2 %q", tc.input, got, ok, problem, tc.err)
			}
			continue
		}
		if problem != "" || !ok || !got.Equal(tc.want) {
			t.Errorf("Deadline(%q) = %v, %v, ZoneError=%q; want %v", tc.input, got, ok, problem, tc.want)
		}
	}
}
