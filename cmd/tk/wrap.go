package main

import (
	"strings"
)

// showWidth — ширина строк tk show в символах. Тикет читают через cut -c1-150…400, и хвост длинной заметки
// журнала пропадал бы молча; перенос по словам оставляет текст целым, файл тикета не меняется.
const showWidth = 150

// wrapLines переносит строки длиннее width по пробелам, продолжение — с отступом строки плюс два пробела.
// Блоки ``` не трогает: команды в них копируют как есть.
func wrapLines(s string, width int) string {
	var b strings.Builder
	fenced := false
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		if fence := strings.HasPrefix(strings.TrimSpace(line), "```"); fence || fenced {
			if fence {
				fenced = !fenced
			}
			b.WriteString(line)
			continue
		}
		wrapLine(&b, line, width)
	}
	return b.String()
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
