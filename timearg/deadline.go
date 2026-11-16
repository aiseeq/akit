package timearg

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// waitAgo — «назад» (30m, 2h): timearg его понимает, сроком ожидания он не бывает.
var waitAgo = regexp.MustCompile(`^[0-9]+[smhd]$`)

// NormalizeSpaces returns a parse-only copy with Unicode space separators and tabs folded to ASCII spaces.
func NormalizeSpaces(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || unicode.Is(unicode.Zs, r) {
			return ' '
		}
		return r
	}, value)
}

var leadingDateToken = regexp.MustCompile(`^([0-9]{1,2}\.[0-9]{1,2}|[0-9]{4}-[0-9]{2}-[0-9]{2})`)

// HasLeadingDate reports whether a date-shaped token starts the value, even when its date is impossible.
func HasLeadingDate(value string) bool { return leadingDateToken.MatchString(value) }

// waitISO — дата и часы через T, без пояса или с Z (2026-10-31T12:00, 2026-10-31T12:00Z), не RFC3339.
var waitISO = regexp.MustCompile(`(?i)^([0-9]{4}-[0-9]{2}-[0-9]{2})T([0-9]{1,2}:[0-9]{2}(?::[0-9]{2})?)((?:UTC|GMT)?[+-][0-9]{1,2}(?::?[0-9]{2})?|[Zz])?$`)

// isoDatePattern — дата ГГГГ-ММ-ДД в начале, часы после неё — как у dayMonthPattern.
var isoDatePattern = regexp.MustCompile(`(?is)^[\s\p{Zs}]*([0-9]{4}-[0-9]{2}-[0-9]{2})(?:(?:T|[,:;][\s\p{Zs}]*|[\s\p{Zs}]+)([0-9]{1,2}:[0-9]{1,2}(?::[0-9]{1,2})?))?(.*)$`)

// Часы после даты — через пробел или разделитель с пробелом или без; минуты и секунды одной цифрой берутся как часы,
// чтобы At отверг их, а не оставил полночь даты.
var dayMonthPattern = regexp.MustCompile(`(?is)^[\s\p{Zs}]*([0-9]{1,2}\.[0-9]{1,2})(?:(?:T|[,:;][\s\p{Zs}]*|[\s\p{Zs}]+)([0-9]{1,2}:[0-9]{1,2}(?::[0-9]{1,2})?))?(.*)$`)
var leadingClockPattern = regexp.MustCompile(`^[\s\p{Zs}]*((?:[01]?[0-9]|2[0-3]):[0-5][0-9])`)
var fractionalClockTail = regexp.MustCompile(`^[.,][0-9]+`)

// DayMonth — дата ДД.ММ без года из начала строки: Date («31.10»), Clock («12:00» или ""), Rest — текст после.
type DayMonth struct {
	Date, Clock, Rest string
}

// LeadingClockSpan returns the byte span of a valid leading clock, using the same date forms as Deadline.
func LeadingClockSpan(value string) (int, int, bool) {
	match := LeadingClockMatch(value)
	if match == nil {
		return 0, 0, false
	}
	return match[4], match[1], true
}

// LeadingClockMatch returns regexp-style spans for date, hour and minute captures at the start of value.
func LeadingClockMatch(value string) []int {
	var match []int
	for _, pattern := range []*regexp.Regexp{dayMonthPattern, isoDatePattern} {
		match = pattern.FindStringSubmatchIndex(value)
		if match != nil && match[4] >= 0 {
			break
		}
		match = nil
	}
	if match == nil {
		match = leadingClockPattern.FindStringSubmatchIndex(value)
		if match == nil {
			return nil
		}
		start, end := match[2], match[3]
		colon := strings.IndexByte(value[start:end], ':') + start
		minuteEnd := min(colon+3, end)
		return []int{start, end, -1, -1, start, colon, colon + 1, minuteEnd}
	}
	dateStart, dateEnd := match[2], match[3]
	clockStart, clockEnd := match[4], match[5]
	colon := strings.IndexByte(value[clockStart:clockEnd], ':') + clockStart
	minuteEnd := min(colon+3, clockEnd)
	return []int{match[0], clockEnd, dateStart, dateEnd, clockStart, colon, colon + 1, minuteEnd}
}

// clockLike — слово-часы после даты (ЧЧ:ММ[:СС]), даже невозможные.
var clockLike = regexp.MustCompile(`^[0-9]{1,2}:[0-9]{1,2}(:[0-9]{1,2})?$`)
var guessedClockRE = regexp.MustCompile(`(?i)(?:^|[^\p{N}:])((?:[01]?\d|2[0-3]):[0-5][xх])(?:$|[^a-z0-9])`)
var dateTokenRE = regexp.MustCompile(`^(?:[0-9]{1,2}\.[0-9]{1,2}|[0-9]{4}-[0-9]{2}-[0-9]{2})$`)
var leadingClockZoneRE = regexp.MustCompile(`(?i)^([ ]*(?:(?:[0-9]{4}-[0-9]{2}-[0-9]{2}|[0-9]{1,2}\.[0-9]{1,2})(?:T|[,:; ]+))?)([0-9]{1,2}:[0-9]{2}(?::[0-9]{2})?)([^ ]*)(.*)$`)
var clockOffsetRE = regexp.MustCompile(`(?i)^([0-9]{1,2}:[0-9]{2}(?::[0-9]{2})?)((?:UTC|GMT)?[+-][0-9]{1,2}(?::?[0-9]{2})?)$`)
var clockZRE = regexp.MustCompile(`(?i)^([0-9]{1,2}:[0-9]{2}(?::[0-9]{2})?)(Z|UTC|GMT|CET|CEST)$`)
var tzRegions = map[string]bool{
	"africa": true, "america": true, "antarctica": true, "arctic": true, "asia": true,
	"atlantic": true, "australia": true, "europe": true, "indian": true, "pacific": true, "etc": true,
	"us": true, "canada": true, "mexico": true, "brazil": true, "chile": true,
}
var knownZones = map[string]bool{
	"MSK": true, "CET": true, "CEST": true, "EET": true, "EEST": true, "WET": true, "WEST": true, "BST": true,
	"GMT": true, "EST": true, "EDT": true, "CST": true, "CDT": true, "MST": true, "MDT": true,
	"PST": true, "PDT": true, "IST": true, "JST": true, "AKST": true, "AKDT": true, "HST": true,
	"AST": true, "ADT": true, "AEST": true, "AEDT": true, "ACST": true, "ACDT": true, "AWST": true,
	"HKT": true, "KST": true, "PKT": true, "NPT": true, "NZST": true, "NZDT": true, "SAST": true,
}

// ZoneError classifies an unsupported leading zone. "utc-space" is the special UTC/GMT spacing error.
func ZoneError(value string) string {
	_, problem := normalizeLeadingZone(NormalizeSpaces(value))
	return problem
}

// UnsupportedZone reports a leading zone token that cannot be translated safely.
func UnsupportedZone(value string) bool { return ZoneError(value) != "" }

func normalizeLeadingZone(value string) (string, string) {
	m := leadingClockZoneRE.FindStringSubmatch(value)
	if m == nil {
		return value, ""
	}
	prefix, clock, suffix, rest := m[1], m[2], m[3], m[4]
	if suffix != "" {
		if fractionalClockTail.MatchString(suffix) {
			return value, "fractional-seconds"
		}
		zoneToken := strings.TrimRight(suffix, ",.;:")
		punctuation := suffix[len(zoneToken):]
		if zoneToken == "" {
			return value, ""
		}
		zone, ok := attachedZone(zoneToken)
		if !ok {
			return value, "unsupported"
		}
		return prefix + clock + " " + zone + punctuation + rest, ""
	}
	trimmed := strings.TrimLeft(rest, " \t")
	if trimmed == rest || trimmed == "" {
		return value, ""
	}
	space := len(rest) - len(trimmed)
	end := strings.IndexAny(trimmed, " \t\r\n")
	if end < 0 {
		end = len(trimmed)
	}
	token, tail := trimmed[:end], trimmed[end:]
	zone, recognized, problem := separatedZone(token, tail)
	if problem != "" {
		return value, problem
	}
	if !recognized {
		return value, ""
	}
	return prefix + clock + rest[:space] + zone + tail, ""
}

func attachedZone(token string) (string, bool) {
	upper := strings.ToUpper(token)
	switch upper {
	case "Z", "UTC", "GMT", "CET", "CEST":
		return upper, true
	}
	if strings.HasPrefix(upper, "UTC+") || strings.HasPrefix(upper, "UTC-") || strings.HasPrefix(upper, "GMT+") || strings.HasPrefix(upper, "GMT-") {
		if offset, ok := parseZoneOffset(upper); ok {
			return formatOffset(offset), true
		}
		return "", false
	}
	if strings.HasPrefix(token, "+") || strings.HasPrefix(token, "-") {
		if offset, ok := parseZoneOffset(token); ok {
			return formatOffset(offset), true
		}
	}
	return "", false
}

func separatedZone(token, tail string) (string, bool, string) {
	clean := unwrapZoneToken(token)
	upper := strings.ToUpper(clean)
	if zone, recognized, problem := separatedNamedZone(clean, upper, tail); recognized || problem != "" {
		return zone, recognized, problem
	}
	if zone, recognized, problem := separatedPrefixedOffset(upper); recognized || problem != "" {
		return zone, recognized, problem
	}
	return separatedNumericOffset(clean)
}

func unwrapZoneToken(token string) string {
	clean := strings.TrimRight(token, ",.;:")
	wrappers := [][2]string{{"(", ")"}, {"[", "]"}, {"\"", "\""}, {"'", "'"}, {"«", "»"}}
	for {
		unwrapped := false
		left, right := 0, 0
		for _, pair := range wrappers {
			if left == 0 && strings.HasPrefix(clean, pair[0]) {
				left = len(pair[0])
			}
			if right == 0 && strings.HasSuffix(clean, pair[1]) {
				right = len(pair[1])
			}
		}
		if left > 0 {
			clean = clean[left:]
		}
		if right > 0 && len(clean) >= right {
			clean = clean[:len(clean)-right]
		}
		if left > 0 || right > 0 {
			unwrapped = true
		}
		if !unwrapped {
			return clean
		}
	}
}

func separatedNamedZone(clean, upper, tail string) (string, bool, string) {
	switch upper {
	case "Z", "UTC", "GMT", "CET", "CEST":
		hasSeparatedSignedOffset := startsSignedNumber(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(tail), ",.;:")))
		if (upper == "UTC" || upper == "GMT") && hasSeparatedSignedOffset {
			return "", false, "utc-space"
		}
		return upper, true, ""
	}
	if strings.EqualFold(clean, "МСК") || isTZRegionToken(clean) || (knownZones[clean] && clean == upper) {
		return "", false, "unsupported"
	}
	return "", false, ""
}

func isTZRegionToken(value string) bool {
	region, _, ok := strings.Cut(value, "/")
	return ok && tzRegions[strings.ToLower(region)]
}

func separatedPrefixedOffset(upper string) (string, bool, string) {
	if strings.HasPrefix(upper, "UTC+") || strings.HasPrefix(upper, "UTC-") || strings.HasPrefix(upper, "GMT+") || strings.HasPrefix(upper, "GMT-") {
		if offset, ok := parseZoneOffset(upper); ok {
			return formatOffset(offset), true, ""
		}
		return "", false, "unsupported"
	}
	return "", false, ""
}

func separatedNumericOffset(clean string) (string, bool, string) {
	if strings.HasPrefix(clean, "+") || strings.HasPrefix(clean, "-") {
		if len(clean) == 1 || clean[1] < '0' || clean[1] > '9' {
			return "", false, ""
		}
		if offsetTokenIsText(clean) {
			return "", false, ""
		}
		if !offsetTokenShape(clean) {
			return "", false, "unsupported"
		}
		offset, ok := parseZoneOffset(clean)
		if !ok {
			return "", false, "unsupported"
		}
		return formatOffset(offset), true, ""
	}
	return "", false, ""
}

func startsSignedNumber(value string) bool {
	return len(value) > 1 && (value[0] == '+' || value[0] == '-') && value[1] >= '0' && value[1] <= '9'
}

func offsetTokenIsText(value string) bool {
	if len(value) == 2 && (value[0] == '+' || value[0] == '-') && value[1] >= '0' && value[1] <= '9' {
		return true
	}
	return false
}

func offsetTokenShape(value string) bool {
	if len(value) < 2 || (value[0] != '+' && value[0] != '-') {
		return false
	}
	body := value[1:]
	if strings.Contains(body, ":") {
		parts := strings.Split(body, ":")
		return len(parts) == 2 && (len(parts[0]) == 1 || len(parts[0]) == 2) && len(parts[1]) == 2 && allDigits(parts[0]) && allDigits(parts[1])
	}
	return len(body) == 2 || len(body) == 4 && allDigits(body)
}

func allDigits(value string) bool {
	_, ok := parseDecimal(value)
	return ok
}

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
	wait = NormalizeSpaces(wait)
	var zoneError string
	wait, zoneError = normalizeLeadingZone(wait)
	if zoneError != "" {
		return time.Time{}, false
	}
	f := strings.Fields(wait)
	if len(f) == 0 || waitAgo.MatchString(f[0]) {
		return time.Time{}, false
	}
	// «2026-10-31 17:30: текст», «2026-11-06, после…» — знак после срока к сроку не относится.
	for i := range min(2, len(f)) {
		f[i] = strings.TrimRight(f[i], ":,;.")
	}
	if HasDatedGuessedTime(wait) {
		return time.Time{}, false
	}
	if t, ok := deadlineLeadingClock(f, now); ok {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, f[0]); err == nil {
		return t, true
	}
	if t, ok := deadlineISO(f[0]); ok {
		return t, true
	}
	if m := waitISO.FindStringSubmatch(f[0]); m != nil {
		f = append([]string{m[1], m[2]}, f[1:]...)
	}
	if t, ok := deadlineDate(wait, now); ok {
		return t, true
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

func deadlineLeadingClock(fields []string, now time.Time) (time.Time, bool) {
	if m := clockOffsetRE.FindStringSubmatch(fields[0]); m != nil {
		if offset, ok := parseZoneOffset(m[2]); ok {
			if t, err := time.Parse(clockLayout(m[1])+"Z07:00", m[1]+formatOffset(offset)); err == nil {
				y, month, day := now.Date()
				return time.Date(y, month, day, t.Hour(), t.Minute(), t.Second(), 0, t.Location()), true
			}
		}
	}
	if m := clockZRE.FindStringSubmatch(fields[0]); m != nil {
		t, err := time.Parse(clockLayout(m[1])+"Z07:00", m[1]+"+00:00")
		if err == nil {
			y, month, day := now.Date()
			return time.Date(y, month, day, t.Hour(), t.Minute(), t.Second(), 0, time.UTC), true
		}
	}
	if !clockLike.MatchString(fields[0]) || len(fields) < 2 {
		return time.Time{}, false
	}
	t, err := Parse(fields[0], now)
	if err != nil {
		return time.Time{}, false //nolint:error-masked-as-false-bool // Not a leading clock; try the remaining deadline forms.
	}
	return deadlineZone(t, strings.Join(fields[1:], " ")), true
}

func deadlineISO(value string) (time.Time, bool) {
	m := waitISO.FindStringSubmatch(value)
	if m == nil || m[3] == "" {
		return time.Time{}, false
	}
	zone := m[3]
	if strings.EqualFold(zone, "Z") {
		zone = "+00:00"
	} else {
		offset, ok := parseZoneOffset(zone)
		if !ok {
			return time.Time{}, false
		}
		zone = formatOffset(offset)
	}
	t, err := time.Parse("2006-01-02T"+clockLayout(m[2])+"Z07:00", m[1]+"T"+m[2]+zone)
	return t, err == nil
}

func deadlineDate(wait string, now time.Time) (time.Time, bool) {
	if parsed, ok := ParseDayMonth(wait); ok {
		if t, ok := closestDayMonth(parsed, now); ok {
			return deadlineZone(t, parsed.Rest), true
		}
	}
	m := isoDatePattern.FindStringSubmatch(wait)
	if m == nil || (m[3] != "" && !strings.ContainsRune(" :,;.", rune(m[3][0]))) {
		return time.Time{}, false
	}
	clock, rest := splitZone(m[2], m[3])
	t, err := Parse(strings.TrimSpace(m[1]+" "+clock), now)
	if err != nil {
		return time.Time{}, false //nolint:error-masked-as-false-bool // Invalid date or clock means this input has no parsed deadline.
	}
	return deadlineZone(t, rest), true
}

// splitZone takes a zone glued to the hours («9:00Z») off the clock and puts it before the rest,
// where deadlineZone reads it as it reads «9:00 Z».
func splitZone(clock, rest string) (string, string) {
	bare := clock
	upper := strings.ToUpper(clock)
	i := strings.IndexAny(clock, "Zz+-")
	for _, prefix := range []string{"UTC", "GMT"} {
		if j := strings.Index(upper, prefix); j > 0 && (i < 0 || j < i) {
			i = j
		}
	}
	if i > 0 {
		bare = clock[:i]
		rest = clock[i:] + rest
	}
	return bare, rest
}

func clockLayout(clock string) string {
	if strings.Count(clock, ":") == 2 {
		return "15:04:05"
	}
	return "15:04"
}

func parseZoneOffset(zone string) (int, bool) {
	zone = strings.ToUpper(zone)
	zone = strings.TrimPrefix(strings.TrimPrefix(zone, "UTC"), "GMT")
	if len(zone) < 2 || (zone[0] != '+' && zone[0] != '-') {
		return 0, false
	}
	sign := 1
	if zone[0] == '-' {
		sign = -1
	}
	value := zone[1:]
	var hour, minute int
	var ok bool
	if before, after, hasMinute := strings.Cut(value, ":"); hasMinute {
		if len(after) != 2 || len(before) > 2 {
			return 0, false
		}
		hour, ok = parseDecimal(before)
		if ok {
			minute, ok = parseDecimal(after)
		}
	} else if len(value) == 4 {
		hour, ok = parseDecimal(value[:2])
		if ok {
			minute, ok = parseDecimal(value[2:])
		}
	} else if len(value) > 2 {
		return 0, false
	} else {
		hour, ok = parseDecimal(value)
	}
	if !ok {
		return 0, false
	}
	if hour > 14 || minute > 59 || hour == 14 && minute != 0 {
		return 0, false
	}
	return sign * (hour*3600 + minute*60), true
}

func parseDecimal(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	n := 0
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		n = n*10 + int(digit-'0')
	}
	return n, true
}

func formatOffset(seconds int) string {
	sign := "+"
	if seconds < 0 {
		sign = "-"
		seconds = -seconds
	}
	return fmt.Sprintf("%s%02d:%02d", sign, seconds/3600, seconds%3600/60)
}

// deadlineZone применяет UTC после разобранного срока, если метка указана.
func deadlineZone(t time.Time, rest string) time.Time {
	if fields := strings.Fields(strings.TrimLeft(rest, " :,;")); len(fields) > 0 {
		zone := strings.TrimRight(fields[0], ",.;:")
		if len(zone) >= 2 && zone[0] == '(' && zone[len(zone)-1] == ')' {
			zone = zone[1 : len(zone)-1]
		}
		upper := strings.ToUpper(zone)
		if upper == "UTC" || upper == "Z" || upper == "GMT" {
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
		}
		if offset, ok := parseZoneOffset(zone); ok && (offsetTokenShape(zone) || strings.HasPrefix(upper, "UTC+") || strings.HasPrefix(upper, "UTC-") || strings.HasPrefix(upper, "GMT+") || strings.HasPrefix(upper, "GMT-")) {
			canonical := formatOffset(offset)
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.FixedZone(canonical, offset))
		}
		if upper == "CET" || upper == "CEST" {
			offset := 3600
			if upper == "CEST" {
				offset = 2 * 3600
			}
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.FixedZone(upper, offset))
		}
	}
	return t
}

// ParseDayMonth распознаёт ДД.ММ[ ЧЧ:ММ[:СС]] и принимает только границу перед текстом.
func ParseDayMonth(value string) (DayMonth, bool) {
	var problem string
	value, problem = normalizeLeadingZone(value)
	if problem != "" {
		return DayMonth{}, false
	}
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
