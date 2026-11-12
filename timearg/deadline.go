package timearg

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// waitAgo — «назад» (30m, 2h): timearg его понимает, сроком ожидания он не бывает.
var waitAgo = regexp.MustCompile(`^[0-9]+[smhd]$`)

// waitISO — дата и часы через T, без пояса или с Z (2026-10-31T12:00, 2026-10-31T12:00Z), не RFC3339.
var waitISO = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})T([0-9]{1,2}:[0-9]{2}(:[0-9]{2})?)(Z?)$`)

// isoDatePattern — дата ГГГГ-ММ-ДД в начале, часы после неё — как у dayMonthPattern.
var isoDatePattern = regexp.MustCompile(`(?s)^\s*([0-9]{4}-[0-9]{2}-[0-9]{2})(?:(?:[,:;]\s*|\s+)([0-9]{1,2}:[0-9]{1,2}(?::[0-9]{1,2})?[Zz]?))?(.*)$`)

// Часы после даты — через пробел или разделитель с пробелом или без; минуты и секунды одной цифрой берутся как часы,
// чтобы At отверг их, а не оставил полночь даты.
var dayMonthPattern = regexp.MustCompile(`(?s)^\s*([0-9]{1,2}\.[0-9]{1,2})(?:(?:[,:;]\s*|\s+)([0-9]{1,2}:[0-9]{1,2}(?::[0-9]{1,2})?[Zz]?))?(.*)$`)

// DayMonth — дата ДД.ММ без года из начала строки: Date («31.10»), Clock («12:00» или ""), Rest — текст после.
type DayMonth struct {
	Date, Clock, Rest string
}

// clockLike — слово-часы после даты (ЧЧ:ММ[:СС]), даже невозможные.
var clockLike = regexp.MustCompile(`^[0-9]{1,2}:[0-9]{1,2}(:[0-9]{1,2})?$`)
var guessedClockRE = regexp.MustCompile(`(?i)(?:^|[^\p{N}:])((?:[01]?\d|2[0-3]):[0-5][xх])(?:$|[^a-z0-9])`)
var dateTokenRE = regexp.MustCompile(`^(?:[0-9]{1,2}\.[0-9]{1,2}|[0-9]{4}-[0-9]{2}-[0-9]{2})$`)

func guessedTime(value string) string {
	match := guessedClockRE.FindStringSubmatch(value)
	if match == nil {
		return ""
	}
	return match[1]
}

// GuessedClock возвращает первое угаданное время независимо от длины часа.
func GuessedClock(value string) (string, bool) {
	clock := guessedTime(value)
	return clock, clock != ""
}

// GuessedTime возвращает первое угаданное время с двузначным часом в тексте записи.
func GuessedTime(value string) (string, bool) {
	for _, match := range guessedClockRE.FindAllStringSubmatch(value, -1) {
		if strings.IndexByte(match[1], ':') == 2 {
			return match[1], true
		}
	}
	return "", false
}

// DatedGuessedTime возвращает угаданное время после даты, с одним словом между ними или без него.
func DatedGuessedTime(value string) (string, bool) {
	fields := strings.Fields(value)
	if len(fields) < 2 || !dateTokenRE.MatchString(strings.TrimRight(fields[0], ":,;")) {
		return "", false
	}
	clock := guessedTime(strings.TrimRight(fields[1], ":,;"))
	if clock != "" {
		return clock, true
	}
	if len(fields) < 3 || strings.ContainsRune(fields[1], ':') {
		return "", false
	}
	clock = guessedTime(strings.TrimRight(fields[2], ":,;"))
	return clock, clock != ""
}

// HasDatedGuessedTime сообщает, что после начальной даты записано угаданное время.
func HasDatedGuessedTime(value string) bool {
	_, ok := DatedGuessedTime(value)
	return ok
}

// Deadline — срок ожидания: время в начале строки «Ждёт:» (дата, дата с часами, «ДД.ММ ЧЧ:ММ», ISO с T, RFC3339,
// часы; UTC после часов — в UTC, иначе местное), дальше — текст. Без времени в начале — условие, срока нет.
// «Назад» (30m) сроком не бывает.
func Deadline(wait string, now time.Time) (time.Time, bool) {
	f := strings.Fields(wait)
	if len(f) == 0 || waitAgo.MatchString(f[0]) {
		return time.Time{}, false
	}
	// «2026-10-31 17:30: текст», «2026-11-06, после…» — знак после срока к сроку не относится.
	for i := range min(2, len(f)) {
		f[i] = strings.TrimRight(f[i], ":,;")
	}
	if HasDatedGuessedTime(wait) {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, f[0]); err == nil {
		return t, true
	}
	if m := waitISO.FindStringSubmatch(f[0]); m != nil {
		f = append([]string{m[1], m[2]}, f[1:]...)
		if m[4] == "Z" {
			f = append(f[:2], append([]string{"UTC"}, f[2:]...)...)
		}
	}
	if parsed, ok := ParseDayMonth(wait); ok {
		if t, ok := closestDayMonth(parsed, now); ok {
			return deadlineZone(t, parsed.Rest), true
		}
	}
	if m := isoDatePattern.FindStringSubmatch(wait); m != nil && (m[3] == "" || strings.ContainsRune(" :,;", rune(m[3][0]))) {
		clock, rest := splitZone(m[2], m[3])
		value := strings.TrimSpace(m[1] + " " + clock)
		if t, err := Parse(value, now); err == nil {
			return deadlineZone(t, rest), true
		}
	}
	for n := min(2, len(f)); n >= 1; n-- {
		// Без двух слов срока пробуем одно: «2026-11-06 итог» — срок одним словом, второе — заметка.
		// Но не часы: «12.11 25:00» с невозможными часами — не полночь 12.11, а срока нет.
		if n == 1 && len(f) > 1 && clockLike.MatchString(f[1]) {
			break
		}
		if t, err := Parse(strings.Join(f[:n], " "), now); err == nil {
			return deadlineZone(t, strings.Join(f[n:], " ")), true
		}
	}
	return time.Time{}, false
}

// splitZone takes a zone glued to the hours («9:00Z») off the clock and puts it before the rest,
// where deadlineZone reads it as it reads «9:00 Z».
func splitZone(clock, rest string) (string, string) {
	bare := strings.TrimRight(clock, "Zz")
	if bare != clock {
		rest = "Z" + rest
	}
	return bare, rest
}

// deadlineZone применяет UTC после разобранного срока, если метка указана.
func deadlineZone(t time.Time, rest string) time.Time {
	if fields := strings.Fields(strings.TrimLeft(rest, " :,;")); len(fields) > 0 {
		zone := strings.TrimRight(fields[0], ":,;")
		if strings.EqualFold(zone, "UTC") || strings.EqualFold(zone, "Z") {
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
		}
	}
	return t
}

// ParseDayMonth распознаёт ДД.ММ[ ЧЧ:ММ[:СС]] и принимает только границу перед текстом.
func ParseDayMonth(value string) (DayMonth, bool) {
	m := dayMonthPattern.FindStringSubmatch(value)
	if m == nil || (m[3] != "" && !strings.ContainsRune(" :,;", rune(m[3][0]))) {
		return DayMonth{}, false
	}
	clock, rest := splitZone(m[2], m[3])
	parsed := DayMonth{Date: m[1], Clock: clock, Rest: rest}
	if _, ok := parsed.At(2000, time.Local); !ok {
		return DayMonth{}, false
	}
	return parsed, true
}

// At — эта дата в году year; false, если такого дня в году нет (29.02) или часы невозможны. День и месяц —
// одной или двумя цифрами.
func (w DayMonth) At(year int, loc *time.Location) (time.Time, bool) {
	layout, value := "2.1 2006", w.Date+" "
	if w.Clock != "" {
		layout, value = "2.1 15:04 2006", w.Date+" "+w.Clock+" "
		if strings.Count(w.Clock, ":") == 2 {
			layout = "2.1 15:04:05 2006"
		}
	}
	candidate, err := time.ParseInLocation(layout, value+strconv.Itoa(year), loc)
	return candidate, err == nil
}

// closestDayMonth выбирает для старой даты ДД.ММ ближайшее к now наступление.
// Проверка прошлого, текущего и следующего года сохраняет смысл даты через Новый год.
func closestDayMonth(wait DayMonth, now time.Time) (time.Time, bool) {
	var closest time.Time
	var distance time.Duration
	for year := now.Year() - 1; year <= now.Year()+1; year++ {
		candidate, ok := wait.At(year, now.Location())
		if !ok {
			continue
		}
		delta := candidate.Sub(now)
		if delta < 0 {
			delta = -delta
		}
		if closest.IsZero() || delta < distance {
			closest, distance = candidate, delta
		}
	}
	return closest, !closest.IsZero()
}
