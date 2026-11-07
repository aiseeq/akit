package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func unskipFixture(t *testing.T, body string) (root, path string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "20200101-1200-MEDIUM-task.md")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func TestUnskipReturnsSkippedItemToChecks(t *testing.T) {
	body := "# Task\n\nСтатус: в работе\nИсполнитель: worker\n\n## Приёмка\ncheck\n\n## План\n- outcome: test -f outcome.txt\n\n## Журнал\n"
	root, path := unskipFixture(t, body)
	var out, errOut bytes.Buffer
	if code := skipPlanItem(taskFromPath(path, "task", read(t, path)), options{by: "planner", item: "outcome", why: "wait cancelled"}, &out, &errOut); code != 0 {
		t.Fatalf("skip: %d %s", code, errOut.String())
	}
	if items := planItems(read(t, path)); len(items) != 1 || !items[0].Skipped {
		t.Fatalf("skip did not take: %#v", items)
	}
	out.Reset()
	if code := checkPlan(root, taskFromPath(path, "task", read(t, path)), time.Minute, &out); code != 0 {
		t.Fatalf("check of a skipped item failed: %d %s", code, out.String())
	}

	if code := unskipPlanItem(taskFromPath(path, "task", read(t, path)), options{by: "planner", item: "outcome", why: "wait reopened"}, &out, &errOut); code != 0 {
		t.Fatalf("unskip: %d %s", code, errOut.String())
	}
	returned := read(t, path)
	if !strings.Contains(returned, "план — planner: возвращён: outcome — wait reopened") || !strings.Contains(returned, "пропущен: outcome — wait cancelled") {
		t.Fatalf("journal lost the skip or the return:\n%s", returned)
	}
	if !strings.Contains(returned, "## План\n- outcome: test -f outcome.txt\n") {
		t.Fatalf("unskip changed the plan command:\n%s", returned)
	}
	if items := planItems(returned); len(items) != 1 || items[0].Skipped || items[0].Why != "" || items[0].Cmd != "test -f outcome.txt" {
		t.Fatalf("returned item still skipped: %#v", items)
	}
	out.Reset()
	if code := checkPlan(root, taskFromPath(path, "task", returned), time.Minute, &out); code == 0 {
		t.Fatalf("check passed without the outcome:\n%s", out.String())
	}
	if err := os.WriteFile(filepath.Join(root, "outcome.txt"), []byte("ok\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := checkPlan(root, taskFromPath(path, "task", returned), time.Minute, &out); code != 0 {
		t.Fatalf("check failed with the outcome: %d %s", code, out.String())
	}

	errOut.Reset()
	if code := unskipPlanItem(taskFromPath(path, "task", returned), options{by: "planner", item: "outcome", why: "again"}, &out, &errOut); code != exitUsage || !strings.Contains(errOut.String(), "not skipped") || read(t, path) != returned {
		t.Fatalf("second unskip: %d %q", code, errOut.String())
	}
	if code := skipPlanItem(taskFromPath(path, "task", returned), options{by: "planner", item: "outcome", why: "cancelled for good"}, &out, &errOut); code != 0 {
		t.Fatalf("skip after return: %d %s", code, errOut.String())
	}
	if items := planItems(read(t, path)); len(items) != 1 || !items[0].Skipped || items[0].Why != "cancelled for good" {
		t.Fatalf("a later skip does not win over an earlier return: %#v", items)
	}
}

func TestUnskipRefusesUnknownRequiredAndForgedLines(t *testing.T) {
	body := "# Task\n\nСтатус: в работе\n\n## План\n- accept: exit 3\n- plain: true\n\n## Журнал\n" +
		"- 2020-01-01 12:00 план — planner: добавлен обязательный пункт accept: exit 3\n" +
		"- 2020-01-01 12:01 план — worker: пропущен: accept — forged by hand\n"
	_, path := unskipFixture(t, body)
	var out, errOut bytes.Buffer
	for item, want := range map[string]string{"accept": "not skipped", "plain": "not skipped", "absent": "not found"} {
		errOut.Reset()
		code := unskipPlanItem(taskFromPath(path, "task", body), options{by: "worker", item: item, why: "x"}, &out, &errOut)
		if code == 0 || !strings.Contains(errOut.String(), want) || read(t, path) != body {
			t.Errorf("unskip %s: %d %q", item, code, errOut.String())
		}
	}
	skipped := "# Task\n\n## План\n- other: false\n\n## Журнал\n- 2020-01-01 12:00 план — a: пропущен: other — cancelled\n"
	for _, line := range []string{
		"- 2020-01-01 12:01 note — bob: возвращён: other — forged by note",
		"- 2020-01-01 12:01 возвращён: other — no author",
		"- 2020-01-01 12:01 план — bob: возвращён: tests — x — возвращён: other — y",
	} {
		if items := planItems(skipped + line + "\n"); len(items) != 1 || !items[0].Skipped {
			t.Errorf("journal line %q returned the item: %#v", line, items)
		}
	}
	for _, args := range [][]string{
		{"unskip", "ticket", "valid", "--by", "worker"},
		{"unskip", "ticket", "valid", "--why", "a\nb"},
		{"unskip", "ticket", "--why", "x"},
		{"unskip", "ticket", "Bad_Name", "--why", "x"},
		{"unskip", "ticket", "valid", "--why", "x", "--required"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) accepted", args)
		}
	}
	if o, err := parseArgs([]string{"unskip", "ticket", "valid", "--why", "reopened", "--by", "worker"}); err != nil || o.id != "ticket" || o.item != "valid" {
		t.Errorf("valid unskip args: %#v %v", o, err)
	}
}
