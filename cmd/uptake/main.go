// uptake — недельный замер подхвата утилит akit по транскриптам агентов: сколько вызовов у каждой утилиты против
// прежнего способа, исходы вызовов, ошибки, пары «утилита против прежнего способа» по эпизодам цели и выборка
// кандидатов для слепой разметки; итог — report.md и TSV в папке --out.
//
// Коды выхода: 0 — замер записан, 1 — ошибка чтения или записи, 2 — ошибка вызова.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/akit/secretmask"
	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/timearg"
	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/waits"
)

const (
	fileMode = 0o644
	dirMode  = 0o755
)

type example struct{ kind, ref, agent, command, output, next, reason, day string }
type call struct {
	ev                           transcript.Event
	command, agent, tool, output string
	code                         int
	ordinal                      int
}
type taskWrite struct {
	agent, file, key, source, op string
	session                      string
	at                           time.Time
	line                         int
	failed                       bool
}

type run struct {
	start, end                                              string
	seed                                                    int
	outputDir, home                                         string
	counts                                                  map[string]int
	examples, subCases, editSamples, waitSamples            []example
	habitCandidates                                         map[string][]example
	failureSamples                                          []example
	refusalSamples                                          map[string][]example
	unknownProbes, tlOldChecks, tlOldEmpty, tlNewEmpty      []example
	tlOldCalls, tlOldRetries, tlNewRetries, tlNewEmptyCount int
	tkPair                                                  map[string]*[2]int
	utilityNames                                            []string
	errorStats                                              map[string]*errorStat
	editAttempts                                            []editAttempt
	unreadFiles, badLines                                   int
	firstUnread, firstBad                                   string
	hookCounts                                              map[string]int
	hookSummary                                             hookSummary
	hookReadErrors                                          []string
	hookMissing                                             []string
	hookBadLines                                            int
	firstHookBad                                            string
	releasesPath, labelsPath                                string
	releases                                                map[string]releaseDate
	labelRows                                               map[string]labelRow
	labels                                                  map[string]string
	pairCalls                                               []pairCall
	pairOldIDs                                              map[string]map[string]struct{}
	pairCandidates                                          []pairCandidate
	pairCandidateSample                                     []pairCandidate
	tkRefusals                                              []tkRefusal
	tkActions                                               []taskWrite
	pairOverlap                                             []pairOverlap
	busyQuiet, busyNoDecision                               int
	rejectedToolUses                                        int
	pairUnknownReasons                                      map[string]int
	decisionRuleSearches                                    int
	priorImproveCalls, priorTLCalls                         int
	sessionEnds                                             map[string]time.Time
	pairPatterns                                            pairPatterns
}

func newRun(since, until time.Time, seed int, out, home string) *run {
	return &run{
		start: since.Format(time.RFC3339Nano), end: until.Format(time.RFC3339Nano), seed: seed,
		outputDir: out, home: home,
		counts: map[string]int{}, habitCandidates: map[string][]example{}, refusalSamples: map[string][]example{},
		tkPair: map[string]*[2]int{}, utilityNames: []string{"busy", "prior", "tk", "user-word", "page-shot", "webtext", "claude-rules-export", "sub", "tl", "wfor"},
		errorStats:   map[string]*errorStat{},
		hookCounts:   map[string]int{},
		pairPatterns: newPairPatterns(),
	}
}

// tkPair: row -> [repeats, eligible writes]

func (u *run) tkPairAdd(row string, repeat bool) {
	c := u.tkPair[row]
	if c == nil {
		c = &[2]int{}
		u.tkPair[row] = c
	}
	c[1]++
	if repeat {
		c[0]++
	}
}

func (u *run) inc(day, agent, tool, metric string, n int) {
	if n > 0 {
		u.counts[strings.Join([]string{day, agent, tool, metric}, "\t")] += n
	}
}
func (u *run) day(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }
func (u *run) valid(t time.Time) bool {
	a, errA := time.Parse(time.RFC3339Nano, u.start)
	b, errB := time.Parse(time.RFC3339Nano, u.end)
	return errA == nil && errB == nil && !t.Before(a) && t.Before(b)
}
func maskSecrets(s string) string {
	masked, _ := secretmask.Mask(s)
	return masked
}

// closeQuote ends a cell whose quote a cut left open: read as a whole file, the
// next row would otherwise become part of a quoted value.
func closeQuote(s string) string {
	var open rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && open != '\'':
			escaped = true
		case open == 0 && (r == '"' || r == '\''):
			open = r
		case r == open:
			open = 0
		}
	}
	if escaped {
		s += `\`
	}
	if open != 0 {
		s += string(open)
	}
	return s
}

// finishCell is the last step of a table cell, after its line breaks are written
// out: a label and the next line now stand side by side, and the mask sees them so.
func finishCell(s string) string {
	return closeQuote(maskSecrets(s))
}

func (u *run) clean(s string) string {
	s = maskSecrets(s)
	s = strings.ReplaceAll(s, "\x00", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return finishCell(strings.TrimSpace(s))
}
func (u *run) modelMap(path string) map[int]string {
	f, e := os.Open(path)
	if e != nil {
		return nil
	}
	defer f.Close()
	m := map[int]string{}
	model := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	n := 0
	for sc.Scan() {
		n++
		var v struct {
			Type    string `json:"type"`
			Payload struct {
				Type  string `json:"type"`
				Model string `json:"model"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &v) == nil && v.Type == "turn_context" && v.Payload.Model != "" {
			model = v.Payload.Model
		}
		if model != "" {
			m[n] = model
		}
	}
	return m
}
func (u *run) modelAt(m map[int]string, n int) string {
	for n > 0 {
		if s := m[n]; s != "" {
			return s
		}
		n--
	}
	return "unknown"
}
func (u *run) agentOf(path string, claudeSub bool, model string) string {
	if strings.Contains(path, "/projects/") {
		if claudeSub {
			return "claude-sub"
		}
		return "claude-main"
	}
	low := strings.ToLower(model)
	if strings.Contains(low, "luna") {
		return "codex-luna"
	}
	if strings.Contains(low, "terra") {
		return "codex-terra"
	}
	if strings.Contains(low, "sol") {
		return "codex-sol"
	}
	return "codex-" + strings.ReplaceAll(strings.ToLower(model), " ", "_")
}
func (u *run) extract(path string) (bool, string) {
	isClaude := strings.Contains(path, "/projects/")
	sub := strings.Contains(path, "/subagents/")
	return isClaude, u.agentOf(path, sub, "")
}
func (u *run) habitType(cmd string) string {
	calls, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // nested scripts can fail after yielding usable outer calls
		slog.Debug("parse shell habit", "error", err)
	}
	for _, c := range calls {
		if habit := u.habitForCall(c, cmd); habit != "" {
			return habit
		}
	}
	return ""
}

func (u *run) habitForCall(c shellcmd.Call, cmd string) string {
	low := strings.ToLower(c.Raw)
	args := strings.ToLower(strings.Join(c.Args, " "))
	if u.isAnchorHabit(c, low) {
		return "habit-anchor"
	}
	if u.isTranscriptHabit(c, args, cmd) {
		return "habit-transcript"
	}
	if directEditCall(c) {
		return "habit-edit"
	}
	return ""
}

func (u *run) isAnchorHabit(c shellcmd.Call, low string) bool {
	if c.Name == "sed" {
		return sedInPlace(c.Args) && (regexp.MustCompile(`(^|\s)[0-9]+(?:,[0-9]+)?[acdi]?(\s|$)`).MatchString(low) || regexp.MustCompile(`(^|\s)'?/(?:[^ ]|\\/)+/\s*,\s*/`).MatchString(low))
	}
	return (c.Name == "python" || c.Name == "python3") && (strings.Contains(low, ".index(") || strings.Contains(low, ".find(")) && (strings.Contains(low, "read_text") || strings.Contains(low, "open(")) && strings.Contains(low, "[") && (strings.Contains(low, "write_text") || strings.Contains(low, ".write("))
}

func (u *run) isTranscriptHabit(c shellcmd.Call, args, cmd string) bool {
	return transcriptSearchCall(c, args, strings.ToLower(cmd), u)
}

func transcriptSearchCall(c shellcmd.Call, args, lowCmd string, u *run) bool {
	if c.Name != "grep" && c.Name != "rg" && c.Name != "jq" && c.Name != "sed" {
		return false
	}
	if !u.transcriptArgs(args, lowCmd) {
		return false
	}
	return c.Name != "jq" || !jqNullInputReadsNothing(c.Args)
}

// directEditCall — единое определение прямой правки для привычек и способов.
func directEditCall(c shellcmd.Call) bool {
	low := strings.ToLower(c.Raw)
	switch c.Name {
	case "sed":
		return sedInPlace(c.Args)
	case "perl":
		return perlInPlace(c.Args)
	}
	if !editInterpreter(c) {
		return false
	}
	scripts := []string{c.Raw}
	if interpreterReadsStdinScript(c) {
		for _, r := range c.Redirs {
			if (r.N == "" || r.N == "0") && (r.Op == "<<" || r.Op == "<<-") {
				scripts = append(scripts, r.Body)
			} else if (r.N == "" || r.N == "0") && r.Op == "<<<" {
				scripts = append(scripts, r.Target)
			}
		}
	}
	low = strings.ToLower(strings.Join(scripts, "\n"))
	return strings.Contains(low, ".replace(") && (strings.Contains(low, "write_text") || strings.Contains(low, ".write(") || strings.Contains(low, "writefilesync("))
}

func interpreterReadsStdinScript(c shellcmd.Call) bool {
	args, python, ok := interpreterStdinArgs(c)
	return ok && stdinScriptArgs(args, python)
}

func interpreterStdinArgs(c shellcmd.Call) ([]string, bool, bool) {
	args := c.Args
	name := strings.ToLower(c.Name)
	python := pythonExecutable(name)
	switch name {
	case "uv", "poetry", "pipx", "pdm":
		if len(args) < 2 || args[0] != "run" || !pythonExecutable(args[1]) {
			return nil, false, false
		}
		return args[2:], true, true
	case "node":
		return args, false, true
	default:
		return args, python, python
	}
}

func stdinScriptArgs(args []string, python bool) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if isInterpreterInlineScriptFlag(a) || (python && a == "-m") {
			return false
		}
		if a == "--" {
			return i+1 == len(args) || args[i+1] == "-"
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			if interpreterFlagTakesValue(a, python) {
				i++
			}
			continue
		}
		return a == "-"
	}
	return true
}

func isInterpreterInlineScriptFlag(arg string) bool {
	return arg == "-c" || arg == "-e" || arg == "--eval" || arg == "--print"
}

func interpreterFlagTakesValue(arg string, python bool) bool {
	if python {
		return arg == "-W" || arg == "-X"
	}
	return arg == "--require" || arg == "-r" || arg == "--loader" || arg == "--import"
}

func editInterpreter(c shellcmd.Call) bool {
	name := strings.ToLower(c.Name)
	if name == "node" || regexp.MustCompile(`^python(?:[0-9]+(?:\.[0-9]+)*)?$`).MatchString(name) {
		return true
	}
	if name == "uv" || name == "poetry" || name == "pipx" || name == "pdm" {
		return len(c.Args) >= 2 && c.Args[0] == "run" && pythonExecutable(c.Args[1])
	}
	return false
}

func pythonExecutable(name string) bool {
	name = strings.ToLower(name)
	return name == "python" || regexp.MustCompile(`^python[0-9]+(?:\.[0-9]+)*$`).MatchString(name)
}

func sedInPlace(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
			return true
		}
		if len(a) < 2 || a[0] != '-' || strings.HasPrefix(a, "--") {
			continue
		}
		flags := a[1:]
		if strings.HasPrefix(flags, "i") || strings.ContainsRune(flags, 'i') && strings.Trim(flags, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
			return true
		}
	}
	return false
}

func perlInPlace(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if len(a) < 2 || a[0] != '-' || strings.HasPrefix(a, "--") {
			continue
		}
		flags := a[1:]
		if strings.ContainsRune(flags, 'i') && strings.Trim(flags, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
			return true
		}
		if strings.HasPrefix(a, "-i.") || strings.HasPrefix(a, "-pi.") {
			return true
		}
	}
	return false
}

func jqNullInputReadsNothing(args []string) bool {
	found, reads := false, false
	filter := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		valueCount := 0
		switch a {
		case "--arg", "--argjson":
			valueCount = 2
		case "--slurpfile", "--rawfile", "--argfile":
			valueCount, reads = 2, true
		case "--indent":
			valueCount = 1
		}
		if valueCount > 0 {
			i += valueCount
			continue
		}
		if a == "--null-input" || strings.HasPrefix(a, "--null-input=") {
			found = true
			continue
		}
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.Trim(a[1:], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" && strings.Contains(a, "n") {
			found = true
			continue
		}
		if !strings.HasPrefix(a, "-") {
			filter = a
			break
		}
	}
	if regexp.MustCompile(`\binputs?\b`).MatchString(filter) {
		reads = true
	}
	return found && !reads
}

// directEditOwnsCode — код команды принадлежит прямой правке, только если она единственная
// прямая правка и последний вызов команды.
func directEditOwnsCode(cmd string) bool {
	calls, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // nested scripts can fail after yielding usable outer calls
		slog.Debug("parse command for direct edit ownership", "error", err)
	}
	n := 0
	for _, c := range calls {
		if directEditCall(c) {
			n++
		}
	}
	return n == 1 && len(calls) > 0 && directEditCall(calls[len(calls)-1])
}

// transcriptArgs — аргументы явно называют транскрипт: папку сессий Claude или rollout codex.
// Прочие .jsonl (graft stats.jsonl, tests.jsonl, файлы в scratchpad) старым способом tl не считаются.
// Путь в переменной (f=…/projects/….jsonl; grep … "$f") — транскрипт, если он назван в той же команде.
func (u *run) transcriptArgs(args, cmd string) bool {
	named := func(s string) bool {
		for _, p := range []string{".claude/projects", "/projects/-", "codex/sessions", "rollout-", "/subagents/agent-"} {
			if strings.Contains(s, p) {
				return true
			}
		}
		return false
	}
	if strings.Contains(args, ".jsonl") && named(args) {
		return true
	}
	return strings.Contains(args, "$") && strings.Contains(cmd, ".jsonl") && named(cmd)
}

func (u *run) utilityHabit(cmd string) []string {
	found := map[string]bool{}
	calls, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // nested scripts can fail after yielding usable outer calls
		slog.Debug("parse utility habits", "error", err)
	}
	for _, c := range calls {
		for _, name := range u.utilityHabitsForCall(c, cmd) {
			found[name] = true
		}

	}
	return slices.Sorted(maps.Keys(found))
}

func (u *run) utilityHabitsForCall(c shellcmd.Call, cmd string) []string {
	low := strings.ToLower(c.Raw)
	args := strings.ToLower(strings.Join(c.Args, " "))
	joined := low + " " + args
	var out []string
	if u.isBusyProbe(c) {
		out = append(out, "busy")
	}
	if u.isPriorSearch(c, joined) {
		out = append(out, "prior")
	}
	if strings.Contains(joined, "docs/tasks/") && ((c.Name == "cat" && (strings.Contains(low, ">") || strings.Contains(low, "<<"))) || c.Name == "sed" && strings.Contains(low, "-i")) {
		out = append(out, "tk")
	}
	if u.isTranscriptHabit(c, args, cmd) {
		out = append(out, "tl")
	}
	return out
}

func (u *run) isPriorSearch(c shellcmd.Call, joined string) bool {
	return (c.Name == "grep" || c.Name == "rg") && (u.decisionSearch(joined) || strings.Contains(joined, "/docs/tasks/") || strings.Contains(joined, "docs/tasks/"))
}

func (u *run) commandHasTruncator(cmd string) bool {
	calls, err := shellcmd.Calls(cmd)
	if err != nil {
		slog.Debug("parse command for truncators", "error", err)
		return false
	}
	for _, c := range calls {
		if c.Name == "head" || c.Name == "tail" || c.Name == "cut" {
			return true
		}
	}
	return false
}

func (u *run) singleUtilityCommand(tool, cmd string) bool {
	calls, err := shellcmd.Calls(cmd)
	if err != nil {
		slog.Debug("parse utility command", "error", err)
		return false
	}
	return len(calls) == 1 && calls[0].Name == tool
}

func (u *run) toolFault(tool, out string, code int, cmd string, ordinal int) string {
	ownError, toolFaultLines, usageMarkers := u.toolOutputFlags(tool, out)
	framesAkit := regexp.MustCompile(`(?m)^\s+[^\n]*github\.com/aiseeq/akit/[^\n]*\.go:\d+`).MatchString(out)
	trimmed := strings.TrimSpace(out)
	startsPanic := strings.HasPrefix(trimmed, "panic:") || strings.HasPrefix(trimmed, "fatal error:")
	if ordinal < toolFaultLines || ordinal == 0 && startsPanic && framesAkit {
		return "crash"
	}
	if ordinal < usageMarkers {
		return "call-error"
	}
	if code == 2 && ownError && u.singleUtilityCommand(tool, cmd) {
		return "call-error"
	}
	if code >= 128 && u.singleUtilityCommand(tool, cmd) && tool != "tk" {
		return "killed"
	}
	if code != 0 && ownError && u.singleUtilityCommand(tool, cmd) {
		return "refusal"
	}
	return ""
}

func (u *run) toolOutputFlags(tool, out string) (bool, int, int) {
	ownError, toolFaultLines, usageMarkers := false, 0, 0
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, tool+": ") {
			ownError = true
			low := strings.ToLower(line)
			message := strings.TrimPrefix(low, tool+": ")
			if strings.HasPrefix(message, "panic") || strings.HasPrefix(message, "fatal error") {
				toolFaultLines++
			}
		}
		if strings.Contains(line, "("+tool+" -h for usage)") {
			usageMarkers++
		}
	}
	return ownError, toolFaultLines, usageMarkers
}

func (u *run) callMetric(code int) string {
	if code < 0 {
		return "exit-unknown"
	}
	return fmt.Sprintf("exit-%d", code)
}
func (u *run) callFor(e transcript.Event, isClaude bool) (string, bool) {
	if isClaude {
		if e.Who != "Bash" {
			return "", false
		}
		return e.Text, true
	}
	if e.Who != "exec" && e.Who != "exec_command" && e.Who != "write_stdin" {
		return "", false
	}
	return e.Text, true
}
func (u *run) outcome(tool, out string, code int) string {
	switch tool {
	case "sub":
		if strings.Contains(out, "sub: dry run, would make") {
			return "dry-run"
		}
		if regexp.MustCompile(`(?m)^sub: \d+ replacements? in \d+ files?`).MatchString(out) {
			return "success"
		}
		if regexp.MustCompile(`expected \d+, found [1-9]\d*`).MatchString(out) {
			return "mismatch"
		}
		if regexp.MustCompile(`expected \d+, found 0`).MatchString(out) {
			return "not-found"
		}
		if code == 2 && regexp.MustCompile(`(?m)^sub: (?:need |FILE given|unknown flag|line \d+:|.*(?:invalid|usage|cannot))`).MatchString(out) {
			return "error"
		}
		return "unknown"
	case "tl":
		if strings.Contains(out, "tl: no events") {
			return "no-events"
		}
		if regexp.MustCompile(`tl: \d+ event\(s\)`).MatchString(out) {
			return "events"
		}
		if code == 2 {
			return "error"
		}
		return "unknown"
	case "wfor":
		if strings.Contains(out, "wfor: done in") {
			return "done"
		}
		if strings.Contains(out, "wfor: not yet after") {
			return "not-yet"
		}
		if code == 2 {
			return "error"
		}
		return "unknown"
	}
	return ""
}

func (u *run) eventExitCode(e transcript.Event) int {
	var v any
	if err := json.Unmarshal(e.Result, &v); err != nil {
		slog.Debug("decode result exit status", "error", err)
	} else {
		if n := u.findExitCode(v); n >= 0 {
			return n
		}
	}
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`(?i)(?:Process exited with )?exit code\s*[:=]?\s*(\d+)`), regexp.MustCompile(`(?i)exit status\s*[:=]?\s*(\d+)`)} {
		if m := re.FindStringSubmatch(e.Text); len(m) > 1 {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				slog.Debug("parse exit code", "value", m[1], "error", err)
				continue
			}
			return n
		}
	}
	if b, ok := u.findIsError(e.Raw); ok {
		if !b {
			return 0
		}
		return 1
	}
	if (e.UUID != "" || e.CallID != "") && !e.Error {
		return 0
	}
	return -1
}

func (u *run) findIsError(raw json.RawMessage) (bool, bool) {
	var walk func(any) (bool, bool)
	walk = func(v any) (bool, bool) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if strings.EqualFold(k, "is_error") {
					b, ok := val.(bool)
					return b, ok
				}
				if b, ok := walk(val); ok {
					return b, true
				}
			}
		case []any:
			for _, val := range x {
				if b, ok := walk(val); ok {
					return b, true
				}
			}
		}
		return false, false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false, false
	}
	return walk(v)
}

func (u *run) utilityCode(e transcript.Event, tool, cmd, out string) (int, error) {
	code := u.eventExitCode(e)
	if u.explicitSuccess(tool, out) {
		if code < 0 {
			return 0, nil
		}
		return code, nil
	}
	if u.explicitRefusal(tool, out) {
		if code < 0 {
			return 1, nil
		}
		return code, nil
	}
	cs, err := shellcmd.Calls(cmd)
	if err != nil {
		return 0, fmt.Errorf("parse shell command: %w", err)
	}
	positions := []int{}
	for i, c := range cs {
		if c.Name == tool {
			positions = append(positions, i)
		}
	}
	if len(positions) == 0 {
		return -1, nil
	}
	last := positions[len(positions)-1]
	single := true
	for i, c := range cs {
		if i == positions[0] {
			continue
		}
		if (c.Name == "head" || c.Name == "tail" || c.Name == "cut") && i > positions[0] {
			continue
		}
		single = false
	}
	if single || last == len(cs)-1 {
		return code, nil
	}
	if code == 0 && strings.Contains(cmd, "&&") {
		return 0, nil
	}
	return -1, nil
}

func (u *run) findExitCode(v any) int {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if strings.EqualFold(k, "exitCode") || strings.EqualFold(k, "exit_code") || strings.EqualFold(k, "returnCode") {
				if f, ok := val.(float64); ok {
					return int(f)
				}
			}
			if n := u.findExitCode(val); n >= 0 {
				return n
			}
		}
	case []any:
		for _, val := range x {
			if n := u.findExitCode(val); n >= 0 {
				return n
			}
		}
	}
	return -1
}

func (u *run) process(path string) {
	isClaude, models, evs, outs, ok := u.readProcessTranscript(path)
	if !ok {
		return
	}
	calls, taskWrites := u.scanProcessEvents(path, isClaude, models, evs, outs)
	for i := range taskWrites {
		taskWrites[i].session = path
	}
	u.resolveCallCodes(calls, outs)
	u.collectPairEvents(path, isClaude, models, evs, outs, calls)
	u.recordProcessCalls(path, evs, models, calls)
	u.processWaits(path, evs, models, outs)
	u.recordTaskWrites(taskWrites)
	u.tkActions = append(u.tkActions, taskWrites...)
}

func (u *run) readProcessTranscript(path string) (bool, map[int]string, []transcript.Event, map[string]transcript.Event, bool) {
	isClaude, _ := u.extract(path)
	models := map[int]string(nil)
	if !isClaude {
		models = u.modelMap(path)
	}
	var evs []transcript.Event
	meta, err := transcript.Read(path, nil, func(e transcript.Event) {
		if u.valid(e.Time) {
			evs = append(evs, e)
		}
	})
	u.badLines += meta.Bad
	if meta.Bad > 0 && u.firstBad == "" {
		u.firstBad = path
	}
	if err != nil {
		slog.Error("read transcript", "path", path, "error", err)
		u.unreadFiles++
		if u.firstUnread == "" {
			u.firstUnread = path
		}
		return false, nil, nil, nil, false
	}
	if len(evs) == 0 {
		return false, nil, nil, nil, false
	}
	outs := map[string]transcript.Event{}
	for i := range evs {
		e := &evs[i]
		if e.Role == transcript.Out {
			outs[e.CallID] = *e
		}
	}
	return isClaude, models, evs, outs, true
}

func (u *run) scanProcessEvents(path string, isClaude bool, models map[int]string, evs []transcript.Event, outs map[string]transcript.Event) ([]call, []taskWrite) {
	var calls []call
	var taskWrites []taskWrite
	for ei := range evs {
		e := evs[ei]
		if err := u.recordNativeEvent(path, isClaude, models, e, outs, &taskWrites); err != nil {
			slog.Debug("decode native event", "error", err)
			continue
		}
		cmd, agent, ok := u.collectCommandCalls(path, isClaude, models, e, outs, &calls, &taskWrites)
		if !ok {
			continue
		}
		u.scanShellPatterns(path, isClaude, models, ei, e, cmd, agent, outs, evs, &taskWrites)
	}
	return calls, taskWrites
}

func (u *run) resolveCallCodes(calls []call, outs map[string]transcript.Event) {
	for i := range calls {
		calls[i].code = -1
		o, ok := outs[calls[i].ev.CallID]
		if ok {
			calls[i].output = o.Text
			code, codeErr := u.utilityCode(o, calls[i].tool, calls[i].command, o.Text)
			if codeErr != nil {
				slog.Debug("determine utility exit code", "error", codeErr)
				continue
			}
			calls[i].code = code
			if strings.Contains(o.Text, "expected ") && strings.Contains(o.Text, "found ") || strings.Contains(o.Text, "nothing changed") {
				calls[i].code = 1
			}
		}
	}
}

func (u *run) recordProcessCalls(path string, evs []transcript.Event, models map[int]string, calls []call) {
	for i := range calls {
		u.recordOneProcessCall(path, evs, models, calls, i)
	}
}

func (u *run) recordOneProcessCall(path string, evs []transcript.Event, models map[int]string, calls []call, index int) {
	c := &calls[index]
	d := u.day(c.ev.Time)
	u.inc(d, c.agent, c.tool, "calls", 1)
	if c.tool == "tl" && strings.Contains(c.output, "tl: no events") {
		if u.recordNewEmptyCall(path, evs, models, c) {
			return
		}
	} else if c.tool != "wfor" {
		u.addError(c.tool, "new", c.code, c.output, false)
	}
	u.inc(d, c.agent, c.tool, u.callMetric(c.code), 1)
	oc := u.outcome(c.tool, c.output, c.code)
	if c.tool == "sub" || c.tool == "tl" {
		metric := oc
		if oc == "unknown" && u.commandHasTruncator(c.command) {
			metric = "truncated"
		}
		u.inc(d, c.agent, c.tool, metric, 1)
	}
	u.recordCallFault(path, d, c)
	u.recordUnknownProbe(path, oc, c)
	u.recordSubCase(path, index, calls, oc, c)
}

func (u *run) recordNewEmptyCall(path string, evs []transcript.Event, models map[int]string, c *call) bool {
	u.tlNewEmptyCount++
	tlCalls, err := shellcmd.Find(c.command, "tl")
	if err != nil {
		return true
	}
	if output, ok := u.pureNoEventsOutput(c.output); ok && len(tlCalls) == 1 {
		u.tlNewEmpty = append(u.tlNewEmpty, example{kind: "errpair-tl-new", ref: fmt.Sprintf("%s:%d", path, c.ev.Line), agent: c.agent, command: c.command, output: output})
	}
	at, found := 0, false
	for i := range evs {
		if evs[i].Line == c.ev.Line {
			at, found = i, true
			break
		}
	}
	if !found {
		// An unmatched line uses the established zero sample slot to keep output stable.
		at = 0
	}
	code := 0
	if u.transcriptSearchAgain(evs, at, c.agent, path, models, strings.Contains(path, "/projects/")) {
		code = 1
		u.tlNewRetries++
		u.addRepeat("tl", "new")
	}
	u.updateNewEmptyReason(path, c.ev.Line, code)
	out := c.output
	if code == 0 {
		out = ""
	}
	u.addError(c.tool, "new", code, out, false)
	return false
}

func (u *run) updateNewEmptyReason(path string, line, code int) {
	ref := fmt.Sprintf("%s:%d", path, line)
	for i := range u.tlNewEmpty {
		if u.tlNewEmpty[i].ref != ref {
			continue
		}
		if code == 1 {
			u.tlNewEmpty[i].reason = "no events; затем повторный поиск в следующих трёх вызовах"
		} else {
			u.tlNewEmpty[i].reason = "no events; повтора в следующих трёх вызовах нет"
		}
		return
	}
}

func (u *run) recordCallFault(path, dayName string, c *call) {
	fault := u.toolFault(c.tool, c.output, c.code, c.command, c.ordinal)
	if fault == "" {
		return
	}
	u.inc(dayName, c.agent, c.tool, fault, 1)
	kind := fault + "-" + c.tool
	if fault == "call-error" {
		kind = "callerr-" + c.tool
	}
	ex := example{kind: kind, ref: fmt.Sprintf("%s:%d", path, c.ev.Line), agent: c.agent, command: c.command, output: c.output, reason: fault}
	if fault == "refusal" {
		u.refusalSamples[c.tool] = append(u.refusalSamples[c.tool], ex)
	} else {
		u.failureSamples = append(u.failureSamples, ex)
	}
}

func (u *run) recordUnknownProbe(path, outcome string, c *call) {
	if (c.tool != "sub" && c.tool != "tl") || outcome != "unknown" || len(u.unknownProbes) >= 200 {
		return
	}
	reason := "код выхода отсутствует в записи"
	if c.code >= 0 {
		reason = fmt.Sprintf("код выхода %d; итог утилиты не распознан", c.code)
	}
	if u.commandHasTruncator(c.command) {
		reason = "команда обрезает поток через head/tail/cut"
	}
	u.unknownProbes = append(u.unknownProbes, example{kind: "unknown-" + c.tool, ref: fmt.Sprintf("%s:%d", path, c.ev.Line), agent: c.agent, command: c.command, output: c.output, reason: reason})
}

func (u *run) recordSubCase(path string, index int, calls []call, outcome string, c *call) {
	if c.tool != "sub" || outcome != "mismatch" && outcome != "error" {
		return
	}
	next := ""
	if index+1 < len(calls) && calls[index+1].agent == c.agent && calls[index+1].tool == "sub" && u.sameFiles(c.command, calls[index+1].command) {
		next = calls[index+1].command
	}
	u.subCases = append(u.subCases, example{kind: "sub-" + outcome, ref: fmt.Sprintf("%s:%d", path, c.ev.Line), agent: c.agent, command: c.command, output: c.output, next: next, reason: u.ruleFlags(c.command)})
}

func (u *run) collectCommandCalls(path string, isClaude bool, models map[int]string, e transcript.Event, outs map[string]transcript.Event, calls *[]call, taskWrites *[]taskWrite) (string, string, bool) {
	cmd, ok := u.callFor(e, isClaude)
	if !ok || e.Role != transcript.Cmd {
		return "", "", false
	}
	agent := u.agentOf(path, strings.Contains(path, "/subagents/"), u.modelAt(models, e.Line))
	found, err := shellcmd.Find(cmd, u.utilityNames...)
	if err != nil {
		slog.Debug("parse command for utility calls", "error", err)
		return cmd, agent, true
	}
	toolOrdinal := map[string]int{}
	for _, f := range found {
		if f.Offset < 0 {
			continue
		}
		c := call{ev: e, command: cmd, agent: agent, tool: f.Name, ordinal: toolOrdinal[f.Name]}
		toolOrdinal[f.Name]++
		if o, ok := outs[e.CallID]; ok {
			c.output = o.Text
			code, codeErr := u.utilityCode(o, f.Name, cmd, o.Text)
			if codeErr != nil {
				slog.Debug("determine utility exit code", "error", codeErr)
				continue
			}
			c.code = code
			if strings.Contains(o.Text, "expected ") && strings.Contains(o.Text, "found ") {
				c.code = 1
			}
		}
		*calls = append(*calls, c)
		if f.Name == "tk" {
			u.recordTKCommandWrites(cmd, agent, e, outs[e.CallID], taskWrites)
		}
	}
	return cmd, agent, true
}

func (u *run) recordTKCommandWrites(cmd, agent string, e transcript.Event, outEv transcript.Event, taskWrites *[]taskWrite) {
	parsed, err := shellcmd.Find(cmd, "tk")
	if err != nil {
		slog.Debug("parse command for task writes", "error", err)
		return
	}
	for _, tc := range parsed {
		if len(tc.Args) < 2 || !u.validTaskOperation(tc.Args[0]) {
			continue
		}
		failed := outEv.Error || u.eventExitCode(outEv) > 0
		*taskWrites = append(*taskWrites, taskWrite{agent: agent, key: tc.Args[1], source: "tk", op: tc.Args[0], at: e.Time, line: e.Line, failed: failed})
	}
}

func (u *run) validTaskOperation(op string) bool {
	return slices.Contains([]string{"new", "note", "plan", "skip", "start", "review", "done", "reject"}, op)
}

func (u *run) scanShellPatterns(path string, isClaude bool, models map[int]string, ei int, e transcript.Event, cmd, agent string, outs map[string]transcript.Event, evs []transcript.Event, taskWrites *[]taskWrite) {
	h := u.habitType(cmd)
	if h != "" {
		u.inc(u.day(e.Time), agent, h, "calls", 1)
		u.habitCandidates[h] = append(u.habitCandidates[h], example{kind: h, ref: fmt.Sprintf("%s:%d", path, e.Line), agent: agent, command: cmd, output: u.outFor(e, outs), day: u.day(e.Time)})
		if h == "habit-edit" || h == "habit-anchor" {
			u.inc(u.day(e.Time), agent, "habit-sub", "calls", 1)
		}
	}
	outEv := outs[e.CallID]
	u.recordShellWays(u.shellWays(cmd), outEv)
	if h == "habit-edit" {
		if directEditOwnsCode(cmd) {
			u.addError("sub", "direct replacements", u.eventExitCode(outEv), outEv.Text, outEv.Error)
		} else {
			u.addError("sub", "direct replacements", -1, "", false)
		}
		if u.repeatedEdit(evs, ei, agent, path, models, isClaude, u.replacementFiles(cmd)) {
			u.addRepeat("sub", "direct replacements")
		}
	}
	if h == "habit-transcript" {
		u.recordOldTranscriptSearch(path, isClaude, models, ei, e, cmd, agent, outEv, evs)
	}
	u.recordManualTaskSearch(e, cmd, agent, outEv, taskWrites)
	if err := u.recordShellEdits(path, isClaude, models, ei, cmd, agent, outEv, evs); err != nil {
		slog.Debug("record shell edits", "error", err)
	}
	for _, habit := range u.utilityHabit(cmd) {
		u.inc(u.day(e.Time), agent, "habit-"+habit, "calls", 1)
	}
}

func (u *run) recordShellWays(ways map[string]bool, outEv transcript.Event) {
	for way := range ways {
		tool := u.shellWayTool(way)
		if tool != "" && tool != "sub" && tool != "tl" {
			u.addError(tool, way, u.eventExitCode(outEv), outEv.Text, outEv.Error)
		}
	}
}

func (u *run) shellWayTool(way string) string {
	switch way {
	case "sed -i", "perl -pi", ".replace":
		return "sub"
	case "grep transcript", "rg transcript", "jq transcript", "sed transcript":
		return "tl"
	case "busy probe":
		return "busy"
	case "grep/rg decisions/tasks":
		return "prior"
	default:
		return ""
	}
}

func (u *run) recordOldTranscriptSearch(path string, isClaude bool, models map[int]string, ei int, e transcript.Event, cmd, agent string, outEv transcript.Event, evs []transcript.Event) {
	u.tlOldCalls++
	s := u.errorStats["tl\ttranscript search"]
	if s == nil {
		s = &errorStat{}
		u.errorStats["tl\ttranscript search"] = s
	}
	s.calls++
	if u.eventExitCode(outEv) < 0 {
		s.unknown++
	}
	empty := u.emptySearchOutput(outEv, outEv.Text)
	retry := empty && u.transcriptSearchAgain(evs, ei, agent, path, models, isClaude)
	if empty {
		reason := "пустой результат; повтора в следующих трёх вызовах нет"
		if retry {
			reason = "пустой результат; тот же агент снова искал в следующих трёх вызовах"
		}
		u.tlOldEmpty = append(u.tlOldEmpty, example{kind: "errpair-tl-old", ref: fmt.Sprintf("%s:%d", path, e.Line), agent: agent, command: cmd, reason: reason})
	}
	if retry {
		s.fail++
		u.tlOldRetries++
		u.addRepeat("tl", "transcript search")
	}
	if len(u.tlOldChecks) < 10 {
		u.tlOldChecks = append(u.tlOldChecks, u.oldSearchCheck(path, e, agent, cmd, empty, retry))
	}
}

func (u *run) oldSearchCheck(path string, e transcript.Event, agent, cmd string, empty, retry bool) example {
	reason := "проверка старого поиска: повтор в следующих трёх вызовах не обнаружен"
	if retry {
		reason = "пустой результат и повторный поиск в следующих трёх вызовах"
	} else if empty {
		reason = "пустой результат, но повторного поиска в следующих трёх вызовах нет"
	}
	return example{kind: "errpair-tl-old", ref: fmt.Sprintf("%s:%d", path, e.Line), agent: agent, command: cmd, reason: reason}
}

func (u *run) recordManualTaskSearch(e transcript.Event, cmd, agent string, outEv transcript.Event, taskWrites *[]taskWrite) {
	if !strings.Contains(strings.ToLower(cmd), "docs/tasks/") || !regexp.MustCompile(`(?i)\b(cat|tee)\b[^\n]*(?:>|<<)|\bsed\s+-i`).MatchString(cmd) && !strings.Contains(cmd, "apply_patch") {
		return
	}
	for _, file := range u.unique(ticketPathRE.FindAllString(cmd, -1)) {
		*taskWrites = append(*taskWrites, taskWrite{agent: agent, file: file, key: filepath.Base(file), source: "manual", at: e.Time, line: e.Line, failed: outEv.Error || u.eventExitCode(outEv) > 0})
	}
	u.addError("tk", "manual docs/tasks", u.eventExitCode(outEv), outEv.Text, outEv.Error)
}

func (u *run) recordShellEdits(path string, isClaude bool, models map[int]string, ei int, cmd, agent string, outEv transcript.Event, evs []transcript.Event) error {
	cfiles, err := u.filesForSub(cmd)
	if err != nil {
		err = fmt.Errorf("find sub files: %w", err)
	}
	if len(cfiles) > 0 {
		if subErr := u.recordSubEdits(path, isClaude, models, ei, cmd, agent, outEv, evs, cfiles); subErr != nil {
			err = errors.Join(err, subErr)
		}
	}
	u.recordDirectEdits(path, isClaude, models, ei, cmd, agent, outEv, evs)
	return err
}

func (u *run) recordSubEdits(path string, isClaude bool, models map[int]string, ei int, cmd, agent string, outEv transcript.Event, evs []transcript.Event, files []string) error {
	calls, err := shellcmd.Find(cmd, "sub")
	if len(calls) == 0 {
		if err != nil {
			return fmt.Errorf("find sub calls: %w", err)
		}
		return nil
	}
	repeated := false
	for _, file := range files {
		repeated = repeated || u.editAgain(evs, ei, agent, path, models, isClaude, file)
	}
	if repeated {
		u.addRepeat("sub", "new")
	}
	success := (regexp.MustCompile(`(?m)^sub: \d+ replacements? in \d+ files?`).MatchString(outEv.Text) || strings.Contains(outEv.Text, "sub: dry run")) && !u.editAgain(evs, ei, agent, path, models, isClaude, files[0])
	for _, file := range files {
		u.recordEdit(editAttempt{tool: "sub", agent: agent, file: file, bytes: len(cmd), success: success})
	}
	if err != nil {
		return fmt.Errorf("find sub calls: %w", err)
	}
	return nil
}

func (u *run) recordDirectEdits(path string, isClaude bool, models map[int]string, ei int, cmd, agent string, outEv transcript.Event, evs []transcript.Event) {
	if !u.hasDirectReplace(cmd) {
		return
	}
	repeated := false
	for _, file := range u.replacementFiles(cmd) {
		repeated = repeated || u.editAgain(evs, ei, agent, path, models, isClaude, file)
		u.recordEdit(editAttempt{tool: "direct", agent: agent, file: file, bytes: len(cmd), success: u.eventExitCode(outEv) == 0 && !u.editAgain(evs, ei, agent, path, models, isClaude, file)})
	}
	if repeated {
		for way := range u.shellWays(cmd) {
			if way == "sed -i" || way == "perl -pi" || way == ".replace" {
				u.addRepeat("sub", way)
			}
		}
	}
}

func (u *run) recordNativeEvent(path string, isClaude bool, models map[int]string, e transcript.Event, outs map[string]transcript.Event, taskWrites *[]taskWrite) error {
	if isClaude && e.Role == transcript.Cmd && (e.Who == "Edit" || e.Who == "MultiEdit" || e.Who == "Write") {
		return u.recordClaudeEdit(path, models, e, outs[e.CallID], taskWrites)
	}
	if !isClaude && e.Role == transcript.Cmd && strings.Contains(e.Text, "apply_patch") {
		u.recordApplyPatch(path, models, e, outs[e.CallID], taskWrites)
	}
	if isClaude && e.Role == transcript.Cmd && e.Who == "WebFetch" {
		u.recordWebFetch(path, models, e, outs)
	}
	return nil
}

func (u *run) recordClaudeEdit(path string, models map[int]string, e, outEv transcript.Event, taskWrites *[]taskWrite) error {
	agent := u.agentOf(path, strings.Contains(path, "/subagents/"), u.modelAt(models, e.Line))
	u.inc(u.day(e.Time), agent, "edit-tool", "calls", 1)
	u.editSamples = append(u.editSamples, example{kind: "edit-tool", ref: fmt.Sprintf("%s:%d", path, e.Line), agent: agent, command: e.Text, output: outEv.Text})
	if e.Who == "Edit" {
		file, old, newText, err := u.editRaw(e)
		if err != nil {
			return err
		}
		failed := outEv.Error || strings.Contains(strings.ToLower(outEv.Text), "string to replace not found")
		editCode := 0
		if failed {
			editCode = 1
		}
		u.addError("sub", "Edit", editCode, outEv.Text, failed)
		u.recordEdit(editAttempt{tool: "Edit", agent: agent, file: file, bytes: len(old) + len(newText), success: !failed})
		if strings.Contains(file, "docs/tasks/") {
			*taskWrites = append(*taskWrites, taskWrite{agent: agent, file: file, key: filepath.Base(file), source: "manual", at: e.Time, line: e.Line, failed: failed})
		}
	}
	if (e.Who == "Edit" || e.Who == "Write") && strings.Contains(strings.ToLower(e.Text), "docs/tasks/") {
		u.addError("tk", "manual docs/tasks", u.eventExitCode(outEv), outEv.Text, outEv.Error)
		for _, file := range ticketPathRE.FindAllString(e.Text, -1) {
			*taskWrites = append(*taskWrites, taskWrite{agent: agent, file: file, key: filepath.Base(file), source: "manual", at: e.Time, line: e.Line, failed: outEv.Error || u.eventExitCode(outEv) > 0})
		}
	}
	if strings.Contains(strings.ToLower(e.Text), "docs/tasks/") {
		u.inc(u.day(e.Time), agent, "habit-tk", "calls", 1)
	}
	return nil
}

func (u *run) recordApplyPatch(path string, models map[int]string, e, outEv transcript.Event, taskWrites *[]taskWrite) {
	agent := u.agentOf(path, strings.Contains(path, "/subagents/"), u.modelAt(models, e.Line))
	u.inc(u.day(e.Time), agent, "apply-patch", "calls", 1)
	u.editSamples = append(u.editSamples, example{kind: "apply-patch", ref: fmt.Sprintf("%s:%d", path, e.Line), agent: agent, command: e.Text, output: outEv.Text})
	patchCode := u.eventExitCode(outEv)
	failed := outEv.Error || patchCode >= 0 && patchCode != 0 || strings.Contains(strings.ToLower(outEv.Text), "failed") || strings.Contains(strings.ToLower(outEv.Text), "does not exist")
	if patchCode < 0 {
		patchCode = 0
	}
	if failed && patchCode == 0 {
		patchCode = 1
	}
	u.addError("sub", "apply_patch", patchCode, outEv.Text, failed)
	for _, file := range u.unique(ticketPathRE.FindAllString(e.Text, -1)) {
		u.inc(u.day(e.Time), agent, "habit-tk", "calls", 1)
		*taskWrites = append(*taskWrites, taskWrite{agent: agent, file: file, key: filepath.Base(file), source: "manual", at: e.Time, line: e.Line, failed: failed})
		u.addError("tk", "manual docs/tasks", patchCode, outEv.Text, failed)
	}
}

func (u *run) recordWebFetch(path string, models map[int]string, e transcript.Event, outs map[string]transcript.Event) {
	agent := u.agentOf(path, strings.Contains(path, "/subagents/"), u.modelAt(models, e.Line))
	u.inc(u.day(e.Time), agent, "habit-webtext", "calls", 1)
	u.habitCandidates["habit-webtext"] = append(u.habitCandidates["habit-webtext"], example{kind: "habit-webtext", ref: fmt.Sprintf("%s:%d", path, e.Line), agent: agent, command: e.Text, output: u.outFor(e, outs), day: u.day(e.Time)})
	outEv := outs[e.CallID]
	u.addError("webtext", "WebFetch", u.eventExitCode(outEv), outEv.Text, outEv.Error || regexp.MustCompile(`(?i)403|429|bot.?check|refused|blocked`).MatchString(outEv.Text))
}

func (u *run) recordTaskWrites(taskWrites []taskWrite) {
	seenWrites := map[string]bool{}
	var uniqueWrites []taskWrite
	for i := range taskWrites {
		w := &taskWrites[i]
		k := fmt.Sprintf("%s|%d|%s|%s", w.agent, w.line, w.source, w.key)
		if !seenWrites[k] {
			seenWrites[k] = true
			uniqueWrites = append(uniqueWrites, *w)
		}
	}
	// Every successful write is in the denominator; the repeat is a later manual
	// write to the same ticket by the same agent within 10 minutes.
	for i := range uniqueWrites {
		w := &uniqueWrites[i]
		if w.failed {
			continue
		}
		if w.source == "manual" {
			create := u.isFirstManualWrite(w, uniqueWrites)
			repeat := u.manualWriteAfter(w, uniqueWrites, u.sameTaskFile)
			if repeat {
				u.addRepeat("tk", "manual docs/tasks")
			}
			if create {
				u.tkPairAdd("manual create", repeat)
			} else {
				u.tkPairAdd("manual other", repeat)
			}
		}
		if w.source == "tk" {
			repeat := u.manualWriteAfter(w, uniqueWrites, u.sameTaskKey)
			if w.op == "new" {
				u.tkPairAdd("tk new", repeat)
			} else {
				u.tkPairAdd("tk other", repeat)
			}
		}
	}
}

func (u *run) isFirstManualWrite(w *taskWrite, writes []taskWrite) bool {
	for i := range writes {
		n := &writes[i]
		if n.at.Before(w.at) && n.agent == w.agent && (n.source == "manual" && n.file == w.file || n.source == "tk" && strings.Contains(filepath.Base(w.file), n.key)) {
			return false
		}
	}
	return true
}

func (u *run) manualWriteAfter(w *taskWrite, writes []taskWrite, matches func(*taskWrite, *taskWrite) bool) bool {
	for i := range writes {
		n := &writes[i]
		if n.source == "manual" && n.line != w.line && n.agent == w.agent && matches(w, n) && n.at.Sub(w.at) >= 0 && n.at.Sub(w.at) <= 10*time.Minute {
			return true
		}
	}
	return false
}

func (u *run) sameTaskFile(w, n *taskWrite) bool { return n.file == w.file }
func (u *run) sameTaskKey(w, n *taskWrite) bool {
	return strings.Contains(filepath.Base(n.file), w.key)
}

func (u *run) processWaits(path string, evs []transcript.Event, models map[int]string, outs map[string]transcript.Event) {
	wr := waits.New()
	for i := range evs {
		e := &evs[i]
		added := wr.Add(*e)
		for wi := range added {
			u.recordWait(path, &added[wi], models, outs)
		}
	}
	closed := wr.Close()
	for i := range closed {
		w := &closed[i]
		u.recordHabitWait(path, w, models, outs)
	}
}

func (u *run) recordWait(path string, w *waits.Wait, models map[int]string, outs map[string]transcript.Event) {
	u.recordHabitWait(path, w, models, outs)
	if w.How != waits.Wfor {
		return
	}
	ag := u.agentOf(path, strings.Contains(path, "/subagents/"), u.modelAt(models, w.Line))
	metric := map[string]string{waits.Done: "done", waits.Timeout: "not-yet", waits.None: "unknown"}[w.Outcome]
	u.inc(u.day(w.Start), ag, "wfor", metric, w.Calls)
	code := 0
	if w.Outcome == waits.Timeout {
		code = 1
	}
	for n := 0; n < w.Calls; n++ {
		u.addError("wfor", "new", code, u.waitOutput(*w, outs), false)
	}
	if w.Repeats > 0 {
		u.inc(u.day(w.Start), ag, "wfor", "repeat", w.Repeats)
	}
}

func (u *run) recordHabitWait(path string, w *waits.Wait, models map[int]string, outs map[string]transcript.Event) {
	if w.How != waits.Loop && w.How != waits.StdinPoll {
		return
	}
	ag := u.agentOf(path, strings.Contains(path, "/subagents/"), u.modelAt(models, w.Line))
	u.inc(u.day(w.Start), ag, "habit-wait", "calls", 1)
	u.inc(u.day(w.Start), ag, "habit-wfor", "calls", 1)
	u.waitSamples = append(u.waitSamples, example{kind: "habit-wait", ref: fmt.Sprintf("%s:%d", path, w.Line), agent: ag, command: w.Command, output: w.Outcome, day: u.day(w.Start)})
	outText := u.waitOutput(*w, outs)
	failed := strings.Contains(strings.ToLower(outText), "timed out") || strings.Contains(outText, "Command timed out") || strings.Contains(outText, "exit code 124") || strings.Contains(outText, "exit_code\":124")
	code := 0
	if failed {
		code = 1
	}
	u.addError("wfor", "habit-wait", code, outText, false)
}
func (u *run) outFor(e transcript.Event, m map[string]transcript.Event) string {
	if o, ok := m[e.CallID]; ok {
		return o.Text
	}
	return ""
}

func (u *run) emptySearchOutput(e transcript.Event, out string) bool {
	if strings.TrimSpace(out) == "(Bash completed with no output)" {
		return true
	}
	cleaned := regexp.MustCompile(`(?im)^\s*(?:command )?(?:exited with )?exit code\s*[:=]?\s*\d+\s*$`).ReplaceAllString(out, "")
	cleaned = regexp.MustCompile(`(?im)^\s*process exited with code\s+\d+\s*$`).ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned) == "" && (strings.TrimSpace(out) == "" || u.eventExitCode(e) == 1 || e.Error)
}
func (u *run) pureNoEventsOutput(out string) (string, bool) {
	var rows []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Exit code ") {
			continue
		}
		if !strings.HasPrefix(line, "tl: no events") {
			return "", false
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n"), len(rows) > 0
}
func (u *run) sameFiles(a, b string) bool {
	ra := regexp.MustCompile(`[^ ]+\.(?:go|md|ts|js|json|yaml|yml|sh|txt)`).FindAllString(a, -1)
	rb := regexp.MustCompile(`[^ ]+\.(?:go|md|ts|js|json|yaml|yml|sh|txt)`).FindAllString(b, -1)
	for _, x := range ra {
		if slices.Contains(rb, x) {
			return true
		}
	}
	return false
}
func (u *run) ruleFlags(cmd string) string {
	r := []string{}
	if regexp.MustCompile(`(^|\s)-n\s+2(?:\s|$)`).MatchString(cmd) {
		r = append(r, "-n 2 похож на пример правил")
	}
	if strings.Contains(cmd, `\n`) {
		r = append(r, "буквальный \\n")
	}
	return strings.Join(r, "; ")
}

type uptakeInputError struct{ message string }

func (e *uptakeInputError) Error() string { return e.message }

const usage = `uptake — weekly uptake of akit tools from agent transcripts.

Usage:
  uptake [--since T] [--until T] [--out DIR] [--seed N] [--releases FILE] [--labels FILE]

  --since T         window start (default: 7 days before now): 12:05, 25.10, "25.10 12:05", RFC3339,
                    or an age such as 3d (three days ago)
  --until T         window end (default: now)
  --out DIR         output directory (default: a new temporary directory; its report path is printed)
  --seed N          seed of the random samples (default: the window start date, YYYYMMDD)
  --releases FILE   TSV tool<TAB>available<TAB>instructed (RFC3339, either may be empty): splits
                    pairs into periods before and after release and instruction
  --labels FILE     TSV id<TAB>label (yes|no) from a blind labeling of candidates.tsv: a pair share
                    is reported only when precision and recall pass their lower bounds
                    (both files are checked before the transcripts are read: a missing, unreadable
                    or malformed file is a usage error)
  -h, --help        this help

Writes report.md and TSV tables (counts, examples, errors, pairs, outcomes, candidates, ...) to DIR.
Exit: 0 written, 1 read/write error, 2 usage error.
`

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		fmt.Print(usage)
		return
	}
	args, err := parseUptakeArgs(os.Args[1:], time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "uptake:", err, "(uptake -h for usage)")
		os.Exit(2)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "uptake: home directory:", err)
		os.Exit(1)
	}
	u := newRun(args.since, args.until, args.seed, args.out, home)
	u.releasesPath, u.labelsPath = args.releases, args.labels
	if err := u.runUptake(args.since, args.until); err != nil {
		code, message := uptakeErrorResult(err)
		fmt.Fprintln(os.Stderr, message)
		os.Exit(code)
	}
}

func uptakeErrorResult(err error) (int, string) {
	var inputErr *uptakeInputError
	if errors.As(err, &inputErr) {
		return 2, fmt.Sprintf("uptake: %v (uptake -h for usage)", inputErr)
	}
	return 1, fmt.Sprintf("uptake: %v", err)
}

// uptakeArgs — разобранные аргументы командной строки.
type uptakeArgs struct {
	since, until          time.Time
	seed                  int
	out, releases, labels string
}

func parseUptakeArgs(args []string, now time.Time) (uptakeArgs, error) {
	a, err := parseUptakeFlags(args, now)
	if err != nil {
		return uptakeArgs{}, err
	}
	if !a.since.Before(a.until) {
		return uptakeArgs{}, fmt.Errorf("--since must be before --until")
	}
	return a, nil
}

func uptakeFlagNames() []string {
	return []string{"--since", "--until", "--seed", "--out", "--releases", "--labels"}
}

func parseUptakeFlags(args []string, now time.Time) (uptakeArgs, error) {
	a := uptakeArgs{since: now.Add(-7 * 24 * time.Hour), until: now}
	seedSet := false
	for i := 0; i < len(args); i++ {
		name, value, has := strings.Cut(args[i], "=")
		if !slices.Contains(uptakeFlagNames(), name) {
			return uptakeArgs{}, fmt.Errorf("unknown argument %q", args[i])
		}
		if !has {
			i++
			if i >= len(args) {
				return uptakeArgs{}, fmt.Errorf("%s needs a value", name)
			}
			value = args[i]
		}
		if name == "--seed" {
			seedSet = true
		}
		if err := a.setFlag(name, value, now); err != nil {
			return uptakeArgs{}, err
		}
	}
	if !seedSet {
		y, m, d := a.since.UTC().Date()
		a.seed = y*10000 + int(m)*100 + d
	}
	return a, nil
}

func (a *uptakeArgs) setFlag(name, value string, now time.Time) error {
	switch name {
	case "--since", "--until":
		t, err := timearg.Parse(value, now)
		if err != nil {
			return fmt.Errorf("invalid %s: %w", name, err)
		}
		if name == "--since" {
			a.since = t
		} else {
			a.until = t
		}
	case "--seed":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("--seed must be an integer: %w", err)
		}
		a.seed = n
	case "--out":
		a.out = value
	case "--releases", "--labels":
		if value == "" {
			return fmt.Errorf("%s needs a non-empty value", name)
		}
		if name == "--releases" {
			a.releases = value
		} else {
			a.labels = value
		}
	}
	return nil
}

func (u *run) runUptake(since, until time.Time) error {
	if err := u.loadInputs(); err != nil {
		return err
	}
	out := u.outputDir
	if out == "" {
		var err error
		out, err = os.MkdirTemp(os.TempDir(), "akit-uptake-")
		if err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}
	u.outputDir = out
	if err := os.MkdirAll(u.outputDir, dirMode); err != nil {
		return fmt.Errorf("create output directory %s: %w", u.outputDir, err)
	}
	sources, err := transcript.AllSourcesWalkOrder(u.home, since, "", true, true)
	if err != nil {
		return fmt.Errorf("list transcripts: %w", err)
	}
	for _, src := range sources {
		u.process(src.Path)
	}
	u.readWebtextHookLogs(since, until)
	if err := u.writeTables(); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Printf("Окно: %s — %s (%s)\nТранскриптов: %d\nОтчёт: %s\n", since.In(time.Local).Format("2006-01-02 15:04 MST"), until.In(time.Local).Format("2006-01-02 15:04 MST"), time.Local, len(sources), filepath.Join(u.outputDir, "report.md"))
	if u.unreadFiles > 0 || u.badLines > 0 || len(u.hookReadErrors) > 0 || u.hookBadLines > 0 {
		unread := u.unreadFiles + len(u.hookReadErrors)
		bad := u.badLines + u.hookBadLines
		fmt.Printf("Не прочитано файлов: %d; повреждённых строк: %d\n", unread, bad)
		return fmt.Errorf("incomplete input: %d unread files, %d damaged lines", unread, bad)
	}
	return nil
}

func (u *run) writeTLEmpty() error {
	var b strings.Builder
	b.WriteString("side\tcalls\tempty\tempty_repeat\n")
	fmt.Fprintf(&b, "old\t%d\t%d\t%d\n", u.tlOldCalls, len(u.tlOldEmpty), u.tlOldRetries)
	newCalls := 0
	if stats := u.errorStats["tl\tnew"]; stats != nil {
		newCalls = stats.calls
	}
	fmt.Fprintf(&b, "new\t%d\t%d\t%d\n", newCalls, u.tlNewEmptyCount, u.tlNewRetries)
	return u.save("tl-empty.tsv", b.String())
}
func (u *run) writeCounts() error {
	cohorts := []string{"claude-main", "claude-sub", "codex-luna", "codex-terra", "codex-sol"}
	metrics := map[string]int{}
	for key, n := range u.counts {
		p := strings.Split(key, "\t")
		if len(p) == 4 && p[3] == "calls" {
			metrics[p[1]+"\t"+p[2]] += n
		}
	}
	all := append([]string(nil), u.utilityNames...)
	for _, name := range u.utilityNames {
		all = append(all, "habit-"+name)
	}
	for _, agent := range cohorts {
		for _, name := range all {
			if metrics[agent+"\t"+name] == 0 {
				from, err := time.Parse(time.RFC3339, u.start)
				if err != nil {
					return fmt.Errorf("parse window start: %w", err)
				}
				u.counts[from.In(time.Local).Format("2006-01-02")+"\t"+agent+"\t"+name+"\tcalls"] = 0
			}
		}
	}
	keys := make([]string, 0, len(u.counts))
	for k := range u.counts {
		keys = append(keys, k)
	}
	// Group by utility, then day, agent, and metric for the weekly audit.
	sort.Slice(keys, func(i, j int) bool {
		a, b := strings.Split(keys[i], "\t"), strings.Split(keys[j], "\t")
		for _, field := range []int{2, 0, 1, 3} { // tool, day, agent, metric
			if a[field] != b[field] {
				return a[field] < b[field]
			}
		}
		return false
	})
	b := strings.Builder{}
	b.WriteString("day\tagent\ttool\tmetric\tn\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s\t%d\n", k, u.counts[k])
	}
	return u.save("counts.tsv", b.String())
}
func (u *run) writeExamples() (result error) {
	rng := rand.New(rand.NewSource(int64(u.seed)))
	for _, k := range slices.Sorted(maps.Keys(u.habitCandidates)) {
		arr := u.habitCandidates[k]
		rng.Shuffle(len(arr), func(i, j int) { arr[i], arr[j] = arr[j], arr[i] })
		limit := 20
		for i := 0; i < len(arr) && i < limit; i++ {
			arr[i].reason = u.sampleReason(arr[i])
			u.examples = append(u.examples, arr[i])
		}
	}
	rng.Shuffle(len(u.waitSamples), func(i, j int) { u.waitSamples[i], u.waitSamples[j] = u.waitSamples[j], u.waitSamples[i] })
	for i := 0; i < len(u.waitSamples) && i < 10; i++ {
		u.waitSamples[i].reason = u.sampleReason(u.waitSamples[i])
		u.examples = append(u.examples, u.waitSamples[i])
	}
	for _, c := range u.subCases {
		u.examples = append(u.examples, c)
	}
	u.examples = append(u.examples, u.failureSamples...)
	for _, tool := range u.utilityNames {
		samples := u.refusalSamples[tool]
		rng.Shuffle(len(samples), func(i, j int) { samples[i], samples[j] = samples[j], samples[i] })
		for i := 0; i < len(samples) && i < 3; i++ {
			u.examples = append(u.examples, samples[i])
		}
	}
	u.examples = append(u.examples, u.sampleTL(u.tlOldEmpty)...)
	u.examples = append(u.examples, u.sampleTL(u.tlNewEmpty)...)
	u.appendUnknownExamples(rng)
	excludedPrefix := "tool" + "use-"
	u.examples = slices.DeleteFunc(u.examples, func(e example) bool { return strings.HasPrefix(e.kind, excludedPrefix) })

	f, err := os.Create(filepath.Join(u.outputDir, "examples.tsv"))
	if err != nil {
		return fmt.Errorf("create examples.tsv: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close examples.tsv: %w", err))
		}
	}()
	w := bufio.NewWriter(f)
	// The writer implementation follows below.
	return u.writeExampleRows(f, w)
}

func (u *run) appendUnknownExamples(rng *rand.Rand) {
	for _, tool := range []string{"sub", "tl"} {
		var candidates []example
		for _, e := range u.unknownProbes {
			if e.kind == "unknown-"+tool {
				candidates = append(candidates, e)
			}
		}
		rng.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
		for i := 0; i < len(candidates) && i < 5; i++ {
			candidates[i].kind = "unknown-probe-" + tool
			u.examples = append(u.examples, candidates[i])
		}
	}
}

func (u *run) writeExampleRows(f *os.File, w *bufio.Writer) error {
	fmt.Fprintln(w, "kind\tfile:line\tagent\tcommand\toutput\tnext\treason")
	for _, e := range u.examples {
		cmd := maskSecrets(e.command)
		if len([]rune(cmd)) > 300 {
			cmd = string([]rune(cmd)[:300])
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", u.clean(e.kind), u.clean(e.ref), u.clean(e.agent), u.clean(cmd), u.clean(e.output), u.clean(e.next), u.clean(e.reason))
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush examples.tsv: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync examples.tsv: %w", err)
	}
	return nil
}

func (u *run) writeTables() error {
	if err := u.writeWebtextHook(); err != nil {
		return err
	}
	if err := u.writeCounts(); err != nil {
		return err
	}
	if err := u.writeExamples(); err != nil {
		return err
	}
	if err := u.writeErrors(); err != nil {
		return err
	}
	if err := u.writeTkPair(); err != nil {
		return err
	}
	if err := u.writeTLEmpty(); err != nil {
		return err
	}
	if err := u.writeSubCost(); err != nil {
		return err
	}
	if err := u.writePairTables(); err != nil {
		return err
	}
	return u.writeReport()
}

func (u *run) sampleTL(in []example) []example {
	rng := rand.New(rand.NewSource(int64(u.seed)))
	var retry, noretry []example
	for _, e := range in {
		if strings.Contains(e.reason, "затем повторный") || strings.Contains(e.reason, "снова искал") {
			retry = append(retry, e)
		} else {
			noretry = append(noretry, e)
		}
	}
	rng.Shuffle(len(retry), func(i, j int) { retry[i], retry[j] = retry[j], retry[i] })
	rng.Shuffle(len(noretry), func(i, j int) { noretry[i], noretry[j] = noretry[j], noretry[i] })
	out := make([]example, 0, 10)
	for i := 0; i < len(retry) && i < 5; i++ {
		out = append(out, retry[i])
	}
	for i := 0; i < len(noretry) && i < 5; i++ {
		out = append(out, noretry[i])
	}
	if len(out) < 10 {
		for _, pool := range [][]example{retry, noretry} {
			for _, e := range pool {
				if len(out) >= 10 {
					break
				}
				if !u.hasExampleRef(out, e.ref) {
					out = append(out, e)
				}
			}
		}
	}
	return out
}

func (u *run) hasExampleRef(examples []example, ref string) bool {
	for i := range examples {
		if examples[i].ref == ref {
			return true
		}
	}
	return false
}

func (u *run) sampleReason(e example) string {
	low := strings.ToLower(e.command)
	if e.kind == "habit-anchor" {
		return "(а) якоря/regex требуют участка или шаблона"
	}
	if strings.Contains(e.command, `\n`) || strings.Contains(low, "old=") || strings.Contains(low, "old =") {
		return "(в) длинный текст или экранирование"
	}
	if strings.Contains(low, "glob(") || strings.Contains(low, "for f in") && (strings.Contains(low, "path") || strings.Contains(low, "glob")) {
		return "(а) генерация или правка набора файлов"
	}
	return "(г) привычка без видимой причины"
}
