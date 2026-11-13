package main

import (
	"strings"
)

// showWidth — ширина строк tk show в символах. Тикет читают через cut -c1-150…400, и хвост длинной заметки
// журнала пропадал бы молча; перенос по словам оставляет текст целым, файл тикета не меняется.
const showWidth = 150
const backtickFence = "\x60\x60\x60"

type listContext struct {
	markerIndent  int
	contentIndent int
}

// wrapLines переносит строки длиннее width по пробелам, продолжение — с отступом строки плюс два пробела.
// Fence-ограждения с отступом до трёх пробелов от столбца содержимого списка не переносятся.
func wrapLines(s string, width int) string {
	var b strings.Builder
	var lists []listContext
	fenced := false
	fenceBaseIndent := 0
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if fenced {
			closeIndent := indent - fenceBaseIndent
			if closeIndent >= 0 && closeIndent <= 3 && strings.HasPrefix(line[indent:], backtickFence) {
				fenced = false
			}
			b.WriteString(line)
			continue
		}
		blank := strings.TrimSpace(line) == ""
		lists = updateListContext(lists, line, indent, blank)
		contentIndent := 0
		if len(lists) > 0 && indent >= lists[len(lists)-1].contentIndent {
			contentIndent = lists[len(lists)-1].contentIndent
		}
		fenceIndent := indent - contentIndent
		fence := !blank && fenceIndent <= 3 && strings.HasPrefix(line[indent:], backtickFence)
		if fence {
			fenced = true
			fenceBaseIndent = contentIndent
			b.WriteString(line)
			continue
		}
		wrapLine(&b, line, width)
	}
	return b.String()
}

func updateListContext(lists []listContext, line string, indent int, blank bool) []listContext {
	if blank {
		return lists
	}
	markerIndent, contentIndent, isList := listItemColumns(line)
	if isList {
		for len(lists) > 0 && markerIndent <= lists[len(lists)-1].markerIndent {
			lists = lists[:len(lists)-1]
		}
		if len(lists) == 0 {
			if markerIndent <= 3 {
				lists = append(lists, listContext{markerIndent: markerIndent, contentIndent: contentIndent})
			}
			return lists
		}
		relativeIndent := markerIndent - lists[len(lists)-1].contentIndent
		if relativeIndent >= 0 && relativeIndent <= 3 {
			lists = append(lists, listContext{markerIndent: markerIndent, contentIndent: contentIndent})
		}
		return lists
	}
	for len(lists) > 0 && indent < lists[len(lists)-1].contentIndent {
		lists = lists[:len(lists)-1]
	}
	return lists
}

func listItemColumns(line string) (markerIndent, contentIndent int, ok bool) {
	markerIndent = len(line) - len(strings.TrimLeft(line, " "))
	if markerIndent >= len(line) {
		return 0, 0, false
	}
	rest := line[markerIndent:]
	markerWidth := 0
	switch rest[0] {
	case '-', '+', '*':
		markerWidth = 1
	default:
		for markerWidth < len(rest) && rest[markerWidth] >= '0' && rest[markerWidth] <= '9' && markerWidth < 9 {
			markerWidth++
		}
		if markerWidth == 0 || markerWidth >= len(rest) || (rest[markerWidth] != '.' && rest[markerWidth] != ')') {
			return 0, 0, false
		}
		markerWidth++
	}
	if markerWidth >= len(rest) || (rest[markerWidth] != ' ' && rest[markerWidth] != '\t') {
		return 0, 0, false
	}
	column := markerIndent + markerWidth
	spaces := 0
	for pos := markerWidth; pos < len(rest); pos++ {
		switch rest[pos] {
		case ' ':
			column++
			spaces++
		case '\t':
			column += 4 - column%4
		default:
			if spaces > 4 {
				column = markerIndent + markerWidth + 1
			}
			return markerIndent, column, true
		}
	}
	return markerIndent, column, true
}

func wrapLine(b *strings.Builder, line string, width int) {
	r := []rune(line)
	cont := []rune(strings.Repeat(" ", len(line)-len(strings.TrimLeft(line, " "))+2))
	// Отступ во всю ширину не оставит места под текст продолжения — такую строку не переносить.
	for len(r) > width && len(cont) < width {
		at, skip := width, 0
		for j := width; j > len(cont); j-- {
			if r[j] == ' ' {
				at, skip = j, 1
				break
			}
		}
		b.WriteString(string(r[:at]))
		b.WriteByte('\n')
		r = append(append([]rune{}, cont...), r[at+skip:]...)
	}
	b.WriteString(string(r))
}
