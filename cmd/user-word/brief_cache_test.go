package main

import (
	"bufio"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
	"github.com/aiseeq/akit/transcript/replycache"
)

func TestBriefCacheReusesFileAcrossQueries(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	path := writeTranscript(t, e.home, "-home-x-proj", "cached-user",
		agent("2026-10-31T09:59:00Z", "Question before."),
		human("2026-10-31T10:00:00Z", "save my note"),
		human("2026-10-31T10:05:00Z", "another reply"),
	)
	e.replyCache = replycache.New()
	uncached := e
	uncached.replyCache = nil
	brief1 := writeBrief(t, t.TempDir(), "Пользователь 31.10: «save my note»\n")
	brief2 := writeBrief(t, t.TempDir(), "Пользователь 31.10: «another reply»\n")
	for _, args := range [][]string{{brief1, "--until", "2026-10-31 10:04"}, {brief2, "--until", "2026-10-31 10:06"}} {
		wantCode, want := briefRun(t, uncached, args...)
		gotCode, got := briefRun(t, e, args...)
		if gotCode != wantCode || got != want {
			t.Fatalf("cached query %v differs\n%s\nwant:\n%s", args, got, want)
		}
	}
	cached, ok, err := e.replyCache.CachedEvents(path)
	if err != nil || !ok {
		t.Fatalf("read shared transcript event cache: ok=%v err=%v", ok, err)
	}
	rawReply := false
	for _, ev := range cached.Events {
		if ev.Role == transcript.User && strings.Contains(string(ev.Raw), "save my note") {
			if ev.Text != "" {
				t.Fatal("cold cache eagerly decoded a raw human reply")
			}
			rawReply = true
			break
		}
	}
	if !rawReply {
		t.Fatal("raw human reply missing from shared transcript cache")
	}
	// Changing until to before the saved reply must use the same file record and
	// derive the answer from its timestamps at query time.
	for _, args := range [][]string{{brief1, "--until", "2026-10-31 10:01"}, {brief1, "--until", "2026-10-31 09:59"}} {
		wantCode, want := briefRun(t, uncached, args...)
		gotCode, got := briefRun(t, e, args...)
		if gotCode != wantCode || got != want {
			t.Fatalf("until query %v differs\n%s\nwant:\n%s", args, got, want)
		}
	}
	checkParity := func() {
		t.Helper()
		wantCode, want := briefRun(t, uncached, brief1, "--until", "2026-10-31 11:00")
		gotCode, got := briefRun(t, e, brief1, "--until", "2026-10-31 11:00")
		if gotCode != wantCode || got != want {
			t.Fatalf("cached changed-file output differs\n%s\nwant:\n%s", got, want)
		}
	}
	for _, mutate := range []struct {
		name string
		edit func() error
	}{
		{"same size and restored mtime", func() error {
			st, err := os.Stat(path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			changed := strings.Replace(string(data), "save my note", "save my data", 1)
			if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
				return err
			}
			return os.Chtimes(path, st.ModTime(), st.ModTime())
		}},
		{"truncate", func() error {
			return os.WriteFile(path, []byte(human("2026-10-31T10:00:00Z", "save my note")+"\n"), 0600)
		}},
		{"append partial line", func() error {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = f.WriteString(human("2026-10-31T10:05:00Z", "another reply") + "\n{")
			closeErr := f.Close()
			if err != nil {
				return err
			}
			return closeErr
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			if err := mutate.edit(); err != nil {
				t.Fatal(err)
			}
			checkParity()
		})
	}
}

func TestBriefCacheKeepsAskAnswers(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	path := writeTranscript(t, e.home, "-home-x-proj", "cached-ask",
		ask("2026-10-31T10:00:00Z", "question-1"),
		answer("2026-10-31T10:01:00Z", "question-1", "Which mode?", "Choose quiet mode"),
	)
	e.replyCache = replycache.New()
	uncached := e
	uncached.replyCache = nil
	brief := writeBrief(t, t.TempDir(), "Пользователь 31.10: «Choose quiet mode»\n")
	args := []string{brief, "--until", "2026-10-31 11:00"}
	wantCode, want := briefRun(t, uncached, args...)
	gotCode, got := briefRun(t, e, args...)
	coldDiffers := gotCode != wantCode || got != want
	cached, ok, err := e.replyCache.CachedEvents(path)
	if err != nil || !ok {
		t.Fatalf("read Ask event cache: ok=%v err=%v", ok, err)
	}
	found := false
	for i := range cached.Events {
		ev := &cached.Events[i]
		if ev.Role == transcript.Ask && strings.Contains(ev.Answer, "Choose quiet mode") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Ask answer absent from cached events; cold differs=%v, events=%+v", coldDiffers, cached.Events)
	}
	if coldDiffers {
		t.Fatalf("cold Ask answer differs despite event in cache\n%s\nwant:\n%s", got, want)
	}
	gotCode, got = briefRun(t, e, args...)
	if gotCode != wantCode || got != want {
		t.Fatalf("warm Ask answer differs\n%s\nwant:\n%s", got, want)
	}
}

func TestBriefCacheBadRowsRemainCandidates(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "quote-cache", human("2026-10-31T10:00:00Z", "save my note"))
	path := writeTranscript(t, e.home, "-home-y-proj", "bad-cache", human("2026-10-31T10:00:00Z", "unrelated text"))
	badUser := `{"type":"user","timestamp":"2026-10-31T10:02:00Z","message":{"role":"user","content":"unfinished"}`
	if err := os.WriteFile(path, []byte(strings.TrimSuffix(mustReadTestFile(t, path), "\n")+"\n"+badUser+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e.replyCache = replycache.New()
	uncached := e
	uncached.replyCache = nil
	brief := writeBrief(t, t.TempDir(), "Пользователь 31.10: «save my note»\n")
	args := []string{brief, "--until", "2026-10-31 11:00"}
	for range 2 {
		wantCode, want := briefRun(t, uncached, args...)
		gotCode, got := briefRun(t, e, args...)
		if gotCode != wantCode || got != want {
			t.Fatalf("malformed transcript differs\n%s\nwant:\n%s", got, want)
		}
	}
}

func TestBriefCacheUsesCurrentLocalZone(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "zone-cache", human("2026-10-30T23:30:00Z", "night reply"))
	e.replyCache = replycache.New()
	uncached := e
	uncached.replyCache = nil
	brief := writeBrief(t, t.TempDir(), "Пользователь 31.10: «night reply»\n")
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []*time.Location{time.UTC, berlin} {
		e.loc, uncached.loc = loc, loc
		wantCode, want := briefRun(t, uncached, brief)
		gotCode, got := briefRun(t, e, brief)
		if gotCode != wantCode || got != want {
			t.Fatalf("cached output in %s differs\n%s\nwant:\n%s", loc, got, want)
		}
		if loc == time.UTC && !strings.Contains(got, "1 not found") {
			t.Fatalf("UTC marker unexpectedly matched next-day local reply:\n%s", got)
		}
		if loc == berlin && !strings.Contains(got, "1 found") {
			t.Fatalf("Berlin marker did not match cached reply at 23:30 UTC:\n%s", got)
		}
	}
}

func TestCachedRawEventUsesBriefPrefilter(t *testing.T) {
	stamp := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	raw := []byte(human(stamp.Format(time.RFC3339), "Save my note"))
	fragments := map[string][]string{"2026-10-31": {"save my note"}}
	filters, err := makeBriefQuoteFilters(fragments, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	cached := replycache.CachedTranscript{
		Events:  []transcript.Event{{Time: stamp, Role: transcript.User, Raw: raw}},
		RawDays: []string{"2026-10-31"},
	}
	if candidate, err := cachedBriefCandidate(cached, filters, fragments, stamp.Add(time.Hour), time.UTC, map[string]bool{}); err != nil || !candidate {
		t.Fatal("raw cached user event failed the same prefilter as the source scan")
	}
	matched, err := cachedRawReplyMatches(raw, stamp, foldBriefQuotes(fragments), stamp.Add(time.Hour), time.UTC)
	if err != nil || !matched {
		t.Fatalf("raw cached reply match = %v, err = %v", matched, err)
	}
}

func TestCachedParsedReplyAvoidsRawMetadataFalseCandidate(t *testing.T) {
	stamp := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	raw := []byte(`{"type":"user","timestamp":"2026-10-31T10:00:00Z","note":"Save my note","message":{"role":"user","content":"Different text"}}`)
	parser := transcript.NewParser()
	events, err := parser.Parse(raw)
	if err != nil || len(events) != 1 {
		t.Fatalf("parse synthetic reply: events=%d err=%v", len(events), err)
	}
	fragments := map[string][]string{"2026-10-31": {"save my note"}}
	filters, err := makeBriefQuoteFilters(fragments, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	cached := replycache.CachedTranscript{
		Events:  []transcript.Event{{Time: stamp, Role: events[0].Role, Raw: raw}},
		RawDays: []string{"2026-10-31"},
	}
	if candidate, err := cachedBriefCandidate(cached, filters, fragments, stamp.Add(time.Hour), time.UTC, map[string]bool{}); err != nil || candidate {
		t.Fatal("unrelated metadata made the cached file a quote candidate")
	}
}

func TestCachedRawUserMarkerRemainsCandidateWhenParserLacksContext(t *testing.T) {
	stamp := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	raw := []byte(`{"type":"metadata","role":"user","timestamp":"2026-10-31T10:00:00Z","message":{"content":"Save my note"}}`)
	fragments := map[string][]string{"2026-10-31": {"save my note"}}
	filters, err := makeBriefQuoteFilters(fragments, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	cached := replycache.CachedTranscript{
		Events:  []transcript.Event{{Time: stamp, Role: transcript.User, Raw: raw}},
		RawDays: []string{"2026-10-31"},
	}
	if candidate, err := cachedBriefCandidate(cached, filters, foldBriefQuotes(fragments), stamp.Add(time.Hour), time.UTC, map[string]bool{}); err != nil || !candidate {
		t.Fatalf("context-dependent raw record candidate=%v err=%v", candidate, err)
	}
}

func TestBriefCacheOnlyIndexesUserLikeQueueAttachments(t *testing.T) {
	ordinary := []byte(`{"type":"attachment","timestamp":"2026-10-31T10:00:00Z","attachment":{"type":"image","text":"Save my note"}}`)
	queued := []byte(`{"type":"attachment","timestamp":"2026-10-31T10:00:00Z","attachment":{"type":"queued_command","origin":{"kind":"human"},"prompt":"Save my note"}}`)
	if mustBriefCacheUserRecord(t, ordinary, classifyBriefRecordPrefix(ordinary)) {
		t.Fatal("ordinary attachment was treated as a user reply")
	}
	if !mustBriefCacheUserRecord(t, queued, classifyBriefRecordPrefix(queued)) {
		t.Fatal("human queued command was not indexed as a user reply")
	}
}

func TestBriefCacheSkipsScannedNonUserRecords(t *testing.T) {
	raw := []byte(`{"type":"assistant","timestamp":"2026-10-31T10:00:00Z","message":{"role":"assistant","content":"synthetic"}}`)
	hints := scanBriefRecordHints(raw)
	hints.scanned = true
	collector := briefQuoteCollector{indexEnabled: true}
	collector.indexRawEventsWithHints(raw, hints)
	if len(collector.indexReplies) != 0 {
		t.Fatalf("cached events=%d, want none", len(collector.indexReplies))
	}
}

func TestBriefCacheClassifiesScannedUserRecordHints(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "human", raw: `{"type":"user","origin":{"kind":"human"},"message":{"role":"user"}}`, want: true},
		{name: "generated", raw: `{"type":"user","message":{"role":"user"}}`},
		{name: "role user", raw: `{"role":"user","message":{"role":"user"}}`, want: true},
		{name: "event user", raw: `{"type":"event_msg","payload":{"type":"user_message"}}`, want: true},
		{name: "queued attachment", raw: `{"attachment":{"type":"queued_command","origin":{"kind":"human"}}}`, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hints := scanBriefRecordHints([]byte(tc.raw))
			got, _, err := briefCacheScannedUserRecordInfo([]byte(tc.raw), hints)
			if err != nil {
				t.Fatalf("classifier error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("userLike=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestBriefCacheSkipsGeneratedUserPrompts(t *testing.T) {
	human := []byte(`{"type":"user","timestamp":"2026-10-31T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"remember this"}}`)
	generated := []byte(`{"type":"user","timestamp":"2026-10-31T10:00:00Z","message":{"role":"user","content":"generated prompt"}}`)
	if !mustBriefCacheUserRecord(t, human, classifyBriefRecordPrefix(human)) {
		t.Fatal("human-origin user record was omitted")
	}
	if mustBriefCacheUserRecord(t, generated, classifyBriefRecordPrefix(generated)) {
		t.Fatal("generated prompt was stored as a human reply")
	}
}

func TestBriefCacheParsesReorderedHumanUserRecord(t *testing.T) {
	stamp := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	raw := []byte(`{"timestamp":"2026-10-31T10:00:00Z","type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"remember this"}}`)
	collector := briefQuoteCollector{indexEnabled: true, indexStamp: stamp}
	collector.indexRawEvents(raw, classifyBriefRecordPrefix(raw))
	if len(collector.indexReplies) != 1 {
		t.Fatalf("cached events = %d, want one", len(collector.indexReplies))
	}
	event := collector.indexReplies[0]
	if event.Role != transcript.User || event.Text != "remember this" || len(event.Raw) != 0 {
		t.Fatalf("cached event role=%q text=%q raw-bytes=%d", event.Role, event.Text, len(event.Raw))
	}
}

func TestBriefCacheStoresExactTimestampForCachedReplies(t *testing.T) {
	raw := []byte(`{"type":"user","timestamp":"2026-10-31T12:00:00.000000001+02:00","origin":{"kind":"human"},"message":{"role":"user","content":"remember this"}}`)
	stamp, err := transcriptTimestamp(raw)
	if err != nil {
		t.Fatal(err)
	}
	collector := briefQuoteCollector{indexEnabled: true}
	hints := scanBriefRecordHints(raw)
	collector.setRawDay([]byte(stamp.UTC().Format(time.DateOnly)), raw, hints)
	if len(collector.indexReplies) != 1 {
		t.Fatalf("cached event count=%d, want one", len(collector.indexReplies))
	}
	if len(collector.indexBuckets) != 1 || collector.indexBuckets[0] != stamp.UnixNano() {
		t.Fatalf("cached record time=%v, want exact Unix nanoseconds %d", collector.indexBuckets, stamp.UnixNano())
	}
	want := time.Date(2026, 10, 31, 10, 0, 0, 1, time.UTC)
	if !collector.indexReplies[0].Time.Equal(want) {
		t.Fatalf("cached event time=%s, want %s", collector.indexReplies[0].Time, want)
	}
}

func TestBriefCacheHumanOriginAfterMessage(t *testing.T) {
	stamp := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	raw := []byte(`{"type":"user","timestamp":"2026-10-31T10:00:00Z","message":{"role":"user","content":"remember this"},"origin":{"kind":"human"}}`)
	collector := briefQuoteCollector{indexEnabled: true, indexStamp: stamp}
	collector.indexRawEvents(raw, classifyBriefRecordPrefix(raw))
	if len(collector.indexReplies) != 1 || len(collector.indexReplies[0].Raw) == 0 || collector.indexReplies[0].Text != "" {
		t.Fatalf("user event count=%d", len(collector.indexReplies))
	}
}

func TestBriefCacheTypeAfterMessage(t *testing.T) {
	stamp := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	raw := []byte(`{"timestamp":"2026-10-31T10:00:00Z","message":{"role":"user","content":"remember this"},"origin":{"kind":"human"},"type":"user"}`)
	collector := briefQuoteCollector{indexEnabled: true, indexStamp: stamp}
	collector.indexRawEvents(raw, classifyBriefRecordPrefix(raw))
	if len(collector.indexReplies) != 1 || collector.indexReplies[0].Text != "remember this" {
		t.Fatalf("user event count=%d", len(collector.indexReplies))
	}
}

func TestBriefCacheReorderedEventMessage(t *testing.T) {
	raw := []byte(`{"timestamp":"2026-10-31T10:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"remember this"}}`)
	if !mustBriefCacheUserRecord(t, raw, classifyBriefRecordPrefix(raw)) {
		t.Fatal("reordered human event message was not indexed")
	}
}

func TestBriefCacheReorderedQueuedCommand(t *testing.T) {
	raw := []byte(`{"timestamp":"2026-10-31T10:00:00Z","attachment":{"type":"queued_command","prompt":"remember this","origin":{"kind":"human"}},"type":"attachment"}`)
	if !mustBriefCacheUserRecord(t, raw, classifyBriefRecordPrefix(raw)) {
		t.Fatal("reordered human queued command was not indexed")
	}
}

func TestBriefCacheQueuedCommandMarkersAfterLongPrompt(t *testing.T) {
	raw := []byte(`{"type":"attachment","timestamp":"2026-10-31T10:00:00Z","attachment":{"type":"queued_command","prompt":"` + strings.Repeat("x", briefCachePrefixLookahead) + `","origin":{"kind":"human"}}}`)
	if !mustBriefCacheUserRecord(t, raw, classifyBriefRecordPrefix(raw)) {
		t.Fatal("human queued command after a long prompt was not indexed")
	}
}

func TestBriefCacheSkipsTailSearchForUnrelatedRecords(t *testing.T) {
	unrelated := []byte(`{"timestamp":"2026-10-31T10:00:00Z","payload":{"content":"` + strings.Repeat("x", 8192) + `"}}`)
	if mustBriefCacheUserRecord(t, unrelated, briefRecordUnknown) {
		t.Fatal("unrelated record classified as a human reply")
	}

	stamp := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	reordered := []byte(`{"timestamp":"2026-10-31T10:00:00Z","message":{"role":"user","content":"remember this"},"origin":{"kind":"human"},"padding":"` + strings.Repeat("x", 8192) + `","type":"user"}`)
	collector := briefQuoteCollector{indexEnabled: true, indexStamp: stamp}
	collector.indexRawEvents(reordered, classifyBriefRecordPrefix(reordered))
	if len(collector.indexReplies) != 1 || collector.indexReplies[0].Text != "remember this" {
		t.Fatalf("user event count=%d", len(collector.indexReplies))
	}
}

func mustBriefCacheUserRecord(t *testing.T, raw []byte, kind briefRecordKind) bool {
	t.Helper()
	user, err := briefCacheUserRecord(raw, kind)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func TestBriefFastHintsKeepRecordKindForCacheIndex(t *testing.T) {
	collector := briefQuoteCollector{filters: briefQuoteFilters{byRawDay: map[string][]func([]byte) bool{}}}
	raw := []byte(`{"type":"user","timestamp":"2026-10-31T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"` + strings.Repeat("x", 2048) + `"}}`)
	hints, ok := collector.fastHintsForRecord(raw)
	if !ok || hints.kind != briefRecordUser {
		t.Fatalf("fast hints kind=%v ok=%v", hints.kind, ok)
	}
}

func TestBriefCacheCompactsCodexUserResponse(t *testing.T) {
	stamp := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	raw := []byte(`{"type":"response_item","timestamp":"2026-10-31T10:00:00Z","payload":{"type":"message","role":"user","content":"remember this"}}`)
	collector := briefQuoteCollector{indexEnabled: true, indexStamp: stamp}
	collector.indexRawEvents(raw, classifyBriefRecordPrefix(raw))
	if len(collector.indexReplies) != 1 {
		t.Fatalf("cached events = %d, want one", len(collector.indexReplies))
	}
	event := collector.indexReplies[0]
	if event.Role != transcript.User || event.Text != "remember this" || len(event.Raw) != 0 {
		t.Fatalf("cached event role=%q text=%q raw-bytes=%d", event.Role, event.Text, len(event.Raw))
	}
}

func TestCachedBadLinesOnlyBroadenTheirMarkerDays(t *testing.T) {
	fragments := map[string][]string{"2026-10-31": {"missing phrase"}}
	filters, err := makeBriefQuoteFilters(fragments, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	farBadDay := replycache.CachedTranscript{Meta: transcript.Meta{Bad: 1}, BadDays: []string{"2026-11-04"}}
	if candidate, err := cachedBriefCandidate(farBadDay, filters, fragments, until, time.UTC, map[string]bool{}); err != nil || candidate {
		t.Fatal("bad row outside quote scan days made the file a candidate")
	}
	matchingBadDay := replycache.CachedTranscript{Meta: transcript.Meta{Bad: 1}, BadDays: []string{"2026-11-01"}}
	if candidate, err := cachedBriefCandidate(matchingBadDay, filters, fragments, until, time.UTC, map[string]bool{}); err != nil || !candidate {
		t.Fatal("bad row in a quote scan day did not make the file a candidate")
	}
	unknownDay := replycache.CachedTranscript{Meta: transcript.Meta{Bad: 1}, BadGlobal: true}
	if candidate, err := cachedBriefCandidate(unknownDay, filters, fragments, until, time.UTC, map[string]bool{}); err != nil || !candidate {
		t.Fatal("unknown-day bad row did not remain a candidate")
	}
	dayOnly := replycache.CachedTranscript{Meta: transcript.Meta{Bad: 1}}
	if candidate, err := cachedBriefCandidate(dayOnly, filters, fragments, until, time.UTC, map[string]bool{}); err != nil || candidate {
		t.Fatal("aggregate bad count broadened a record without unknown-day metadata")
	}
}

func TestCachedBadLineWarningsRespectQueryRange(t *testing.T) {
	stamp := time.Date(2026, 10, 31, 5, 40, 0, 0, time.UTC)
	filters, err := makeBriefQuoteFilters(map[string][]string{"2026-10-31": {"missing phrase"}}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	cached := replycache.CachedTranscript{
		BadLines: []replycache.BadLine{{Time: stamp, Day: "2026-10-31", Line: 7, Message: "invalid synthetic row"}},
		BadDays:  []string{"2026-10-31"}, BadCoverageDays: []string{"2026-10-31"},
	}
	if candidate, err := cachedBriefCandidate(cached, filters, nil, stamp.Add(time.Hour), time.UTC, map[string]bool{}); err != nil || candidate {
		t.Fatalf("covered malformed line forced reread: candidate=%v err=%v", candidate, err)
	}
	warnings := cachedBriefWarnings("synthetic.jsonl", cached, filters, stamp.Add(time.Hour))
	if len(warnings) != 1 || warnings[0].warning != "skipped 1 malformed transcript lines (line 7: invalid synthetic row) in synthetic.jsonl" {
		t.Fatalf("cached warning metadata = %#v", warnings)
	}
	if warnings := cachedBriefWarnings("synthetic.jsonl", cached, filters, stamp.Add(-time.Minute)); len(warnings) != 0 {
		t.Fatalf("warning after --until was retained: %#v", warnings)
	}
	otherDay := stamp.Add(24 * time.Hour)
	cached.BadLines = []replycache.BadLine{{Time: otherDay, Day: "2026-11-01", Line: 9, Message: "other synthetic row"}}
	if warnings := cachedBriefWarnings("synthetic.jsonl", cached, filters, otherDay.Add(time.Hour)); len(warnings) != 1 {
		t.Fatal("warning in a scanned day without a quote marker was dropped")
	}
}

func TestBriefScanWarningsPreserveUnknownDayAlongsideKnownDay(t *testing.T) {
	warnings, days, unknown := briefScanWarnings([]line{
		{warning: "known day", badDays: []string{"2026-10-31"}},
		{warning: "unknown day"},
	}, nil)
	if len(warnings) != 2 || len(days) != 1 || days[0] != "2026-10-31" || !unknown {
		t.Fatalf("warning metadata = %d, %v, unknown=%v", len(warnings), days, unknown)
	}
}

func TestBriefCacheMergesRecordDaysFromNonCandidateFiles(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "quote-candidate", human("2026-10-30T10:00:00Z", "known note"))
	writeTranscript(t, e.home, "-home-y-proj", "record-only", human("2026-10-31T10:00:00Z", "unrelated response"))
	e.replyCache = replycache.New()
	uncached := e
	uncached.replyCache = nil
	brief := writeBrief(t, t.TempDir(), "Пользователь 30.10: «known note»\nПользователь 31.10: «missing phrase»\n")
	for range 2 {
		wantCode, want := briefRun(t, uncached, brief, "--until", "2026-10-31 11:00")
		gotCode, got := briefRun(t, e, brief, "--until", "2026-10-31 11:00")
		if gotCode != wantCode || got != want {
			t.Fatalf("cached marker-day diagnostics differ\n%s\nwant:\n%s", got, want)
		}
		if !strings.Contains(got, "no matching user line on marker day") || strings.Contains(got, "no transcript records on marker day") {
			t.Fatalf("record-only file day was lost on warm pass:\n%s", got)
		}
	}
}

func TestCachedRecordDayUsesExclusiveEnd(t *testing.T) {
	start := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	nextDayBucket := cachedRecordTime("2026-11-01T00:00:00Z")
	if cachedRecordDay([]replycache.RecordTime{nextDayBucket}, start, start.AddDate(0, 0, 1)) {
		t.Fatal("00:05 next-day record bucket counted in prior day")
	}
	if cachedRecordDay([]replycache.RecordTime{cachedRecordTime("2026-10-31T10:00:00Z")}, start, start.Add(time.Hour)) {
		t.Fatal("record exactly at the exclusive end was counted")
	}
}

func TestCachedRecordDayIncludesUntilBoundary(t *testing.T) {
	until := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	interval := markerRecordInterval{start: until, end: until.AddDate(0, 0, 1)}
	key := markerRecordDayKey(interval.start)
	recordDays := make(map[string]bool)
	filters := briefQuoteFilters{recordWindows: map[string]markerRecordInterval{key: interval}}
	if _, err := cachedBriefCandidate(replycache.CachedTranscript{RecordTimes: []replycache.RecordTime{cachedRecordTime(until.Format(time.RFC3339Nano))}}, filters, nil, until, time.UTC, recordDays); err != nil {
		t.Fatal(err)
	}
	if !recordDays[key] {
		t.Fatal("record at the --until boundary was dropped from cached marker days")
	}
}

func TestCachedRecordDayExcludesLaterRecordInFormerQuarterHour(t *testing.T) {
	until := time.Date(2026, 11, 11, 10, 2, 0, 0, time.UTC)
	after := time.Date(2026, 11, 11, 10, 8, 0, 0, time.UTC)
	interval := markerRecordInterval{start: until.Add(-time.Hour), end: after.Add(time.Hour)}
	key := markerRecordDayKey(interval.start)
	filters := briefQuoteFilters{recordWindows: map[string]markerRecordInterval{key: interval}}
	recordDays := make(map[string]bool)
	_, err := cachedBriefCandidate(replycache.CachedTranscript{
		RecordTimes: []replycache.RecordTime{cachedRecordTime(after.Format(time.RFC3339Nano))},
	}, filters, nil, until, time.UTC, recordDays)
	if err != nil {
		t.Fatal(err)
	}
	if recordDays[key] {
		t.Fatal("record after --until was attributed to the marker day")
	}
}

func TestCachedRecordDayUsesNanosecondUntilBoundary(t *testing.T) {
	until := time.Date(2026, 11, 11, 10, 2, 0, 0, time.UTC)
	interval := markerRecordInterval{start: until.Truncate(24 * time.Hour), end: until.AddDate(0, 0, 1)}
	key := markerRecordDayKey(interval.start)
	filters := briefQuoteFilters{recordWindows: map[string]markerRecordInterval{key: interval}}
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{name: "exact boundary", at: until, want: true},
		{name: "one nanosecond after", at: until.Add(time.Nanosecond), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recordDays := make(map[string]bool)
			_, err := cachedBriefCandidate(replycache.CachedTranscript{RecordTimes: []replycache.RecordTime{cachedRecordTime(tc.at.Format(time.RFC3339Nano))}}, filters, nil, until, time.UTC, recordDays)
			if err != nil {
				t.Fatal(err)
			}
			if recordDays[key] != tc.want {
				t.Fatalf("recordDays[%q]=%v, want %v", key, recordDays[key], tc.want)
			}
		})
	}
}

func TestCachedRecordDayMatchesBaseUTCRecordDayRules(t *testing.T) {
	until := time.Date(2026, 10, 31, 10, 2, 0, 0, time.UTC)
	start := until.Add(-time.Hour)
	end := until.Add(time.Hour)
	for _, tc := range []struct {
		stamp string
		want  bool
	}{
		{stamp: "2026-10-31T12:02:00+02:00", want: false},
		{stamp: "2026-11-01T00:02:00+14:00", want: false},
	} {
		if got := cachedRecordDay([]replycache.RecordTime{cachedRecordTime(tc.stamp)}, start, end); got != tc.want {
			t.Errorf("cachedRecordDay(%q)=%v, want %v", tc.stamp, got, tc.want)
		}
	}
}

func TestBriefCacheFallsBackForUnclosedTimestamp(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	writeTranscript(t, e.home, "-home-x-proj", "known", human("2026-10-30T10:00:00Z", "known note"))
	path := writeTranscript(t, e.home, "-home-y-proj", "growing",
		human("2026-10-31T09:00:00Z", "unrelated"),
		human("2026-10-31T10:03:00Z", "after cutoff"),
		`{"type":"user","timestamp":"2026-10-31T10:04:00Z`,
		human("2026-10-31T10:05:00Z", "later"),
	)
	e.replyCache = replycache.New()
	uncached := e
	uncached.replyCache = nil
	brief := writeBrief(t, t.TempDir(), "Пользователь 30.10: «known note»\nПользователь 31.10: «missing phrase»\n")
	for range 2 {
		wantCode, want := briefRun(t, uncached, brief, "--until", "2026-10-31 10:02")
		gotCode, got := briefRun(t, e, brief, "--until", "2026-10-31 10:02")
		if gotCode != wantCode || got != want {
			t.Fatalf("cached scan of an incomplete final row differs (code %d, want %d)", gotCode, wantCode)
		}
		if gotCode != 1 || strings.Contains(got, "malformed transcript lines") {
			t.Fatalf("row after --until did not follow the direct reader's cutoff (code %d)", gotCode)
		}
	}
	if _, complete, _ := e.replyCache.CachedEvents(path); complete {
		t.Fatal("failed cache scan published a record")
	}
}

func TestBriefCacheFallsBackOnIndexIssue(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	path := writeTranscript(t, e.home, "-home-x-proj", "bad-user",
		`{"type":"user","timestamp":"2026-10-31T10:00:00Z","origin":{"kind":"human"},"message":{"content":"broken"`,
	)
	e.replyCache = replycache.New()
	uncached := e
	uncached.replyCache = nil
	brief := writeBrief(t, t.TempDir(), "Пользователь 31.10: «broken»\n")
	wantCode, want := briefRun(t, uncached, brief, "--until", "2026-10-31 10:02")
	gotCode, got := briefRun(t, e, brief, "--until", "2026-10-31 10:02")
	if gotCode != wantCode || got != want {
		t.Fatalf("cache index issue changed direct result (code %d, want %d)", gotCode, wantCode)
	}
	if _, complete, _ := e.replyCache.CachedEvents(path); complete {
		t.Fatal("cache index error published a record")
	}
}

func TestBriefCacheFallsBackOutsideUnixNanoRange(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	e := testEnv(t)
	e.replyCache = replycache.New()
	path := writeTranscript(t, e.home, "-home-x-proj", "in-range-source", human("2026-10-31T10:00:00Z", "synthetic"))
	filters := briefQuoteFilters{from: briefCacheCoverageUntil().Add(time.Nanosecond)}
	until := filters.from.Add(time.Hour)
	got := scanBriefTranscript(path, filters, nil, until, time.UTC, e.replyCache, false)
	want := scanBriefTranscript(path, filters, nil, until, time.UTC, nil, false)
	if got.err != want.err || len(got.hits) != len(want.hits) || len(got.warnings) != len(want.warnings) {
		t.Fatalf("out-of-range cache path differs from direct scan (cached err=%v, direct err=%v)", got.err, want.err)
	}
	if _, complete, _ := e.replyCache.CachedEvents(path); complete {
		t.Fatal("out-of-range query published a cache record")
	}
}

func cachedRecordTime(value string) replycache.RecordTime {
	stamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return replycache.RecordTime{Seconds: stamp.Unix(), Nanosecond: int32(stamp.Nanosecond()), TextDay: value[:len(time.DateOnly)], RecordDay: validUTCTimestampDate([]byte(value))}
}

func TestBriefCacheHumanOriginParsesWholeUnorderedRecord(t *testing.T) {
	ordered := []byte(`{"type":"user","timestamp":"2026-10-31T10:00:00Z","origin":{"source":"cli","kind":"human"},"message":{"content":"synthetic"}}`)
	if human, err := briefCacheHumanOrigin(ordered, briefCacheRecordHeader(ordered), false); err != nil || !human {
		t.Fatal("human origin with reordered keys was missed")
	}
	long := []byte(`{"type":"user","timestamp":"2026-10-31T10:00:00Z","message":{"content":"` + strings.Repeat("x", 5000) + `"},"padding":"` + strings.Repeat("y", 5000) + `","origin":{"source":"cli","kind":"human"}}`)
	if human, err := briefCacheHumanOrigin(long, briefCacheRecordHeader(long), false); err != nil || !human {
		t.Fatal("human origin beyond the message tail was missed")
	}
}

func TestBriefCacheClassifierReturnsMalformedRecordError(t *testing.T) {
	raw := []byte(`{"type":"user","origin":{"kind":"human"}`)
	if _, _, err := briefCacheUserRecordInfo(raw, briefRecordUser); err == nil {
		t.Fatal("malformed cached user record was classified without an error")
	}
}

func TestBriefVisibleLinesHonorsExactUntil(t *testing.T) {
	until := time.Date(2026, 11, 11, 10, 2, 0, 0, time.UTC)
	lines := []line{{t: until.Add(-time.Second)}, {t: until}, {t: until.Add(time.Second)}}
	got := briefVisibleLines(lines, until)
	if len(got) != 2 || !got[1].t.Equal(until) {
		t.Fatalf("visible lines = %#v; want rows through exact cutoff", got)
	}
}

func TestKeepWithHintsExcludesRecordAfterUntil(t *testing.T) {
	until := time.Date(2026, 11, 11, 10, 2, 0, 0, time.UTC)
	collector := briefQuoteCollector{
		indexEnabled: true,
		until:        until,
		untilDate:    []byte(until.UTC().Format(time.DateOnly)),
		untilUTC:     []byte(until.UTC().Format("2006-01-02T15:04:05.000000000Z")),
	}
	raw := []byte(`{"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-11T10:08:00Z","message":{"content":"synthetic"}}`)
	hints, _ := collector.fastHintsForRecord(raw)
	if collector.keepWithHints(raw, hints) {
		t.Fatal("record after --until was retained")
	}
	if len(collector.indexBuckets) != 0 {
		t.Fatal("record after --until entered the activity index")
	}
	collector.lastAgent = []byte("agent context before cutoff")
	agent := []byte(`{"type":"assistant","timestamp":"2026-11-11T10:02:01Z","message":{"content":[{"type":"text","text":"agent context after cutoff"}]}}`)
	agentHints, _ := collector.fastHintsForRecord(agent)
	if collector.keepWithHints(agent, agentHints) {
		t.Fatal("agent record after --until was retained")
	}
	if string(collector.lastAgent) != "agent context before cutoff" {
		t.Fatalf("post-cutoff agent replaced context: %q", collector.lastAgent)
	}
}

func TestBriefTimestampAfterUntilUsesBaseRule(t *testing.T) {
	until := time.Date(2026, 10, 31, 10, 2, 0, 0, time.UTC)
	for _, raw := range []string{
		`{"timestamp":"2026-10-31T12:02:00.000000000+02:00"}`,
		`{"timestamp":"2026-10-31T09:59:00Z"}`,
	} {
		past, err := briefTimestampAfterUntil([]byte(raw), until)
		if err != nil || past {
			t.Errorf("record at/before until treated as after: %s", raw)
		}
	}
	for _, raw := range []string{
		`{"timestamp":"2026-10-31T10:02:00.000000001Z"}`,
		`{"timestamp":"2026-10-31T11:02:00-01:00"}`,
		`{"timestamp":"2026-11-01T00:02:00+14:00"}`,
	} {
		past, err := briefTimestampAfterUntil([]byte(raw), until)
		if err != nil || !past {
			t.Errorf("record after until treated as before: %s", raw)
		}
	}
	past, err := briefTimestampAfterUntil([]byte(`{"type":"user"}`), until)
	if err != nil || past {
		t.Fatal("missing timestamp was treated as after --until")
	}
	if _, err := briefTimestampAfterUntil([]byte(`{"timestamp":"invalid"}`), until); err == nil {
		t.Fatal("invalid timestamp error was hidden")
	}
}

func TestIndexRecordRetainsDataPastCurrentUntil(t *testing.T) {
	until := time.Date(2026, 10, 31, 10, 2, 0, 0, time.UTC)
	collector := briefQuoteCollector{indexEnabled: true, until: until}
	first := []byte(`{"timestamp":"2026-11-01T00:02:00+14:00","type":"user"}`)
	within, err := collector.indexRecordTimestampValid(first)
	if err != nil || !within {
		t.Fatalf("valid record after --until: indexable=%v err=%v, want true", within, err)
	}
	malformed := []byte(`{"type":"user","message":`)
	within, err = collector.indexRecordTimestampValid(malformed)
	if err != nil || !within {
		t.Fatalf("timestamp-less record: indexable=%v err=%v, want true", within, err)
	}
}

func TestKeepWithHintsSurfacesTimestampParseError(t *testing.T) {
	collector := briefQuoteCollector{indexEnabled: true}
	raw := []byte(`{"type":"user","timestamp":"not-a-timestamp","message":{"content":"synthetic"}}`)
	hints := scanBriefRecordHints(raw)
	if !collector.keepWithHints(raw, hints) {
		t.Fatal("record with invalid timestamp was not handed to the scanner")
	}
	if collector.timestampErr == nil {
		t.Fatal("timestamp parse error was hidden")
	}
}

func TestBriefScanNoteBadInheritsLastTimestamp(t *testing.T) {
	previous := time.Date(2026, 10, 31, 10, 2, 0, 1, time.UTC)
	scan := briefScan{lastTimestamp: previous, badDays: make(map[string]bool)}
	scan.noteBad(2, errors.New("synthetic malformed line"), nil,
		[]byte(`{"type":"user"}`), true)
	if got := scan.badRecords[0].Time; !got.Equal(previous) {
		t.Fatalf("malformed row time=%s, want previous record time %s", got, previous)
	}
}

func TestBriefScanKeepsExactPreviousTimestampForUntimestampedBadRow(t *testing.T) {
	previous := time.Date(2026, 10, 31, 10, 2, 0, 1, time.UTC)
	data := `{"type":"user","timestamp":"2026-10-31T10:02:00.000000001Z","message":{"content":"synthetic"}}` + "\n" + `{broken}` + "\n"
	scan := briefScan{badDays: make(map[string]bool), linesBefore: func() (int, error) { return 0, nil }}
	_, err := scan.lines(bufio.NewReader(strings.NewReader(data)), transcript.NewParser(), previous.Add(time.Minute), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.badRecords) != 1 {
		t.Fatalf("bad row count=%d, want 1", len(scan.badRecords))
	}
	if !scan.badRecords[0].Time.Equal(previous) {
		t.Fatalf("bad row time=%s, want previous exact timestamp %s", scan.badRecords[0].Time, previous)
	}
}

func TestBriefScanWarningRelevantInheritsLastTimestamp(t *testing.T) {
	previous := time.Date(2026, 10, 31, 10, 2, 0, 1, time.UTC)
	raw := []byte(`{"type":"user"}`)
	scan := briefScan{
		lastTimestamp: previous,
		warningFrom:   previous,
		warningUntil:  previous,
	}
	if !scan.warningRelevant(raw) {
		t.Fatal("missing timestamp did not inherit the previous timestamp for warning filtering")
	}
	scan.warningFrom = previous.Add(time.Second)
	scan.warningUntil = previous.Add(time.Minute)
	if scan.warningRelevant(raw) {
		t.Fatal("missing timestamp warning escaped the inherited-time window")
	}
}

func mustReadTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
