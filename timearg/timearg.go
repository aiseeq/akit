// Package timearg разбирает время из аргумента утилиты: «назад» (30m), часы сегодня (12:05), день, день с часами, RFC3339.
package timearg

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Parse разбирает время из аргумента: «назад» (30m, 2h, 3d), часы сегодня (12:05), день (25.10, 2026-10-25),
// день с часами («25.10 12:05») или RFC3339. Время без пояса — местное.
func Parse(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if m := agoArg.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("time %q: %w", s, err)
		}
		return now.Add(-time.Duration(n) * agoUnits[m[2]]), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	loc := now.Location()
	year := strconv.Itoa(now.Year())
	layouts := []struct{ layout, prefix string }{
		{"15:04", now.Format("2006-01-02 ")},
		{"15:04:05", now.Format("2006-01-02 ")},
		{"02.01", ""},
		{"02.01 15:04", ""},
		{"02.01 15:04:05", ""},
		{"2006-01-02", ""},
		{"2006-01-02 15:04", ""},
		{"2006-01-02 15:04:05", ""},
	}
	for _, l := range layouts {
		layout, value := l.layout, s
		if l.prefix != "" {
			layout, value = "2006-01-02 "+l.layout, l.prefix+s
		} else if strings.HasPrefix(l.layout, "02.01") {
			layout, value = l.layout+" 2006", s+" "+year
		}
		if t, err := time.ParseInLocation(layout, value, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("time %q: use 12:05, 25.10, \"25.10 12:05\", 2026-10-25T10:05:00Z or 30m/2h/3d", s)
}

// agoArg — «N единиц назад»: 90s, 30m, 2h, 3d.
var agoArg = regexp.MustCompile(`^([0-9]{1,6})([smhd])$`)

var agoUnits = map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}
