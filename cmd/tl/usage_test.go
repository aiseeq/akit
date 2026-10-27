package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
	opus, _ := price.Cost("claude-opus-5-5", price.Usage{Input: 10, CacheWrite: 1000, CacheRead: 20000, Output: 300})
	sonnet, _ := price.Cost("claude-sonnet-5-5", price.Usage{Input: 5, CacheRead: 30000, Output: 100})
	f := strings.Split(rows[2], "\t")
	if f[0] != "total" || f[1] != "3" || f[2] != "16" || f[3] != "1000" || f[4] != "50000" || f[5] != "401" {
		t.Errorf("split reply counted twice or sums wrong: %q", rows[2])
	}
	if got, _ := strconv.ParseFloat(f[6], 64); math.Abs(got-(opus+sonnet)) > 0.005 {
		t.Errorf("usd_api %v, want %.4f", got, opus+sonnet)
	}
	if !strings.Contains(rows[3], "1 reply(ies) without a price") {
		t.Errorf("unpriced reply not reported: %q", rows[3])
	}
	if code, _, _ := runTL(t, e, "--usage", "--role", "user", path); code != exitNone {
		t.Errorf("--role user has no replies, code %d", code)
	}
}
