package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/waits"
)

type errorStat struct{ calls, fail, callerr, unknown, repeat int }

func (u *run) addError(tool, way string, code int, out string, toolErr bool) {
	k := tool + "\t" + way
	s := u.errorStats[k]
	if s == nil {
		s = &errorStat{}
		u.errorStats[k] = s
	}
	s.calls++
	if strings.Contains(out, " -h for usage)") {
		s.callerr++
		return
	}
	if code < 0 {
		if toolErr || u.explicitRefusal(tool, out) {
			s.fail++
			return
		}
		if u.explicitSuccess(tool, out) {
			return
		}
		s.unknown++
		return
	}
	if toolErr || code != 0 || u.explicitRefusal(tool, out) {
		if tool == "wfor" && code == 1 {
			return
		}
		if tool == "busy" && code == 1 {
			return
		}
		s.fail++
	}
}

func (u *run) addRepeat(tool, way string) {
	k := tool + "\t" + way
	s := u.errorStats[k]
	if s == nil {
		s = &errorStat{}
		u.errorStats[k] = s
	}
	s.repeat++
}

func (u *run) writeTkPair() error {
	var b strings.Builder
	b.WriteString("way\trepeats\tcalls\trate\twilson95\n")
	for _, name := range []string{"tk new", "tk other", "manual create", "manual other"} {
		var k, n int
		if c := u.tkPair[name]; c != nil {
			k, n = c[0], c[1]
		}
		rate := 0.0
		lo, hi := u.wilson(k, n)
		if n > 0 {
			rate = float64(k) / float64(n)
		}
		fmt.Fprintf(&b, "%s\t%d\t%d\t%.6f\t%.6f-%.6f\n", name, k, n, rate, lo, hi)
	}
	return u.save("tk-pair.tsv", b.String())
}

func (u *run) explicitSuccess(tool, out string) bool {
	switch tool {
	case "sub":
		return regexp.MustCompile(`(?m)^sub: (?:\d+ replacements? in \d+ files?|dry run, would make )`).MatchString(out)
	case "tl":
		return regexp.MustCompile(`(?m)^tl: \d+ event\(s\)`).MatchString(out)
	case "wfor":
		return strings.Contains(out, "wfor: done in")
	}
	return false
}

func (u *run) explicitRefusal(tool, out string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(line, tool+": ") {
			continue
		}
		l := strings.ToLower(line)
		for _, word := range []string{"nothing changed", "expected ", "no events", "not yet after", "empty ", "error", "invalid", "unknown", "failed", "refused", "blocked", "panic", "fatal"} {
			if strings.Contains(l, word) {
				return true
			}
		}
	}
	return false
}

func (u *run) shellWays(cmd string) map[string]bool {
	calls, err := shellcmd.Calls(cmd)
	if err != nil { //nolint:fallback-return,silent-error-handling // nested scripts can fail after yielding usable outer calls
		slog.Debug("parse shell ways", "error", err)
	}
	w := map[string]bool{}
	for _, c := range calls {
		if way := u.shellWay(c, cmd); way != "" {
			w[way] = true
		}
	}
	// Python/.replace may be recognized inside an embedded shell script, so inspect the full command too.
	l := strings.ToLower(cmd)
	if strings.Contains(l, ".replace(") && (strings.Contains(l, "write_text") || strings.Contains(l, ".write(")) {
		w[".replace"] = true
	}
	return w
}

func (u *run) shellWay(c shellcmd.Call, cmd string) string {
	low := strings.ToLower(c.Raw)
	if way := u.editShellWay(c, low); way != "" {
		return way
	}
	args := strings.ToLower(strings.Join(c.Args, " "))
	if way := u.transcriptShellWay(c, args, cmd); way != "" {
		return way
	}
	if u.isBusyProbe(c) {
		return "busy probe"
	}
	if u.isDecisionOrTaskSearch(c, low) {
		return "grep/rg decisions/tasks"
	}
	return ""
}

func (u *run) editShellWay(c shellcmd.Call, low string) string {
	if c.Name == "sed" && strings.Contains(low, "-i") {
		return "sed -i"
	}
	if c.Name == "perl" && strings.Contains(low, "-pi") {
		return "perl -pi"
	}
	if strings.Contains(low, ".replace(") && (strings.Contains(low, "write_text") || strings.Contains(low, ".write(") || strings.Contains(low, "open(")) {
		return ".replace"
	}
	return ""
}

func (u *run) transcriptShellWay(c shellcmd.Call, args, cmd string) string {
	if !u.transcriptArgs(args, strings.ToLower(cmd)) {
		return ""
	}
	if c.Name == "grep" || c.Name == "rg" || c.Name == "jq" {
		return c.Name + " transcript"
	}
	if c.Name == "sed" {
		return "sed transcript"
	}
	return ""
}

func (u *run) isBusyProbe(c shellcmd.Call) bool {
	return c.Name == "uptime" || c.Name == "top" || c.Name == "vmstat" || c.Name == "cat" && strings.Contains(strings.ToLower(c.Raw+" "+strings.Join(c.Args, " ")), "/proc/loadavg")
}

func (u *run) isDecisionOrTaskSearch(c shellcmd.Call, low string) bool {
	return (c.Name == "grep" || c.Name == "rg") && (u.decisionSearch(low) || strings.Contains(low, "/docs/tasks/") || strings.Contains(low, "docs/tasks/"))
}

func (u *run) decisionSearch(command string) bool {
	return u.home != "" && strings.Contains(command, strings.ToLower(filepath.Join(u.home, ".claude", "decisions.md")))
}

func (u *run) editRaw(e transcript.Event) (path, old, new string, err error) {
	var v struct {
		File string `json:"file_path"`
		Old  string `json:"old_string"`
		New  string `json:"new_string"`
	}
	if err := json.Unmarshal(e.Raw, &v); err != nil {
		return "", "", "", fmt.Errorf("decode edit event: %w", err)
	}
	return v.File, v.Old, v.New, nil
}

func (u *run) writeErrors() error {
	tools := u.errorToolNames()
	ways := map[string][]string{
		"sub":  {"new", "direct replacements", "Edit", "apply_patch"},
		"tl":   {"new", "transcript search"},
		"wfor": {"new", "habit-wait"}, "busy": {"new", "busy probe"}, "tk": {"new", "manual docs/tasks"},
		"prior": {"new", "grep/rg decisions/tasks"}, "user-word": {"new"}, "page-shot": {"new"},
		"webtext": {"new", "WebFetch"}, "claude-rules-export": {"new"},
	}
	var b, rb strings.Builder
	b.WriteString("tool\tway\tcalls\tfail\tcallerr\tunknown\tfail_share\n")
	rb.WriteString("tool\tway\tcalls\trepeat\trate\twilson95\n")
	countFor := u.countToolCalls
	for _, tool := range tools {
		for _, way := range ways[tool] {
			s := u.errorStats[tool+"\t"+way]
			if s == nil {
				s = &errorStat{}
			}
			if way == "new" {
				s.calls = countFor(tool)
			}
			if tool == "wfor" && way == "habit-wait" {
				s.calls = countFor("habit-wait")
			}
			if tool == "tk" && way == "manual docs/tasks" {
				s.calls = countFor("habit-tk")
			}
			if tool == "sub" && way == "direct replacements" {
				s.calls = countFor("habit-edit")
			}
			if tool == "tl" && way == "transcript search" {
				s.calls = countFor("habit-transcript")
			}
			den := s.calls - s.callerr - s.unknown
			fail := min(s.fail, den)
			share := "n/a"
			if den > 0 {
				share = fmt.Sprintf("%.6f", float64(fail)/float64(den))
			}
			fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%d\t%d\t%s\n", tool, way, s.calls, fail, s.callerr, s.unknown, share)
			// the common failure sign of a pair whose explicit failures do not compare
			if tool == "sub" && (way == "new" || way == "direct replacements") || tool == "tl" {
				lo, hi := u.wilson(s.repeat, s.calls)
				rate := 0.0
				if s.calls > 0 {
					rate = float64(s.repeat) / float64(s.calls)
				}
				fmt.Fprintf(&rb, "%s\t%s\t%d\t%d\t%.6f\t%.6f-%.6f\n", tool, way, s.calls, s.repeat, rate, lo, hi)
			}
		}
	}
	if err := u.save("errors.tsv", b.String()); err != nil {
		return err
	}
	return u.save("repeats.tsv", rb.String())
}

func (u *run) countToolCalls(tool string) int {
	n := 0
	for key, v := range u.counts {
		p := strings.Split(key, "\t")
		if len(p) == 4 && p[2] == tool && p[3] == "calls" {
			n += v
		}
	}
	return n
}

func (u *run) errorToolNames() []string {
	return append(u.utilityNames[7:], u.utilityNames[:7]...)
}

type editAttempt struct {
	tool, agent, file string
	bytes             int
	success           bool
}
type editSeries struct {
	tool            string
	bytes, attempts int
}

var ticketPathRE = regexp.MustCompile(`(?:[^\s"']*/)?docs/tasks/[^\s"']+\.md`)
var sourcePathRE = regexp.MustCompile(`(?:[A-Za-z0-9_./-]+\.(?:go|md|ts|tsx|js|jsx|json|yaml|yml|sh|txt|py))`)

func (u *run) hasDirectReplace(cmd string) bool {
	for w := range u.shellWays(cmd) {
		if w == "sed -i" || w == "perl -pi" || w == ".replace" {
			return true
		}
	}
	return false
}
func (u *run) replacementFiles(cmd string) []string {
	return u.unique(sourcePathRE.FindAllString(cmd, -1))
}
func (u *run) filesForSub(cmd string) ([]string, error) {
	calls, err := shellcmd.Find(cmd, "sub")
	var out []string
	for _, c := range calls {
		if len(c.Args) > 2 {
			out = append(out, c.Args[2:]...)
		}
		for _, m := range ticketPathRE.FindAllString(cmd, -1) {
			out = append(out, m)
		}
	}
	if err != nil {
		return u.unique(out), fmt.Errorf("find sub call: %w", err)
	}
	return u.unique(out), nil
}
func (u *run) unique(in []string) []string {
	m := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (u *run) recordEdit(at editAttempt) {
	if at.file != "" {
		u.editAttempts = append(u.editAttempts, at)
	}
}

func (u *run) writeSubCost() error {
	// A new file or agent ends the run; a successful operation closes and records one series.
	type key struct{ agent, file string }
	active := map[key]*editSeries{}
	per := map[string][]editSeries{}
	for _, op := range u.editAttempts {
		k := key{op.agent, op.file}
		for old := range active {
			if old.agent == op.agent && old.file != op.file {
				delete(active, old)
			}
		}
		s := active[k]
		if s == nil {
			s = &editSeries{tool: op.tool}
			active[k] = s
		}
		if s.tool != op.tool {
			delete(active, k)
			s = &editSeries{tool: op.tool}
			active[k] = s
		}
		s.bytes += op.bytes
		s.attempts++
		if op.success {
			per[s.tool] = append(per[s.tool], *s)
			delete(active, k)
		}
	}
	var b strings.Builder
	b.WriteString("tool\tsuccessful_edits\tmedian_input_bytes\tmean_input_bytes\tmean_attempts\n")
	for _, tool := range []string{"sub", "direct", "Edit"} {
		series := per[tool]
		sizes := make([]int, 0, len(series))
		sum, natt := 0, 0
		for _, s := range series {
			sizes = append(sizes, s.bytes)
			sum += s.bytes
			natt += s.attempts
		}
		sort.Ints(sizes)
		median := 0.0
		if len(sizes) > 0 {
			if len(sizes)%2 == 1 {
				median = float64(sizes[len(sizes)/2])
			} else {
				median = float64(sizes[len(sizes)/2-1]+sizes[len(sizes)/2]) / 2
			}
		}
		mean, attemptMean := 0.0, 0.0
		if len(series) > 0 {
			mean = float64(sum) / float64(len(series))
			attemptMean = float64(natt) / float64(len(series))
		}
		fmt.Fprintf(&b, "%s\t%d\t%.1f\t%.1f\t%.3f\n", tool, len(series), median, mean, attemptMean)
	}
	return u.save("sub-cost.tsv", b.String())
}

func (u *run) save(name, contents string) error {
	if err := os.WriteFile(filepath.Join(u.outputDir, name), []byte(contents), fileMode); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func (u *run) wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 0
	}
	z := 1.96
	p := float64(k) / float64(n)
	d := 1 + z*z/float64(n)
	c := (p + z*z/(2*float64(n))) / d
	r := z * math.Sqrt(p*(1-p)/float64(n)+z*z/(4*float64(n*n))) / d
	return c - r, c + r
}

func (u *run) waitOutput(w waits.Wait, outs map[string]transcript.Event) string {
	var b strings.Builder
	for _, id := range w.CallIDs {
		if o, ok := outs[id]; ok {
			b.WriteString(o.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (u *run) hasTranscriptSearch(cmd string) bool {
	for way := range u.shellWays(cmd) {
		if strings.HasSuffix(way, " transcript") {
			return true
		}
	}
	return false
}

func (u *run) transcriptSearchAgain(evs []transcript.Event, at int, agent, path string, models map[int]string, isClaude bool) bool {
	seen := 0
	for i := at + 1; i < len(evs) && seen < 3; i++ {
		e := evs[i]
		if e.Role != transcript.Cmd {
			continue
		}
		if u.agentOf(path, strings.Contains(path, "/subagents/"), u.modelAt(models, e.Line)) != agent {
			continue
		}
		seen++
		cmd, ok := u.callFor(e, isClaude)
		if ok && u.hasTranscriptSearch(cmd) {
			return true
		}
	}
	return false
}

func (u *run) editAgain(evs []transcript.Event, at int, agent, path string, models map[int]string, isClaude bool, file string) bool {
	seen := 0
	for i := at + 1; i < len(evs) && seen < 3; i++ {
		e := evs[i]
		if e.Role != transcript.Cmd {
			continue
		}
		if u.agentOf(path, strings.Contains(path, "/subagents/"), u.modelAt(models, e.Line)) != agent {
			continue
		}
		seen++
		cmd, ok := u.callFor(e, isClaude)
		if !ok {
			if e.Who == "Edit" || e.Who == "Write" || e.Who == "MultiEdit" || strings.Contains(e.Text, "apply_patch") {
				cmd = e.Text
				ok = true
			}
		}
		files, err := u.filesForSub(cmd)
		if err != nil { //nolint:fallback-return,silent-error-handling // retain shell calls parsed before a nested script error
			slog.Debug("find files in following call", "error", err)
		}
		if ok && (len(files) > 0 || u.hasDirectReplace(cmd) || strings.Contains(cmd, "apply_patch")) && u.sameFiles(file, cmd) {
			return true
		}
	}
	return false
}

func (u *run) repeatedEdit(evs []transcript.Event, at int, agent, path string, models map[int]string, isClaude bool, files []string) bool {
	for _, file := range files {
		if u.editAgain(evs, at, agent, path, models, isClaude, file) {
			return true
		}
	}
	return false
}
