package main

import (
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
	if briefCacheUserRecord(ordinary, classifyBriefRecordPrefix(ordinary)) {
		t.Fatal("ordinary attachment was treated as a user reply")
	}
	if !briefCacheUserRecord(queued, classifyBriefRecordPrefix(queued)) {
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
			got, _ := briefCacheScannedUserRecordInfo([]byte(tc.raw), hints)
			if got != tc.want {
				t.Fatalf("userLike=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestBriefCacheSkipsGeneratedUserPrompts(t *testing.T) {
	human := []byte(`{"type":"user","timestamp":"2026-10-31T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"remember this"}}`)
	generated := []byte(`{"type":"user","timestamp":"2026-10-31T10:00:00Z","message":{"role":"user","content":"generated prompt"}}`)
	if !briefCacheUserRecord(human, classifyBriefRecordPrefix(human)) {
		t.Fatal("human-origin user record was omitted")
	}
	if briefCacheUserRecord(generated, classifyBriefRecordPrefix(generated)) {
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

func TestBriefCacheUsesPrefetchedTimestampForCachedReplies(t *testing.T) {
	raw := []byte(`{"type":"user","timestamp":"2026-10-31T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"remember this"}}`)
	collector := briefQuoteCollector{indexEnabled: true}
	hints, ok := collector.fastHintsForRecord(raw)
	if !ok {
		t.Fatal("expected fast timestamp hints")
	}
	collector.setRawDay(hints.rawDay, raw, hints)
	if len(collector.indexReplies) != 1 {
		t.Fatalf("cached event count=%d, want one", len(collector.indexReplies))
	}
	want := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
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
	if !briefCacheUserRecord(raw, classifyBriefRecordPrefix(raw)) {
		t.Fatal("reordered human event message was not indexed")
	}
}

func TestBriefCacheReorderedQueuedCommand(t *testing.T) {
	raw := []byte(`{"timestamp":"2026-10-31T10:00:00Z","attachment":{"type":"queued_command","prompt":"remember this","origin":{"kind":"human"}},"type":"attachment"}`)
	if !briefCacheUserRecord(raw, classifyBriefRecordPrefix(raw)) {
		t.Fatal("reordered human queued command was not indexed")
	}
}

func TestBriefCacheQueuedCommandMarkersAfterLongPrompt(t *testing.T) {
	raw := []byte(`{"type":"attachment","timestamp":"2026-10-31T10:00:00Z","attachment":{"type":"queued_command","prompt":"` + strings.Repeat("x", briefCachePrefixLookahead) + `","origin":{"kind":"human"}}}`)
	if !briefCacheUserRecord(raw, classifyBriefRecordPrefix(raw)) {
		t.Fatal("human queued command after a long prompt was not indexed")
	}
}

func TestBriefCacheSkipsTailSearchForUnrelatedRecords(t *testing.T) {
	unrelated := []byte(`{"timestamp":"2026-10-31T10:00:00Z","payload":{"content":"` + strings.Repeat("x", 8192) + `"}}`)
	if briefCacheUserRecord(unrelated, briefRecordUnknown) {
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
	nextDayBucket := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix() / replycache.RecordBucketSeconds * replycache.RecordBucketSeconds
	if cachedRecordDay([]int64{nextDayBucket}, start, start.AddDate(0, 0, 1)) {
		t.Fatal("00:05 next-day record bucket counted in prior day")
	}
}

func TestCachedRecordDayIncludesUntilBoundary(t *testing.T) {
	until := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	interval := markerRecordInterval{start: until, end: until.AddDate(0, 0, 1)}
	key := markerRecordDayKey(interval.start)
	bucket := until.Unix() / replycache.RecordBucketSeconds * replycache.RecordBucketSeconds
	recordDays := make(map[string]bool)
	filters := briefQuoteFilters{recordWindows: map[string]markerRecordInterval{key: interval}}
	if _, err := cachedBriefCandidate(replycache.CachedTranscript{RecordBuckets: []int64{bucket}}, filters, nil, until, time.UTC, recordDays); err != nil {
		t.Fatal(err)
	}
	if !recordDays[key] {
		t.Fatal("record at the --until boundary was dropped from cached marker days")
	}
}

func TestBriefBucketTimestampParsesOffsets(t *testing.T) {
	got, ok := briefBucketTimestamp([]byte("2026-10-31T00:05:00.125+01:00"))
	want := time.Date(2026, 10, 30, 23, 5, 0, 0, time.UTC)
	if !ok || !got.Equal(want) {
		t.Fatalf("offset timestamp = %v, %v; want %v", got, ok, want)
	}
	if _, ok := briefBucketTimestamp([]byte("2026-02-30T12:00:00Z")); ok {
		t.Fatal("invalid calendar date accepted")
	}
}

func TestBriefTimestampAfterUTCUsesFractionalCutoff(t *testing.T) {
	cutoff := []byte("2026-10-31T10:00:05.500000000Z")
	for _, raw := range []string{"2026-10-31T10:00:04Z", "2026-10-31T10:00:05Z", "2026-10-31T10:00:05.5Z"} {
		after, canonical := briefTimestampAfterUTC([]byte(raw), cutoff)
		if !canonical || after {
			t.Fatalf("timestamp %q after cutoff=%v canonical=%v", raw, after, canonical)
		}
	}
	for _, raw := range []string{"2026-10-31T10:00:05.500000001Z", "2026-10-31T10:00:06Z"} {
		after, canonical := briefTimestampAfterUTC([]byte(raw), cutoff)
		if !canonical || !after {
			t.Fatalf("timestamp %q after cutoff=%v canonical=%v", raw, after, canonical)
		}
	}
	if _, canonical := briefTimestampAfterUTC([]byte("2026-10-31T10:00:06+01:00"), cutoff); canonical {
		t.Fatal("offset timestamp entered the canonical UTC path")
	}
	dateCutoff := []byte("2026-10-31")
	utcCutoff := []byte("2026-10-31T00:00:00.000000000Z")
	until, _ := time.Parse(time.RFC3339Nano, string(utcCutoff))
	if briefTimestampAfterScanUntil([]byte("2026-10-30"), []byte("2026-10-30T23:59:59Z"), dateCutoff, utcCutoff, until) {
		t.Fatal("earlier UTC day was treated as after --until")
	}
	if !briefTimestampAfterScanUntil([]byte("2026-10-30"), []byte("2026-10-30T23:30:00-01:00"), dateCutoff, utcCutoff, until) {
		t.Fatal("offset timestamp crossing into the cutoff day was skipped")
	}
}

func TestBriefBucketUnixMatchesTimeParse(t *testing.T) {
	for _, raw := range []string{
		"2026-10-30T23:05:00.125+01:00",
		"2000-02-29T12:34:56Z",
		"1969-12-31T23:59:59Z",
		"2026-11-11T00:00:00-07:30",
	} {
		want, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := briefBucketUnix([]byte(raw))
		if !ok || got != want.Unix() {
			t.Fatalf("briefBucketUnix(%q) = %d, %v; want %d", raw, got, ok, want.Unix())
		}
	}
	for _, raw := range []string{"2026-02-30T12:00:00Z", "2026-10-30T23:05:00+24:00"} {
		if _, ok := briefBucketUnix([]byte(raw)); ok {
			t.Fatalf("briefBucketUnix(%q) accepted invalid timestamp", raw)
		}
	}
}

func TestBriefUTCBucketKeyReusesFractionalTimestampBucket(t *testing.T) {
	first, ok := briefUTCBucketKey([]byte("2026-10-31T10:02:03Z"))
	if !ok {
		t.Fatal("UTC timestamp key was rejected")
	}
	if !briefUTCBucketMatchesKey([]byte("2026-10-31T10:14:59.123456Z"), first) {
		t.Fatal("same 15-minute UTC bucket did not match key")
	}
	if briefUTCBucketMatchesKey([]byte("2026-10-31T10:15:00Z"), first) || briefUTCBucketMatchesKey([]byte("2026-11-01T10:02:03Z"), first) {
		t.Fatal("different UTC bucket matched key")
	}
	second, ok := briefUTCBucketKey([]byte("2026-10-31T10:14:59.123456Z"))
	if !ok || first != second {
		t.Fatal("timestamps in one 15-minute UTC bucket got different keys")
	}
	if next, ok := briefUTCBucketKey([]byte("2026-10-31T10:15:00Z")); !ok || first == next {
		t.Fatal("timestamps on opposite sides of a bucket boundary got the same key")
	}
	if _, ok := briefUTCBucketKey([]byte("2026-10-31T10:15:03+01:00")); ok {
		t.Fatal("offset timestamp entered the UTC fast path")
	}
	for _, value := range []string{"2026-10-31T10:02:60Z", "2026-10-31T10:60:03Z"} {
		if _, ok := briefUTCBucketKey([]byte(value)); ok {
			t.Fatalf("invalid timestamp %q entered the fast path", value)
		}
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
