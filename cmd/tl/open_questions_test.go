package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

func TestOpenQuestionRecognition(t *testing.T) {
	tests := []struct {
		name  string
		event transcript.Event
		want  []string
	}{
		{"AskUserQuestion field", transcript.Event{Role: transcript.Cmd, Who: "AskUserQuestion", CallID: "call", Raw: []byte(`{"questions":[{"question":"Which option?"}]}`)}, []string{"Which option?"}},
		{"last paragraph", transcript.Event{Role: transcript.Agent, Text: "An earlier question?\n\nWhat should happen?"}, []string{"What should happen?"}},
		{"question sentence in long paragraph", transcript.Event{Role: transcript.Agent, Text: "Context " + strings.Repeat("detail ", 40) + ". Ship it?"}, []string{"Ship it?"}},
		{"long question sentence", transcript.Event{Role: transcript.Agent, Text: "Would you like to ship this change today?"}, []string{"Would you like to ship this change today?"}},
		{"decimal periods", transcript.Event{Role: transcript.Agent, Text: "Which version, 1.2 or 1.3?"}, []string{"Which version, 1.2 or 1.3?"}},
		{"period inside file name", transcript.Event{Role: transcript.Agent, Text: "Move it to the skill file SKILL.md?"}, []string{"Move it to the skill file SKILL.md?"}},
		{"two question sentences", transcript.Event{Role: transcript.Agent, Text: "Merge it? Then tag the release. Delete the branch?"}, []string{"Merge it?", "Delete the branch?"}},
		{"guillemet quote with inner question", transcript.Event{Role: transcript.Agent, Text: "Approved: «Ready? Yes.» Deploying."}, nil},
		{"guillemet quote with inner period", transcript.Event{Role: transcript.Agent, Text: "Approved: «Tests pass. Ship it?» — yes."}, nil},
		{"guillemet quote with inner exclamation", transcript.Event{Role: transcript.Agent, Text: "He said «Stop! Why?» and left."}, nil},
		{"multiline guillemet quote", transcript.Event{Role: transcript.Agent, Text: "Approved: «Ship it?\nyes» — done."}, nil},
		{"multiline curly quote", transcript.Event{Role: transcript.Agent, Text: "He said “First. Is this ready?” and left."}, nil},
		{"straight quote with inner period", transcript.Event{Role: transcript.Agent, Text: "He wrote \"Tests pass. Ship it?\" yesterday."}, nil},
		{"code with dots and question in URL", transcript.Event{Role: transcript.Agent, Text: "Set `api.example.com/x?key=1` in the config."}, nil},
		{"code ternary question mark", transcript.Event{Role: transcript.Agent, Text: "Use `x ? y : z` here."}, nil},
		{"code span before question after dot", transcript.Event{Role: transcript.Agent, Text: "The check is in `main.go`. Move it?"}, []string{"Move it?"}},
		{"URL inside guillemet quote", transcript.Event{Role: transcript.Agent, Text: "Approved «see https://example.com/a?x» now."}, nil},
		{"URL inside guillemet quote leaves later question visible", transcript.Event{Role: transcript.Agent, Text: "Approved «see https://example.com/a?x» now. Ship it?"}, []string{"Ship it?"}},
		{"URL inside inline code", transcript.Event{Role: transcript.Agent, Text: "Run `curl https://example.com/a?x` now."}, nil},
		{"URL inside inline code leaves later question visible", transcript.Event{Role: transcript.Agent, Text: "Run `curl https://example.com/a?x` now. Ship it?"}, []string{"Ship it?"}},
		{"backtick pairing stops at period boundary", transcript.Event{Role: transcript.Agent, Text: "Use `unfinished. Should we continue? Then run `x`."}, []string{"Should we continue?"}},
		{"backtick pairing stops at exclamation boundary", transcript.Event{Role: transcript.Agent, Text: "Use `unfinished! Should we continue? Then run `x`."}, []string{"Should we continue?"}},
		{"backtick pairing stops at newline", transcript.Event{Role: transcript.Agent, Text: "Use `unfinished\nShould we continue? Then run `x`."}, []string{"Should we continue?"}},
		{"straight quote pairing stops at period boundary", transcript.Event{Role: transcript.Agent, Text: "He wrote \"unfinished. Should we continue? Then said \"done\"."}, []string{"Should we continue?"}},
		{"straight quote pairing stops at exclamation boundary", transcript.Event{Role: transcript.Agent, Text: "He wrote \"unfinished! Should we continue? Then said \"done\"."}, []string{"Should we continue?"}},
		{"straight quote pairing stops at newline", transcript.Event{Role: transcript.Agent, Text: "He wrote \"unfinished\nShould we continue? Then said \"done\"."}, []string{"Should we continue?"}},
		{"unclosed guillemet question at end", transcript.Event{Role: transcript.Agent, Text: "Approved «ship it?"}, []string{"Approved «ship it?"}},
		{"nested guillemets hide nested question", transcript.Event{Role: transcript.Agent, Text: "Approved: «step «one?» is done»."}, nil},
		{"nested guillemets stay open through the inner close", transcript.Event{Role: transcript.Agent, Text: "Approved: «step «one?»? is done»."}, nil},
		{"question after inch mark and later quoted text", transcript.Event{Role: transcript.Agent, Text: "The 27\" panel is in stock. Order it? The model is \"B\"."}, []string{"Order it?"}},
		{"inch mark does not open quote in same sentence", transcript.Event{Role: transcript.Agent, Text: "The 27\" panel is ready, Order it? The model is \"B\"."}, []string{"The 27\" panel is ready, Order it?"}},
		{"question after unmatched backtick and later code", transcript.Event{Role: transcript.Agent, Text: "Split on the ` char. Is that fine? Then run `make`."}, []string{"Is that fine?"}},
		{"straight quote at paragraph start", transcript.Event{Role: transcript.Agent, Text: `"Ready" is the label. Ship it?`}, []string{"Ship it?"}},
		{"code period before question and closer", transcript.Event{Role: transcript.Agent, Text: "Run `echo done. ok?` now."}, []string{"ok?"}},
		{"straight quote period then newline before question close", transcript.Event{Role: transcript.Agent, Text: "He said \"a. b\nc?\" ok."}, []string{"c?"}},
		{"inch mark followed by paired quote with question", transcript.Event{Role: transcript.Agent, Text: `The 27" panel is "fine?" per spec.`}, nil},
		{"unbalanced guillemet does not pair with later quote", transcript.Event{Role: transcript.Agent, Text: "He wrote «draft. Ship it? See «B» later."}, []string{"Ship it?"}},
		{"repeated question marks form one row", transcript.Event{Role: transcript.Agent, Text: "Really?? Ship it."}, []string{"Really??"}},
		{"bold close follows sentence period", transcript.Event{Role: transcript.Agent, Text: "**Tests pass.** Ship it?"}, []string{"Ship it?"}},
		{"question mark after URL before whitespace", transcript.Event{Role: transcript.Agent, Text: "Is the draft at https://example.com/draft? I will publish it."}, []string{"Is the draft at https://example.com/draft?"}},
		{"question mark inside URL before parenthesis", transcript.Event{Role: transcript.Agent, Text: "Found the page (https://example.com/a?) for review."}, []string{"Found the page (https://example.com/a?"}},
		{"exclamation separates preceding sentence", transcript.Event{Role: transcript.Agent, Text: "Done! Ship it?"}, []string{"Ship it?"}},
		{"newline separates sentences", transcript.Event{Role: transcript.Agent, Text: "Earlier statement\nShould we proceed?"}, []string{"Should we proceed?"}},
		{"bullet prefix trimmed", transcript.Event{Role: transcript.Agent, Text: "• Should we proceed?"}, []string{"Should we proceed?"}},
		{"duplicate question sentence once", transcript.Event{Role: transcript.Agent, Text: "Proceed? Proceed?"}, []string{"Proceed?"}},
		{"trim surrounding whitespace", transcript.Event{Role: transcript.Agent, Text: "  \n  Do we proceed?  \n"}, []string{"Do we proceed?"}},
		{"question only in earlier paragraph", transcript.Event{Role: transcript.Agent, Text: "Should we proceed?\n\nDone."}, nil},
		{"question mark in URL", transcript.Event{Role: transcript.Agent, Text: "Sources:\n- [doc](https://example.com/page?id=1)"}, nil},
		{"question mark in inline code", transcript.Event{Role: transcript.Agent, Text: "The key is in the query (`?apiKey=`). Fixed."}, nil},
		{"question mark in guillemet quote", transcript.Event{Role: transcript.Agent, Text: "Approved: «Do the second one?» — yes."}, nil},
		{"question mark in double quote", transcript.Event{Role: transcript.Agent, Text: `Quoted "Really?" text.`}, nil},
		{"question after matched straight quote", transcript.Event{Role: transcript.Agent, Text: `The word is "ready" Should we proceed?`}, []string{`The word is "ready" Should we proceed?`}},
		{"curly double quoted question excluded", transcript.Event{Role: transcript.Agent, Text: "The reply was “Proceed?” in the note. Continue?"}, []string{"Continue?"}},
		{"question after curly quoted URL", transcript.Event{Role: transcript.Agent, Text: "Source: «https://example.com/path» Is that the right page?"}, []string{"Source: «https://example.com/path» Is that the right page?"}},
		{"question after curly double quote", transcript.Event{Role: transcript.Agent, Text: "Done. “yes” Should we continue?"}, []string{"“yes” Should we continue?"}},
		{"question after unbalanced straight quote", transcript.Event{Role: transcript.Agent, Text: `The panel is 27" wide. Order the second one?`}, []string{"Order the second one?"}},
		{"question after unbalanced backtick", transcript.Event{Role: transcript.Agent, Text: "Split on the ` character. Is that fine?"}, []string{"Is that fine?"}},
		{"trailing question after URL", transcript.Event{Role: transcript.Agent, Text: "The draft is at https://example.com/draft?"}, []string{"The draft is at https://example.com/draft?"}},
		{"straight quote pairing ignores question boundaries", transcript.Event{Role: transcript.Agent, Text: "The panel is \"odd? Order it? The model is \"B\"."}, nil},
		{"question mark inside URL query", transcript.Event{Role: transcript.Agent, Text: "See https://example.com/draft?mode=full"}, nil},
		{"question after URL closing parenthesis", transcript.Event{Role: transcript.Agent, Text: "Review https://example.com/path)?mode=full"}, []string{"Review https://example.com/path)?"}},
		{"question mark inside www URL", transcript.Event{Role: transcript.Agent, Text: "See www.example.com/draft?mode=full"}, nil},
		{"uppercase URL scheme", transcript.Event{Role: transcript.Agent, Text: "See HTTPS://example.com/draft?mode=full"}, nil},
		{"URL inside inline code does not hide question", transcript.Event{Role: transcript.Agent, Text: "Find `https://example.com/item` then proceed?"}, []string{"Find `https://example.com/item` then proceed?"}},
		{"URL stops before closing punctuation", transcript.Event{Role: transcript.Agent, Text: "The draft is https://example.com/draft)?"}, []string{"The draft is https://example.com/draft)?"}},
		{"question mark in fenced code", transcript.Event{Role: transcript.Agent, Text: "Ran this:\n\n```\nx=1\n\ncurl 'http://h/q?a=b'\n```"}, nil},
		{"fence at end keeps prior paragraph", transcript.Event{Role: transcript.Agent, Text: "Run this on the server?\n\n```\nmake deploy\n```"}, []string{"Run this on the server?"}},
		{"indented list fence is skipped", transcript.Event{Role: transcript.Agent, Text: "- example:\n  ```\n  Hidden question?\n  ```"}, nil},
		{"only fenced code", transcript.Event{Role: transcript.Agent, Text: "```\n\nquery?\n```"}, nil},
		{"question mark only in trailer", transcript.Event{Role: transcript.Agent, Text: "Create the repository `x`?\n\nSearched: `prior x` — no decisions."}, nil},
		{"tool output", transcript.Event{Role: transcript.Out, Text: "Should we proceed?"}, nil},
		{"other tool call", transcript.Event{Role: transcript.Cmd, Who: "Bash", Text: "Should we proceed?"}, nil},
		{"empty AskUserQuestion", transcript.Event{Role: transcript.Cmd, Who: "AskUserQuestion", Raw: []byte(`{"questions":[]}`)}, nil},
		{"sidechain question", transcript.Event{Role: transcript.Agent, Text: "Should we proceed?", Sidechain: true}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := questionTexts(tt.event)
			if err != nil {
				t.Fatalf("questionTexts() error = %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("questionTexts() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOpenQuestionsKeepOnlyQuestionFields(t *testing.T) {
	ev := transcript.Event{Role: transcript.Cmd, Who: "AskUserQuestion", Raw: []byte(`{"questions":[{"question":""},{"question":"First?"},{"question":"Second?"}]}`)}
	got, err := questionTexts(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "First?|Second?" {
		t.Fatalf("questionTexts() = %q", got)
	}
}

func TestOpenQuestionsLastParagraphSkipsIndentedListFence(t *testing.T) {
	text := "- example:\n  ```\n  Hidden question?\n  ```"
	if got := lastPlainParagraph(text); got != "- example:" {
		t.Fatalf("lastPlainParagraph() = %q, want preceding list paragraph", got)
	}
}

func TestOpenQuestionMalformedAskInputIsIgnored(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:00Z","message":{"content":[{"type":"tool_use","id":"bad","name":"AskUserQuestion","input":{"questions":"not an array"}},{"type":"tool_use","id":"null","name":"AskUserQuestion","input":{"questions":null}},{"type":"tool_use","id":"scalar","name":"AskUserQuestion","input":"not an object"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-05T10:01:00Z","message":{"content":[{"type":"text","text":"Ship the release?"}]}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	if code != 0 || strings.Count(errOut, "ignoring AskUserQuestion input") != 3 || !strings.Contains(out, "Ship the release?") || strings.Count(out, "\n") != 1 {
		t.Fatalf("malformed AskUserQuestion was not skipped with a warning: code %d, stderr %q, output %q", code, errOut, out)
	}
}

func TestOpenQuestionsCommandJSONL(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:00Z","message":{"content":[{"type":"text","text":"`+strings.Repeat("context ", 35)+`. Ship it?"}]}}`,
		`{"type":"user","timestamp":"2026-01-05T10:01:00Z","origin":{"kind":"human"},"message":{"content":"yes"}}`,
		`{"type":"assistant","timestamp":"2026-01-05T10:02:00Z","message":{"content":[{"type":"text","text":"Would you like to ship this change today?"}]}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q, output:\n%s", code, errOut, out)
	}
	wantReply := time.Date(2026, 1, 5, 10, 1, 0, 0, time.UTC).Local().Format("15:04:05")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "закрыт") || !strings.Contains(lines[0], wantReply) || !strings.Contains(lines[0], "Ship it?") || !strings.HasSuffix(lines[0], path+":1") {
		t.Fatalf("unexpected first question row: %q", out)
	}
	if !strings.Contains(lines[1], "открыт") || !strings.Contains(lines[1], "Would you like to ship this change today?") || !strings.HasSuffix(lines[1], path+":3") {
		t.Fatalf("unexpected second question row: %q", out)
	}
	for _, line := range lines {
		ref := line[strings.LastIndex(line, " ")+1:]
		refCode, refOut, refErr := runTL(t, e, ref)
		if refCode != 0 || refErr != "" || !strings.Contains(refOut, "agent") {
			t.Errorf("event reference %q: code %d, stderr %q, output %q", ref, refCode, refErr, refOut)
		}
	}
}

func TestOpenQuestionsSortByQuestionTime(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:02:00Z","message":{"content":[{"type":"text","text":"Later?"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-05T10:01:00Z","message":{"content":[{"type":"text","text":"Earlier?"}]}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || errOut != "" || len(lines) != 2 || !strings.Contains(lines[0], "Earlier?") || !strings.HasSuffix(lines[0], path+":2") || !strings.Contains(lines[1], "Later?") || !strings.HasSuffix(lines[1], path+":1") {
		t.Fatalf("questions are not sorted by time: code %d, stderr %q, output %q", code, errOut, out)
	}
}

func TestOpenQuestionsQueuedReplyUsesFileOrder(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:07Z","message":{"content":[{"type":"text","text":"Stop the worker now?"}]}}`,
		`{"type":"attachment","timestamp":"2026-01-05T10:00:03Z","attachment":{"type":"queued_command","prompt":"yes, stop it","origin":{"kind":"human"}}}`,
		`{"type":"user","timestamp":"2026-01-05T11:00:00Z","origin":{"kind":"human"},"message":{"content":"unrelated later note"}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	wantReply := time.Date(2026, 1, 5, 10, 0, 3, 0, time.UTC).Local().Format("15:04:05")
	if code != 0 || errOut != "" || strings.Count(out, "\n") != 1 || !strings.Contains(out, "закрыт") || !strings.Contains(out, wantReply) {
		t.Fatalf("queued response did not close at its record time: code %d, stderr %q, output %q", code, errOut, out)
	}
}

func TestOpenQuestionsIgnoreReminderOnlyReply(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:00Z","message":{"content":[{"type":"text","text":"Continue?"}]}}`,
		`{"type":"user","timestamp":"2026-01-05T10:01:00Z","origin":{"kind":"human"},"message":{"content":"<system-reminder>synthetic reminder</system-reminder>"}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	if code != 0 || errOut != "" || !strings.Contains(out, "открыт") {
		t.Fatalf("reminder-only message closed question: code %d, stderr %q, output %q", code, errOut, out)
	}
}

func TestOpenQuestionsIgnoreAskBlockedByHook(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:00Z","message":{"content":[{"type":"tool_use","id":"ask1","name":"AskUserQuestion","input":{"questions":[{"question":"Which option?"}]}}]}}`,
		`{"type":"user","timestamp":"2026-01-05T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"ask1","is_error":true,"content":"synthetic hook error"}]}}`,
		`{"type":"user","timestamp":"2026-01-05T11:00:00Z","origin":{"kind":"human"},"message":{"content":"unrelated note"}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("blocked dialog was listed: code %d, stderr %q, output %q", code, errOut, out)
	}
}

func TestOpenQuestionsNonErrorAskResultRemainsOpen(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:00Z","message":{"content":[{"type":"tool_use","id":"ask1","name":"AskUserQuestion","input":{"questions":[{"question":"Which option?"}]}}]}}`,
		`{"type":"user","timestamp":"2026-01-05T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"ask1","is_error":false,"content":"window displayed"}]}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	if code != 0 || errOut != "" || !strings.Contains(out, "открыт") || !strings.Contains(out, "Which option?") {
		t.Fatalf("non-error AskUserQuestion result was suppressed: code %d, stderr %q, output %q", code, errOut, out)
	}
}

func TestOpenQuestionsSuppressesOnlyMatchingBlockedAsk(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:00Z","message":{"content":[{"type":"tool_use","id":"blocked","name":"AskUserQuestion","input":{"questions":[{"question":"Blocked question?"}]}},{"type":"tool_use","id":"visible","name":"AskUserQuestion","input":{"questions":[{"question":"Visible question?"}]}}]}}`,
		`{"type":"user","timestamp":"2026-01-05T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"blocked","is_error":true,"content":"synthetic hook error"},{"type":"tool_result","tool_use_id":"visible","is_error":false,"content":"window displayed"}]}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	if code != 0 || errOut != "" || strings.Contains(out, "Blocked question?") || !strings.Contains(out, "Visible question?") {
		t.Fatalf("blocked call filtering crossed call IDs: code %d, stderr %q, output %q", code, errOut, out)
	}
}

func TestOpenQuestionsAskWindowAnswerClosesQuestion(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:00Z","message":{"content":[{"type":"text","text":"Which option should we choose?"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:01Z","message":{"content":[{"type":"tool_use","id":"ask1","name":"AskUserQuestion","input":{}}]}}`,
		`{"type":"user","timestamp":"2026-01-05T10:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"ask1","content":"accepted"}]},"toolUseResult":{"answers":{"Choice":"accepted"}}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	wantReply := time.Date(2026, 1, 5, 10, 0, 2, 0, time.UTC).Local().Format("15:04:05")
	if code != 0 || errOut != "" || !strings.Contains(out, "закрыт") || !strings.Contains(out, wantReply) {
		t.Fatalf("AskUserQuestion answer did not close question: code %d, stderr %q, output %q", code, errOut, out)
	}
}

func TestOpenQuestionsEmptyAskWindowAnswerDoesNotCloseQuestion(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:00Z","message":{"content":[{"type":"text","text":"Which option should we choose?"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:01Z","message":{"content":[{"type":"tool_use","id":"ask1","name":"AskUserQuestion","input":{}}]}}`,
		`{"type":"user","timestamp":"2026-01-05T10:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"ask1","content":""}]},"toolUseResult":{"answers":{"Choice":" "}}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	if code != 0 || errOut != "" || !strings.Contains(out, "открыт") || strings.Contains(out, "закрыт") {
		t.Fatalf("empty AskUserQuestion answer closed question: code %d, stderr %q, output %q", code, errOut, out)
	}
}

func TestOpenQuestionsIgnoreNonHumanReplies(t *testing.T) {
	e := fakeHome(t)
	path := writeOpenQuestionsTranscript(t,
		`{"type":"assistant","timestamp":"2026-01-05T10:00:00Z","message":{"content":[{"type":"text","text":"Continue?"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-05T10:01:00Z","message":{"content":[{"type":"tool_use","id":"tool1","name":"Bash","input":{"command":"echo answer"}}]}}`,
		`{"type":"user","timestamp":"2026-01-05T10:02:00Z","message":{"content":[{"type":"tool_result","tool_use_id":"tool1","content":"answer"}]}}`,
		`{"type":"user","timestamp":"2026-01-05T10:03:00Z","origin":{"kind":"peer"},"message":{"content":"<cross-session-message from=\"peer\" from-name=\"synthetic-peer\">\nanswer from another session\n</cross-session-message>"}}`,
		`{"type":"user","timestamp":"2026-01-05T10:04:00Z","isSidechain":true,"origin":{"kind":"human"},"message":{"content":"sidechain answer"}}`,
	)
	code, out, errOut := runTL(t, e, "--open-questions", path)
	if code != 0 || errOut != "" || !strings.Contains(out, "открыт") || strings.Contains(out, "закрыт") {
		t.Fatalf("non-human event closed question: code %d, stderr %q, output %q", code, errOut, out)
	}
}

func TestOpenQuestionsIgnoreSubagentSources(t *testing.T) {
	var out, stderr strings.Builder
	code := reportOpenQuestions([]source{{Path: "missing.jsonl", ParentID: "parent"}}, &out, &stderr)
	if code != 0 || out.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("subagent source: code %d, stdout %q, stderr %q", code, out.String(), stderr.String())
	}
}

func TestOpenQuestionsReadErrorIsReported(t *testing.T) {
	var out, stderr strings.Builder
	code := reportOpenQuestions([]source{{Path: filepath.Join(t.TempDir(), "missing.jsonl")}}, &out, &stderr)
	if code != exitUsage || out.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("read error: code %d, stdout %q, stderr %q", code, out.String(), stderr.String())
	}
}

func TestOpenQuestionsFlagCombinations(t *testing.T) {
	now := time.Now()
	for _, flags := range [][]string{
		{"--all"}, {"--usage"}, {"--worktime", "--grep", "synthetic"}, {"--wait", "true"}, {"--q", "50"},
		{"--role", "user"}, {"--since", "1h"}, {"--until", "1h"}, {"--grep", "synthetic"}, {"--raw", "synthetic"},
		{"--subagents"}, {"--max", "1"}, {"--width", "80"}, {"-C", "1"},
	} {
		args := append([]string{"--open-questions"}, flags...)
		args = append(args, "session")
		if _, err := parseArgs(args, now); err == nil {
			t.Errorf("parseArgs(%q) accepted incompatible flags", args)
		}
	}
}

func TestOpenQuestionsRejectEventReference(t *testing.T) {
	e := fakeHome(t)
	code, out, errOut := runTL(t, e, "--open-questions", sessionID+":1")
	if code == 0 || out != "" || !strings.Contains(errOut, "event reference") {
		t.Fatalf("event reference accepted: code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func writeOpenQuestionsTranscript(t *testing.T, records ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic-session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
