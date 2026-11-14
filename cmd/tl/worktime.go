package main

import (
	"fmt"
	"io"
	"maps"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/aiseeq/akit/transcript"
)

const defaultWorkGap = 15 * time.Minute

type interval struct{ start, end time.Time }

type sourceActivity struct {
	src     source
	times   []time.Time
	matched bool
}

// activitySegments closes each burst at its last event. A single event represents
// one minute of activity so an isolated short burst remains measurable.
func activitySegments(events []time.Time, gap time.Duration) []interval {
	slices.SortFunc(events, func(a, b time.Time) int { return a.Compare(b) })
	if len(events) == 0 {
		return nil
	}
	segments := make([]interval, 0, len(events))
	start, last := events[0], events[0]
	for _, at := range events[1:] {
		if at.Sub(last) > gap {
			end := last
			if end.Equal(start) {
				end = end.Add(time.Minute)
			}
			segments = append(segments, interval{start, end})
			start = at
		}
		last = at
	}
	end := last
	if end.Equal(start) {
		end = end.Add(time.Minute)
	}
	return append(segments, interval{start, end})
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

func worktimeEvents(src source, opts options, re *regexp.Regexp) ([]time.Time, bool, error) {
	var times []time.Time
	matched := false
	_, err := transcript.ReadWithTools(src.Path, nil, func(ev transcript.Event) {
		if ev.Time.IsZero() || (!opts.since.IsZero() && ev.Time.Before(opts.since)) ||
			(!opts.until.IsZero() && ev.Time.After(opts.until)) {
			return
		}
		switch ev.Role {
		case transcript.User, transcript.Agent, transcript.Cmd, transcript.Out:
		default:
			return
		}
		if re.MatchString(ev.Who + " " + ev.Shown()) {
			matched = true
		}
		times = append(times, ev.Time)
	})
	return times, matched, err
}

func eventBounds(events []time.Time) (interval, bool) {
	if len(events) == 0 {
		return interval{}, false
	}
	start, end := events[0], events[0]
	for _, at := range events[1:] {
		if at.Before(start) {
			start = at
		}
		if at.After(end) {
			end = at
		}
	}
	return interval{start, end.Add(time.Nanosecond)}, true
}

func collectActivities(sources []source, opts options, re *regexp.Regexp) ([]sourceActivity, map[string]interval, error) {
	activities := make([]sourceActivity, 0, len(sources))
	windows := map[string]interval{}
	for _, src := range sources {
		events, hasKey, readErr := worktimeEvents(src, opts, re)
		if readErr != nil {
			return nil, nil, fmt.Errorf("read %s: %w", src.Path, readErr)
		}
		if bounds, ok := eventBounds(events); ok {
			windows[src.ID] = bounds
		}
		activities = append(activities, sourceActivity{src: src, times: events, matched: hasKey})
	}
	for _, activity := range activities {
		if activity.src.ParentID == "" {
			continue
		}
		if _, ok := windows[activity.src.ParentID]; ok {
			continue
		}
		parentPath := filepath.Dir(filepath.Dir(activity.src.Path)) + ".jsonl"
		parent := transcript.NewSource(parentPath)
		if parent.ID != activity.src.ParentID {
			continue
		}
		events, _, readErr := worktimeEvents(source{Path: parentPath, ID: parent.ID}, opts, re)
		if readErr != nil {
			return nil, nil, fmt.Errorf("read parent %s: %w", parentPath, readErr)
		}
		if bounds, ok := eventBounds(events); ok {
			windows[parent.ID] = bounds
		}
	}
	return activities, windows, nil
}

func worktimeIntervals(activities []sourceActivity, windows map[string]interval, gap time.Duration) ([]interval, bool) {
	var intervals []interval
	matched := false
	for _, activity := range activities {
		if !activity.matched {
			continue
		}
		matched = true
		segments := activitySegments(activity.times, gap)
		if activity.src.ParentID != "" {
			if window, ok := windows[activity.src.ParentID]; ok {
				segments = clipIntervals(segments, window)
			}
		}
		intervals = append(intervals, segments...)
	}
	return intervals, matched
}

func reportWorktime(sources []source, opts options, w io.Writer) int {
	re, err := regexp.Compile("(?i)" + opts.grep)
	if err != nil {
		fmt.Fprintf(w, "tl: --grep: %v\n", err)
		return exitUsage
	}
	activities, windows, err := collectActivities(sources, opts, re)
	if err != nil {
		fmt.Fprintf(w, "tl: %v\n", err)
		return exitUsage
	}
	intervals, matched := worktimeIntervals(activities, windows, opts.gap)
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
			day := at.In(time.Local).Format(time.DateOnly)
			midnight := time.Date(at.In(time.Local).Year(), at.In(time.Local).Month(), at.In(time.Local).Day()+1, 0, 0, 0, 0, time.Local)
			end := span.end
			if midnight.Before(end) {
				end = midnight
			}
			byDay[day] += end.Sub(at)
			at = end
		}
	}
	var sum time.Duration
	for _, duration := range byDay {
		sum += duration
	}
	fmt.Fprintf(w, "total\t%s\n", jiraDuration(sum))
	days := slices.Sorted(maps.Keys(byDay))
	for _, day := range days {
		fmt.Fprintf(w, "%s\t%s\n", day, jiraDuration(byDay[day]))
	}
	return 0
}
