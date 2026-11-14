package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/akit/transcript"
)

const (
	defaultWorkGap  = 15 * time.Minute
	defaultKeyGap   = 30 * time.Minute
	maxWorktimeLine = 256 << 20
	maxWorktimeYear = 9999
)

type interval struct{ start, end time.Time }

type sourceActivity struct {
	src      source
	events   []time.Time
	keyTimes []time.Time
	loaded   bool
}

func isActivityEvent(role transcript.Role) bool {
	switch role {
	case transcript.User, transcript.Ask, transcript.Peer, transcript.Agent, transcript.Cmd, transcript.Out, transcript.Note:
		return true
	default:
		return false
	}
}

func isKeyEvent(role transcript.Role) bool {
	switch role {
	case transcript.User, transcript.Agent, transcript.Cmd, transcript.Note, transcript.Peer:
		return true
	default:
		return false
	}
}

// activitySegments bounds each burst by its event times and gives every burst at least one minute.
func activitySegments(events []time.Time, gap time.Duration) []interval {
	slices.SortFunc(events, func(a, b time.Time) int { return a.Compare(b) })
	if len(events) == 0 {
		return nil
	}
	segments := make([]interval, 0, len(events))
	start, last := events[0], events[0]
	closeSegment := func() {
		end := last
		if end.Sub(start) < time.Minute {
			end = start.Add(time.Minute)
		}
		segments = append(segments, interval{start, end})
	}
	for _, at := range events[1:] {
		if at.Sub(last) > gap {
			closeSegment()
			start = at
		}
		last = at
	}
	closeSegment()
	return segments
}

func unionIntervals(in []interval) []interval {
	if len(in) == 0 {
		return nil
	}
	slices.SortFunc(in, func(a, b interval) int { return a.start.Compare(b.start) })
	out := []interval{in[0]}
	for _, cur := range in[1:] {
		last := &out[len(out)-1]
		if !cur.start.After(last.end) {
			if cur.end.After(last.end) {
				last.end = cur.end
			}
		} else {
			out = append(out, cur)
		}
	}
	return out
}

func clipIntervals(in []interval, window interval) []interval {
	out := make([]interval, 0, len(in))
	for _, span := range in {
		if span.start.Before(window.start) {
			span.start = window.start
		}
		if span.end.After(window.end) {
			span.end = window.end
		}
		if span.start.Before(span.end) {
			out = append(out, span)
		}
	}
	return out
}

func roundedWork(d time.Duration) time.Duration {
	const quantum = 15 * time.Minute
	if d <= 0 {
		return 0
	}
	return time.Duration(math.Ceil(float64(d)/float64(quantum))) * quantum
}

func jiraDuration(d time.Duration) string {
	minutes := int(roundedWork(d).Minutes())
	hours, mins := minutes/60, minutes%60
	if hours == 0 {
		return fmt.Sprintf("%dm", mins)
	}
	if mins == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh %dm", hours, mins)
}

func rawLineMatches(raw []byte, re *regexp.Regexp, literals []string) bool {
	if len(literals) > 0 && !strings.Contains(strings.Join(literals, ""), "\n") {
		candidate := false
		for _, literal := range literals {
			if hasFolded(raw, literal) {
				candidate = true
				break
			}
		}
		if !candidate {
			return false
		}
	}
	return re.Match(raw)
}

func readKeyTimes(src source, re *regexp.Regexp, literals []string) ([]time.Time, error) {
	var times []time.Time
	_, err := transcript.ReadWithTools(src.Path, func(raw []byte, _ transcript.ToolNames) bool {
		return rawLineMatches(raw, re, literals)
	}, func(ev transcript.Event) {
		if !ev.Time.IsZero() && isKeyEvent(ev.Role) && re.MatchString(ev.Who+" "+ev.Shown()) {
			times = append(times, ev.Time)
		}
	})
	return times, err
}

func readActivityTimes(src source) ([]time.Time, error) {
	if isCodexRollout(src.Path) {
		var times []time.Time
		_, err := transcript.ReadWithTools(src.Path, nil, func(ev transcript.Event) {
			if !ev.Time.IsZero() && isActivityEvent(ev.Role) {
				times = append(times, ev.Time)
			}
		})
		return times, err
	}
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxWorktimeLine)
	var times []time.Time
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		var row struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("decode transcript line %d: %w", line, err)
		}
		active := row.Type == "user" && row.Timestamp != ""
		if row.Type == "assistant" {
			var parts []struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(row.Message.Content, &parts); err != nil {
				return nil, fmt.Errorf("decode assistant content at line %d: %w", line, err)
			}
			for _, part := range parts {
				if part.Type == "text" || part.Type == "tool_use" {
					active = true
					break
				}
			}
		}
		if active && row.Timestamp != "" {
			at, parseErr := time.Parse(time.RFC3339Nano, row.Timestamp)
			if parseErr != nil {
				return nil, fmt.Errorf("parse timestamp at line %d: %w", line, parseErr)
			}
			times = append(times, at)
		}
	}
	readErr, closeErr := scanner.Err(), f.Close()
	if readErr != nil {
		return nil, fmt.Errorf("scan transcript: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close transcript: %w", closeErr)
	}
	return times, nil
}

func worktimePrefilter(pattern string) ([]string, error) {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("parse regexp syntax: %w", err)
	}
	var best []string
	bestMin := 0
	for _, group := range requiredLiterals(parsed) {
		if len(group) == 0 {
			continue
		}
		shortest := len(group[0])
		for _, literal := range group[1:] {
			if len(literal) < shortest {
				shortest = len(literal)
			}
		}
		if shortest > bestMin {
			best, bestMin = group, shortest
		}
	}
	return best, nil
}

func worktimePattern(pattern string) string {
	if strings.ContainsAny(pattern, `.+*?()|[]{}^$\\`) {
		return pattern
	}
	return `\b` + pattern + `\b`
}

func keyWindows(activity sourceActivity, keyGap time.Duration) []interval {
	if len(activity.keyTimes) == 0 {
		return nil
	}
	keys := append([]time.Time(nil), activity.keyTimes...)
	events := append([]time.Time(nil), activity.events...)
	slices.SortFunc(keys, func(a, b time.Time) int { return a.Compare(b) })
	slices.SortFunc(events, func(a, b time.Time) int { return a.Compare(b) })
	var windows []interval
	eventIndex := 0
	start, lastKey := keys[0], keys[0]
	appendWindow := func(from, through time.Time) {
		limit := through.Add(keyGap)
		for eventIndex < len(events) && events[eventIndex].Before(from) {
			eventIndex++
		}
		lastActivity := from
		for eventIndex < len(events) && !events[eventIndex].After(limit) {
			lastActivity = events[eventIndex]
			eventIndex++
		}
		end := lastActivity
		if end.Equal(from) {
			end = from.Add(time.Minute)
		}
		windows = append(windows, interval{from, end})
	}
	for _, at := range keys[1:] {
		if at.Sub(lastKey) > keyGap {
			appendWindow(start, lastKey)
			start = at
		}
		lastKey = at
	}
	appendWindow(start, lastKey)

	return windows
}

func containsTime(windows []interval, at time.Time) bool {
	for _, window := range windows {
		if !at.Before(window.start) && !at.After(window.end) {
			return true
		}
	}
	return false
}

func collectActivities(sources []source, re *regexp.Regexp, literals []string, keyGap time.Duration, stderr io.Writer) (map[string]sourceActivity, map[string][]interval, error) {
	activities := make(map[string]sourceActivity, len(sources))
	matchedParent := make(map[string]bool, len(sources))
	keyTimes, keyErrors := readKeyTimesParallel(sources, re, literals)
	for i, src := range sources {
		if keyErrors[i] != nil {
			return nil, nil, fmt.Errorf("read %s: %w", src.Path, keyErrors[i])
		}
		activities[src.ID] = sourceActivity{src: src, keyTimes: keyTimes[i]}
		matchedParent[src.ID] = len(keyTimes[i]) > 0
	}
	if err := loadMissingParentKeys(sources, activities, matchedParent, re, literals, stderr); err != nil {
		return nil, nil, err
	}
	if err := loadSelectedActivityEvents(sources, activities, matchedParent); err != nil {
		return nil, nil, err
	}
	parentWindows := matchedParentWindows(activities, matchedParent, keyGap)
	return activities, parentWindows, nil
}

func readKeyTimesParallel(sources []source, re *regexp.Regexp, literals []string) ([][]time.Time, []error) {
	times := make([][]time.Time, len(sources))
	errs := make([]error, len(sources))
	if len(sources) == 0 {
		return times, errs
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(scanWorkerCount(), len(sources)) {
		wg.Go(func() {
			for i := range jobs {
				times[i], errs[i] = readKeyTimes(sources[i], re, literals)
			}
		})
	}
	for i := range sources {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return times, errs
}

func loadMissingParentKeys(sources []source, activities map[string]sourceActivity, matched map[string]bool,
	re *regexp.Regexp, literals []string, stderr io.Writer) error {
	warned := map[string]bool{}
	for _, src := range sources {
		if src.ParentID == "" || len(activities[src.ID].keyTimes) == 0 {
			continue
		}
		if _, present := activities[src.ParentID]; present {
			continue
		}
		parentPath := filepath.Dir(filepath.Dir(src.Path)) + ".jsonl"
		parent := transcript.NewSource(parentPath)
		if parent.ID != src.ParentID {
			continue
		}
		if _, err := os.Stat(parentPath); err != nil {
			if os.IsNotExist(err) {
				if !warned[parentPath] {
					fmt.Fprintf(stderr, "tl: warning: parent transcript %s is missing; subagent uses its own key window\n", parentPath)
					warned[parentPath] = true
				}
				continue
			}
			return fmt.Errorf("stat parent %s: %w", parentPath, err)
		}
		parentSrc := source{Path: parentPath, ID: parent.ID}
		keyTimes, err := readKeyTimes(parentSrc, re, literals)
		if err != nil {
			return fmt.Errorf("read parent %s: %w", parentPath, err)
		}
		activities[parent.ID] = sourceActivity{src: parentSrc, keyTimes: keyTimes}
		matched[parent.ID] = len(keyTimes) > 0
	}
	return nil
}

func loadSelectedActivityEvents(sources []source, activities map[string]sourceActivity, matched map[string]bool) error {
	load := func(id string) error {
		activity := activities[id]
		if activity.loaded {
			return nil
		}
		events, err := readActivityTimes(activity.src)
		if err != nil {
			return fmt.Errorf("read activity %s: %w", activity.src.Path, err)
		}
		activity.events, activity.loaded = events, true
		activities[id] = activity
		return nil
	}
	for _, src := range sources {
		if len(activities[src.ID].keyTimes) > 0 || matched[src.ParentID] {
			if err := load(src.ID); err != nil {
				return err
			}
		}
	}
	for id, hasKey := range matched {
		if hasKey {
			if err := load(id); err != nil {
				return err
			}
		}
	}
	return nil
}

func matchedParentWindows(activities map[string]sourceActivity, matched map[string]bool, keyGap time.Duration) map[string][]interval {
	windows := make(map[string][]interval)
	for id, hasKey := range matched {
		if hasKey {
			windows[id] = keyWindows(activities[id], keyGap)
		}
	}
	return windows
}

func worktimeIntervals(activities map[string]sourceActivity, windows map[string][]interval,
	gap, keyGap time.Duration, timeWindow interval) ([]interval, bool) {
	var intervals []interval
	matched := false
	for _, id := range slices.Sorted(maps.Keys(activities)) {
		activity := activities[id]
		ownWindows := keyWindows(activity, keyGap)
		parentWindows := windows[activity.src.ParentID]
		launchedInParent := containsTime(parentWindows, firstTime(activity.events))
		if len(ownWindows) == 0 && !launchedInParent {
			continue
		}
		matched = true
		segments := activitySegments(append([]time.Time(nil), activity.events...), gap)
		if len(ownWindows) > 0 {
			for _, ownWindow := range ownWindows {
				intervals = append(intervals, clipIntervals(clipIntervals(segments, ownWindow), timeWindow)...)
			}
		}
		if launchedInParent && len(activity.events) > 0 {
			intervals = append(intervals, clipIntervals(segments, timeWindow)...)
		}
	}
	return intervals, matched
}

func firstTime(events []time.Time) time.Time {
	if len(events) == 0 {
		return time.Time{}
	}
	first := events[0]
	for _, at := range events[1:] {
		if at.Before(first) {
			first = at
		}
	}
	return first
}

func reportWorktime(sources []source, opts options, e env, w, stderr io.Writer) int {
	workRE, err := regexp.Compile("(?i)" + worktimePattern(opts.grep))
	if err != nil {
		fmt.Fprintf(stderr, "tl: --grep: %v\n", err)
		return exitUsage
	}
	literals, err := worktimePrefilter(worktimePattern(opts.grep))
	if err != nil {
		fmt.Fprintf(stderr, "tl: parse --grep for prefilter: %v\n", err)
		return exitUsage
	}
	activities, parentWindows, err := collectActivities(sources, workRE, literals, opts.keyGap, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "tl: %v\n", err)
		return exitUsage
	}
	since := opts.since
	if since.IsZero() {
		since = e.now.Add(-sinceDefault)
	}
	until := opts.until
	if until.IsZero() {
		until = time.Date(maxWorktimeYear, time.December, 31, 23, 59, 59, 999999999, time.Local)
	}
	intervals, matched := worktimeIntervals(activities, parentWindows, opts.gap, opts.keyGap, interval{since, until})
	if !matched {
		fmt.Fprintln(w, "tl: no sessions with matching key")
		return exitNone
	}
	merged := unionIntervals(intervals)
	if len(merged) == 0 {
		fmt.Fprintln(w, "tl: no activity in the selected time window")
		return exitNone
	}
	byDay := map[string]time.Duration{}
	for _, span := range merged {
		for at := span.start; at.Before(span.end); {
			local := at.In(time.Local)
			day := local.Format(time.DateOnly)
			midnight := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, time.Local)
			end := span.end
			if midnight.Before(end) {
				end = midnight
			}
			byDay[day] += end.Sub(at)
			at = end
		}
	}
	days := slices.Sorted(maps.Keys(byDay))
	var total time.Duration
	for _, day := range days {
		daily := roundedWork(byDay[day])
		total += daily
		fmt.Fprintf(w, "%s\t%s\n", day, jiraDuration(byDay[day]))
	}
	fmt.Fprintf(w, "total\t%s\n", jiraDuration(total))
	return 0
}
