package main

import (
	"slices"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

func TestContextIndexesUsesFilteredFileOrderAndMergesWindows(t *testing.T) {
	rows := []sortedHit{
		{hit: hit{src: source{Path: "/b.jsonl"}, ev: transcript.Event{Line: 4}}, seq: 2, matched: true},
		{hit: hit{src: source{Path: "/a.jsonl"}, ev: transcript.Event{Line: 8}}, seq: 1},
		{hit: hit{src: source{Path: "/b.jsonl"}, ev: transcript.Event{Line: 1}}, seq: 1},
		{hit: hit{src: source{Path: "/a.jsonl"}, ev: transcript.Event{Line: 8}}, seq: 2, matched: true},
		{hit: hit{src: source{Path: "/b.jsonl"}, ev: transcript.Event{Line: 9}}, seq: 3},
		{hit: hit{src: source{Path: "/a.jsonl"}, ev: transcript.Event{Line: 3}}, seq: 1},
		{hit: hit{src: source{Path: "/b.jsonl"}, ev: transcript.Event{Line: 1}}, seq: 0},
	}
	if got, want := contextIndexes(rows, 1), []int{0, 1, 2, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("context indexes = %v, want %v", got, want)
	}
}

func TestContextWindowHugeRadiusAndSharedEventSequence(t *testing.T) {
	sequence := []bool{false, false, false, true, false, false, false, true, false, false}
	shared := contextWindow(sequence, 2)
	stream := contextStream{src: source{Path: "synthetic.jsonl"}, radius: 2}
	for i, matched := range sequence {
		stream.add(transcript.Event{Line: i + 1}, matched)
	}
	streamSelected := make([]bool, len(sequence))
	for _, h := range stream.hits() {
		streamSelected[h.ev.Line-1] = true
	}
	if !slices.Equal(streamSelected, shared) {
		t.Fatalf("file stream = %v, database window = %v", streamSelected, shared)
	}
	all := make([]bool, len(sequence))
	all[2] = true
	wantAll := make([]bool, len(sequence))
	for i := range wantAll {
		wantAll[i] = true
	}
	if got := contextWindow(all, int(^uint(0)>>1)); !slices.Equal(got, wantAll) {
		t.Fatalf("max-int radius overflowed: %v", got)
	}
	if got := contextWindow(make([]bool, len(sequence)), int(^uint(0)>>1)); !slices.Equal(got, make([]bool, len(sequence))) {
		t.Fatalf("no-match window retained events: %v", got)
	}
}

func TestContextWindowManyOverlappingMatchesIsLinear(t *testing.T) {
	const count = 250_000
	matches := make([]bool, count)
	for i := range matches {
		matches[i] = true
	}
	started := time.Now()
	got := contextWindow(matches, count/2)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("context selection took %s for %d overlapping matches", elapsed, count)
	}
	if len(got) != count {
		t.Fatalf("got %d rows, want %d", len(got), count)
	}
	for i, selected := range got {
		if !selected {
			t.Fatalf("row %d was not selected", i)
		}
	}
}

func TestContextStreamKeepsOnlyRadiusBeforeFirstMatch(t *testing.T) {
	const count, radius = 500_000, 64
	stream := contextStream{src: source{Path: "synthetic.jsonl"}, radius: radius}
	started := time.Now()
	for i := range count {
		stream.add(transcript.Event{Line: i + 1}, false)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("streaming %d unmatched events took %s", count, elapsed)
	}
	if buffered := len(stream.before); buffered > radius {
		t.Fatalf("stream retained %d prior events, want at most radius %d", buffered, radius)
	}
	if len(stream.hits()) != 0 {
		t.Fatalf("unmatched stream emitted %d events", len(stream.hits()))
	}
}

func TestContextWindowBoundsDoNotOverflowNearMaxInt(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	lo, hi := contextWindowBounds(maxInt-5, maxInt-10, maxInt)
	if lo != 5 || hi != maxInt-1 {
		t.Fatalf("bounds near MaxInt = (%d,%d), want (5,%d)", lo, hi, maxInt-1)
	}
	if lo, hi := contextWindowBounds(0, maxInt, 100); lo != 0 || hi != 99 {
		t.Fatalf("huge radius bounds = (%d,%d), want (0,99)", lo, hi)
	}
}
