package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransferRunsUnderLockAndRecordsOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "git", "sample")
	dir := filepath.Join(project, "docs", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "calls")
	input := filepath.Join(home, "input")
	t.Setenv("TRANSFER_MARKER", marker)
	t.Setenv("TRANSFER_INPUT", input)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TRANSFER_MARKER\"\ncat > \"$TRANSFER_INPUT\"\nprintf '%s' '{\"key\":\"EXT-7\",\"url\":\"https://example.test/EXT-7\"}'\n"
	command := filepath.Join(home, "git", ".tk-transfer")
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	name := "20261104-1200-MED-DO-task.md"
	ticketPath := filepath.Join(dir, name)
	body := "# Task\n\nСтатус: открыт\nЗаказчик: back-item\n\n## Что сделать\nbody\n\n## Приёмка\ncheck\n\n## Журнал\n"
	if err := os.WriteFile(ticketPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	o := options{cmd: "transfer", id: "task", parent: "PARENT-1", baseRoot: project, mainRoot: project, by: "tester"}
	var out, errOut bytes.Buffer
	if code := runLockedCommand(project, dir, o, &out, &errOut); code != 0 {
		t.Fatalf("transfer exit %d; stdout %q stderr %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "task перенесён: EXT-7") {
		t.Fatalf("transfer output %q", out.String())
	}
	if _, err := os.Stat(ticketPath); !os.IsNotExist(err) {
		t.Fatalf("ticket remains after transfer: %v", err)
	}
	gotInput, err := os.ReadFile(input)
	if err != nil || string(gotInput) != body {
		t.Fatalf("command stdin = %q, %v", gotInput, err)
	}
	gotCalls, err := os.ReadFile(marker)
	if err != nil || string(gotCalls) != "sample 20261104-1200-MED-DO-task PARENT-1\n" {
		t.Fatalf("command args = %q, %v", gotCalls, err)
	}
	ledger, err := os.ReadFile(filepath.Join(dir, ".transferred.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var row transferRecord
	if err := json.Unmarshal(bytes.TrimSpace(ledger), &row); err != nil || row.Slug != "task" || row.Stem != strings.TrimSuffix(name, ".md") || row.Back != "back-item" || row.Key != "EXT-7" {
		t.Fatalf("ledger row = %#v; %v; %s", row, err, ledger)
	}
	out.Reset()
	errOut.Reset()
	if code := runLockedCommand(project, dir, o, &out, &errOut); code != 0 || !strings.Contains(out.String(), "уже перенесён: EXT-7") {
		t.Fatalf("repeat transfer = %d %q %q", code, out.String(), errOut.String())
	}
	gotCalls, _ = os.ReadFile(marker)
	if strings.Count(string(gotCalls), "sample ") != 1 {
		t.Fatalf("transfer command called repeatedly: %q", gotCalls)
	}
}

func TestParseTransferResult(t *testing.T) {
	if got, err := parseTransferResult([]byte("{\"key\":\"K-1\",\"url\":\"https://example.test\"}")); err != nil || got.Key != "K-1" {
		t.Fatalf("parsed result = %#v, %v", got, err)
	}
	for _, input := range []string{"null", "{}", "{\"key\":\"K\\n1\"}", "{\"key\":\"K\"} trailing"} {
		if _, err := parseTransferResult([]byte(input)); err == nil {
			t.Errorf("accepted transfer output %q", input)
		}
	}
}
