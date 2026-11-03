package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/transcript"
)

func pairAt(way, goal string, minute int) pairCall {
	base := time.Now().UTC().Truncate(time.Hour)
	return pairCall{tool: "wfor", way: way, goal: goal, agent: "claude-main", session: "session", id: "session:" + time.Duration(minute).String(), at: base.Add(time.Duration(minute) * time.Minute), command: way + " " + goal, code: 0}
}

func TestPairEpisodesGroupByGoal(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	u.pairCalls = []pairCall{pairAt("new", "/one", 0), pairAt("old", "/one", 5), pairAt("new", "/one", 10), pairAt("new", "/two", 11)}
	if err := u.writePairs(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(u.outputDir, "pairs.tsv"))
	s := string(b)
	if !strings.Contains(s, "wfor\tclaude-main\tno\tno-date\tnew\t1\t0\t1\t2") {
		t.Fatalf("mixed episode and switches missing:\n%s", s)
	}
}

func TestPairEpisodesPollsCountOnce(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for i := range 5 {
		c := pairAt("old", "pid:42", i)
		c.command = "sleep 1; kill -0 42"
		u.pairCalls = append(u.pairCalls, c)
	}
	if err := u.writePairs(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(u.outputDir, "pairs.tsv"))
	if !strings.Contains(string(b), "wfor\tclaude-main\tno\tno-date\told\t0\t1\t0\t0\t5") {
		t.Fatalf("poll commands formed multiple episodes:\n%s", b)
	}
}

func TestPairBusyNeedsDecision(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	base := time.Now()
	evs := []transcript.Event{{Role: transcript.Cmd, Who: "Bash", Time: base, Text: "uptime"}}
	if got := u.busyGoal(evs, 0, "uptime", shellcmd.Call{Name: "uptime", Raw: "uptime", Offset: 0}); got != "" {
		t.Fatalf("probe without decision goal=%q", got)
	}
	evs = append(evs, transcript.Event{Role: transcript.Cmd, Who: "Bash", Time: base.Add(3 * time.Minute), Text: "go test ./..."})
	if got := u.busyGoal(evs, 0, "uptime", shellcmd.Call{Name: "uptime", Raw: "uptime", Offset: 0}); got == "" {
		t.Fatal("probe before heavy command was not paired")
	}
	if got := u.busyGoal(evs[:1], 0, "busy", shellcmd.Call{Name: "busy", Raw: "busy", Offset: 0}); got != "" {
		t.Fatalf("busy without heavy command goal=%q", got)
	}
	u.collectPairEvents("session", true, nil, evs, nil, nil)
	if len(u.pairCalls) != 1 || u.pairCalls[0].tool != "busy" || u.pairCalls[0].way != "old" {
		t.Fatalf("previous probe was not recorded in the pair: %+v", u.pairCalls)
	}
}

func TestPairLayersByAgentAndBrief(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	add := func(agent string, instructed bool, day, newN, oldN int) {
		base := time.Now().UTC().Truncate(24 * time.Hour).Add(time.Duration(day) * 24 * time.Hour)
		for i := range newN {
			c := pairAt("new", fmt.Sprintf("%s-%d-new-%d", agent, day, i), i)
			c.tool, c.agent, c.instructed, c.at = "prior", agent, instructed, base.Add(time.Duration(i)*time.Second)
			u.pairCalls = append(u.pairCalls, c)
		}
		for i := range oldN {
			c := pairAt("old", fmt.Sprintf("%s-%d-old-%d", agent, day, i), i)
			c.tool, c.agent, c.instructed, c.at = "prior", agent, instructed, base.Add(time.Duration(i)*time.Second)
			u.pairCalls = append(u.pairCalls, c)
		}
	}
	add("A", true, 1, 90, 10)
	add("B", false, 1, 1, 9)
	add("A", true, 2, 9, 1)
	add("B", false, 2, 10, 90)
	if err := u.writePairs(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(u.outputDir, "pairs.tsv"))
	s := string(b)
	if strings.Contains(strings.Split(s, "\n")[0], "day") || !strings.Contains(s, "prior\tA\tyes\tno-date\tnew\t99\t11\t0\t0") || !strings.Contains(s, "prior\tB\tno\tno-date\tnew\t11\t99\t0\t0") {
		t.Fatalf("pair rows not stratified:\n%s", b)
	}
}

func TestOutcomeTablePriority(t *testing.T) {
	for _, tc := range []struct {
		tool, out string
		code      int
		want      string
	}{{"page-shot", "page-shot: render failed", 1, "fail"}, {"page-shot", "layout issue", 2, "answer"}, {"busy", "busy: runnable 4 is above limit", 1, "answer"}, {"busy", "Exit code 1\nbusy: runnable 46 is at or above 22; runnable 46 (busy at 22, threshold)", 1, "answer"}, {"busy", "busy: refusing invalid config", 1, "fail"}, {"prior", "(prior -h for usage)", 1, "callerr"}, {"prior", "== пусто: журнал решений; артефакты", -1, "answer"}, {"wfor", "wfor: not yet after 1s", 1, "answer"}, {"user-word", "user-word: no user line with \"two words\" in /synthetic/transcript.jsonl\nin other sessions: user-word --search \"two words\"", 1, "answer"}, {"claude-rules-export", "claude-rules-export: export is not clean", 1, "answer"}, {"claude-rules-export", "Exit code 1\nclaude-rules-export: export is not clean", 1, "answer"}, {"busy", "(busy -h for usage)", 1, "callerr"}} {
		got := pairOutcome(pairCall{tool: tc.tool, out: tc.out, code: tc.code})
		if got != tc.want {
			t.Errorf("%s %q => %s, want %s", tc.tool, tc.out, got, tc.want)
		}
	}
	for _, tc := range []struct {
		tool, output, want string
	}{
		{"page-shot", "page-shot: 1366x768: горизонтальный скролл, страница шире окна", "answer"},
		{"page-shot", "page-shot: 1366x768: наложение подписей", "answer"},
		{"page-shot", "page-shot: 1366x768: открыть страницу: timeout", "fail"},
		{"page-shot", "filtered page-shot output: /synthetic/shots/1366x768.png", "answer"},
	} {
		if got := pairOutcome(pairCall{tool: tc.tool, code: -1, out: tc.output}); got != tc.want {
			t.Errorf("%s output => %s, want %s", tc.tool, got, tc.want)
		}
	}
	for _, tc := range []struct{ tool, output string }{
		{"user-word", "--- agent before the line (10:30):\ncontext\n--- user 10:31:\ntwo synthetic words\n--- no other user lines within 30 min"},
		{"user-word", "--- agent before the line (10:30):\ncontext"},
		{"page-shot", "./page-shots/1440x900.png\n./page-shots/1440x900-full.png"},
		{"page-shot", "saved: ./page-shots/1440x900.png (full)"},
		{"prior", "== совпадения: решения\n/synthetic/.claude/improve/topic.md:4: two words"},
	} {
		if got := pairOutcome(pairCall{tool: tc.tool, code: -1, out: tc.output}); got != "answer" {
			t.Errorf("%s output without attributed code => %s, want answer", tc.tool, got)
		}
	}
}

func TestOutcomeUnknownBounds(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for i := range 100 {
		outcome := "unknown"
		if i == 0 {
			outcome = "fail"
		} else if i < 50 {
			outcome = "answer"
		}
		u.pairCalls = append(u.pairCalls, pairCall{tool: "prior", way: "new", outcome: outcome})
	}
	if err := u.writeOutcomes(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(u.outputDir, "outcomes.tsv"))
	if !strings.Contains(string(b), "prior\tnew\t100\t49\t0\t1\t50\t0\t0\t0.010000\t0.510000\t0.020000") {
		t.Fatalf("bad error bounds:\n%s", b)
	}
}

func TestOutcomeLineNeedsSingleCall(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	e := transcript.Event{Role: transcript.Out}
	calls, err := shellcmd.Calls("prior one; prior two")
	if err != nil {
		t.Fatal(err)
	}
	code := u.pairUtilityCode("prior", calls, 1, u.eventExitCode(e), "== пусто: журнал решений")
	if pairOutcome(pairCall{tool: "prior", code: code, out: "== пусто: журнал решений"}) != "answer" {
		t.Fatal("ambiguous line counted as answer")
	}
}

func TestPairRepeatsIncomplete(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	t0 := time.Now().UTC().Truncate(time.Hour)
	u.sessionEnds = map[string]time.Time{"s": t0.Add(20 * time.Minute)}
	u.pairCalls = []pairCall{{tool: "user-word", way: "old", goal: "same phrase", session: "s", command: "rg phrase", at: t0}, {tool: "user-word", way: "old", goal: "same phrase", session: "s", command: "rg phrase -n", at: t0.Add(3 * time.Minute)}, {tool: "user-word", way: "old", goal: "same phrase", session: "s", command: "rg phrase --fixed", at: t0.Add(18 * time.Minute)}}
	if err := u.writePairRepeats(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(u.outputDir, "pair-repeats.tsv"))
	if !strings.Contains(string(b), "user-word\told\t3\t1\t1") {
		t.Fatalf("repeat/incomplete counters wrong:\n%s", b)
	}
}

func TestPairPageShotLocalOnly(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	local, _ := shellcmd.Calls("chromium --headless --screenshot=file:///x.html")
	remote, _ := shellcmd.Calls("chromium --headless --screenshot https://example.org")
	if w, g := u.oldPair(local[0], "chromium --headless --screenshot=file:///x.html"); w != "page-shot" || g == "" {
		t.Fatalf("local screenshot = %q %q", w, g)
	}
	if w, _ := u.oldPair(remote[0], "chromium --headless --screenshot https://example.org"); w != "" {
		t.Fatalf("remote screenshot classified as %q", w)
	}
}

func TestPairPriorTasksSeparate(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	cs, _ := shellcmd.Calls("rg deadline docs/tasks/")
	if w, _ := u.oldPair(cs[0], "rg deadline docs/tasks/"); w != "" {
		t.Fatalf("task search classified as %q", w)
	}
}

func TestPairReleasesDate(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "releases.tsv")
	available := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	releaseData := fmt.Sprintf("busy\t%s\t\nprior\t\t\nuser-word\t\t\npage-shot\t\t\nclaude-rules-export\t\t\nwfor\t\t\n", available)
	if err := os.WriteFile(path, []byte(releaseData), 0600); err != nil {
		t.Fatal(err)
	}
	u := newRun(time.Time{}, time.Time{}, 1, d, d)
	u.releasesPath = path
	if _, err := u.loadReleases(); err != nil {
		t.Fatal(err)
	}
	u.pairCalls = []pairCall{{tool: "busy", way: "old", goal: "make", at: time.Now().UTC()}}
	if err := u.writePairTables(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(d, "pairs.tsv"))
	if strings.Contains(string(b), "busy\tunknown\tno\tno-date\told\t0\t1") {
		t.Fatal("pre-release old episode counted")
	}
	os.WriteFile(path, []byte("bad\n"), 0600)
	if _, err := u.loadReleases(); err == nil {
		t.Fatal("malformed release file accepted")
	}
}

func TestTkRefusalOutcomes(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	base := time.Now().UTC().Truncate(time.Hour)
	u.sessionEnds = map[string]time.Time{}
	for i, outcome := range []string{"same", "other", "bypass", "incomplete", "dropped"} {
		session := fmt.Sprintf("session-%d", i)
		r := tkRefusal{kind: "bad args", session: session, at: base, ticket: "x.md", subcommand: "new"}
		u.tkRefusals = append(u.tkRefusals, r)
		u.sessionEnds[session] = base.Add(20 * time.Minute)
		switch outcome {
		case "same":
			u.tkActions = append(u.tkActions, taskWrite{session: session, source: "tk", key: "x.md", op: "new", at: base.Add(time.Minute)})
		case "other":
			u.tkActions = append(u.tkActions, taskWrite{session: session, source: "tk", key: "x.md", op: "edit", at: base.Add(time.Minute)})
		case "bypass":
			u.tkActions = append(u.tkActions, taskWrite{session: session, source: "manual", key: "x.md", at: base.Add(time.Minute)})
		case "incomplete":
			u.sessionEnds[session] = base.Add(time.Minute)
		}
	}
	if err := u.writeTKRefusals(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(u.outputDir, "tk-refusals.tsv"))
	if !strings.Contains(string(b), "bad args\t5\t1\t1\t1\t1\t1") {
		t.Fatalf("refusal outcomes misclassified:\n%s", b)
	}
}

func TestCandidatesBlind(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	for i := range 40 {
		u.pairCandidates = append(u.pairCandidates, pairCandidate{id: fmt.Sprintf("synthetic:%d", i), pair: "busy", command: "uptime", yes: i%2 == 0})
	}
	if err := u.writeCandidates(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(u.outputDir, "candidates.tsv"))
	v, _ := os.ReadFile(filepath.Join(u.outputDir, "candidates-verdict.tsv"))
	if strings.Contains(strings.Split(string(b), "\n")[0], "verdict") {
		t.Fatal("candidate file exposes verdict")
	}
	if len(strings.Split(strings.TrimSpace(string(v)), "\n")) != 41 {
		t.Fatal("missing blinded verdict records")
	}
}

func TestCandidatesOrderHidesVerdict(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 91, t.TempDir(), t.TempDir())
	for i := range 40 {
		u.pairCandidates = append(u.pairCandidates, pairCandidate{id: fmt.Sprintf("same:%03d", i), pair: "busy", command: "uptime", yes: true})
		u.pairCandidates = append(u.pairCandidates, pairCandidate{id: fmt.Sprintf("same:%03d", i+40), pair: "busy", command: "uptime", yes: false})
	}
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(u.outputDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := u.writeCandidates(); err != nil {
		t.Fatal(err)
	}
	first, verdicts := read("candidates.tsv"), read("candidates-verdict.tsv")
	labels := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(verdicts)), "\n")[1:] {
		fields := strings.Split(line, "\t")
		labels[fields[0]] = fields[2]
	}
	last, switches := "", 0
	for _, line := range strings.Split(strings.TrimSpace(string(first)), "\n")[1:] {
		id := strings.Split(line, "\t")[0]
		if last != "" && labels[id] != last {
			switches++
		}
		last = labels[id]
	}
	if switches < 5 {
		t.Fatalf("candidate order reveals verdict: only %d switches", switches)
	}
	if err := u.writeCandidates(); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(string(first), string(read("candidates.tsv"))) || string(verdicts) != string(read("candidates-verdict.tsv")) {
		t.Fatal("candidate sample changed with the same seed")
	}
}

func TestReportShareNeedsLabels(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	base := time.Now()
	for i := range 20 {
		old := pairAt("old", fmt.Sprintf("goal-%d", i), i)
		old.tool, old.session, old.at = "prior", fmt.Sprintf("s-%d", i), base.Add(time.Duration(i)*time.Hour)
		newCall := old
		newCall.way = "new"
		newCall.id += "-new"
		newCall.at = old.at.Add(time.Minute)
		u.pairCalls = append(u.pairCalls, old, newCall)
	}
	var b strings.Builder
	u.writePairReport(&b)
	if !strings.Contains(b.String(), "не проверено разметкой") || strings.Contains(b.String(), "50.0%") {
		t.Fatalf("unlabeled share was presented as a conclusion:\n%s", b.String())
	}
}

func TestReportShareLabelsThresholds(t *testing.T) {
	build := func(nYes, nNo, yesCorrect, noPositive int) *run {
		u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
		u.labelsPath = "synthetic-labels"
		for i := range nYes {
			id := fmt.Sprintf("yes-%d", i)
			candidate := pairCandidate{id: id, pair: "prior", yes: true}
			u.pairCandidates = append(u.pairCandidates, candidate)
			u.pairCandidateSample = append(u.pairCandidateSample, candidate)
			label := "no"
			if i < yesCorrect {
				label = "yes"
			}
			u.labels = ensureLabels(u.labels)
			u.labels[id] = label
		}
		for i := range nNo {
			id := fmt.Sprintf("no-%d", i)
			candidate := pairCandidate{id: id, pair: "prior"}
			u.pairCandidates = append(u.pairCandidates, candidate)
			u.pairCandidateSample = append(u.pairCandidateSample, candidate)
			label := "no"
			if i < noPositive {
				label = "yes"
			}
			u.labels = ensureLabels(u.labels)
			u.labels[id] = label
		}
		return u
	}
	precisionBad := build(50, 0, 35, 0)
	_, _, pass, reason := precisionBad.pairLabelMetrics("prior")
	if pass || !strings.Contains(reason, "точность") {
		t.Fatalf("precision threshold not enforced: %q", reason)
	}
	recallBad := build(30, 90, 30, 25)
	_, _, pass, reason = recallBad.pairLabelMetrics("prior")
	if pass || !strings.Contains(reason, "полнота") {
		t.Fatalf("recall threshold not enforced: %q", reason)
	}
	passRun := build(90, 10, 90, 0)
	_, _, pass, reason = passRun.pairLabelMetrics("prior")
	if !pass {
		t.Fatalf("valid labels rejected: %s", reason)
	}
}
func ensureLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return labels
}

func TestLabelsFileErrors(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"header", "id\twrong\n"},
		{"empty-id", "id\tlabel\n\tyes\n"},
		{"invalid-label", "id\tlabel\ncandidate\tmaybe\n"},
		{"duplicate", "id\tlabel\ncandidate\tyes\ncandidate\tno\n"},
		{"unknown", "id\tlabel\nunknown\tyes\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "labels.tsv")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
			u.labelsPath = path
			u.pairCandidates = []pairCandidate{{id: "candidate", pair: "prior"}}
			u.pairCandidateSample = append(u.pairCandidateSample, u.pairCandidates[0])
			err := u.loadPairLabels()
			if err == nil || !strings.Contains(err.Error(), "line ") {
				t.Fatalf("expected line-specific error, got %v", err)
			}
		})
	}
}

func TestReportShareByLayer(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	u.labelsPath = "labels"
	u.labels = map[string]string{}
	for _, layer := range []struct{ agent, instructed, period string }{{"agent-a", "yes", "no-date"}, {"agent-b", "no", "no-date"}} {
		for i, way := range []string{"new", "old"} {
			call := pairAt(way, layer.agent+fmt.Sprint(i), i)
			call.tool, call.agent, call.session, call.period = "prior", layer.agent, layer.agent, layer.period
			call.instructed = layer.instructed == "yes"
			u.pairCalls = append(u.pairCalls, call)
		}
	}
	var b strings.Builder
	u.writePairReport(&b)
	for _, want := range []string{"| prior | agent-a | yes | no-date |", "| prior | agent-b | no | no-date |"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing layer row %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "| prior | all |") {
		t.Fatal("report merged layers")
	}
}

func TestPairCodeSeveralCallsNotOwned(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	calls, err := shellcmd.Calls("page-shot a.html; page-shot b.html")
	if err != nil {
		t.Fatal(err)
	}
	for i := range calls {
		if got := u.pairUtilityCode("page-shot", calls, i, 1, ""); got != -1 {
			t.Fatalf("call %d incorrectly owns command exit code %d", i, got)
		}
	}
}

func TestPageShotNewLocalOnly(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	session := filepath.Join(home, ".claude", "projects", "synthetic", "transcript.jsonl")
	base := time.Now().UTC().Truncate(time.Second)
	lines := []string{fmt.Sprintf(`{"parentUuid":null,"type":"user","timestamp":%q,"message":{"role":"user","content":"synthetic"}}`, base.Format(time.RFC3339))}
	for i, cmd := range []string{"page-shot file:///synthetic/page.html", "page-shot /synthetic/local.html", "page-shot https://example.org/page.html"} {
		at, id := base.Add(time.Duration(i+1)*time.Minute), fmt.Sprintf("shot-%d", i)
		lines = append(lines,
			fmt.Sprintf(`{"parentUuid":"a%d","type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q}}]}}`, i, at.Format(time.RFC3339), id, cmd),
			fmt.Sprintf(`{"parentUuid":"b%d","type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"captured"}]}}`, i, at.Add(time.Second).Format(time.RFC3339), id))
	}
	writeTranscript(t, session, strings.Join(lines, "\n"))
	u := newRun(base.Add(-time.Minute), base.Add(time.Hour), 5, filepath.Join(root, "out"), home)
	if err := u.runUptake(base.Add(-time.Minute), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	count := 0
	for i := range u.pairCalls {
		if u.pairCalls[i].tool == "page-shot" && u.pairCalls[i].way == "new" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("new page-shot local calls=%d, want 2: %+v", count, u.pairCalls)
	}
}

func TestPairRepeatsNearSessionEnd(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	first := pairAt("new", "/same", 0)
	first.tool, first.session, first.command = "wfor", "s", "wfor file /same"
	second := first
	second.at = first.at.Add(5 * time.Minute)
	second.command = "wfor file /same --timeout 10"
	u.pairCalls = []pairCall{first, second}
	u.sessionEnds = map[string]time.Time{"s": first.at.Add(10 * time.Minute)}
	if err := u.writePairRepeats(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(u.outputDir, "pair-repeats.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
		fields := strings.Split(line, "\t")
		if fields[0] == "wfor" && fields[1] == "new" {
			if fields[2] != "2" || fields[3] != "1" || fields[4] != "1" || fields[5] != "1.000000" {
				t.Fatalf("repeat accounting wrong: %s", line)
			}
			return
		}
	}
	t.Fatal("wfor new row missing")
}

func TestBusyDecisionAfterProbe(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	probeGoal := func(command string) string {
		calls, err := shellcmd.Calls(command)
		if err != nil {
			t.Fatal(err)
		}
		for i := range calls {
			if calls[i].Name == "busy" {
				return u.busyGoal(nil, 0, command, calls[i])
			}
		}
		t.Fatalf("busy call missing in %q", command)
		return ""
	}
	if got := probeGoal("go test ./...; busy"); got != "" {
		t.Fatalf("heavy command before busy paired: %q", got)
	}
	if got := probeGoal("busy; go test ./..."); got == "" {
		t.Fatal("heavy command after busy was not paired")
	}
}

func TestReportLimitationsAndErrors(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	u.pairCalls = []pairCall{{tool: "busy", way: "new", goal: "synthetic", agent: "agent", session: "s", period: "no-date", outcome: "fail", at: time.Now()}}
	var b strings.Builder
	u.writePairReport(&b)
	text := b.String()
	for _, phrase := range []string{"не по смыслу", "не обязательность", "внутри широкого надмножества", "Взвешивания по составу проектов нет", "версии утилит не учитываются", "ошибки вызова (fail)"} {
		if !strings.Contains(text, phrase) {
			t.Errorf("report missing %q", phrase)
		}
	}
	if !strings.Contains(text, "| busy | agent | no | no-date |") || !strings.Contains(text, "| 1 |") {
		t.Fatalf("failure count missing:\n%s", text)
	}
}

func TestPairUserWordOverlap(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	session := filepath.Join(home, ".claude", "projects", "-x", "session.jsonl")
	base := time.Now().UTC().Truncate(time.Second)
	var transcriptLines []string
	transcriptLines = append(transcriptLines, fmt.Sprintf(`{"parentUuid":null,"type":"user","timestamp":%q,"message":{"role":"user","content":"synthetic session"}}`, base.Format(time.RFC3339)))
	add := func(i int, cmd, out string, code int) {
		at := base.Add(time.Duration(i*60) * time.Second).Format(time.RFC3339)
		id := fmt.Sprintf("call-%d", i)
		transcriptLines = append(transcriptLines,
			fmt.Sprintf(`{"parentUuid":"a%d","type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q}}]}}`, i, at, id, cmd),
			fmt.Sprintf(`{"parentUuid":"b%d","type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"is_error":%t,"content":%q}]}}`, i, base.Add(time.Duration(i*60+1)*time.Second).Format(time.RFC3339), id, code != 0, out))
	}
	add(1, "user-word --text 'two words'", `user-word: no user line with "two words" in transcript`, 1)
	add(2, "claude-rules-export "+filepath.Join(root, "rules-out"), "claude-rules-export: target is missing", 1)
	add(9, "prior synthetic query", "prior: invalid query\n(prior -h for usage)", 1)
	add(3, "grep 'две фразы' "+session, "", 0)
	add(4, "cat "+filepath.Join(home, ".claude", "improve", "state", "x.md"), "state", 0)
	add(5, "uptime", "", 0)
	add(6, "echo no decision", "", 0)
	ticket := "synthetic-ticket-42"
	add(7, "tk done "+ticket, "tk: cannot update "+filepath.Join(home, ".cache", "claude-tmp", ticket)+".md", 1)
	add(8, "tk done "+ticket, "done", 0)
	add(10, "jq keys "+session, "[]", 0)
	decisions := filepath.Join(home, ".claude", "decisions.md")
	add(11, "rg deadline "+decisions, "deadline: synthetic", 0)
	improve := filepath.Join(home, ".claude", "improve", "state", "notes.md")
	add(12, "rg deadline "+improve, "deadline: synthetic", 0)
	add(13, "tl --role user --grep deadline", "== пусто: synthetic", 0)
	add(20, "echo finished", "", 0)
	writeTranscript(t, session, strings.Join(transcriptLines, "\n"))
	u := newRun(base.Add(-time.Minute), base.Add(time.Hour), 71, filepath.Join(root, "out"), home)
	if err := u.runUptake(base.Add(-time.Minute), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(u.outputDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	outcomes := read("outcomes.tsv")
	for _, want := range []string{"user-word\tnew\t1\t1\t0\t0", "claude-rules-export\tnew\t1\t1\t0\t0"} {
		if !strings.Contains(outcomes, want) {
			t.Errorf("outcomes.tsv missing %q:\n%s", want, outcomes)
		}
	}
	if got := read("pair-overlap.tsv"); !strings.Contains(got, "tl\tuser-word\t1") {
		t.Errorf("transcript grep overlap missing:\n%s", got)
	}
	if got := read("pairs.tsv"); strings.Contains(got, "user-word\tclaude-main\tno\tno-date\told\t0\t1") {
		t.Errorf("jq keys was classified as user-word search:\n%s", got)
	}
	pairs := read("pairs.tsv")
	priorOldCalls := 0
	for row := range strings.SplitSeq(pairs, "\n") {
		fields := strings.Split(row, "\t")
		if len(fields) > 9 && fields[0] == "prior" && fields[4] == "old" {
			priorOldCalls, _ = strconv.Atoi(fields[9])
		}
	}
	if priorOldCalls != 2 {
		t.Errorf("prior old should count improve search and tl --grep, but not cat: %d", priorOldCalls)
	}
	cand, verdict := read("candidates.tsv"), read("candidates-verdict.tsv")
	if !strings.Contains(cand, "uptime") {
		t.Errorf("busy probe absent from candidates:\n%s", cand)
	}
	if strings.Contains(verdict, "busy\tyes") {
		t.Errorf("probe without own decision has positive verdict:\n%s", verdict)
	}
	refusals := read("tk-refusals.tsv")
	if !strings.Contains(refusals, "cannot update\t1\t1\t0\t0\t0\t0") {
		t.Errorf("tk id did not match follow-up action:\n%s", refusals)
	}
	if got := read("pair-fail-samples.tsv"); !strings.HasPrefix(got, "tool\tid\tcode\toutput\n") {
		t.Errorf("failure sample output missing header:\n%s", got)
	}
	report := read("report.md")
	for _, tool := range pairTools() {
		if !strings.Contains(report, "| "+tool+" |") {
			t.Errorf("report missing %s pair row", tool)
		}
	}
	if !strings.Contains(report, "## Ограничения замера") {
		t.Error("report limitations missing")
	}
	if !strings.Contains(report, "Поиск решений (по правилу): 1 вызовов.") {
		t.Errorf("decision rule search count missing: %s", report)
	}
	if !strings.Contains(report, "Prior old по источнику: .claude/improve/ — 1 вызовов; tl --role user --grep — 1 вызовов.") {
		t.Errorf("prior old source counts missing: %s", report)
	}
}

func TestPairRejectedToolUseNotCounted(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "synthetic-home")
	session := filepath.Join(home, ".claude", "projects", "synthetic", "session.jsonl")
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	id := "rejected-shot"
	quoted := "quoting-prior"
	lines := []string{
		fmt.Sprintf(`{"parentUuid":null,"type":"user","timestamp":%q,"message":{"role":"user","content":"synthetic"}}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"parentUuid":"a","type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"page-shot file:///synthetic.html"}}]}}`, base.Add(time.Second).Format(time.RFC3339), id),
		fmt.Sprintf(`{"parentUuid":"b","type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"is_error":true,"content":"The user doesn't want to proceed with this tool use."}]}}`, base.Add(2*time.Second).Format(time.RFC3339), id),
		// An ordinary result that only quotes the refusal text is a real call.
		fmt.Sprintf(`{"parentUuid":"c","type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"prior refusal"}}]}}`, base.Add(3*time.Second).Format(time.RFC3339), quoted),
		fmt.Sprintf(`{"parentUuid":"d","type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"is_error":false,"content":"synthetic: The user doesn't want to proceed with this tool use."}]}}`, base.Add(4*time.Second).Format(time.RFC3339), quoted),
	}
	writeTranscript(t, session, strings.Join(lines, "\n"))
	u := newRun(base.Add(-time.Minute), base.Add(time.Minute), 1, filepath.Join(root, "out"), home)
	if err := u.runUptake(base.Add(-time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	outcomes, err := os.ReadFile(filepath.Join(u.outputDir, "outcomes.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(outcomes), "page-shot\tnew\t1") {
		t.Fatalf("rejected tool use counted as a pair call: %s", outcomes)
	}

	if !strings.Contains(string(outcomes), "prior\tnew\t1\t") {
		t.Fatalf("a result quoting the refusal text must count as a call: %s", outcomes)
	}
	if u.rejectedToolUses != 1 {
		t.Fatalf("rejected tool use count = %d, want 1", u.rejectedToolUses)
	}
}

func TestPairUnknownSamples(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 3, t.TempDir(), t.TempDir())
	u.pairCalls = []pairCall{
		{tool: "page-shot", way: "new", id: "synthetic:7", code: -1, codeNotOwned: true, outcome: "unknown", command: "page-shot file:///synthetic.html\n| tail -8", out: "first\tline\nsecond"},
		{tool: "prior", way: "new", id: "synthetic:8", code: -1, outcome: "unknown", command: "prior query", out: ""},
		{tool: "prior", way: "old", id: "synthetic:9", code: -1, outcome: "unknown"},
		{tool: "page-shot", way: "new", id: "synthetic:10", code: -1, codeNotOwned: true, outcome: "unknown", command: "page-shot x.html 2>&1 && echo ok", out: "ok"},
	}
	if err := u.writePairUnknown(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(u.outputDir, "pair-unknown.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	want := "tool\tref\tcode\tcommand\toutput\treason\npage-shot\tsynthetic:7\t-1\tpage-shot file:///synthetic.html\\n| tail -8\tfirst\\tline\\nsecond\toutput-hidden\nprior\tsynthetic:8\t-1\tprior query\t\tno-output-form\npage-shot\tsynthetic:10\t-1\tpage-shot x.html 2>&1 && echo ok\tok\tcode-not-own\n"
	if string(data) != want {
		t.Fatalf("unknown samples differ:\n%s", data)
	}
	if u.pairUnknownReasons["output-hidden"] != 1 || u.pairUnknownReasons["no-output-form"] != 1 || u.pairUnknownReasons["code-not-own"] != 1 {
		t.Fatalf("unknown reason counts: %#v", u.pairUnknownReasons)
	}
}

func TestPairUtilityExitCodeAttribution(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "synthetic-home")
	session := filepath.Join(home, ".claude", "projects", "synthetic", "transcript.jsonl")
	base := time.Now().UTC().Truncate(time.Second)
	lines := []string{fmt.Sprintf(`{"parentUuid":null,"type":"user","timestamp":%q,"message":{"role":"user","content":"synthetic"}}`, base.Format(time.RFC3339))}
	add := func(i int, command string, code int, output string) {
		at := base.Add(time.Duration(i) * time.Minute)
		id := fmt.Sprintf("call-%d", i)
		lines = append(lines,
			fmt.Sprintf(`{"parentUuid":"assistant-%d","type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q}}]}}`, i, at.Format(time.RFC3339), id, command),
			fmt.Sprintf(`{"parentUuid":"result-%d","type":"user","timestamp":%q,"toolUseResult":{"exitCode":%d},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"is_error":%t,"content":%q}]}}`, i, at.Add(time.Second).Format(time.RFC3339), code, id, code != 0, output))
	}
	add(1, "busy | head -1; go test ./...", 1, "ambiguous output")
	add(2, "echo prefix; busy; timeout 590 bash script.sh | tail -30", 1, "Exit code 1\nbusy: runnable 22 is at or above 22; runnable 22 (busy at 22, threshold)")
	add(3, "go test ./... && busy", 1, "ambiguous output")
	add(4, "date '+%T'; busy; go test ./... | sed -n '1,8p'", 0, "busy: runnable 22 is at or above 22; runnable 22 (busy at 22, threshold)")
	add(5, "prior alpha beta 2>&1 | head -1", 1, "== пусто: журнал решений; артефакты")
	add(6, "page-shot file:///synthetic.html | head -1", 1, "/synthetic.html-shots/1440x900.png")
	writeTranscript(t, session, strings.Join(lines, "\n"))
	u := newRun(base.Add(-time.Minute), base.Add(time.Hour), 17, filepath.Join(root, "output"), home)
	if err := u.runUptake(base.Add(-time.Minute), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(u.outputDir, "outcomes.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	row := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range row[1:] {
		fields := strings.Split(line, "\t")
		if len(fields) == 13 && fields[0] == "busy" && fields[1] == "new" {
			if fields[3] != "2" || fields[5] != "1" || fields[6] != "1" {
				t.Fatalf("busy exit codes were assigned to wrong calls: %s", line)
			}
		}
	}
	for _, tool := range []string{"prior", "page-shot"} {
		found := false
		for _, line := range row[1:] {
			fields := strings.Split(line, "\t")
			if len(fields) == 13 && fields[0] == tool && fields[1] == "new" {
				found = true
				if fields[3] != "1" || fields[6] != "0" {
					t.Errorf("%s output outcome lost when outer code is unowned: %s", tool, line)
				}
			}
		}
		if !found {
			t.Errorf("%s new outcomes missing: %s", tool, data)
		}
	}
}

func TestLabelRecallWithoutLabelledNo(t *testing.T) {
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	u.labelsPath = "synthetic-labels"
	u.labels = map[string]string{}
	for i := range 40 {
		c := pairCandidate{id: fmt.Sprintf("yes-%d", i), pair: "prior", yes: true}
		u.pairCandidates = append(u.pairCandidates, c)
		u.pairCandidateSample = append(u.pairCandidateSample, c)
		u.labels[c.id] = "yes"
	}
	// "no" candidates exist but none was sampled and labelled: the recall is unknown, not 1.
	for i := range 40 {
		u.pairCandidates = append(u.pairCandidates, pairCandidate{id: fmt.Sprintf("no-%d", i), pair: "prior"})
	}
	_, recall, pass, reason := u.pairLabelMetrics("prior")
	if pass || recall >= 0.7 || !strings.Contains(reason, "полнота") {
		t.Fatalf("recall without labelled no = %.3f, pass %v, %q; want a failed recall", recall, pass, reason)
	}
}

func TestLabelsFileMissingSampledID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labels.tsv")
	if err := os.WriteFile(path, []byte("id\tlabel\nfirst\tyes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := newRun(time.Time{}, time.Time{}, 1, t.TempDir(), t.TempDir())
	u.labelsPath = path
	u.pairCandidateSample = []pairCandidate{{id: "first", pair: "prior"}, {id: "second", pair: "prior"}}
	if err := u.loadPairLabels(); err == nil || !strings.Contains(err.Error(), "1 sampled candidates have no label") {
		t.Fatalf("partial labels: %v", err)
	}
}
