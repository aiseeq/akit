package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"maps"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/transcript"
	"mvdan.cc/sh/v3/syntax"
)

// New tools go last: candidates are sampled with one generator in this order, so a tool put in the
// middle would reshuffle the samples, and the blind labels, of the tools after it.
const pairToolNames = "busy prior user-word page-shot claude-rules-export wfor timeit"
const pairCode2Tools = "busy prior user-word wfor claude-rules-export timeit"
const candidateContextRunes = 600

type pairPatterns struct {
	busyProbe, searchTool, copyTool, heavyCommand, spaces *regexp.Regexp
	waitGoals                                             []*regexp.Regexp
	tkKindClean, tkPath, tkTicketID                       *regexp.Regexp
	timeCommand                                           *regexp.Regexp
}

func newPairPatterns() pairPatterns {
	return pairPatterns{
		busyProbe:    regexp.MustCompile(`\b(uptime|w|top|vmstat|mpstat|loadavg|pressure)\b`),
		searchTool:   regexp.MustCompile(`\b(grep|rg|jq|sed|tl)\b`),
		copyTool:     regexp.MustCompile(`\b(cp|rsync|install)\b`),
		heavyCommand: regexp.MustCompile(`\b(graft\s+(gate|test|commit|install|release)|graft\s+exec\s+--heavy|go\s+(test|build)|make|docker\s+build|podman\s+build|worker\s+run|(npm|pnpm|yarn)\s+(run\s+)?(test|build))\b`),
		spaces:       regexp.MustCompile(`\s+`),
		waitGoals: []*regexp.Regexp{
			regexp.MustCompile(`(?:test\s+-[efsd]\s+|\[\[?\s+-[efsd]\s+)([^]\s;]+)`),
			regexp.MustCompile(`curl\s+-\S+\s+([^\s;]+)`), regexp.MustCompile(`kill\s+-0\s+([^\s;]+)`),
			regexp.MustCompile(`pgrep\s+(?:-\S+\s+)*([^\s;]+)`), regexp.MustCompile(`(?:grep|rg)\s+-[^\s]*q[^\s]*\s+.*?\s+([^\s;]+)`),
			regexp.MustCompile(`ps\s+-p\s+([^\s;]+)`), regexp.MustCompile(`ls\s+([^\s;]+)`),
		},
		tkKindClean: regexp.MustCompile(`[0-9]+|[^\p{L}\w -]`),
		tkPath:      regexp.MustCompile(`(?:^|\s)(?:/\S+|\S*\.md)(?:\s|$)`),
		tkTicketID:  regexp.MustCompile(`\b[0-9]{8}-[0-9]{4}-[A-Za-z0-9-]+\b`),
		timeCommand: regexp.MustCompile(`(?:^|[;&|()]\s*)(?:/usr/bin/)?time(?:\s|$)`),
	}
}

func (u *run) isPairBusyProbe(c shellcmd.Call) bool {
	l := strings.ToLower(c.Raw + " " + strings.Join(c.Args, " "))
	return c.Name == "uptime" && !uptimeOnly(c.Args) || c.Name == "w" || c.Name == "top" && strings.Contains(l, "-b") && (strings.Contains(l, "-n1") || strings.Contains(l, "-n 1")) && !strings.Contains(l, "-p") && !strings.Contains(l, "-h") || c.Name == "vmstat" || c.Name == "mpstat" || c.Name == "cat" && (strings.Contains(l, "/proc/loadavg") || strings.Contains(l, "/proc/pressure/"))
}

// uptimeOnly identifies options that report uptime rather than system load.
func uptimeOnly(args []string) bool {
	for _, arg := range args {
		if slices.Contains([]string{"--since", "--pretty", "--help", "--version"}, arg) {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsAny(strings.TrimPrefix(arg, "-"), "sphV") {
			return true
		}
	}
	return false
}

func pairTools() []string { return strings.Fields(pairToolNames) }

type pairCall struct {
	tool, way, goal, agent, session, id, command, out, outcome, period string
	at                                                                 time.Time
	code                                                               int
	instructed                                                         bool
	codeNotOwned                                                       bool
	// severalCalls — the utility is called more than once in the command: a line of the shared output
	// cannot be attributed to one of the calls.
	severalCalls bool
}

type pairCandidate struct {
	id, pair, command, context string
	yes                        bool
	agent                      string
}
type tkRefusal struct {
	kind, ticket, subcommand, agent, session string
	at                                       time.Time
	line                                     int
}
type pairOverlap struct{ id, tool string }
type releaseDate struct{ available, instructed time.Time }

// loadInputs reads the --releases and --labels files before any transcript is read: a missing, unreadable
// or malformed file is a usage error found at once. Labels are checked against the sample after the scan.
func (u *run) loadInputs() error {
	releases, err := u.loadReleases()
	if err != nil {
		return err
	}
	u.releases = releases
	if u.rescorePath != "" {
		u.labelsPath = u.rescorePath
		err := u.readPairLabels()
		u.labelsPath = ""
		return err
	}
	return u.readPairLabels()
}

// readInputTSV calls each with the line number and the tab-separated fields of every line of an input
// file and returns the number of lines; failing to open or read the file is a usage error.
func readInputTSV(kind, path string, each func(line int, fields []string) error) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, &uptakeInputError{message: fmt.Sprintf("%s: %v", kind, err)}
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	line := 0
	for s.Scan() {
		line++
		if err := each(line, strings.Split(s.Text(), "\t")); err != nil {
			return line, err
		}
	}
	if err := s.Err(); err != nil {
		return line, &uptakeInputError{message: fmt.Sprintf("%s %s: %v", kind, path, err)}
	}
	return line, nil
}

func (u *run) loadReleases() (map[string]releaseDate, error) {
	m := map[string]releaseDate{}
	if u.releasesPath == "" {
		return m, nil
	}
	_, err := readInputTSV("releases", u.releasesPath, func(line int, p []string) error {
		if len(p) != 3 || !contains(pairTools(), p[0]) {
			return &uptakeInputError{message: fmt.Sprintf("%s:%d: expected known tool and two RFC3339 dates", u.releasesPath, line)}
		}
		d := releaseDate{}
		for i, v := range p[1:] {
			if v == "" {
				continue
			}
			t, e := time.Parse(time.RFC3339, v)
			if e != nil {
				return &uptakeInputError{message: fmt.Sprintf("%s:%d: invalid date", u.releasesPath, line)}
			}
			if i == 0 {
				d.available = t
			} else {
				d.instructed = t
			}
		}
		if _, ok := m[p[0]]; ok {
			return &uptakeInputError{message: fmt.Sprintf("%s:%d: duplicate tool", u.releasesPath, line)}
		}
		m[p[0]] = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, tool := range pairTools() {
		if _, ok := m[tool]; !ok {
			return nil, &uptakeInputError{message: fmt.Sprintf("%s: missing %s", u.releasesPath, tool)}
		}
	}
	return m, nil
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

func (u *run) collectPairEvents(path string, isClaude bool, models map[int]string, evs []transcript.Event, outs map[string]transcript.Event, calls []call) {
	if u.sessionEnds == nil {
		u.sessionEnds = map[string]time.Time{}
	}
	if len(evs) > 0 {
		u.sessionEnds[path] = evs[len(evs)-1].Time
	}
	callByID := map[string]*call{}
	for i := range calls {
		callByID[calls[i].ev.CallID] = &calls[i]
	}
	firstUser := ""
	for i := range evs {
		if evs[i].Role == transcript.User {
			firstUser = evs[i].Text
			break
		}
	}
	rejected := map[string]bool{}
	for i := range evs {
		e := &evs[i]
		if e.Role != transcript.Cmd {
			continue
		}
		if rejectedToolUse(outs[e.CallID]) {
			if !rejected[e.CallID] {
				u.rejectedToolUses++
				rejected[e.CallID] = true
			}
			continue
		}
		u.collectPairCommand(path, isClaude, models, evs, outs, callByID, firstUser, i, e)
	}
}

// rejectedToolUse reports a tool result that is the harness's refusal: it is an
// error result, so an ordinary output quoting the refusal text still counts.
func rejectedToolUse(out transcript.Event) bool {
	if !out.Error {
		return false
	}
	text := strings.ToLower(out.Text)
	return strings.Contains(text, "the user doesn't want to proceed with this tool use") ||
		strings.Contains(text, "tool use was rejected")
}

func (u *run) collectPairCommand(path string, isClaude bool, models map[int]string, evs []transcript.Event, outs map[string]transcript.Event, callByID map[string]*call, firstUser string, i int, e *transcript.Event) {
	cmd, ok := u.callFor(*e, isClaude)
	if !ok {
		return
	}
	agent := u.agentOf(path, strings.Contains(path, "/subagents/"), u.modelAt(models, e.Line))
	calls, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // The parser returns valid prefix calls alongside NestedError; those calls still represent executed commands.
		slog.Debug("parse pair command", "error", err)
	}
	u.decisionRuleSearches += decisionRuleSearchCount(calls)
	for ci := range calls {
		sc := calls[ci]
		u.collectPairOverlap(path, e, sc, cmd)
		if sc.Name == "tk" {
			u.collectPairTK(path, agent, e, sc, outs, callByID)
		}
		if contains(pairTools(), sc.Name) {
			u.collectPairNew(path, agent, firstUser, evs, outs, callByID, calls, ci, cmd, i, e)
		}
		u.collectPairOld(path, agent, firstUser, evs, outs, sc, cmd, i, e)
	}
	u.collectCandidate(path, evs, i, cmd, agent)
}

func decisionRuleSearchCount(calls []shellcmd.Call) int {
	n := 0
	for i := range calls {
		c := &calls[i]
		if (c.Name == "grep" || c.Name == "rg") && strings.Contains(strings.ToLower(c.Raw+" "+strings.Join(c.Args, " ")), "decisions.md") {
			n++
		}
	}
	return n
}

func (u *run) collectPairOverlap(path string, e *transcript.Event, c shellcmd.Call, cmd string) {
	if c.Name == "tl" && strings.Contains(strings.ToLower(cmd), "--grep") || u.isTranscriptHabit(c, strings.ToLower(strings.Join(c.Args, " ")), strings.ToLower(cmd)) {
		u.pairOverlap = append(u.pairOverlap, pairOverlap{id: callRef(path, e), tool: "tl"})
	}
}

func (u *run) collectPairTK(path, agent string, e *transcript.Event, sc shellcmd.Call, outs map[string]transcript.Event, callByID map[string]*call) {
	out := outs[e.CallID]
	code := u.eventExitCode(out)
	if out.Error && code <= 0 {
		code = 1
	}
	if c := callByID[e.CallID]; c != nil && c.code > code {
		code = c.code
	}
	u.collectTKRefusal(path, agent, *e, sc, out.Text, code)
}

func (u *run) collectPairNew(path, agent, firstUser string, evs []transcript.Event, outs map[string]transcript.Event, callByID map[string]*call, calls []shellcmd.Call, ci int, cmd string, i int, e *transcript.Event) {
	tool := calls[ci].Name
	if tool == "busy" && strings.Contains(strings.ToLower(cmd), "--quiet") {
		u.busyQuiet++
		return
	}
	goal := pairGoal(tool, calls[ci])
	if tool == "page-shot" && !isLocalPage(goal) {
		return
	}
	if tool == "busy" {
		goal = u.busyGoal(evs, i, cmd, calls[ci])
	}
	if goal == "" {
		if tool == "busy" {
			u.busyNoDecision++
		}
		return
	}
	outEvent, hasOut := outs[e.CallID]
	out, baseCode := "", -1
	if hasOut {
		out = outEvent.Text
		baseCode = u.eventExitCode(outEvent)
		if outEvent.Error && baseCode <= 0 {
			baseCode = 1
		}
	}
	if baseCode < 0 && callByID[e.CallID] != nil {
		baseCode = callByID[e.CallID].code
	}
	code := u.pairUtilityCode(tool, calls, ci, baseCode, out)
	pc := pairCall{tool: tool, way: "new", goal: goal, agent: agent, session: path, id: callRef(path, e), command: cmd, out: out, at: e.Time, code: code, codeNotOwned: baseCode >= 0 && code < 0, severalCalls: utilityCallCount(calls, tool) > 1, instructed: strings.Contains(strings.ToLower(firstUser), strings.ToLower(tool))}
	pc.outcome = pairOutcome(pc)
	// The outcome is decided on the whole output; only its ends are kept for the samples.
	pc.out = clipOutput(out, pairOutputKeep)
	u.pairCalls = append(u.pairCalls, pc)
}

// pairOutputKeep — bytes kept from each end of a new call's output: enough for the unknown and failure
// samples, which show at most 2000 runes, without holding every long output of the window in memory.
const pairOutputKeep = 4096

// clipOutput keeps the first and the last keep bytes of s, cut on rune boundaries.
func clipOutput(s string, keep int) string {
	if len(s) <= 2*keep {
		return s
	}
	// Cells mask what they get; a value cut here would no longer be recognized.
	s = maskSecrets(s)
	if len(s) <= 2*keep {
		return s
	}
	head, tail := keep, len(s)-keep
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + "\n…\n" + s[tail:]
}

// callRef — the reference of one tool call: events of one transcript record share the line number, so the
// call id tells apart several calls of one record.
func callRef(path string, e *transcript.Event) string {
	return fmt.Sprintf("%s:%d:%s", path, e.Line, e.CallID)
}

func (u *run) collectPairOld(path, agent, firstUser string, evs []transcript.Event, outs map[string]transcript.Event, sc shellcmd.Call, cmd string, i int, e *transcript.Event) {
	way, goal := u.oldPair(sc, cmd)
	if way == "user-word" && sc.Name == "tl" && priorSearch(sc) {
		u.recordPairOld(path, agent, firstUser, evs, outs, sc, cmd, i, e, "prior", goal)
	}
	u.recordPairOld(path, agent, firstUser, evs, outs, sc, cmd, i, e, way, goal)
}

func (u *run) recordPairOld(path, agent, firstUser string, evs []transcript.Event, outs map[string]transcript.Event, sc shellcmd.Call, cmd string, i int, e *transcript.Event, way, goal string) {
	if way == "" {
		return
	}
	if way == "busy" {
		if hardwareInventory(cmd) {
			return
		}
		shadowed, err := sc.ShadowedByFunction(cmd)
		if err != nil {
			slog.Debug("busy probe left unclassified", "error", err)
			return
		}
		if shadowed {
			return
		}
		goal = u.oldBusyGoal(evs, i, cmd, sc)
		if goal == "" {
			u.busyNoDecision++
			return
		}
	}
	id := callRef(path, e)
	if way == "prior" {
		if sc.Name == "tl" {
			u.priorTLCalls++
		} else {
			u.priorImproveCalls++
		}
	}
	// The old way's outcome is taken from the exit code alone, so its output is not kept.
	u.pairCalls = append(u.pairCalls, pairCall{tool: way, way: "old", goal: goal, agent: agent, session: path, id: id, command: cmd, at: e.Time, code: u.eventExitCode(outs[e.CallID]), instructed: strings.Contains(strings.ToLower(firstUser), way)})
	if u.pairOldIDs == nil {
		u.pairOldIDs = make(map[string]map[string]struct{})
	}
	if u.pairOldIDs[id] == nil {
		u.pairOldIDs[id] = make(map[string]struct{})
	}
	u.pairOldIDs[id][way] = struct{}{}
}

func pairGoal(tool string, c shellcmd.Call) string {
	switch tool {
	case "prior":
		return priorGoal(c.Args)
	case "user-word":
		return userWordGoal(c.Args)
	case "wfor":
		goal, _ := wforGoal(c.Args)
		return goal
	case "page-shot":
		return pageGoal(c.Args)
	case "claude-rules-export":
		return lastArg(positionals(c.Args, "src-home", "rules"))
	case "timeit":
		args := c.Args
		if at := slices.Index(args, "--"); at >= 0 {
			args = args[at+1:]
		} else {
			return ""
		}
		return normalizePairGoal(strings.Join(args, " "))
	default:
		return ""
	}
}

func normalizePairGoal(goal string) string {
	return strings.ToLower(strings.Join(strings.Fields(goal), " "))
}

func callPairGoal(c shellcmd.Call) string {
	return normalizePairGoal(strings.Join(append([]string{c.Name}, c.Args...), " "))
}

// positionals returns the arguments that are neither flags nor values of the named value flags (names
// without dashes; both "--name V" and "--name=V" are understood). Everything after "--" is positional.
func positionals(args []string, valueFlags ...string) []string {
	var words []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(words, args[i+1:]...)
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			words = append(words, arg)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !hasValue && contains(valueFlags, name) {
			i++
		}
	}
	return words
}

// wforConditionWords — words after a wfor condition keyword; the goal is the last of them. exit takes
// one or more PIDs and is handled apart.
func wforConditionWords(cond string) (int, bool) {
	switch cond {
	case "file", "gone", "time", "ok":
		return 1, true
	case "line":
		return 2, true
	case "idle":
		return 0, true
	}
	return 0, false
}

// wforGoal returns the goal of a wfor call — the objects of its conditions in order: the path of file,
// gone and line, the PIDs of exit, the moment of time, the command of ok, and "idle". A call for help or
// without conditions is not a call of the utility (ok false). A malformed condition list is still a call
// (it ends in a usage error); its goal is then the condition words as written.
func wforGoal(args []string) (string, bool) {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return "", false
		}
	}
	words := positionals(args, "timeout", "every", "after")
	if len(words) == 0 {
		return "", false
	}
	targets, ok := wforTargets(words)
	if !ok {
		return strings.Join(words, " "), true
	}
	return strings.Join(targets, ", "), true
}

func wforTargets(words []string) ([]string, bool) {
	var targets []string
	for i := 0; i < len(words); {
		cond := words[i]
		i++
		if cond == "exit" {
			start := i
			for i < len(words) && words[i] != "" && strings.Trim(words[i], "0123456789") == "" {
				targets = append(targets, words[i])
				i++
			}
			if i == start {
				return nil, false
			}
			continue
		}
		n, known := wforConditionWords(cond)
		if !known || i+n > len(words) {
			return nil, false
		}
		if n == 0 {
			targets = append(targets, cond)
			continue
		}
		targets = append(targets, words[i+n-1])
		i += n
	}
	return targets, true
}

func priorGoal(args []string) string {
	var words []string
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			words = append(words, strings.ToLower(arg))
		}
	}
	sort.Strings(words)
	return strings.Join(words, " ")
}

func userWordGoal(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") || strings.HasSuffix(strings.ToLower(arg), ".jsonl") {
			continue
		}
		if hasLetters(arg) && (len(strings.Fields(arg)) >= 2 || len([]rune(arg)) >= 12) {
			return strings.ToLower(strings.Join(strings.Fields(arg), " "))
		}
	}
	return ""
}

func pageGoal(args []string) string {
	for _, arg := range positionals(args, "at", "out", "element", "timeout") {
		if isLocalPage(arg) {
			return arg
		}
	}
	return ""
}

func isLocalPage(arg string) bool {
	if strings.HasPrefix(arg, "file://") || !strings.Contains(arg, "://") {
		return arg != ""
	}
	u, err := url.Parse(arg)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.Trim(u.Hostname(), "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func hasLetters(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= 'а' && r <= 'я' || r >= 'А' && r <= 'Я' {
			return true
		}
	}
	return false
}

func pairOutcome(c pairCall) string {
	if c.severalCalls {
		// A summary or usage line of the shared output may belong to any of the calls.
		c.out = ""
	}
	usage := strings.Contains(c.out, "("+c.tool+" -h for usage)")
	if usage || c.code == 2 && contains(strings.Fields(pairCode2Tools), c.tool) {
		return "callerr"
	}
	if c.tool == "timeit" {
		return timeitOutcome(c)
	}
	if c.code >= 128 || strings.HasPrefix(strings.TrimSpace(c.out), "panic:") || strings.HasPrefix(strings.TrimSpace(c.out), "fatal error:") {
		return "fail"
	}
	if c.tool == "page-shot" {
		if c.code == 1 || pageShotOutputFailure(c.out) {
			return "fail"
		}
		if pairOutputAnswer(c.tool, c.out) || pairCodeAnswer(c.tool, c.code) {
			return "answer"
		}
		if c.code > 0 {
			return "fail"
		}
		return "unknown"
	}
	if pairOutputAnswer(c.tool, c.out) {
		return "answer"
	}
	if pairCodeAnswer(c.tool, c.code) {
		return "answer"
	}
	if pairOutputFailure(c) {
		return "fail"
	}
	if c.code > 0 && c.code < 128 {
		return "fail"
	}
	return "unknown"
}

func timeitOutcome(c pairCall) string {
	if c.code == 0 && pairOutputAnswer(c.tool, c.out) {
		return "answer"
	}
	if c.code > 0 {
		return "fail"
	}
	return "unknown"
}

func pageShotOutputFailure(output string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "page-shot: ") {
			continue
		}
		if strings.Contains(line, "горизонтальный скролл") || strings.Contains(line, "наложение подписей") || strings.Contains(line, "значки не выровнены с текстом") {
			continue
		}
		return true
	}
	return false
}

func pairCodeAnswer(tool string, code int) bool {
	switch tool {
	case "busy", "prior":
		return code == 0
	case "user-word", "claude-rules-export":
		return code == 0 || code == 1
	case "page-shot":
		return code == 0 || code == 2
	case "wfor":
		return code == 0 || code == 1
	case "timeit":
		return code == 0
	}
	return false
}

func pairOutputFailure(c pairCall) bool {
	for line := range strings.SplitSeq(c.out, "\n") {
		if !strings.HasPrefix(line, c.tool+": ") || strings.Contains(line, " -h for usage") {
			continue
		}
		if c.tool == "busy" && c.code == 1 && !strings.HasPrefix(line, "busy: refusing ") {
			continue
		}
		return true
	}
	return false
}

func pairOutputAnswer(tool, out string) bool {
	lines := strings.Split(out, "\n")
	lineHas := func(prefix string) bool {
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), prefix) {
				return true
			}
		}
		return false
	}
	switch tool {
	case "busy":
		return lineHas("free; ") || lineHas("busy: ") && !lineHas("busy: refusing ")
	case "prior":
		return lineHas("== ")
	case "user-word":
		return lineHas("user-word: no user line with ") || strings.Contains(out, "--- user ") || strings.Contains(out, "--- agent before the line (")
	case "claude-rules-export":
		return lineHas("claude-rules-export: ") && !strings.Contains(out, " -h for usage")
	case "wfor":
		return lineHas("wfor: done in") || lineHas("wfor: not yet after")
	case "timeit":
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				if _, err := strconv.ParseInt(line, 10, 64); err == nil {
					return true
				}
			}
		}
	case "page-shot":
		return strings.Contains(out, ".png") || lineHas("page-shot: ")
	}
	return false
}

func (u *run) pairUtilityCode(tool string, calls []shellcmd.Call, at, code int, out string) int {
	if strings.Contains(out, "("+tool+" -h for usage)") {
		if utilityCallCount(calls, tool) == 1 {
			return code
		}
		return -1
	}
	if at < 0 || at >= len(calls) || at != len(calls)-1 || calls[at].Name != tool || utilityCallCount(calls, tool) != 1 {
		return -1
	}
	return code
}

func hardwareInventory(cmd string) bool {
	calls, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // Calls before a parsing error are still useful.
		slog.Debug("parse hardware inventory", "error", err)
	}
	return slices.ContainsFunc(calls, func(c shellcmd.Call) bool {
		return c.Name == "lscpu" || c.Name == "lshw" || c.Name == "dmidecode"
	})
}

func (u *run) oldBusyGoal(evs []transcript.Event, at int, cmd string, probe shellcmd.Call) string {
	_, after := u.executedHeavyGoals(cmd, probe.Offset)
	if after != "" {
		return after
	}
	for i := at + 1; i < len(evs); i++ {
		if evs[i].Role != transcript.Cmd {
			continue
		}
		if evs[i].Time.Sub(evs[at].Time) > 15*time.Minute {
			return ""
		}
		_, goal := u.executedHeavyGoals(evs[i].Text, -1)
		if goal != "" {
			return goal
		}
	}
	return ""
}

// executedHeavyGoals identifies executed commands on either side of the probe.
func (u *run) executedHeavyGoals(cmd string, probeOffset int) (string, string) {
	calls, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // Calls parsed before a nested error still identify executed programs.
		slog.Debug("parse heavy command", "error", err)
	}
	before, after := "", ""
	for _, c := range calls {
		if c.Offset < 0 || !slices.Contains([]string{"graft", "go", "make", "docker", "podman", "worker", "npm", "pnpm", "yarn"}, c.Name) {
			continue
		}
		goal := u.heavyCommandGoal(c.Name + " " + strings.Join(c.Args, " "))
		if c.Offset <= probeOffset {
			if goal != "" {
				before = goal
			}
		} else if goal != "" {
			after = goal
		}
	}
	return before, after
}

func (u *run) busyGoal(evs []transcript.Event, at int, cmd string, probe shellcmd.Call) string {
	start := min(max(probe.Offset+len(probe.Raw), 0), len(cmd))
	if g := u.heavyCommandGoal(cmd[start:]); g != "" {
		return g
	}
	for i := at + 1; i < len(evs); i++ {
		e := evs[i]
		if e.Role != transcript.Cmd {
			continue
		}
		if e.Time.Sub(evs[at].Time) > 15*time.Minute {
			return ""
		}
		c := e.Text
		if g := u.heavyCommandGoal(c); g != "" {
			return g
		}
	}
	return ""
}

func (u *run) heavyCommandGoal(cmd string) string {
	l := strings.ToLower(cmd)
	if u.pairPatterns.heavyCommand.MatchString(l) {
		return u.pairPatterns.spaces.ReplaceAllString(strings.TrimSpace(l), " ")
	}
	return ""
}

func (u *run) oldPair(c shellcmd.Call, cmd string) (string, string) {
	if tool, goal := oldTimeitPair(c, cmd); tool != "" {
		return tool, goal
	}
	if u.isPairBusyProbe(c) {
		return "busy", ""
	}
	if priorRead(c) && !discardedRead(cmd, c.Offset) && !debuggingRunOutput(c, cmd) {
		return "prior", callPairGoal(c)
	}
	if tool, goal := u.oldSearchPair(c, cmd); tool != "" {
		return tool, goal
	}
	if goal := u.waitPairGoal(c, cmd); goal != "" {
		return "wfor", goal
	}
	if goal := u.embeddedWaitGoal(c); goal != "" {
		return "wfor", goal
	}
	if goal := u.pageShotGoal(c); goal != "" {
		return "page-shot", goal
	}
	if goal := u.exportGoal(c); goal != "" {
		return "claude-rules-export", goal
	}
	return "", ""
}

var timeitDateRead = regexp.MustCompile(`\bdate\s+\+%s(?:%3N|%N)?\b`)
var timeitDateSubtract = regexp.MustCompile(`\$\(\(.*-.*\)\)|\bexpr\b[^;|&]*-[^;|&]*|(?:echo|printf)\s+[^;|]*-[^;|&]*\|\s*bc\b`)

// oldTimeitPair recognises shell time commands and date readings used as an elapsed-time subtraction.
func oldTimeitPair(c shellcmd.Call, cmd string) (string, string) {
	if c.Name == "time" {
		if goal := timeCommandGoal(c); goal != "" {
			return "timeit", goal
		}
	}
	return oldDateTimingPair(c, cmd)
}

func timeCommandGoal(c shellcmd.Call) string {
	if slices.Contains(c.Via, "time") {
		return callPairGoal(c)
	}
	raw := strings.TrimSpace(c.Raw)
	if strings.HasPrefix(raw, "time ") {
		calls, err := shellcmd.Calls(raw)
		if err == nil {
			for i := range calls {
				if slices.Contains(calls[i].Via, "time") {
					return callPairGoal(calls[i])
				}
			}
		}
		return ""
	}
	if strings.HasPrefix(raw, "/usr/bin/time ") {
		return normalizePairGoal(strings.Join(timedCommand(c.Args), " "))
	}
	return ""
}

// timedCommand is what /usr/bin/time runs: its arguments past the leading options of time itself.
func timedCommand(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		flag := args[0]
		args = args[1:]
		switch flag {
		case "--":
			return args
		case "-f", "-o", "--format", "--output":
			if len(args) > 0 {
				args = args[1:]
			}
		}
	}
	return args
}

func oldDateTimingPair(c shellcmd.Call, cmd string) (string, string) {
	if c.Name != "date" || !timeitDateRead.MatchString(c.Raw) {
		return "", ""
	}
	reads := timeitDateRead.FindAllStringIndex(cmd, -1)
	if len(reads) != 2 || c.Offset != reads[0][0] || !timeitDateSubtract.MatchString(cmd[reads[0][1]:]) {
		return "", ""
	}
	calls, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // Calls parsed before a nested shell error still identify the measured command.
		slog.Debug("parse date timing", "error", err)
	}
	var measured []string
	seen := map[string]bool{}
	for i := range calls {
		call := calls[i]
		// A wrapper (timeout, nice) and the command it runs are two calls over one text: one goal.
		if call.Offset > reads[0][1] && call.Offset < reads[1][0] && !seen[call.Raw] && call.Name != "date" && call.Name != "printf" && call.Name != "echo" && call.Name != "expr" && call.Name != "bc" {
			seen[call.Raw] = true
			measured = append(measured, callPairGoal(call))
		}
	}
	goal := normalizePairGoal(strings.Join(measured, " "))
	if goal == "" {
		return "", ""
	}
	return "timeit", goal
}

func (u *run) oldSearchPair(c shellcmd.Call, cmd string) (string, string) {
	name := strings.ToLower(c.Name)
	if transcriptSearch(u, c, cmd, name) {
		return "user-word", searchGoal(c)
	}
	if priorSearch(c) && !debuggingRunOutput(c, cmd) {
		return "prior", searchGoal(c)
	}
	return "", ""
}

// debuggingRunOutput treats reading run output after source inspection in the same command as debugging.
func debuggingRunOutput(c shellcmd.Call, cmd string) bool {
	if c.Offset < 0 || !slices.ContainsFunc(c.Args, func(arg string) bool { return strings.Contains(arg, "/.claude/improve/runs/") }) {
		return false
	}
	calls, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // Parsed prefix calls still identify source inspection.
		slog.Debug("parse source inspection", "error", err)
	}
	for _, before := range calls {
		if before.Offset < 0 || before.Offset >= c.Offset || !slices.Contains([]string{"cat", "sed", "head", "tail"}, before.Name) {
			continue
		}
		if slices.ContainsFunc(before.Args, func(arg string) bool { return slices.Contains([]string{".go", ".py", ".rs", ".js"}, filepath.Ext(arg)) }) {
			return true
		}
	}
	return false
}

func priorSearch(c shellcmd.Call) bool {
	if c.Name == "tl" {
		return userRoleSearch(c.Args)
	}
	if c.Name != "grep" && c.Name != "rg" {
		return false
	}
	if dataOnlyGlobs(c.Args) {
		return false
	}
	pattern := searchGoal(c)
	for _, arg := range c.Args {
		if strings.EqualFold(arg, pattern) {
			continue
		}
		p := strings.ToLower(filepath.ToSlash(arg))
		if !isPriorSource(p) {
			continue
		}
		if !rawPriorData(p) && (!priorReport(p) || historicalSearch(searchGoal(c))) {
			return true
		}
	}
	return false
}

// dataOnlyGlobs reports an explicit restriction to structured data files.
func dataOnlyGlobs(args []string) bool {
	included := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var pattern string
		switch {
		case arg == "-g" || arg == "--glob" || arg == "--include":
			if i+1 >= len(args) {
				return false
			}
			i++
			pattern = args[i]
		case strings.HasPrefix(arg, "--glob=") || strings.HasPrefix(arg, "--include="):
			_, pattern, _ = strings.Cut(arg, "=")
		case strings.HasPrefix(arg, "-g") && len(arg) > 2:
			pattern = arg[2:]
		default:
			continue
		}
		if strings.HasPrefix(pattern, "!") {
			continue
		}
		included = true
		ext := strings.ToLower(filepath.Ext(pattern))
		if ext != ".json" && ext != ".jsonl" {
			return false
		}
	}
	return included
}

// isPriorSource accepts the historical store without assuming a fixed set of sections.
func isPriorSource(p string) bool {
	return strings.HasSuffix(p, "/decisions.md") || strings.Contains(p, "/.claude/improve/")
}

// rawPriorData excludes machine-readable logs and instructional material, not registries.
func rawPriorData(p string) bool {
	p = strings.ToLower(p)
	return strings.HasSuffix(p, ".json") || strings.HasSuffix(p, ".jsonl") || strings.HasSuffix(p, ".log") ||
		priorDataSection(p, "perf") || priorDataSection(p, "templates") || priorDataSection(p, "prompts")
}

func priorDataSection(p, section string) bool {
	return strings.Contains(p, "/"+section+"/") || strings.HasSuffix(p, "/"+section)
}

func priorReport(p string) bool {
	base := filepath.Base(p)
	return strings.HasPrefix(base, "report.") || strings.HasPrefix(base, "report-")
}

var historicalWords = regexp.MustCompile(`(?i)(решени|проб(?:а|ы|у|е|н|овал)|попыт|эксперимент|decision|trial|attempt|experiment)`)

func historicalSearch(pattern string) bool { return historicalWords.MatchString(pattern) }

// priorRead recognises direct reading of recorded tasks and trials.
func priorRead(c shellcmd.Call) bool {
	if c.Name != "cat" && c.Name != "less" && c.Name != "head" && c.Name != "tail" && c.Name != "sed" {
		return false
	}
	if c.Name == "sed" && slices.ContainsFunc(c.Args, func(arg string) bool { return arg == "-i" || strings.HasPrefix(arg, "--in-place") }) {
		return false
	}
	for _, arg := range c.Args {
		p := strings.ToLower(filepath.ToSlash(arg))
		if isPriorSource(p) && !rawPriorData(p) && !priorReport(p) && (strings.Contains(p, "/tasks/") || strings.Contains(p, "/runs/")) && (filepath.Ext(p) == "" || filepath.Ext(p) == ".md") {
			return true
		}
	}
	return false
}

// discardedRead reports a read whose output is discarded, including at the end of a pipe.
func discardedRead(cmd string, offset int) bool {
	if offset < 0 {
		return false
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		slog.Debug("parse read output", "error", err)
		return false
	}
	discarded := false
	syntax.Walk(file, func(n syntax.Node) bool {
		s, ok := n.(*syntax.Stmt)
		if !ok || int(s.Pos().Offset()) > offset || int(s.End().Offset()) <= offset {
			return true
		}
		end := s
		if pipe, ok := s.Cmd.(*syntax.BinaryCmd); ok && (pipe.Op == syntax.Pipe || pipe.Op == syntax.PipeAll) {
			end = pipe.Y
		}
		for _, redir := range end.Redirs {
			if redir.N == nil && redir.Word != nil && redir.Word.Lit() == "/dev/null" {
				discarded = true
			}
		}
		return !discarded
	})
	return discarded
}

func transcriptSearch(u *run, c shellcmd.Call, cmd, name string) bool {
	search := name == "grep" || name == "rg" || name == "jq" || name == "sed"
	if name == "tl" {
		return userRoleSearch(c.Args)
	}
	if !search || !u.userTranscriptArgs(c, cmd) {
		return false
	}
	if invokesUserWord(cmd) {
		return false
	}
	phrase := searchGoal(c)
	return !(name == "jq" && phrase == "keys") && !u.commandIdentifier(phrase) && naturalLanguagePhrase(phrase)
}

// commandIdentifier rejects known program names and command-shaped searches.
func (u *run) commandIdentifier(phrase string) bool {
	if strings.Contains(phrase, "|") {
		return false
	}
	fields := strings.Fields(phrase)
	if len(fields) == 0 {
		return false
	}
	name := fields[0]
	known := slices.Contains(u.utilityNames, name) || u.heavyCommandGoal(name+" run") != "" || slices.Contains([]string{"graft", "glint", "mut", "bash", "sh", "zsh", "grep", "rg", "git", "curl", "python", "python3"}, name)
	if !known {
		return false
	}
	if len(fields) == 1 {
		return true
	}
	for _, arg := range fields[1:] {
		if strings.HasPrefix(arg, "-") || strings.IndexFunc(arg, unicode.IsDigit) >= 0 {
			return true
		}
	}
	return false
}

func userRoleSearch(args []string) bool {
	role, grep := false, false
	for i, arg := range args {
		if arg == "--role" && i+1 < len(args) {
			role = slices.Contains(strings.Split(args[i+1], ","), "user")
		}
		if after, ok := strings.CutPrefix(arg, "--role="); ok {
			role = slices.Contains(strings.Split(after, ","), "user")
		}
		if arg == "--grep" && i+1 < len(args) || strings.HasPrefix(arg, "--grep=") {
			grep = true
		}
	}
	return role && grep
}

// userTranscriptArgs binds variable references to their own transcript assignment.
func (u *run) userTranscriptArgs(c shellcmd.Call, cmd string) bool {
	args := c.Args
	if u.transcriptArgs(strings.ToLower(strings.Join(args, " ")), "") {
		return true
	}
	variable := regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)
	for _, arg := range args {
		arg = strings.Trim(arg, "'\"")
		match := variable.FindStringSubmatch(arg)
		if len(match) < 2 {
			continue
		}
		assignment := regexp.MustCompile(`(?:^|[;\s])` + regexp.QuoteMeta(match[1]) + `=([^;\s]+)`)
		for _, m := range assignment.FindAllStringSubmatch(cmd, -1) {
			if u.transcriptArgs(strings.ToLower(m[1]+arg[len(match[0]):]), "") {
				return true
			}
		}
		if u.transcriptVariableSource(cmd, match[1], c.Offset) {
			return true
		}
	}
	return false
}

// transcriptVariableSource follows assignments and loop inputs that name a transcript.
func (u *run) transcriptVariableSource(cmd, name string, offset int) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		slog.Debug("parse transcript variable", "error", err)
		return false
	}
	found := false
	isTranscript := func(n syntax.Node) bool {
		start, end := int(n.Pos().Offset()), int(n.End().Offset())
		return start >= 0 && end <= len(cmd) && start < end && u.transcriptArgs(strings.ToLower(cmd[start:end]), "")
	}
	syntax.Walk(file, func(n syntax.Node) bool {
		if n == nil || found {
			return false
		}
		switch node := n.(type) {
		case *syntax.Assign:
			found = node.Name != nil && node.Name.Value == name && node.Value != nil && int(node.Pos().Offset()) < offset && isTranscript(node.Value)
		case *syntax.ForClause:
			iter, ok := node.Loop.(*syntax.WordIter)
			found = ok && iter.Name.Value == name && int(node.DoPos.Offset()) < offset && offset < int(node.End().Offset()) && isTranscript(iter)
		}
		return !found
	})
	return found
}

func invokesUserWord(cmd string) bool {
	calls, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // Parsed calls remain useful after a nested error.
		slog.Debug("parse user-word call", "error", err)
	}
	return slices.ContainsFunc(calls, func(c shellcmd.Call) bool { return c.Name == "user-word" })
}

var diagnosticPhrase = regexp.MustCompile(`(?i)\b(?:error|exception|failed|exit\s+(?:code|status))\b`)

// naturalLanguagePhrase accepts words inside search patterns, including regular-expression
// alternatives and character classes, while excluding paths and structured field expressions.
func naturalLanguagePhrase(s string) bool {
	if diagnosticPhrase.MatchString(s) {
		return false
	}
	if strings.ContainsAny(s, "/_$=<>") {
		return false
	}
	if len(strings.Fields(s)) == 1 {
		for _, r := range s {
			if !unicode.IsLetter(r) && r != '|' {
				return false
			}
		}
	}
	words, run := 0, 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			run++
			continue
		}
		if run >= 3 {
			words++
		}
		run = 0
	}
	if run >= 3 {
		words++
	}
	return words >= 1
}

func (u *run) waitPairGoal(c shellcmd.Call, cmd string) string {
	if c.Name != "sleep" || len(c.Args) == 0 {
		return ""
	}
	duration := strings.TrimRight(c.Args[0], "smhd")
	if n, err := strconv.ParseFloat(duration, 64); err == nil && n <= 0 {
		return ""
	}
	goal := u.waitGoal(cmd)
	if goal == "" || c.Offset < 0 {
		return ""
	}
	if c.Loop {
		return u.waitLoopGoal(cmd, c.Offset)
	}
	after := strings.TrimLeft(cmd[min(c.Offset+len(c.Raw), len(cmd)):], " \t\r\n;|&")
	goal = u.waitGoal(after)
	if goal == "" {
		return ""
	}
	calls, err := shellcmd.Calls(after)
	if err != nil { //nolint:fallback-return,silent-error-handling // Calls before a nested error still identify the check.
		slog.Debug("parse wait check", "error", err)
	}
	before := cmd[:min(c.Offset, len(cmd))]
	previous, err := shellcmd.Calls(before)
	if err != nil { //nolint:fallback-return,silent-error-handling // A parsed prefix still identifies a preceding request.
		slog.Debug("parse preceding request", "error", err)
	}
	for _, check := range calls {
		if check.Name != "curl" || u.waitGoal(check.Raw) != goal {
			continue
		}
		for i := len(previous) - 1; i >= 0; i-- {
			if previous[i].Name != "curl" {
				continue
			}
			endpoint := curlEndpoint(check)
			if endpoint != "" && endpoint == curlEndpoint(previous[i]) && conditionalCall(after, check.Offset) {
				break
			}
			return ""
		}
	}
	return goal
}

// curlEndpoint returns a literal URL, avoiding option values when comparing requests.
func curlEndpoint(c shellcmd.Call) string {
	for _, arg := range c.Args {
		if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
			return arg
		}
	}
	return ""
}

// conditionalCall reports whether a call controls an if or a boolean chain.
func conditionalCall(cmd string, offset int) bool {
	if offset < 0 {
		return false
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		slog.Debug("parse conditional check", "error", err)
		return false
	}
	found := false
	contains := func(n syntax.Node) bool {
		return n != nil && int(n.Pos().Offset()) <= offset && offset < int(n.End().Offset())
	}
	syntax.Walk(file, func(n syntax.Node) bool {
		if n == nil || found {
			return false
		}
		switch node := n.(type) {
		case *syntax.IfClause:
			for _, condition := range node.Cond {
				found = found || contains(condition)
			}
		case *syntax.BinaryCmd:
			if node.Op == syntax.AndStmt || node.Op == syntax.OrStmt {
				found = found || contains(node.X)
			}
		}
		return true
	})
	return found
}

// embeddedWaitGoal handles a literal shell-script fragment joined to a dynamic word.
func (u *run) embeddedWaitGoal(c shellcmd.Call) string {
	if !shellcmd.IsShell(c.Name) {
		return ""
	}
	for i, arg := range c.Args {
		if arg != "-c" || i+1 >= len(c.Args) {
			continue
		}
		file, err := syntax.NewParser().Parse(strings.NewReader("sh -c "+c.Args[i+1]), "")
		if err != nil {
			slog.Debug("parse shell script word", "error", err)
			return ""
		}
		if len(file.Stmts) != 1 {
			return ""
		}
		call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) != 3 || len(call.Args[2].Parts) < 2 {
			return ""
		}
		for _, part := range call.Args[2].Parts {
			quoted, ok := part.(*syntax.SglQuoted)
			if !ok {
				continue
			}
			script := strings.TrimLeft(quoted.Value, " \t\r\n;")
			calls, err := shellcmd.Calls(script)
			if err != nil { //nolint:fallback-return,silent-error-handling // A valid prefix may contain the complete loop.
				slog.Debug("parse shell script fragment", "error", err)
			}
			for _, nested := range calls {
				if goal := u.waitPairGoal(nested, script); goal != "" {
					return goal
				}
			}
		}
	}
	return ""
}

// waitLoopGoal examines the loop containing the sleep, not unrelated loops in the command.
func (u *run) waitLoopGoal(cmd string, offset int) string {
	return u.waitLoopGoalDepth(cmd, offset, 0)
}

func (u *run) waitLoopGoalDepth(cmd string, offset, depth int) string {
	if depth > 4 || offset < 0 || offset >= len(cmd) {
		return ""
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		slog.Debug("parse wait loop", "error", err)
		return ""
	}
	loop, quoted := enclosingWaitNodes(file, offset)
	if loop == nil {
		start, end, ok := quotedWaitBounds(cmd, quoted, offset)
		if !ok {
			return ""
		}
		return u.waitLoopGoalDepth(cmd[start:end], offset-start, depth+1)
	}
	start, end := int(loop.Pos().Offset()), min(int(loop.End().Offset()), len(cmd))
	text := cmd[start:end]
	goal := u.waitGoal(text)
	if goal == "" {
		return ""
	}
	if w, ok := loop.(*syntax.WhileClause); ok {
		do := int(w.DoPos.Offset()) - start
		if do >= 0 && do <= len(text) && u.waitGoal(text[:do]) != "" {
			return goal
		}
	}
	return u.conditionalLoopExit(loop, cmd)
}

func enclosingWaitNodes(file *syntax.File, offset int) (syntax.Node, syntax.Node) {
	var loop, quoted syntax.Node
	syntax.Walk(file, func(n syntax.Node) bool {
		if n == nil {
			return true
		}
		start, end := int(n.Pos().Offset()), int(n.End().Offset())
		switch n.(type) {
		case *syntax.WhileClause, *syntax.ForClause:
			if start <= offset && offset < end && (loop == nil || end-start < int(loop.End().Offset()-loop.Pos().Offset())) {
				loop = n
			}
		case *syntax.SglQuoted, *syntax.DblQuoted:
			if start < offset && offset < end && (quoted == nil || end-start < int(quoted.End().Offset()-quoted.Pos().Offset())) {
				quoted = n
			}
		}
		return true
	})
	return loop, quoted
}

func quotedWaitBounds(cmd string, quoted syntax.Node, offset int) (int, int, bool) {
	if quoted == nil {
		return 0, 0, false
	}
	start, end := int(quoted.Pos().Offset()), min(int(quoted.End().Offset())-1, len(cmd))
	for start < end && cmd[start] != '\'' && cmd[start] != '"' {
		start++
	}
	start++
	return start, end, start < end && offset >= start && offset < end
}

// conditionalLoopExit requires a tested condition to govern the break itself.
func (u *run) conditionalLoopExit(loop syntax.Node, cmd string) string {
	goal := ""
	syntax.Walk(loop, func(n syntax.Node) bool {
		if n == nil || goal != "" {
			return false
		}
		if n != loop {
			switch n.(type) {
			case *syntax.WhileClause, *syntax.ForClause:
				return false
			}
		}
		switch c := n.(type) {
		case *syntax.IfClause:
			if len(c.Cond) == 0 || len(c.Then) == 0 {
				break
			}
			condition := cmd[c.Cond[0].Pos().Offset():c.Cond[len(c.Cond)-1].End().Offset()]
			branches := c.Then
			if c.Else != nil {
				branches = append(slices.Clone(branches), c.Else.Then...)
			}
			for _, statement := range branches {
				if containsShellBreak(statement) {
					goal = u.waitGoal(condition)
					break
				}
			}
		case *syntax.CaseClause:
			for _, item := range c.Items {
				for _, statement := range item.Stmts {
					if containsShellBreak(statement) {
						goal = u.caseWaitGoal(c, loop, cmd)
					}
				}
			}
		case *syntax.BinaryCmd:
			if (c.Op == syntax.AndStmt || c.Op == syntax.OrStmt) && containsShellBreak(c.Y) {
				goal = u.waitGoal(cmd[c.X.Pos().Offset():c.X.End().Offset()])
			}
		}
		return true
	})
	return goal
}

// caseWaitGoal resolves the expression tested by case to the command producing it.
func (u *run) caseWaitGoal(c *syntax.CaseClause, loop syntax.Node, cmd string) string {
	start, end := int(c.Word.Pos().Offset()), int(c.Word.End().Offset())
	expression := cmd[start:end]
	if goal := u.waitGoal(expression); goal != "" {
		return goal
	}
	name := strings.Trim(expression, "'\"${} \t")
	goal := ""
	syntax.Walk(loop, func(n syntax.Node) bool {
		if assign, ok := n.(*syntax.Assign); ok && assign.Name != nil && assign.Name.Value == name && assign.Value != nil && assign.Pos().Offset() < c.Pos().Offset() {
			goal = u.waitGoal(cmd[assign.Value.Pos().Offset():assign.Value.End().Offset()])
		}
		return true
	})
	return goal
}

func containsShellBreak(node syntax.Node) bool {
	found := false
	syntax.Walk(node, func(n syntax.Node) bool {
		if n == nil || found {
			return false
		}
		switch c := n.(type) {
		case *syntax.WhileClause, *syntax.ForClause:
			return false
		case *syntax.CallExpr:
			if len(c.Args) > 0 && len(c.Args[0].Parts) == 1 {
				if lit, ok := c.Args[0].Parts[0].(*syntax.Lit); ok {
					found = lit.Value == "break" || (lit.Value == "exit" || lit.Value == "return") && (len(c.Args) == 1 || c.Args[1].Lit() == "0")
				}
			}
		}
		return true
	})
	return found
}

func (u *run) pageShotGoal(c shellcmd.Call) string {
	line := strings.ToLower(c.Raw + " " + strings.Join(c.Args, " "))
	name := strings.ToLower(c.Name)
	browser := (name == "chrome" || name == "chromium" || name == "google-chrome" || name == "google-chrome-stable") && strings.Contains(line, "--screenshot")
	browser = browser || name == "playwright" && len(c.Args) > 0 && c.Args[0] == "screenshot" || name == "wkhtmltoimage"
	script := strings.Contains(line, "screenshot(") && (strings.Contains(line, ".html") || strings.Contains(line, "localhost") || strings.Contains(line, "127.0.0.1") || strings.Contains(line, "file://"))
	if !browser && !script {
		return ""
	}
	return findLocalPage(line)
}

// copyValueFlags — flags of cp, rsync and install that take a value, so the value is not a path.
func copyValueFlags(tool string) ([]string, bool) {
	switch tool {
	case "cp":
		return []string{"t", "target-directory", "S", "suffix"}, true
	case "install":
		return []string{"t", "target-directory", "S", "suffix", "m", "mode", "o", "owner", "g", "group"}, true
	case "rsync":
		return []string{"e", "rsh", "f", "filter", "exclude", "include", "exclude-from", "include-from", "files-from", "chmod", "chown", "rsync-path", "log-file", "partial-dir", "temp-dir", "T", "compare-dest", "copy-dest", "link-dest"}, true
	}
	return nil, false
}

// exportGoal recognises the old way of claude-rules-export: a copy (cp, rsync, install) whose source is a
// Claude Code rules or skills location; the goal is the destination, the last path or the -t directory.
func (u *run) exportGoal(c shellcmd.Call) string {
	valueFlags, ok := copyValueFlags(c.Name)
	if !ok {
		return ""
	}
	paths := positionals(c.Args, valueFlags...)
	dest := copyTargetDirectory(c.Args)
	sources := paths
	if dest == "" {
		if len(paths) < 2 {
			return ""
		}
		dest, sources = paths[len(paths)-1], paths[:len(paths)-1]
	}
	if !slices.ContainsFunc(sources, isClaudeRulesSource) {
		return ""
	}
	return dest
}

func copyTargetDirectory(args []string) string {
	for i, arg := range args {
		if v, ok := strings.CutPrefix(arg, "--target-directory="); ok {
			return v
		}
		if (arg == "-t" || arg == "--target-directory") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// isClaudeRulesSource reports a path inside the Claude Code rules, skills or global instructions.
func isClaudeRulesSource(path string) bool {
	p := filepath.ToSlash(filepath.Clean(path))
	for _, part := range []string{".claude/rules", ".claude/skills", ".claude/CLAUDE.md"} {
		if strings.HasSuffix(p, part) {
			return true
		}
		if strings.Contains(p, part+"/") {
			base := filepath.Base(p)
			if !strings.Contains(base, ".") {
				return true
			}
			return strings.HasSuffix(strings.ToLower(base), ".md")
		}
	}
	return false
}

func searchGoal(c shellcmd.Call) string {
	if c.Name == "tl" {
		for i, arg := range c.Args {
			if arg == "--grep" && i+1 < len(c.Args) {
				return strings.ToLower(c.Args[i+1])
			}
			if value, ok := strings.CutPrefix(arg, "--grep="); ok {
				return strings.ToLower(value)
			}
		}
	}
	for i := 0; i < len(c.Args); i++ {
		a := c.Args[i]
		if a == "-e" || a == "--regexp" || a == "--grep" {
			if i+1 < len(c.Args) {
				return strings.ToLower(c.Args[i+1])
			}
			return ""
		}
		if slices.Contains([]string{"-m", "-A", "-B", "-C", "-g", "--glob", "--include", "--role", "--max-count", "--after-context", "--before-context", "--context"}, a) {
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			return strings.ToLower(a)
		}
	}
	return ""
}
func (u *run) waitGoal(cmd string) string {
	for _, r := range u.pairPatterns.waitGoals {
		if m := r.FindStringSubmatch(cmd); len(m) > 1 {
			return strings.Trim(m[1], "'\"")
		}
	}
	return ""
}
func findLocalPage(s string) string {
	for a := range strings.FieldsSeq(s) {
		a = strings.Trim(a, "'\"")
		if strings.HasPrefix(a, "file://") || strings.HasSuffix(strings.ToLower(a), ".html") || strings.Contains(a, "localhost") || strings.Contains(a, "127.0.0.1") || strings.Contains(a, "[::1]") {
			if strings.HasPrefix(a, "http") {
				p, err := url.Parse(a)
				if err != nil {
					slog.Debug("parse local page URL", "error", err)
					return ""
				}
				if p.Hostname() == "localhost" || p.Hostname() == "127.0.0.1" || p.Hostname() == "::1" {
					return a
				}
				continue
			}
			return a
		}
	}
	return ""
}
func lastArg(a []string) string {
	if len(a) == 0 {
		return ""
	}
	return a[len(a)-1]
}

func (u *run) collectCandidate(path string, evs []transcript.Event, i int, cmd, agent string) {
	l := strings.ToLower(cmd)
	candidates := u.candidateClasses(l, cmd)
	u.collectRescoreCandidate(callRef(path, &evs[i]))
	for pair := range candidates {
		eventID := callRef(path, &evs[i])
		_, yes := u.pairOldIDs[eventID][pair]
		id := pair + ":" + eventID
		context := u.candidateContext(evs, i)
		u.pairCandidates = append(u.pairCandidates, pairCandidate{id: id, pair: pair, command: maskSecrets(cmd), context: context, yes: yes, agent: agent})
	}
}

func (u *run) collectRescoreCandidate(eventID string) {
	if u.rescorePath == "" {
		return
	}
	originalID := eventID
	if prefix, suffix, ok := strings.Cut(eventID, "#"); ok {
		if _, err := strconv.Atoi(suffix); err == nil {
			originalID = prefix
		}
	}
	for _, pair := range pairTools() {
		for _, ref := range []string{eventID, originalID} {
			id := pair + ":" + ref
			if _, ok := u.labelRows[id]; !ok {
				continue
			}
			if u.rescoreCandidates == nil {
				u.rescoreCandidates = make(map[string]pairCandidate)
			}
			_, yes := u.pairOldIDs[eventID][pair]
			yes = yes || u.rescoreCandidates[id].yes
			u.rescoreCandidates[id] = pairCandidate{id: id, pair: pair, yes: yes}
		}
	}
}

func (u *run) candidateClasses(l, cmd string) map[string]bool {
	candidates := map[string]bool{}
	if u.pairPatterns.busyProbe.MatchString(l) {
		candidates["busy"] = u.isBusyProbeCall(cmd)
	}
	if (strings.Contains(l, ".jsonl") || strings.Contains(l, "decisions.md") || strings.Contains(l, "/.claude/improve/")) && u.pairPatterns.searchTool.MatchString(l) {
		candidates["prior"] = u.oldPairCandidate("prior", cmd)
		candidates["user-word"] = u.oldPairCandidate("user-word", cmd)
	}
	if strings.Contains(l, "screenshot") || strings.Contains(l, "--headless") || strings.Contains(l, "wkhtmltoimage") {
		candidates["page-shot"] = u.oldPairCandidate("page-shot", cmd)
	}
	if strings.Contains(l, "sleep") {
		candidates["wfor"] = u.waitGoal(cmd) != ""
	}
	if u.pairPatterns.copyTool.MatchString(l) {
		candidates["claude-rules-export"] = u.oldPairCandidate("claude-rules-export", cmd)
	}
	if strings.Contains(l, "+%s") || u.pairPatterns.timeCommand.MatchString(l) {
		candidates["timeit"] = u.oldPairCandidate("timeit", cmd)
	}
	return candidates
}

func (u *run) candidateContext(evs []transcript.Event, i int) string {
	start, end := max(i-2, 0), min(i+3, len(evs))
	var lines []string
	for j := start; j < end; j++ {
		if evs[j].Role == transcript.Cmd {
			lines = append(lines, evs[j].Text)
		}
	}
	return truncateRunes(maskSecrets(strings.Join(lines, " | ")), candidateContextRunes)
}
func (u *run) isBusyProbeCall(cmd string) bool {
	cs, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // Preserve detector results parsed before a nested shell error.
		slog.Debug("parse busy candidate", "error", err)
	}
	return slices.ContainsFunc(cs, u.isPairBusyProbe)
}
func (u *run) oldPairCandidate(tool, cmd string) bool {
	cs, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // Preserve detector results parsed before a nested shell error.
		slog.Debug("parse pair candidate", "error", err)
	}
	for i := range cs {
		w, goal := u.oldPair(cs[i], cmd)
		if w == tool && goal != "" {
			return true
		}
	}
	return false
}

func ticketMatches(key, id string) bool {
	key = strings.TrimSuffix(filepath.Base(key), ".md")
	id = filepath.Base(id)
	return key == id || strings.HasPrefix(key, id) || strings.HasPrefix(id, key)
}
func (u *run) collectTKRefusal(path, agent string, e transcript.Event, c shellcmd.Call, out string, code int) {
	if c.Name != "tk" || code <= 0 {
		return
	}
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(line, "tk: ") || strings.Contains(line, " -h for usage") {
			continue
		}
		msg := maskSecrets(strings.TrimPrefix(line, "tk: "))
		kind := strings.ToLower(u.pairPatterns.tkTicketID.ReplaceAllString(u.pairPatterns.tkPath.ReplaceAllString(msg, " "), " "))
		kind = u.pairPatterns.tkKindClean.ReplaceAllString(kind, " ")
		kind = strings.Join(strings.Fields(kind), " ")
		if words := strings.Fields(kind); len(words) > 4 {
			kind = strings.Join(words[:4], " ")
		}
		ticket := ""
		if len(c.Args) > 1 {
			ticket = c.Args[1]
		}
		sub := ""
		if len(c.Args) > 0 {
			sub = c.Args[0]
		}
		u.tkRefusals = append(u.tkRefusals, tkRefusal{kind: kind, ticket: ticket, subcommand: sub, agent: agent, session: path, at: e.Time, line: e.Line})
		break
	}
}

type labelRow struct {
	label string
	line  int
}

// readPairLabels checks the format of the labels file — header, columns, values, duplicates — and keeps
// its rows; whether the ids are those of the sample is checked by checkPairLabels after the scan.
func (u *run) readPairLabels() error {
	if u.labelsPath == "" {
		return nil
	}
	rows := map[string]labelRow{}
	lines, err := readInputTSV("labels", u.labelsPath, func(line int, fields []string) error {
		if line == 1 {
			if len(fields) != 2 || fields[0] != "id" || fields[1] != "label" {
				return &uptakeInputError{message: fmt.Sprintf("%s:1: expected id<TAB>label header", u.labelsPath)}
			}
			return nil
		}
		if len(fields) != 2 || fields[0] == "" || (fields[1] != "yes" && fields[1] != "no") {
			return &uptakeInputError{message: fmt.Sprintf("%s:%d: expected candidate id and yes|no label", u.labelsPath, line)}
		}
		if _, ok := rows[fields[0]]; ok {
			return &uptakeInputError{message: fmt.Sprintf("%s:%d: duplicate id %q", u.labelsPath, line, fields[0])}
		}
		rows[fields[0]] = labelRow{label: fields[1], line: line}
		return nil
	})
	if err != nil {
		return err
	}
	if lines == 0 {
		return &uptakeInputError{message: fmt.Sprintf("%s: empty file, expected id<TAB>label header", u.labelsPath)}
	}
	u.labelRows = rows
	return nil
}

// checkPairLabels matches the read labels to the candidate sample: every label names a sampled
// candidate and every sampled candidate has a label.
func (u *run) checkPairLabels() error {
	if u.labelsPath == "" {
		return nil
	}
	validIDs := make(map[string]struct{}, len(u.pairCandidateSample))
	for i := range u.pairCandidateSample {
		validIDs[u.pairCandidateSample[i].id] = struct{}{}
	}
	ids := slices.SortedFunc(maps.Keys(u.labelRows), func(a, b string) int { return u.labelRows[a].line - u.labelRows[b].line })
	labels := make(map[string]string, len(ids))
	for _, id := range ids {
		row := u.labelRows[id]
		if _, ok := validIDs[id]; !ok {
			return &uptakeInputError{message: fmt.Sprintf("%s:%d: unknown candidate id %q", u.labelsPath, row.line, id)}
		}
		labels[id] = row.label
	}
	missing := 0
	for id := range validIDs {
		if _, ok := labels[id]; !ok {
			missing++
		}
	}
	if missing > 0 {
		return &uptakeInputError{message: fmt.Sprintf("%s: %d sampled candidates have no label; label the whole candidates.tsv", u.labelsPath, missing)}
	}
	u.labels = labels
	return nil
}

func (u *run) writePairTables() error {
	for i := range u.pairCalls {
		c := &u.pairCalls[i]
		d := u.releases[c.tool]
		if u.releasesPath != "" {
			if d.instructed.IsZero() {
				c.period = "no-date"
			} else if c.at.Before(d.instructed) {
				c.period = "before-instr"
			} else {
				c.period = "after-instr"
			}
			if c.way == "old" && !d.available.IsZero() && c.at.Before(d.available) {
				c.goal = ""
			}
		} else {
			c.period = "no-date"
		}
	}
	if err := u.writeOutcomes(); err != nil {
		return err
	}
	if err := u.writePairRepeats(); err != nil {
		return err
	}
	if err := u.writePairOverlap(); err != nil {
		return err
	}
	if err := u.writeTKRefusals(); err != nil {
		return err
	}
	if err := u.writeCandidates(); err != nil {
		return err
	}
	if err := u.writeRescore(); err != nil {
		return err
	}
	if err := u.checkPairLabels(); err != nil {
		return err
	}
	if err := u.writePairs(); err != nil {
		return err
	}
	if err := u.writePairFailSamples(); err != nil {
		return err
	}
	return u.writePairUnknown()
}

func (u *run) writePairFailSamples() error {
	var b strings.Builder
	b.WriteString("tool\tid\tcode\toutput\n")
	rng := rand.New(rand.NewSource(int64(u.seed)))
	for tool := range strings.FieldsSeq(pairCode2Tools) {
		var samples []pairCall
		for i := range u.pairCalls {
			c := &u.pairCalls[i]
			if c.tool == tool && c.way == "new" && c.outcome == "fail" {
				samples = append(samples, *c)
			}
		}
		rng.Shuffle(len(samples), func(i, j int) { samples[i], samples[j] = samples[j], samples[i] })
		if len(samples) > 10 {
			samples = samples[:10]
		}
		for i := range samples {
			c := &samples[i]
			out := u.clean(c.out)
			out = finishCell(truncateRunes(out, 300))
			fmt.Fprintf(&b, "%s\t%s\t%d\t%s\n", tool, c.id, c.code, out)
		}
	}
	return u.save("pair-fail-samples.tsv", b.String())
}

func (u *run) writePairUnknown() error {
	var b strings.Builder
	b.WriteString("tool\tref\tcode\tcommand\toutput\treason\n")
	counts := map[string]int{}
	for i := range u.pairCalls {
		c := &u.pairCalls[i]
		if c.way != "new" || c.outcome != "unknown" {
			continue
		}
		reason := pairUnknownReason(*c)
		counts[reason]++
		fmt.Fprintf(&b, "%s\t%s\t%d\t%s\t%s\t%s\n", c.tool, c.id, c.code, finishCell(escapePairField(truncateRunes(maskSecrets(c.command), 300))), finishCell(escapePairField(truncateRunes(maskSecrets(c.out), 2000))), reason)
	}
	u.pairUnknownReasons = counts
	return u.save("pair-unknown.tsv", b.String())
}

func pairUnknownReason(c pairCall) string {
	// 2>&1 merges stderr into the visible output, so only other redirects hide it.
	if c.severalCalls {
		return "several-calls"
	}
	command := strings.ReplaceAll(strings.ToLower(c.command), "2>&1", "")
	if strings.Contains(command, ">/dev/null") || strings.Contains(command, "2>") || strings.Contains(command, "| grep") || strings.Contains(command, "| tail") || strings.Contains(command, "| head") {
		return "output-hidden"
	}
	if c.codeNotOwned {
		return "code-not-own"
	}
	return "no-output-form"
}

func escapePairField(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\x00", "\\0")
	s = strings.ReplaceAll(s, "\t", "\\t")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return strings.ReplaceAll(s, "\n", "\\n")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type epKey struct{ tool, agent, instructed, period, session, goal string }
type epCount struct{ new, old, mixed, switches int }

type pairCallCountKey struct{ tool, agent, instructed, period, way string }

func (u *run) writePairs() error {
	groups, calls := u.pairEpisodeGroups()
	counts := countPairEpisodes(groups)
	ensurePairRows(counts)
	return u.writePairRows(counts, calls)
}

func (u *run) pairEpisodeGroups() (map[epKey][]pairCall, map[pairCallCountKey]int) {
	groups := map[epKey][]pairCall{}
	calls := map[pairCallCountKey]int{}
	for i := range u.pairCalls {
		c := &u.pairCalls[i]
		if c.goal == "" {
			continue
		}
		if c.period == "" {
			c.period = "no-date"
		}
		instructed := "no"
		if c.instructed {
			instructed = "yes"
		}
		key := epKey{c.tool, c.agent, instructed, c.period, c.session, c.goal}
		groups[key] = append(groups[key], *c)
		calls[pairCallCountKey{c.tool, c.agent, instructed, c.period, c.way}]++
	}
	return groups, calls
}

func countPairEpisodes(groups map[epKey][]pairCall) map[string]*epCount {
	counts := map[string]*epCount{}
	for key, calls := range groups {
		sort.Slice(calls, func(i, j int) bool { return calls[i].at.Before(calls[j].at) })
		for i := 0; i < len(calls); {
			end, ways, switches := pairEpisodeEnd(calls, i)
			groupKey := strings.Join([]string{key.tool, key.agent, key.instructed, key.period}, "\t")
			if counts[groupKey] == nil {
				counts[groupKey] = &epCount{}
			}
			addEpisodeCount(counts[groupKey], ways, switches)
			i = end
		}
	}
	return counts
}

func pairEpisodeEnd(calls []pairCall, start int) (int, map[string]bool, int) {
	end, switches := start+1, 0
	ways := map[string]bool{calls[start].way: true}
	for end < len(calls) && calls[end].at.Sub(calls[end-1].at) <= 30*time.Minute {
		if calls[end].way != calls[end-1].way {
			switches++
		}
		ways[calls[end].way] = true
		end++
	}
	return end, ways, switches
}

func addEpisodeCount(count *epCount, ways map[string]bool, switches int) {
	switch {
	case ways["new"] && ways["old"]:
		count.mixed++
		count.switches += switches
	case ways["new"]:
		count.new++
	case ways["old"]:
		count.old++
	}
}

func ensurePairRows(counts map[string]*epCount) {
	for _, tool := range pairTools() {
		found := false
		for key := range counts {
			if strings.HasPrefix(key, tool+"\t") {
				found = true
				break
			}
		}
		if !found {
			counts[strings.Join([]string{tool, "unknown", "no", "no-date"}, "\t")] = &epCount{}
		}
	}
}

func (u *run) writePairRows(counts map[string]*epCount, calls map[pairCallCountKey]int) error {
	var b strings.Builder
	b.WriteString("tool\tagent\tinstructed\tperiod\tway\tepisodes_new\tepisodes_old\tepisodes_mixed\tswitches\tcalls\tshare\twilson95\tgate\n")
	for _, key := range slices.Sorted(maps.Keys(counts)) {
		u.writePairRow(&b, key, counts[key], calls)
	}
	return u.save("pairs.tsv", b.String())
}

func (u *run) writePairRow(b *strings.Builder, key string, count *epCount, calls map[pairCallCountKey]int) {
	parts := strings.Split(key, "\t")
	for _, way := range []string{"new", "old"} {
		ncalls := calls[pairCallCountKey{parts[0], parts[1], parts[2], parts[3], way}]
		total := count.new + count.old
		gate := u.pairLabelGate(parts[0])
		shareText, interval := "-", "-"
		if gate == "ok" && total > 0 {
			low, high := u.wilson(count.new, total)
			shareText = fmt.Sprintf("%.6f", float64(count.new)/float64(total))
			interval = fmt.Sprintf("%.6f-%.6f", low, high)
		}
		fmt.Fprintf(b, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n", parts[0], parts[1], parts[2], parts[3], way, count.new, count.old, count.mixed, count.switches, ncalls, shareText, interval, gate)
	}
}

type pairOutcomeStat struct{ n, answer, callerr, fail, unknown, empty, notyet int }

func (u *run) writeOutcomes() error {
	stats := u.pairOutcomeStats()
	var b strings.Builder
	b.WriteString("tool\tway\tcalls\tanswer\tcallerr\tfail\tunknown\tempty\tnotyet\tfail_low\tfail_high\tfail_known\twilson95\n")
	for _, tool := range pairTools() {
		for _, way := range []string{"new", "old"} {
			u.writeOutcomeRow(&b, tool, way, stats[tool+"\t"+way])
		}
	}
	return u.save("outcomes.tsv", b.String())
}

func (u *run) pairOutcomeStats() map[string]*pairOutcomeStat {
	stats := map[string]*pairOutcomeStat{}
	for i := range u.pairCalls {
		call := &u.pairCalls[i]
		stat := outcomeStat(stats, call.tool+"\t"+call.way)
		stat.n++
		if call.way == "new" {
			addNewOutcome(stat, call.outcome)
		} else {
			addOldOutcome(stat, call)
		}
	}
	return stats
}

func outcomeStat(stats map[string]*pairOutcomeStat, key string) *pairOutcomeStat {
	if stats[key] == nil {
		stats[key] = &pairOutcomeStat{}
	}
	return stats[key]
}

func addNewOutcome(stat *pairOutcomeStat, outcome string) {
	switch outcome {
	case "answer":
		stat.answer++
	case "callerr":
		stat.callerr++
	case "fail":
		stat.fail++
	case "unknown":
		stat.unknown++
	}
}

func addOldOutcome(stat *pairOutcomeStat, call *pairCall) {
	switch {
	case call.code == 1 && (call.tool == "prior" || call.tool == "user-word"):
		stat.empty++
	case call.code == 124:
		stat.notyet++
	case call.code > 0:
		stat.fail++
	case call.code == 0:
		stat.answer++
	default:
		stat.unknown++
	}
}

func (u *run) writeOutcomeRow(b *strings.Builder, tool, way string, stat *pairOutcomeStat) {
	if stat == nil {
		stat = &pairOutcomeStat{}
	}
	fails, knownN := stat.fail+stat.callerr, stat.n-stat.unknown
	// A share over zero calls is undefined: it is written as "-", not as 0.
	low, high, known, interval := "-", "-", "-", "-"
	if stat.n > 0 {
		low = fmt.Sprintf("%.6f", float64(fails)/float64(stat.n))
		high = fmt.Sprintf("%.6f", float64(fails+stat.unknown)/float64(stat.n))
	}
	if knownN > 0 {
		wilsonLow, wilsonHigh := u.wilson(fails, knownN)
		known = fmt.Sprintf("%.6f", float64(fails)/float64(knownN))
		interval = fmt.Sprintf("%.6f-%.6f", wilsonLow, wilsonHigh)
	}
	fmt.Fprintf(b, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n", tool, way, stat.n, stat.answer, stat.callerr, stat.fail, stat.unknown, stat.empty, stat.notyet, low, high, known, interval)
}

type pairRepeatKey struct{ session, goal string }

func (u *run) writePairRepeats() error {
	var b strings.Builder
	b.WriteString("tool\tway\tcalls\trepeats\tincomplete\trate\twilson95\n")
	for _, t := range pairTools() {
		for _, w := range []string{"new", "old"} {
			cs := []pairCall{}
			for i := range u.pairCalls {
				c := &u.pairCalls[i]
				if c.tool == t && c.way == w && c.goal != "" {
					cs = append(cs, *c)
				}
			}
			incomplete := 0
			byGoal := map[pairRepeatKey][]pairCall{}
			eligibleCalls := map[string]bool{}
			eligible := 0
			for i := range cs {
				c := &cs[i]
				if u.sessionObservedFor(*c, 10*time.Minute) {
					eligible++
					eligibleCalls[c.id] = true
				} else {
					incomplete++
				}
				key := pairRepeatKey{c.session, c.goal}
				byGoal[key] = append(byGoal[key], *c)
			}
			rep := countEligiblePairRepeats(byGoal, eligibleCalls)
			rate := 0.0
			if eligible > 0 {
				rate = float64(rep) / float64(eligible)
			}
			interval := "-"
			rateText := "-"
			if eligible > 0 {
				lo, hi := u.wilson(rep, eligible)
				interval = fmt.Sprintf("%.6f-%.6f", lo, hi)
				rateText = fmt.Sprintf("%.6f", rate)
			}
			fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%d\t%s\t%s\n", t, w, len(cs), rep, incomplete, rateText, interval)
		}
	}
	return u.save("pair-repeats.tsv", b.String())
}
func countEligiblePairRepeats(groups map[pairRepeatKey][]pairCall, eligible map[string]bool) int {
	total := 0
	for _, calls := range groups {
		sort.Slice(calls, func(i, j int) bool { return calls[i].at.Before(calls[j].at) })
		for i := range calls {
			if eligible[calls[i].id] && hasDifferentRepeat(calls, i) {
				total++
			}
		}
	}
	return total
}

func hasDifferentRepeat(calls []pairCall, index int) bool {
	for j := index + 1; j < len(calls) && calls[j].at.Sub(calls[index].at) <= 10*time.Minute; j++ {
		if calls[index].command != calls[j].command {
			return true
		}
	}
	return false
}

func (u *run) sessionObservedFor(c pairCall, d time.Duration) bool {
	return u.sessionEnds[c.session].Sub(c.at) >= d
}

func (u *run) writePairOverlap() error {
	var b strings.Builder
	b.WriteString("tool_a\ttool_b\tevents\n")
	counts := map[string]int{}
	byID := map[string]map[string]bool{}
	for i := range u.pairCalls {
		c := &u.pairCalls[i]
		if byID[c.id] == nil {
			byID[c.id] = map[string]bool{}
		}
		byID[c.id][c.tool] = true
	}
	for _, c := range u.pairOverlap {
		if byID[c.id] == nil {
			byID[c.id] = map[string]bool{}
		}
		byID[c.id][c.tool] = true
	}
	for _, tools := range byID {
		names := slices.Sorted(maps.Keys(tools))
		for i := range names {
			for j := i + 1; j < len(names); j++ {
				counts[names[i]+"\t"+names[j]]++
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		fmt.Fprintf(&b, "%s\t%d\n", k, counts[k])
	}
	return u.save("pair-overlap.tsv", b.String())
}

type tkRefusalStat struct{ refusals, same, other, bypass, incomplete, dropped int }

func (u *run) writeTKRefusals() error {
	stats := u.tkRefusalStats()
	var b strings.Builder
	b.WriteString("kind\trefusals\tfixed_same\tfixed_other\tbypass\tincomplete\tdropped\n")
	keys := sortedTKKinds(stats)
	for _, kind := range keys {
		s := stats[kind]
		fmt.Fprintf(&b, "%s\t%d\t%d\t%d\t%d\t%d\t%d\n", kind, s.refusals, s.same, s.other, s.bypass, s.incomplete, s.dropped)
	}
	return u.save("tk-refusals.tsv", b.String())
}

func (u *run) tkRefusalStats() map[string]*tkRefusalStat {
	stats := map[string]*tkRefusalStat{}
	for i := range u.tkRefusals {
		refusal := &u.tkRefusals[i]
		stat := stats[refusal.kind]
		if stat == nil {
			stat = &tkRefusalStat{}
			stats[refusal.kind] = stat
		}
		stat.refusals++
		addTKRefusalOutcome(stat, u.tkRefusalState(refusal))
	}
	return stats
}

// tkRefusalTotals sums tk-refusals.tsv over the kinds of refusal.
func (u *run) tkRefusalTotals() tkRefusalStat {
	total := tkRefusalStat{}
	for _, s := range u.tkRefusalStats() {
		total.refusals += s.refusals
		total.same += s.same
		total.other += s.other
		total.bypass += s.bypass
		total.incomplete += s.incomplete
		total.dropped += s.dropped
	}
	return total
}

func addTKRefusalOutcome(stat *tkRefusalStat, state string) {
	switch state {
	case "same":
		stat.same++
	case "other":
		stat.other++
	case "bypass":
		stat.bypass++
	case "incomplete":
		stat.incomplete++
	default:
		stat.dropped++
	}
}

func (u *run) tkRefusalState(refusal *tkRefusal) string {
	for i := range u.tkActions {
		action := &u.tkActions[i]
		if !tkActionMatchesRefusal(action, refusal, "tk") {
			continue
		}
		if action.op == refusal.subcommand {
			return "same"
		}
		return "other"
	}
	for i := range u.tkActions {
		action := &u.tkActions[i]
		if tkActionMatchesRefusal(action, refusal, "manual") {
			return "bypass"
		}
	}
	event := pairCall{agent: refusal.agent, session: refusal.session, at: refusal.at}
	if !u.sessionObservedFor(event, 10*time.Minute) {
		return "incomplete"
	}
	return "dropped"
}

func tkActionMatchesRefusal(action *taskWrite, refusal *tkRefusal, source string) bool {
	return action.session == refusal.session && action.source == source && !action.failed &&
		ticketMatches(action.key, refusal.ticket) && !action.at.Before(refusal.at) &&
		action.at.Sub(refusal.at) <= 10*time.Minute
}

func sortedTKKinds(stats map[string]*tkRefusalStat) []string {
	return slices.Sorted(maps.Keys(stats))
}

func (u *run) writeRescore() error {
	if u.rescorePath == "" {
		return nil
	}
	candidates := make(map[string]pairCandidate, len(u.pairCandidates))
	for _, c := range u.pairCandidates {
		candidates[c.id] = c
	}
	maps.Copy(candidates, u.rescoreCandidates)
	ids := slices.SortedFunc(maps.Keys(u.labelRows), func(a, b string) int { return u.labelRows[a].line - u.labelRows[b].line })
	var missing []string
	var b strings.Builder
	b.WriteString("id\tpair\tlabel\tverdict\n")
	for _, id := range ids {
		c, ok := candidates[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		verdict := "no"
		if c.yes {
			verdict = "yes"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", id, c.pair, u.labelRows[id].label, verdict)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: IDs not found in window: %s", u.rescorePath, strings.Join(missing, ", "))
	}
	return u.save("rescore.tsv", b.String())
}

func (u *run) writeCandidates() error {
	rng := rand.New(rand.NewSource(int64(u.seed)))
	var b, v strings.Builder
	b.WriteString("id\tpair\tcommand\tcontext\n")
	v.WriteString("id\tpair\tverdict\n")
	sort.Slice(u.pairCandidates, func(i, j int) bool { return u.pairCandidates[i].id < u.pairCandidates[j].id })
	u.pairCandidateSample = nil
	for _, tool := range pairTools() {
		selected := make([]pairCandidate, 0, 60)
		for _, yes := range []bool{true, false} {
			a := []pairCandidate{}
			for i := range u.pairCandidates {
				c := &u.pairCandidates[i]
				if c.pair == tool && c.yes == yes {
					a = append(a, *c)
				}
			}
			rng.Shuffle(len(a), func(i, j int) { a[i], a[j] = a[j], a[i] })
			if len(a) > 30 {
				a = a[:30]
			}
			selected = append(selected, a...)
		}
		rng.Shuffle(len(selected), func(i, j int) { selected[i], selected[j] = selected[j], selected[i] })
		u.pairCandidateSample = append(u.pairCandidateSample, selected...)
		for i := range selected {
			c := &selected[i]
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", c.id, c.pair, u.clean(c.command), u.clean(c.context))
			verdict := "no"
			if c.yes {
				verdict = "yes"
			}
			fmt.Fprintf(&v, "%s\t%s\t%s\n", c.id, c.pair, verdict)
		}
	}
	if err := u.save("candidates.tsv", b.String()); err != nil {
		return err
	}
	return u.save("candidates-verdict.tsv", v.String())
}

// utilityCallCount — how many calls of tool a command makes. A wfor without conditions (-h, --help, flags
// only) prints usage and waits for nothing, so it is not a call: `wfor -h | head; wfor exit 1` is one call.
func utilityCallCount(calls []shellcmd.Call, tool string) int {
	n := 0
	for i := range calls {
		if calls[i].Name != tool {
			continue
		}
		if tool == "wfor" {
			if _, isCall := wforGoal(calls[i].Args); !isCall {
				continue
			}
		}
		n++
	}
	return n
}
