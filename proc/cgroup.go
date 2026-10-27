package proc

import "strings"

// scopePrefix — начало имени scope сессии, который создаёт cl.
const scopePrefix = "agent-"

// ParseCgroup — путь из содержимого /proc/<pid>/cgroup (строка cgroup v2 "0::/path").
func ParseCgroup(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if v, ok := strings.CutPrefix(l, "0::"); ok {
			return v
		}
	}
	return ""
}

// ServiceUnit — systemd-сервис, под которым идёт процесс (компонент пути
// cgroup с .service, кроме самого user@<uid>.service), или "".
func ServiceUnit(cgroup string) string {
	for c := range strings.SplitSeq(cgroup, "/") {
		if strings.HasSuffix(c, ".service") && !strings.HasPrefix(c, "user@") {
			return c
		}
	}
	return ""
}

// ScopeName — имя сессии и pid cl из имени scope "agent-<имя>-<pid>.scope"; после n-го перезапуска
// claude cl заводит "agent-<имя>-<pid>-r<n>.scope", имя и pid те же.
// pid в имени — это cl, а не claude: pid claude смотреть в реестре сессий.
func ScopeName(scope string) (name string, pid int, ok bool) {
	s, found := strings.CutPrefix(scope, scopePrefix)
	if !found {
		return "", 0, false
	}
	s, found = strings.CutSuffix(s, ".scope")
	if !found {
		return "", 0, false
	}
	if i := strings.LastIndex(s, "-r"); i > 0 && isDigits(s[i+2:]) {
		s = s[:i]
	}
	i := strings.LastIndexByte(s, '-')
	if i <= 0 {
		return "", 0, false
	}
	digits := s[i+1:]
	if !isDigits(digits) || len(digits) > 9 {
		return "", 0, false
	}
	for j := range len(digits) {
		pid = pid*10 + int(digits[j]-'0')
	}
	return s[:i], pid, true
}

// isDigits — s непусто и из одних цифр.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for j := range len(s) {
		if s[j] < '0' || s[j] > '9' {
			return false
		}
	}
	return true
}

// SessionScope — scope сессии агента в пути cgroup: имя сессии и pid cl.
// Процесс вне scope сессии — ok = false.
func SessionScope(cgroup string) (name string, pid int, ok bool) {
	for c := range strings.SplitSeq(cgroup, "/") {
		if name, pid, ok = ScopeName(c); ok {
			return name, pid, true
		}
	}
	return "", 0, false
}
