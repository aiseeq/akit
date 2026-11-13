package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/price"
)

// usageRecord — ответ модели в записи Claude Code; один ответ Claude Code раскладывает на несколько записей с тем
// же id сообщения, расход в них растёт к последней.
func usageRecord(msgID, model, text string, in, write, read, out int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-10-25T10:00:00Z","requestId":"req-%s","message":{"id":%q,`+
		`"model":%q,"role":"assistant","content":[{"type":"text","text":%q}],"usage":{"input_tokens":%d,`+
		`"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}}}`,
		msgID, msgID, model, text, in, write, read, out)
}

func TestUsage(t *testing.T) {
	e := fakeHome(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		usageRecord("m1", "claude-opus-5-5", "part one", 10, 1000, 20000, 1),
		usageRecord("m1", "claude-opus-5-5", "part two", 10, 1000, 20000, 300),
		usageRecord("m2", "claude-sonnet-5-5", "other", 5, 0, 30000, 100),
		usageRecord("m3", "some-unknown-model", "unpriced", 1, 0, 0, 1),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runTL(t, e, "--usage", path)
	if code != 0 {
		t.Fatalf("code %d, stderr %s", code, errOut)
	}
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if len(rows) != 4 {
		t.Fatalf("want header, session, total and summary:\n%s", out)
	}
	if !strings.Contains(rows[0], "pct_week100\tusd_api") || strings.Split(rows[2], "\t")[6] != "-" {
		t.Errorf("missing pct_week100 column or absent-rate marker:\n%s", out)
	}
	if !strings.Contains(rows[3], "configure XDG_CONFIG_HOME/akit/rates.tsv") {
		t.Errorf("missing rates setup hint: %q", rows[3])
	}
	opus, _ := price.Cost("claude-opus-5-5", price.Usage{Input: 10, CacheWrite: 1000, CacheRead: 20000, Output: 300})
	sonnet, _ := price.Cost("claude-sonnet-5-5", price.Usage{Input: 5, CacheRead: 30000, Output: 100})
	f := strings.Split(rows[2], "\t")
	if f[0] != "total" || f[1] != "3" || f[2] != "16" || f[3] != "1000" || f[4] != "50000" || f[5] != "401" {
		t.Errorf("split reply counted twice or sums wrong: %q", rows[2])
	}
	if got, _ := strconv.ParseFloat(f[7], 64); math.Abs(got-(opus+sonnet)) > 0.005 {
		t.Errorf("usd_api %v, want %.4f", got, opus+sonnet)
	}
	if !strings.Contains(rows[3], "1 reply(ies) without a price") {
		t.Errorf("unpriced reply not reported: %q", rows[3])
	}
	if !strings.Contains(rows[3], "read: "+path) || !strings.Contains(rows[3], "--no-session-persistence") {
		t.Errorf("summary does not name what was read and what is left out: %q", rows[3])
	}
	if code, _, _ := runTL(t, e, "--usage", "--role", "user", path); code != exitNone {
		t.Errorf("--role user has no replies, code %d", code)
	}
}

func TestUsageWeek100(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.tsv")
	content := "# synthetic rates\napi_usd_week\tprovider\tvalid_from\tplan_usd_month\n" +
		"1000\tanthropic\t2026-01-01\t200\n" +
		"1500\tanthropic\t2026-02-01\t200\n" +
		"2000\tanthropic\t2026-02-01\t200\n" +
		"9000\tanthropic\t2099-01-01\t200\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rates, err := readUsageRates(path, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got := rates["anthropic"]; got.apiWeek != 2000 || got.validFrom.Format(time.DateOnly) != "2026-02-01" {
		t.Fatalf("latest equal-date row = %+v", got)
	}
	var gotTally tally
	// $10 / ($2000 × $100 / $200 = $1000 API per $100 week) × 100 = 1.00%.
	gotTally.addWeek100("claude-example", 10, true, rates)
	if gotTally.pctWeek100 != 1 || gotTally.week100Replies != 1 || gotTally.noWeek100 != 0 {
		t.Fatalf("$10 with plan $200 and api_usd_week $2000 = %+v", gotTally)
	}
	// The illustrative $1000 weekly API limit yields $500 API per $100 week: $10 is 2.00%.
	var example tally
	example.addWeek100("claude-example", 10, true, map[string]usageRate{"anthropic": {apiWeek: 1000, planMonth: 200}})
	if example.pctWeek100 != 2 {
		t.Fatalf("$10 with plan $200 and api_usd_week $1000 = %.2f%%, want 2.00%%", example.pctWeek100)
	}
	mixedRates := map[string]usageRate{
		"anthropic": {apiWeek: 1000, planMonth: 200},
		"openai":    {apiWeek: 500, planMonth: 100},
	}
	var mixed tally
	mixed.addWeek100("claude-example", 10, true, mixedRates)
	mixed.addWeek100("gpt-example", 5, true, mixedRates)
	if mixed.pctWeek100 != 3 || mixed.week100Replies != 2 {
		t.Fatalf("mixed providers should calculate each reply using its provider rate: %+v", mixed)
	}
	if got := usageProvider("mystery-model"); got != "" {
		t.Fatalf("unknown model provider = %q", got)
	}
	var unknown tally
	unknown.addWeek100("mystery-model", 10, true, rates)
	if unknown.noWeek100 != 1 {
		t.Fatalf("unknown model should have no rate: %+v", unknown)
	}
	var missingProvider tally
	missingProvider.addWeek100("gpt-example", 10, true, rates)
	if missingProvider.noWeek100 != 1 {
		t.Fatalf("provider without a course should be counted: %+v", missingProvider)
	}
	if _, err := readUsageRates(filepath.Join(t.TempDir(), "absent.tsv"), time.Now()); err == nil {
		t.Fatal("missing rates file should be reported")
	}
}

func TestUsageProvider(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus": "anthropic", "gpt-5": "openai", "o3-mini": "openai", "codex-test": "openai", "local-model": "",
	} {
		if got := usageProvider(model); got != want {
			t.Errorf("usageProvider(%q) = %q, want %q", model, got, want)
		}
	}
}

// Итог --all называет прочитанные папки: расход запусков без журнала в нём не виден.
func TestUsageScopeAll(t *testing.T) {
	e := fakeHome(t)
	projects := filepath.Join(e.home, ".claude", "projects")
	rec := usageRecord("m1", "claude-opus-5-5", "reply", 10, 0, 0, 1)
	if err := os.WriteFile(filepath.Join(projects, "-home-x-proj", "usage.jsonl"), []byte(rec+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args       []string
		read       string
		codex      bool
		noUsageRow bool
	}{
		{[]string{"--all", "--usage"}, "read: " + projects + "/*;", false, false},
		{[]string{"--all", "--codex", "--usage"}, "read: " + projects + "/*;", true, false},
		{[]string{"--all", "--project", "proj", "--usage"}, "read: " + projects + "/*proj*;", false, false},
		{[]string{"--all", "--no-subagents", "--usage"}, "read: " + projects + "/* without subagents;", false, false},
		// пустое окно: расхода нет, но охват и то, что не входит, всё равно названы
		{[]string{"--all", "--since", "1m", "--usage"}, "read: " + projects + "/*;", false, true},
	}
	for _, c := range cases {
		_, out, errOut := runTL(t, e, c.args...)
		last := lastLine(out)
		if !strings.Contains(last, c.read) || !strings.Contains(last, "--no-session-persistence") ||
			strings.Contains(last, "codex rollouts") != c.codex || strings.Contains(last, "no model replies") != c.noUsageRow {
			t.Errorf("%v: summary %q, stderr %s", c.args, last, errOut)
		}
	}
}

// Rollout codex по пути: его счёт токенов не читается, итог говорит, что он не входит.
func TestUsageScopeCodexRollout(t *testing.T) {
	e := fakeHome(t)
	data, err := os.ReadFile("../../transcript/testdata/codex.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rollout-2026-10-25T10-00-00-0000.jsonl")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runTL(t, e, "--usage", path)
	if last := lastLine(out); !strings.Contains(last, "read: "+path+";") || !strings.Contains(last, "codex rollouts (their token counts are not read)") {
		t.Errorf("codex rollout: summary %q", last)
	}
}

func lastLine(s string) string {
	rows := strings.Split(strings.TrimSpace(s), "\n")
	return rows[len(rows)-1]
}
