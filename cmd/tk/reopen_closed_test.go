package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestTransitionsRefuseClosedTicketsWithClosingJournal(t *testing.T) {
	for _, tc := range []struct {
		status, closing, actor string
	}{
		{"отклонён", "- 2026-11-20 12:26 отклонён — lead — лишний тикет", "lead"},
		{"готово", "- 2026-11-20 12:26 готово — reviewer: проверено", "reviewer"},
	} {
		for _, cmd := range []string{"start", "wait", "review"} {
			t.Run(tc.status+"/"+cmd, func(t *testing.T) {
				_, dir := testRunMainRoot(t)
				body := "# Sample\n\nСтатус: " + tc.status + "\n\n## Приёмка\n- check\n\n## Журнал\n" + tc.closing + "\n"
				path := writeRunMainTask(t, dir, "20261120-1200-MED-DO-sample.md", body)
				args := []string{cmd, "sample", "--by", "tester"}
				if cmd == "wait" {
					args = append(args, "--until", "2026-11-25 10:00: inspect")
				}
				var out, errout strings.Builder
				code := runMain(args, time.Now(), &out, &errout)
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if code == 0 || string(got) != body {
					t.Fatalf("code=%d, file changed=%v, stdout=%q, stderr=%q", code, string(got) != body, out.String(), errout.String())
				}
				for _, want := range []string{tc.status, tc.closing, tc.actor} {
					if !strings.Contains(errout.String(), want) {
						t.Errorf("stderr %q lacks %q", errout.String(), want)
					}
				}
			})
		}
	}
}

func TestStartReopensClosedTicketExplicitly(t *testing.T) {
	for _, status := range []string{"отклонён", "готово"} {
		t.Run(status, func(t *testing.T) {
			_, dir := testRunMainRoot(t)
			body := "# Sample\n\nСтатус: " + status + "\n\n## Приёмка\n- check\n\n## Журнал\n- 2026-11-20 12:26 " + status + " — lead — причина закрытия\n"
			path := writeRunMainTask(t, dir, "20261120-1200-MED-DO-sample.md", body)
			var out, errout strings.Builder
			code := runMain([]string{"start", "sample", "--by", "tester", "--reopen"}, time.Now(), &out, &errout)
			updated, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if code != 0 || !strings.Contains(string(updated), "Статус: в работе") || !strings.Contains(string(updated), "переоткрыт") {
				t.Fatalf("code=%d, body=%q, stdout=%q, stderr=%q", code, updated, out.String(), errout.String())
			}
		})
	}
}

func TestDoneAndRejectRefuseClosedTickets(t *testing.T) {
	for _, tc := range []struct {
		cmd, status string
		flags       []string
	}{
		{"done", "готово", []string{"--check", "verified"}},
		{"reject", "отклонён", []string{"--why", "already closed"}},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			_, dir := testRunMainRoot(t)
			body := "# Sample\n\nСтатус: " + tc.status + "\n\n## Приёмка\n- check\n\n## Журнал\n- 2026-11-20 12:26 " + tc.status + " — lead — причина закрытия\n"
			path := writeRunMainTask(t, dir, "20261120-1200-MED-DO-sample.md", body)
			args := append([]string{tc.cmd, "sample"}, tc.flags...)
			args = append(args, "--by", "tester")
			var out, errout strings.Builder
			if code := runMain(args, time.Now(), &out, &errout); code == 0 {
				t.Fatalf("closed ticket accepted %s: stdout=%q stderr=%q", tc.cmd, out.String(), errout.String())
			}
			updated, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(updated) != body || !strings.Contains(errout.String(), "2026-11-20 12:26 "+tc.status+" — lead — причина закрытия") {
				t.Fatalf("closed ticket changed or closing line missing: body=%q stderr=%q", updated, errout.String())
			}
		})
	}
}

func TestReopenFlagIsStartOnly(t *testing.T) {
	if _, err := parseArgs([]string{"wait", "sample", "--reopen", "--until", "tomorrow"}); err == nil || !strings.Contains(err.Error(), "only for start") {
		t.Fatalf("wait --reopen error = %v", err)
	}
}

func TestLastClosingJournalLineUsesNewestClosure(t *testing.T) {
	body := "## Журнал\n" +
		"- 2026-11-22 12:00 отклонён — lead: latest reason\n" +
		"- 2026-11-21 12:00 note — worker: intervening note\n" +
		"- 2026-11-20 12:00 готово — reviewer: older reason\n"
	if got := lastClosingJournalLine(body); got != "- 2026-11-22 12:00 отклонён — lead: latest reason" {
		t.Fatalf("lastClosingJournalLine = %q", got)
	}
}
