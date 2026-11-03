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

	"strings"
	"time"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/transcript"
)

const pairToolNames = "busy prior user-word page-shot claude-rules-export wfor"
const pairCode2Tools = "busy prior user-word wfor claude-rules-export"
const candidateContextRunes = 600

type pairPatterns struct {
	busyProbe, searchTool, taskWrite, copyTool, heavyCommand, spaces *regexp.Regexp
	waitGoals                                                        []*regexp.Regexp
	tkKindClean, tkPath, tkTicketID                                  *regexp.Regexp
}

func newPairPatterns() pairPatterns {
	return pairPatterns{
		busyProbe:    regexp.MustCompile(`\b(uptime|w|top|vmstat|mpstat|loadavg|pressure)\b`),
		searchTool:   regexp.MustCompile(`\b(grep|rg|jq|sed|tl)\b`),
		taskWrite:    regexp.MustCompile(`\b(cat|tee|sed|perl|python|apply_patch)\b`),
		copyTool:     regexp.MustCompile(`\b(cp|rsync|install)\b`),
		heavyCommand: regexp.MustCompile(`\b(graft\s+(gate|test|commit|install|release)|graft\s+exec\s+--heavy|go\s+(test|build)|make|docker\s+build|podman\s+build|worker\s+run|(npm|pnpm|yarn)\s+(run\s+)?(test|build))\b`),
		spaces:       regexp.MustCompile(`\s+`),
		waitGoals: []*regexp.Regexp{
			regexp.MustCompile(`(?:test\s+-[ef]\s+|\[\[?\s+-[ef]\s+)([^]\s;]+)`),
			regexp.MustCompile(`curl\s+-\S+\s+([^\s;]+)`), regexp.MustCompile(`kill\s+-0\s+(\d+)`),
			regexp.MustCompile(`pgrep\s+(?:-\S+\s+)*([^\s;]+)`), regexp.MustCompile(`grep\s+-q\s+.*?\s+([^\s;]+)`),
			regexp.MustCompile(`ls\s+([^\s;]+)`),
		},
		tkKindClean: regexp.MustCompile(`[0-9]+|[^\p{L}\w -]`),
		tkPath:      regexp.MustCompile(`(?:^|\s)(?:/\S+|\S*\.md)(?:\s|$)`),
		tkTicketID:  regexp.MustCompile(`\b[0-9]{8}-[0-9]{4}-[A-Za-z0-9-]+\b`),
	}
}

func (u *run) isPairBusyProbe(c shellcmd.Call) bool {
	l := strings.ToLower(c.Raw + " " + strings.Join(c.Args, " "))
	return c.Name == "uptime" || c.Name == "w" || c.Name == "top" && strings.Contains(l, "-b") && (strings.Contains(l, "-n1") || strings.Contains(l, "-n 1")) && !strings.Contains(l, "-p") && !strings.Contains(l, "-h") || c.Name == "vmstat" || c.Name == "mpstat" || c.Name == "cat" && (strings.Contains(l, "/proc/loadavg") || strings.Contains(l, "/proc/pressure/"))
}

func pairTools() []string { return strings.Fields(pairToolNames) }

type pairCall struct {
	tool, way, goal, agent, session, id, command, out, outcome, period string
	at                                                                 time.Time
	code                                                               int
	instructed                                                         bool
	codeNotOwned                                                       bool
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

func (u *run) loadReleases() (map[string]releaseDate, error) {
	m := map[string]releaseDate{}
	if u.releasesPath == "" {
		return m, nil
	}
	f, err := os.Open(u.releasesPath)
	if err != nil {
		return nil, fmt.Errorf("open releases: %w", err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	line := 0
	for s.Scan() {
		line++
		p := strings.Split(s.Text(), "\t")
		if len(p) != 3 || !contains(pairTools(), p[0]) {
			return nil, &uptakeInputError{message: fmt.Sprintf("%s:%d: expected known tool and two RFC3339 dates", u.releasesPath, line)}
		}
		d := releaseDate{}
		for i, v := range p[1:] {
			if v == "" {
				continue
			}
			t, e := time.Parse(time.RFC3339, v)
			if e != nil {
				return nil, &uptakeInputError{message: fmt.Sprintf("%s:%d: invalid date", u.releasesPath, line)}
			}
			if i == 0 {
				d.available = t
			} else {
				d.instructed = t
			}
		}
		if _, ok := m[p[0]]; ok {
			return nil, &uptakeInputError{message: fmt.Sprintf("%s:%d: duplicate tool", u.releasesPath, line)}
		}
		m[p[0]] = d
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read releases: %w", err)
	}
	for _, tool := range pairTools() {
		if _, ok := m[tool]; !ok {
			return nil, &uptakeInputError{message: fmt.Sprintf("%s:1: missing %s", u.releasesPath, tool)}
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
		u.pairOverlap = append(u.pairOverlap, pairOverlap{id: fmt.Sprintf("%s:%d", path, e.Line), tool: "tl"})
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
	id := fmt.Sprintf("%s:%d", path, e.Line)
	pc := pairCall{tool: tool, way: "new", goal: goal, agent: agent, session: path, id: id, command: cmd, out: out, at: e.Time, code: code, codeNotOwned: baseCode >= 0 && code < 0, instructed: strings.Contains(strings.ToLower(firstUser), strings.ToLower(tool))}
	pc.outcome = pairOutcome(pc)
	u.pairCalls = append(u.pairCalls, pc)
}

func (u *run) collectPairOld(path, agent, firstUser string, evs []transcript.Event, outs map[string]transcript.Event, sc shellcmd.Call, cmd string, i int, e *transcript.Event) {
	way, goal := u.oldPair(sc, cmd)
	if way == "" {
		return
	}
	if way == "busy" {
		goal = u.busyGoal(evs, i, cmd, sc)
		if goal == "" {
			u.busyNoDecision++
			return
		}
	}
	id := fmt.Sprintf("%s:%d", path, e.Line)
	if way == "prior" {
		if sc.Name == "tl" {
			u.priorTLCalls++
		} else {
			u.priorImproveCalls++
		}
	}
	u.pairCalls = append(u.pairCalls, pairCall{tool: way, way: "old", goal: goal, agent: agent, session: path, id: id, command: cmd, out: u.outFor(*e, outs), at: e.Time, code: u.eventExitCode(outs[e.CallID]), instructed: strings.Contains(strings.ToLower(firstUser), way)})
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
		return lastArg(c.Args)
	case "page-shot":
		return pageGoal(c.Args)
	case "claude-rules-export":
		return firstPositional(c.Args)
	default:
		return ""
	}
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
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
			if contains([]string{"at", "out", "element", "timeout"}, name) {
				if !strings.Contains(arg, "=") {
					i++
				}
			}
			continue
		}
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

func firstPositional(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
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
	usage := strings.Contains(c.out, "("+c.tool+" -h for usage)")
	if usage || c.code == 2 && contains(strings.Fields(pairCode2Tools), c.tool) {
		return "callerr"
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
	case "page-shot":
		return strings.Contains(out, ".png") || lineHas("page-shot: ")
	}
	return false
}

func (u *run) pairUtilityCode(tool string, calls []shellcmd.Call, at, code int, out string) int {
	if strings.Contains(out, "("+tool+" -h for usage)") {
		count := 0
		for i := range calls {
			if calls[i].Name == tool {
				count++
			}
		}
		if count == 1 {
			return code
		}
		return -1
	}
	if at < 0 || at >= len(calls) || at != len(calls)-1 || calls[at].Name != tool {
		return -1
	}
	for i := range calls {
		if i != at && calls[i].Name == tool {
			return -1
		}
	}
	return code
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
	if u.isPairBusyProbe(c) {
		return "busy", ""
	}
	if tool, goal := u.oldSearchPair(c, cmd); tool != "" {
		return tool, goal
	}
	if goal := u.waitPairGoal(c, cmd); goal != "" {
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

func (u *run) oldSearchPair(c shellcmd.Call, cmd string) (string, string) {
	line := strings.ToLower(c.Raw + " " + strings.Join(c.Args, " "))
	name := strings.ToLower(c.Name)
	if priorSearch(name, line) {
		return "prior", searchGoal(c)
	}
	if transcriptSearch(u, c, cmd, name, line) {
		sample := searchGoal(c)
		if searchPhrase(sample) {
			return "user-word", strings.ToLower(sample)
		}
	}
	return "", ""
}

func priorSearch(name, line string) bool {
	return (name == "grep" || name == "rg") && strings.Contains(line, "/.claude/improve/") ||
		name == "tl" && strings.Contains(line, "--role user") && strings.Contains(line, "--grep")
}

func transcriptSearch(u *run, c shellcmd.Call, cmd, name, line string) bool {
	search := name == "grep" || name == "rg" || name == "jq" || name == "sed"
	return u.transcriptArgs(strings.ToLower(strings.Join(c.Args, " ")), strings.ToLower(cmd)) && search ||
		name == "tl" && strings.Contains(line, "--grep")
}

func searchPhrase(sample string) bool {
	return hasLetters(sample) && (len(strings.Fields(sample)) >= 2 || len([]rune(sample)) >= 12) &&
		!strings.Contains(sample, ".type") && sample != "keys"
}

func (u *run) waitPairGoal(c shellcmd.Call, cmd string) string {
	if c.Name != "sleep" && !(strings.Contains(strings.ToLower(cmd), "while ") && strings.Contains(strings.ToLower(cmd), "sleep")) {
		return ""
	}
	return u.waitGoal(cmd)
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

func (u *run) exportGoal(c shellcmd.Call) string {
	if c.Name != "cp" && c.Name != "rsync" && c.Name != "install" {
		return ""
	}
	line := strings.ToLower(c.Raw + " " + strings.Join(c.Args, " "))
	if strings.Contains(line, "work/claude-rules") {
		return lastArg(c.Args)
	}
	return ""
}

func searchGoal(c shellcmd.Call) string {
	for _, a := range c.Args {
		if !strings.HasPrefix(a, "-") && !strings.Contains(a, ".jsonl") && !strings.Contains(a, ".md") {
			return strings.ToLower(a)
		}
	}
	return "search"
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
	for pair := range candidates {
		eventID := fmt.Sprintf("%s:%d", path, evs[i].Line)
		_, yes := u.pairOldIDs[eventID][pair]
		id := pair + ":" + eventID
		context := u.candidateContext(evs, i)
		u.pairCandidates = append(u.pairCandidates, pairCandidate{id: id, pair: pair, command: truncateRunes(cmd, 300), context: context, yes: yes, agent: agent})
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
	if strings.Contains(l, "docs/tasks/") {
		candidates["tk"] = u.pairPatterns.taskWrite.MatchString(l)
	}
	if u.pairPatterns.copyTool.MatchString(l) {
		candidates["claude-rules-export"] = strings.Contains(l, "work/claude-rules")
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
	return truncateRunes(strings.Join(lines, " | "), candidateContextRunes)
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
		msg := strings.TrimPrefix(line, "tk: ")
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

func (u *run) loadPairLabels() error {
	if u.labelsPath == "" {
		return nil
	}
	f, err := os.Open(u.labelsPath)
	if err != nil {
		return fmt.Errorf("open labels: %w", err)
	}
	defer f.Close()
	validIDs := make(map[string]struct{}, len(u.pairCandidateSample))
	for i := range u.pairCandidateSample {
		validIDs[u.pairCandidateSample[i].id] = struct{}{}
	}
	labels := map[string]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		fields := strings.Split(scanner.Text(), "\t")
		if line == 1 {
			if len(fields) != 2 || fields[0] != "id" || fields[1] != "label" {
				return &uptakeInputError{message: fmt.Sprintf("%s:1: expected id<TAB>label header", u.labelsPath)}
			}
			continue
		}
		if len(fields) != 2 || fields[0] == "" || (fields[1] != "yes" && fields[1] != "no") {
			return &uptakeInputError{message: fmt.Sprintf("%s:%d: expected candidate id and yes|no label", u.labelsPath, line)}
		}
		if _, ok := labels[fields[0]]; ok {
			return &uptakeInputError{message: fmt.Sprintf("%s:%d: duplicate id %q", u.labelsPath, line, fields[0])}
		}
		if _, ok := validIDs[fields[0]]; !ok {
			return &uptakeInputError{message: fmt.Sprintf("%s:%d: unknown candidate id %q", u.labelsPath, line, fields[0])}
		}
		labels[fields[0]] = fields[1]
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read labels: %w", err)
	}
	if line == 0 {
		return &uptakeInputError{message: fmt.Sprintf("%s:1: missing id<TAB>label header", u.labelsPath)}
	}
	missing := 0
	for id := range validIDs {
		if _, ok := labels[id]; !ok {
			missing++
		}
	}
	if missing > 0 {
		return &uptakeInputError{message: fmt.Sprintf("%s:2: %d sampled candidates have no label; label the whole candidates.tsv", u.labelsPath, missing)}
	}
	u.labels = labels
	return nil
}

func (u *run) writePairTables() error {
	releases, err := u.loadReleases()
	if err != nil {
		return err
	}
	for i := range u.pairCalls {
		c := &u.pairCalls[i]
		d := releases[c.tool]
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
	if err := u.loadPairLabels(); err != nil {
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
			out = truncateRunes(out, 300)
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
		fmt.Fprintf(&b, "%s\t%s\t%d\t%s\t%s\t%s\n", c.tool, c.id, c.code, escapePairField(truncateRunes(c.command, 300)), escapePairField(truncateRunes(c.out, 2000)), reason)
	}
	u.pairUnknownReasons = counts
	return u.save("pair-unknown.tsv", b.String())
}

func pairUnknownReason(c pairCall) string {
	// 2>&1 merges stderr into the visible output, so only other redirects hide it.
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
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		u.writePairRow(&b, key, counts[key], calls)
	}
	return u.save("pairs.tsv", b.String())
}

func (u *run) writePairRow(b *strings.Builder, key string, count *epCount, calls map[pairCallCountKey]int) {
	parts := strings.Split(key, "\t")
	for _, way := range []string{"new", "old"} {
		ncalls := calls[pairCallCountKey{parts[0], parts[1], parts[2], parts[3], way}]
		total := count.new + count.old
		share := 0.0
		if total > 0 {
			share = float64(count.new) / float64(total)
		}
		low, high := u.wilson(count.new, total)
		gate := u.pairLabelGate(parts[0])
		shareText, interval := "-", "-"
		if gate == "ok" {
			shareText = fmt.Sprintf("%.6f", share)
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
	lowN := stat.n - stat.unknown
	low, high, known := outcomeBounds(stat, lowN)
	wilsonLow, wilsonHigh := u.wilson(stat.fail+stat.callerr, lowN)
	fmt.Fprintf(b, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.6f\t%.6f\t%.6f\t%.6f-%.6f\n", tool, way, stat.n, stat.answer, stat.callerr, stat.fail, stat.unknown, stat.empty, stat.notyet, low, high, known, wilsonLow, wilsonHigh)
}

func outcomeBounds(stat *pairOutcomeStat, knownN int) (float64, float64, float64) {
	low, high, known := 0.0, 0.0, 0.0
	fails := stat.fail + stat.callerr
	if stat.n > 0 {
		low = float64(fails) / float64(stat.n)
		high = float64(fails+stat.unknown) / float64(stat.n)
	}
	if knownN > 0 {
		known = float64(fails) / float64(knownN)
	}
	return low, high, known
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

func countPairRepeats(groups map[pairRepeatKey][]pairCall) int {
	total := 0
	for _, calls := range groups {
		sort.Slice(calls, func(i, j int) bool { return calls[i].at.Before(calls[j].at) })
		total += countGroupRepeats(calls)
	}
	return total
}

func countGroupRepeats(calls []pairCall) int {
	count := 0
	for i := range calls {
		if hasDifferentRepeat(calls, i) {
			count++
		}
	}
	return count
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
		names := []string{}
		for n := range tools {
			names = append(names, n)
		}
		sort.Strings(names)
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				counts[names[i]+"\t"+names[j]]++
			}
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
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
