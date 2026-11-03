package main

import (
	"fmt"
	"strings"
	"testing"
)

func toolResultLine(ts string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":"%s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`, ts)
}

// The warning names the file line of the malformed record: tool results before
// it are lines of the file too, even though --brief skips them unparsed.
func TestBriefMalformedLineCountsToolResults(t *testing.T) {
	e := testEnv(t)
	lines := []string{human("2026-10-31T10:00:00Z", "Фрагмент перед результатами")}
	for i := range 8 {
		lines = append(lines, toolResultLine(fmt.Sprintf("2026-10-31T10:00:%02dZ", i+1)))
	}
	lines = append(lines, "{broken json") // line 10
	p := writeTranscript(t, e.home, "-home-lineno", "7a000000-0000", lines...)
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10: «Фрагмент перед результатами»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "номер битой строки считает результаты инструментов", out, "malformed", "line 10:", p)
}

// A transcript over 256 KiB is read from the middle; the line number still
// counts from the start of the file.
func TestBriefMalformedLineAfterSeek(t *testing.T) {
	e := testEnv(t)
	filler := strings.Repeat("заполнение ", 40)
	var lines []string
	for i := range 1200 {
		lines = append(lines, human(fmt.Sprintf("2026-10-29T%02d:%02d:%02dZ", i/3600, i/60%60, i%60), filler))
	}
	lines = append(lines, human("2026-10-31T10:00:00Z", "Фрагмент в конце большого файла"), "{broken json")
	want := len(lines)
	p := writeTranscript(t, e.home, "-home-lineno", "7b000000-0000", lines...)
	if size := len(strings.Join(lines, "\n")); size <= 256<<10 {
		t.Fatalf("transcript is %d bytes, the test needs more than 256 KiB", size)
	}
	b := writeBrief(t, t.TempDir(), "Пользователь 31.10 10:00 UTC: «Фрагмент в конце большого файла»\n")
	_, out := briefRun(t, e, b, "--until", "2026-10-31 11:00")
	mustHave(t, "номер битой строки — от начала файла", out, "malformed", fmt.Sprintf("line %d:", want), p)
}
