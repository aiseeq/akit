package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/timearg"
)

func TestGuidePrintsBothSections(t *testing.T) {
	var out, errout bytes.Buffer
	if code := runMain([]string{"guide"}, time.Now(), &out, &errout); code != 0 {
		t.Fatalf("guide code=%d stderr=%q", code, errout.String())
	}
	for _, text := range []string{
		"Зависимости", "tk new A --blocks B", "tk set A --unblocks B",
		"уровня B", "внутри группы приоритета", "Блокирует", "Заблокирован",
		"Ожидание", "tk wait ID --until", "прошедший день в ДД.ММ",
		"ISO с T принимается", "со смещением или без него", "T можно писать и после ДД.ММ", "RFC3339 принимается",
		"с целыми секундами; дробные секунды дают ошибку",
		"07:30+3 означает +03:00", "CET —", "UTC/GMT со смещением",
		"строчные и смешанные слова", "«ёлочками»", "«2h»",
		"Слитный диапазон ЧЧ:ММ-ЧЧ:ММ", "тире через пробелы (10:00 - 11:00)",
		"Слитное тире после времени", "10:00–11:00, 10:00—11:00", "тире через пробелы (10:00 - 11:00)",
		"Слитное -12 без минут неоднозначно", "областей Africa",
		"Etc, US, Canada, Mexico, Brazil и Chile", "«МСК» и «мск» в любом регистре",
		"tk ls --due", "tk start ID",
	} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("guide output lacks %q: %s", text, out.String())
		}
	}
	if strings.Contains(out.String(), "ЧЧ:ММ: текст") {
		t.Errorf("guide recommends a clock-only wait: %s", out.String())
	}
	var help, erroutHelp bytes.Buffer
	if code := runMain([]string{"-h"}, time.Now(), &help, &erroutHelp); code != 0 || !strings.Contains(help.String(), "tk guide") {
		t.Fatalf("tk -h omits guide: code=%d stdout=%q stderr=%q", code, help.String(), erroutHelp.String())
	}
}

func TestGuideHelpAndExtraArguments(t *testing.T) {
	var guideHelp, otherHelp, guideErr, otherErr bytes.Buffer
	if code := runMain([]string{"guide", "-h"}, time.Now(), &guideHelp, &guideErr); code != 0 {
		t.Fatalf("guide -h code=%d stderr=%q", code, guideErr.String())
	}
	if code := runMain([]string{"new", "-h"}, time.Now(), &otherHelp, &otherErr); code != 0 {
		t.Fatalf("new -h code=%d stderr=%q", code, otherErr.String())
	}
	if guideHelp.String() != otherHelp.String() {
		t.Fatalf("guide -h differs from other subcommand help")
	}
	if code := runMain([]string{"guide", "extra"}, time.Now(), &guideHelp, &guideErr); code != exitUsage {
		t.Fatalf("guide with extra argument code=%d, want %d", code, exitUsage)
	}
}

func TestGuideExamplesWork(t *testing.T) {
	_, dir := testRunMainRoot(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	gateDir := filepath.Join(home, ".local", "state", "queue")
	if err := os.MkdirAll(gateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gate, err := json.Marshal(map[string]any{"at": time.Now().Format(time.RFC3339), "open_by_root": map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gateDir, "gate.json"), gate, 0o644); err != nil {
		t.Fatal(err)
	}
	writeRunMainTask(t, dir, "20200101-1000-MED-a.md", "# A\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1001-HIGH-b.md", "# B\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1002-LOW-d.md", "# D\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1003-HIGH-e.md", "# E\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	call := func(args []string, now time.Time) string {
		t.Helper()
		var out, errout bytes.Buffer
		if code := runMain(args, now, &out, &errout); code != 0 {
			t.Fatalf("%v code=%d stdout=%q stderr=%q", args, code, out.String(), errout.String())
		}
		return out.String()
	}
	writeClock := time.Now()
	call([]string{"set", "a", "--blocks", "b", "--by", "tester"}, writeClock)
	a, err := find(dir, "a")
	if err != nil || a.prio != "HIGH" {
		t.Fatalf("--blocks did not raise A to B's priority: A=%#v err=%v", a, err)
	}
	b, err := find(dir, "b")
	if err != nil || b.prio != "HIGH" {
		t.Fatalf("--blocks changed B's priority: B=%#v err=%v", b, err)
	}
	call([]string{"new", "c", "--prio", "LOW", "--blocks", "b", "--by", "tester"}, writeClock)
	c, err := find(dir, "c")
	if err != nil || c.prio != "HIGH" {
		t.Fatalf("new --blocks did not raise blocker C to B's priority: C=%#v err=%v", c, err)
	}
	b, err = find(dir, "b")
	if err != nil || b.prio != "HIGH" {
		t.Fatalf("new --blocks changed B's priority: B=%#v err=%v", b, err)
	}
	showA := call([]string{"show", "a"}, writeClock)
	showB := call([]string{"show", "b"}, writeClock)
	if !strings.Contains(showA, "Блокирует: b") || !strings.Contains(showB, "Заблокирован: a") || !strings.Contains(showB, "Заблокирован: c") {
		t.Fatalf("show did not print each ticket's side of the links: A=%q B=%q", showA, showB)
	}
	list := call([]string{"ls"}, writeClock)
	if line := guideLineForSlug(list, "b"); line == "" || !strings.Contains(line, "заблокирован") {
		t.Fatalf("ls did not mark B as blocked: %q", list)
	}
	call([]string{"start", "a", "--by", "other"}, writeClock)
	call([]string{"start", "c", "--by", "other"}, writeClock)
	if next := call([]string{"next", "--by", "tester"}, writeClock); guideFirstField(next) != "e" {
		t.Fatalf("within the HIGH group, unblocked E should precede blocked B: %q", next)
	}
	call([]string{"start", "e", "--by", "other"}, writeClock)
	if next := call([]string{"next", "--by", "tester"}, writeClock); guideFirstField(next) != "b" {
		t.Fatalf("higher priority blocked B did not precede lower priority D: %q", next)
	}
	call([]string{"set", "a", "--unblocks", "b", "--by", "tester"}, writeClock)
	showA = call([]string{"show", "a"}, writeClock)
	showB = call([]string{"show", "b"}, writeClock)
	if strings.Contains(showA, "Блокирует: b") || strings.Contains(showB, "Заблокирован: a") || !strings.Contains(showB, "Заблокирован: c") {
		t.Fatalf("--unblocks did not remove only A → B: A=%q B=%q", showA, showB)
	}
	call([]string{"start", "b", "--by", "other"}, writeClock)
	call([]string{"start", "d", "--by", "other"}, writeClock)
	call([]string{"new", "waiter", "--prio", "HIGH", "--why", "test", "--by", "tester"}, writeClock)
	futureUntil := writeClock.Add(time.Hour).Format("2006-01-02 15:04") + ": проверка"
	call([]string{"wait", "waiter", "--until", futureUntil, "--by", "tester"}, writeClock)
	if due := call([]string{"ls", "--due"}, writeClock); guideLineForSlug(due, "waiter") != "" {
		t.Fatalf("future wait appeared in ls --due: %q", due)
	}
	var futureNext, futureErr bytes.Buffer
	if code := runMain([]string{"next", "--by", "tester"}, writeClock, &futureNext, &futureErr); code != exitNone || guideFirstField(futureNext.String()) == "waiter" {
		t.Fatalf("future wait appeared in tk next: code=%d stdout=%q stderr=%q", code, futureNext.String(), futureErr.String())
	}
	// The injected clock is used when writing --until. ls and next read time.Now
	// directly, so cover their due behavior with a separately written past date.
	pastUntil := time.Now().Add(-time.Hour).Format("2006-01-02 15:04") + ": проверка"
	call([]string{"wait", "waiter", "--until", pastUntil, "--by", "tester"}, time.Now())
	if due := call([]string{"ls", "--due"}, time.Now()); guideLineForSlug(due, "waiter") == "" {
		t.Fatalf("past wait omitted from ls --due: %q", due)
	}
	if next := call([]string{"next", "--by", "tester"}, time.Now()); guideFirstField(next) != "waiter" {
		t.Fatalf("due wait omitted from tk next: %q", next)
	}
	call([]string{"start", "waiter", "--by", "tester"}, time.Now())
	if shown := call([]string{"show", "waiter"}, time.Now()); strings.Contains(shown, "Ждёт: ") {
		t.Fatalf("tk start did not remove the wait: %q", shown)
	}
	call([]string{"wait", "waiter", "--until", "2h", "--by", "tester"}, time.Now())
	if due := call([]string{"ls", "--due"}, time.Now()); guideLineForSlug(due, "waiter") != "" {
		t.Fatalf("relative condition appeared in ls --due: %q", due)
	}
	if shown := call([]string{"show", "waiter"}, time.Now()); !strings.Contains(shown, "Ждёт: 2h") {
		t.Fatalf("relative wait was not stored as a condition: %q", shown)
	}
}

func TestGuideProjectNameBlockReference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "git", "current")
	remote := filepath.Join(home, "git", "remote")
	currentDir := filepath.Join(current, "docs", "tasks")
	remoteDir := filepath.Join(remote, "docs", "tasks")
	if err := os.MkdirAll(currentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunMainTask(t, currentDir, "20200101-1000-MED-source.md", "# Source\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, remoteDir, "20200101-1001-HIGH-target.md", "# Target\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	t.Chdir(current)
	var out, errout bytes.Buffer
	if code := runMain([]string{"set", "source", "--blocks", "remote/target", "--by", "tester"}, time.Now(), &out, &errout); code != 0 {
		t.Fatalf("--blocks project/slug code=%d stdout=%q stderr=%q", code, out.String(), errout.String())
	}
	source, err := find(currentDir, "source")
	if err != nil || source.prio != "HIGH" || !strings.Contains(source.body, "Блокирует: git/remote/target") {
		t.Fatalf("project/slug did not resolve as a blocking link: task=%#v err=%v", source, err)
	}
	target, err := find(remoteDir, "target")
	if err != nil || !strings.Contains(target.body, "Заблокирован: git/current/source") {
		t.Fatalf("project/slug did not write the reverse link: task=%#v err=%v", target, err)
	}
}

func TestGuideRepeatedBlocks(t *testing.T) {
	_, dir := testRunMainRoot(t)
	writeRunMainTask(t, dir, "20200101-1000-MED-source.md", "# Source\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1001-HIGH-first.md", "# First\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1002-MED-second.md", "# Second\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	var out, errout bytes.Buffer
	if code := runMain([]string{"set", "source", "--blocks", "first", "--blocks", "second", "--by", "tester"}, time.Now(), &out, &errout); code != 0 {
		t.Fatalf("repeated --blocks code=%d stdout=%q stderr=%q", code, out.String(), errout.String())
	}
	source, err := find(dir, "source")
	if err != nil || !strings.Contains(source.body, "Блокирует: first") || !strings.Contains(source.body, "Блокирует: second") {
		t.Fatalf("repeated --blocks did not add both outgoing links: task=%#v err=%v", source, err)
	}
	for _, slug := range []string{"first", "second"} {
		target, err := find(dir, slug)
		if err != nil || !strings.Contains(target.body, "Заблокирован: source") {
			t.Errorf("repeated --blocks did not add reverse link to %s: task=%#v err=%v", slug, target, err)
		}
	}
}

func TestGuideWaitDateAndZoneExamples(t *testing.T) {
	_, dir := testRunMainRoot(t)
	writeRunMainTask(t, dir, "20200101-1000-MED-date.md", "# Date\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1001-MED-offset.md", "# Offset\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1002-MED-short.md", "# Short\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1003-MED-utc.md", "# UTC\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1004-MED-z.md", "# Z\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1005-MED-spaceoffset.md", "# Space offset\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	writeRunMainTask(t, dir, "20200101-1006-MED-impossible.md", "# Impossible\n\nСтатус: открыт\n\n## Приёмка\n- check\n\n## Журнал\n")
	call := func(args []string, now time.Time) {
		t.Helper()
		var out, errout bytes.Buffer
		if code := runMain(args, now, &out, &errout); code != 0 {
			t.Fatalf("%v code=%d stdout=%q stderr=%q", args, code, out.String(), errout.String())
		}
	}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.Local)
	call([]string{"wait", "date", "--until", "15.11 06:40: проверка", "--by", "tester"}, now)
	dateTask, err := find(dir, "date")
	if err != nil || waiting(dateTask.body) != "2027-11-15 06:40: проверка" {
		t.Fatalf("past DD.MM was not moved to next year: wait=%q err=%v", waiting(dateTask.body), err)
	}
	call([]string{"wait", "offset", "--until", "2026-11-17T06:40:00+03:00 проверка", "--by", "tester"}, now)
	offsetTask, err := find(dir, "offset")
	if err != nil {
		t.Fatal(err)
	}
	deadline, ok := timearg.Deadline(waiting(offsetTask.body), now)
	wantOffset := time.Date(2026, 11, 17, 6, 40, 0, 0, time.FixedZone("+03:00", 3*60*60))
	if !ok || !deadline.Equal(wantOffset) {
		t.Fatalf("RFC3339 offset with seconds deadline=%v ok=%v, want %v", deadline, ok, wantOffset)
	}
	call([]string{"wait", "utc", "--until", "2026-11-17 06:40 UTC проверка", "--by", "tester"}, now)
	call([]string{"wait", "z", "--until", "2026-11-17 06:40Z проверка", "--by", "tester"}, now)
	for _, slug := range []string{"utc", "z"} {
		task, err := find(dir, slug)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := timearg.Deadline(waiting(task.body), now)
		if want := time.Date(2026, 11, 17, 6, 40, 0, 0, time.UTC); !ok || !got.Equal(want) {
			t.Errorf("%s deadline=%v ok=%v, want %v", slug, got, ok, want)
		}
	}
	call([]string{"wait", "spaceoffset", "--until", "2026-11-17 06:40 +03:00 проверка", "--by", "tester"}, now)
	spaceOffsetTask, err := find(dir, "spaceoffset")
	if err != nil {
		t.Fatal(err)
	}
	spaceDeadline, ok := timearg.Deadline(waiting(spaceOffsetTask.body), now)
	wantSpaceOffset := time.Date(2026, 11, 17, 6, 40, 0, 0, time.FixedZone("+03:00", 3*3600))
	if !ok || !spaceDeadline.Equal(wantSpaceOffset) {
		t.Fatalf("space-separated offset deadline=%v ok=%v want %v", spaceDeadline, ok, wantSpaceOffset)
	}
	call([]string{"wait", "short", "--until", "2026-11-17T06:40+03:00 проверка", "--by", "tester"}, now)
	shortTask, err := find(dir, "short")
	if err != nil {
		t.Fatal(err)
	}
	shortDeadline, ok := timearg.Deadline(waiting(shortTask.body), now)
	wantShort := time.Date(2026, 11, 17, 6, 40, 0, 0, time.FixedZone("+03:00", 3*3600))
	if !ok || !shortDeadline.Equal(wantShort) {
		t.Fatalf("RFC3339 offset without seconds deadline=%v ok=%v want %v", shortDeadline, ok, wantShort)
	}
	call([]string{"wait", "impossible", "--until", "2026-02-30 10:00 проверка", "--by", "tester"}, now)
	impossibleTask, err := find(dir, "impossible")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := timearg.Deadline(waiting(impossibleTask.body), now); ok {
		t.Fatalf("impossible date unexpectedly had a deadline: %q", waiting(impossibleTask.body))
	}
}

func guideLineForSlug(output, slug string) string {
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == slug {
			return line
		}
	}
	return ""
}

func guideFirstField(output string) string {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
