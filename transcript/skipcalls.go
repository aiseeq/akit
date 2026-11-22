package transcript

import (
	"bytes"
	"strings"
)

// skipCall — вызов в строке, которую Follow пропускает (SkipThrough), запоминается сразу, как его запомнил бы
// flushCalls после всех отложенных: имена по id из разбора строки отдельным парсером. Результаты пропущенных строк
// не разбираются, поэтому порядок применения тот же. Простую строку Claude читает toolCalls без json.Unmarshal.
func (rd *reader) skipCall(raw []byte, plainType bool) {
	if !bytes.Contains(raw, []byte(`"tool_use"`)) && (plainType || !MayHideType(raw)) {
		return
	}
	if len(raw) > 64<<20 {
		rd.rememberTools(raw)
		return
	}
	calls, ok := toolCalls(raw)
	if !ok {
		tmp := reader{claude: newClaudeParser(), toolNames: make(map[string]struct{})}
		tmp.rememberTools(raw)
		for id, name := range tmp.claude.tools {
			calls = append(calls, savedCall{id, name})
		}
	}
	for _, c := range calls {
		rd.claude.tools[c.id] = c.name
		rd.toolNames[c.name] = struct{}{}
	}
}

// toolCalls — вызовы инструментов записи Claude: id и имя частей tool_use в message.content записи assistant, как
// их берёт claudeParser. ok false — строка не так проста, чтобы сказать без json.Unmarshal: ключ с escape или
// не-ASCII, нужный ключ в другом регистре или дважды, у частей content поле не того типа, id, имя или тип с escape,
// синтаксис не JSON или вложенность глубже предела. Типы прочих полей записи и message не проверяются: Claude Code
// пишет их одними и теми же.
func toolCalls(raw []byte) (calls []savedCall, ok bool) {
	s := &jsonWalk{b: raw}
	var typ string
	var msgCalls []savedCall
	payload := false
	seen := map[string]bool{}
	if !s.object(func(key string) bool {
		if !s.claim(seen, key, "type", "message", "payload") {
			return false
		}
		switch key {
		case "type":
			v, isStr := s.plainString()
			typ = v
			return isStr
		case "message":
			if s.peek() != '{' {
				return s.null() // другой тип — ошибка разбора записи, её решает json.Unmarshal
			}
			var good bool
			msgCalls, good = s.message()
			return good
		case "payload":
			if s.peek() == 'n' {
				return s.null()
			}
			payload = true
		}
		return s.value(0)
	}) {
		return nil, false
	}
	if s.ws(); s.i != len(s.b) {
		return nil, false
	}
	if payload || typ != "assistant" {
		return nil, true
	}
	return msgCalls, true
}

// message — объект message: вызовы из его content.
func (s *jsonWalk) message() ([]savedCall, bool) {
	var calls []savedCall
	seen := map[string]bool{}
	good := s.object(func(key string) bool {
		if !s.claim(seen, key, "content") {
			return false
		}
		if key != "content" || s.peek() != '[' {
			return s.value(1)
		}
		return s.array(func() bool {
			c, isCall, ok := s.part()
			if isCall {
				calls = append(calls, c)
			}
			return ok
		})
	})
	return calls, good
}

// part — часть content: объект, у которого все поля part (claude.go) нужного типа; иначе — false, и строку
// разбирает json.Unmarshal (ошибка типа в любой части — у неё нет вызовов вовсе).
func (s *jsonWalk) part() (c savedCall, isCall, ok bool) {
	if s.peek() != '{' {
		return c, false, false
	}
	var typ string
	seen := map[string]bool{}
	ok = s.object(func(key string) bool {
		if !s.claim(seen, key, "type", "text", "thinking", "name", "id", "input", "tool_use_id", "content", "is_error") {
			return false
		}
		switch key {
		case "type", "name", "id":
			if s.peek() == 'n' {
				return s.null()
			}
			v, isStr := s.plainString()
			switch key {
			case "type":
				typ = v
			case "id":
				c.id = v
			default:
				c.name = v
			}
			return isStr
		case "text", "thinking", "tool_use_id":
			if s.peek() == 'n' {
				return s.null()
			}
			return s.peek() == '"' && s.str()
		case "is_error":
			return s.boolOrNull()
		}
		return s.value(2)
	})
	return c, typ == "tool_use", ok
}

// claim: ключ без escape и не-ASCII; нужный ключ — только в своём регистре и один раз (json.Unmarshal сравнивает
// ключи без регистра, а повтор сливает или заменяет значение).
func (s *jsonWalk) claim(seen map[string]bool, key string, fields ...string) bool {
	if s.escaped {
		return false
	}
	low := strings.ToLower(key)
	for _, f := range fields {
		if low == f {
			if key != f || seen[f] {
				return false
			}
			seen[f] = true
		}
	}
	return true
}

// jsonWalk проходит JSON без декодирования; ok false на любом отступлении от синтаксиса.
type jsonWalk struct {
	b       []byte
	i       int
	escaped bool // у последней прочитанной строки был escape или не-ASCII
}

const walkDepth = 1000

func (s *jsonWalk) ws() {
	for s.i < len(s.b) && (s.b[s.i] == ' ' || s.b[s.i] == '\t' || s.b[s.i] == '\r' || s.b[s.i] == '\n') {
		s.i++
	}
}

func (s *jsonWalk) peek() byte {
	s.ws()
	if s.i == len(s.b) {
		return 0
	}
	return s.b[s.i]
}

// object проходит объект; field читает значение ключа key (позиция — на значении).
func (s *jsonWalk) object(field func(key string) bool) bool {
	if s.peek() != '{' {
		return false
	}
	s.i++
	if s.peek() == '}' {
		s.i++
		return true
	}
	for {
		if s.peek() != '"' {
			return false
		}
		from := s.i + 1
		if !s.str() {
			return false
		}
		key := string(s.b[from : s.i-1])
		if s.peek() != ':' {
			return false
		}
		s.i++
		s.ws()
		if !field(key) {
			return false
		}
		switch s.peek() {
		case ',':
			s.i++
		case '}':
			s.i++
			return true
		default:
			return false
		}
	}
}

func (s *jsonWalk) array(elem func() bool) bool {
	if s.peek() != '[' {
		return false
	}
	s.i++
	if s.peek() == ']' {
		s.i++
		return true
	}
	for {
		if !elem() {
			return false
		}
		switch s.peek() {
		case ',':
			s.i++
		case ']':
			s.i++
			return true
		default:
			return false
		}
	}
}

// str проходит строку от кавычки; escaped — был ли в ней escape или не-ASCII.
func (s *jsonWalk) str() bool {
	s.i++ // открывающая кавычка
	s.escaped = false
	for s.i < len(s.b) {
		c := s.b[s.i]
		switch {
		case c == '"':
			s.i++
			return true
		case c == '\\':
			s.escaped = true
			if s.i+1 >= len(s.b) {
				return false
			}
			switch s.b[s.i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				s.i += 2
			case 'u':
				if s.i+6 > len(s.b) || !isHex4(s.b[s.i+2:s.i+6]) {
					return false
				}
				s.i += 6
			default:
				return false
			}
		case c < 0x20:
			return false
		default:
			if c >= 0x80 {
				s.escaped = true
			}
			s.i++
		}
	}
	return false
}

func isHex4(b []byte) bool {
	for _, c := range b {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// plainString — строка без escape и не-ASCII: её значение — байты как есть.
func (s *jsonWalk) plainString() (string, bool) {
	if s.peek() != '"' {
		return "", false
	}
	from := s.i + 1
	if !s.str() || s.escaped {
		return "", false
	}
	return string(s.b[from : s.i-1]), true
}

func (s *jsonWalk) literal(word string) bool {
	if !bytes.HasPrefix(s.b[s.i:], []byte(word)) {
		return false
	}
	s.i += len(word)
	return true
}

func (s *jsonWalk) null() bool { return s.peek() == 'n' && s.literal("null") }

func (s *jsonWalk) boolOrNull() bool {
	switch s.peek() {
	case 't':
		return s.literal("true")
	case 'f':
		return s.literal("false")
	case 'n':
		return s.literal("null")
	}
	return false
}

// value проходит любое значение.
func (s *jsonWalk) value(depth int) bool {
	if depth > walkDepth {
		return false
	}
	switch c := s.peek(); {
	case c == '"':
		return s.str()
	case c == '{':
		return s.object(func(string) bool { return s.value(depth + 1) })
	case c == '[':
		return s.array(func() bool { return s.value(depth + 1) })
	case c == 't' || c == 'f' || c == 'n':
		return s.boolOrNull()
	case c == '-' || '0' <= c && c <= '9':
		return s.number()
	}
	return false
}

// number — число по грамматике JSON.
func (s *jsonWalk) number() bool {
	digits := func() int {
		n := 0
		for s.i < len(s.b) && '0' <= s.b[s.i] && s.b[s.i] <= '9' {
			s.i++
			n++
		}
		return n
	}
	if s.b[s.i] == '-' {
		s.i++
	}
	if s.i < len(s.b) && s.b[s.i] == '0' {
		s.i++
	} else if digits() == 0 {
		return false
	}
	if s.i < len(s.b) && s.b[s.i] == '.' {
		s.i++
		if digits() == 0 {
			return false
		}
	}
	if s.i < len(s.b) && (s.b[s.i] == 'e' || s.b[s.i] == 'E') {
		s.i++
		if s.i < len(s.b) && (s.b[s.i] == '+' || s.b[s.i] == '-') {
			s.i++
		}
		if digits() == 0 {
			return false
		}
	}
	return true
}
