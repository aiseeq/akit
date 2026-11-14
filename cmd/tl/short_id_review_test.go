package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/akit/proc"
)

func reviewHome(t *testing.T) env {
	t.Helper()
	home := t.TempDir()
	return env{home: home, now: time.Date(2026, 11, 14, 8, 0, 0, 0, time.Local)}
}

func writeReviewCodex(t *testing.T, e env, worker, id, text string, at time.Time) string {
	t.Helper()
	path := filepath.Join(e.home, ".local", "state", "worker", worker, "codex", "sessions", at.Format("2006"), at.Format("01"), at.Format("02"), "rollout-"+at.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	rows := []string{
		`{"timestamp":"` + at.Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"` + id + `"}}`,
		`{"timestamp":"` + at.Add(time.Second).Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}}`,
	}
	writeWorkFile(t, path, rows)
	return path
}

func writeReviewClaude(t *testing.T, e env, project, filename string, rows ...string) string {
	t.Helper()
	path := filepath.Join(e.home, ".claude", "projects", project, filename+".jsonl")
	writeWorkFile(t, path, rows)
	return path
}

func reviewEventRefs(out string) [][2]string {
	re := regexp.MustCompile(`(?m)^\d{2}\.\d{2} \d{2}:\d{2}:\d{2} ([^\s]+):([0-9]+) `)
	matches := re.FindAllStringSubmatch(out, -1)
	refs := make([][2]string, len(matches))
	for i, match := range matches {
		refs[i] = [2]string{match[1], match[2]}
	}
	return refs
}

func TestLinksResolveOutsideTheirListingSelection(t *testing.T) {
	for _, mode := range []string{"project", "session", "since"} {
		t.Run(mode, func(t *testing.T) {
			e := reviewHome(t)
			firstID := "76543210-0001-7000-8000-000000000001"
			secondID := "76543210-0002-7000-8000-000000000002"
			firstPath := writeReviewCodex(t, e, "selected-worker", firstID, "X selected marker", e.now)
			secondPath := writeReviewCodex(t, e, "other-worker", secondID, "X other marker", e.now.Add(time.Minute))
			args := []string{"--all", "--codex", "--grep", "X", "--since", "2026-11-13T00:00:00Z"}
			switch mode {
			case "project":
				args = append(args, "--project", "selected-worker")
			case "session":
				args = []string{firstID}
			case "since":
				old := e.now.Add(-10 * 24 * time.Hour)
				if err := os.Chtimes(secondPath, old, old); err != nil {
					t.Fatal(err)
				}
				args = []string{"--all", "--codex", "--grep", "X", "--since", "7d"}
			}
			code, out, stderr := runTL(t, e, args...)
			if code != 0 || stderr != "" {
				t.Fatalf("listing code %d, stderr %q, output:\n%s", code, stderr, out)
			}
			refs := reviewEventRefs(out)
			if len(refs) == 0 {
				t.Fatalf("no event references:\n%s", out)
			}
			for _, ref := range refs {
				if len(ref[0]) <= 8 {
					t.Errorf("link id %q ignores the wider find scope", ref[0])
				}
				code, opened, stderr := runTL(t, e, ref[0]+":"+ref[1])
				if code != 0 || stderr != "" || !strings.Contains(opened, "selected marker") {
					t.Errorf("link %q did not resolve to selected source %s: code %d, stderr %q, output %q", ref[0], firstPath, code, stderr, opened)
				}
				if !strings.Contains(opened, ref[0]+":") {
					t.Errorf("expanded reference output shortened %q:\n%s", ref[0], opened)
				}
			}
		})
	}
}

func TestSubagentReferencesDistinguishDuplicateAndPrimaryIDs(t *testing.T) {
	e := reviewHome(t)
	parentA := "aaaa0000-0000-4000-8000-000000000001"
	parentB := "aaaa0000-0000-4000-8000-000000000002"
	mainID := "a2222222-0000-4000-8000-000000000003"
	writeReviewClaude(t, e, "synthetic", parentA, `{"type":"agent-name","agentName":"synthetic-parent"}`)
	writeReviewClaude(t, e, "synthetic", parentB, `{"type":"agent-name","agentName":"synthetic-parent"}`)
	writeReviewClaude(t, e, "synthetic", mainID, `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:00Z","message":{"role":"user","content":"X main marker"}}`)
	childDirA := filepath.Join(e.home, ".claude", "projects", "synthetic", parentA, "subagents")
	childDirB := filepath.Join(e.home, ".claude", "projects", "synthetic", parentB, "subagents")
	childID := "a2222222ddd000002"
	rowA := `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:01Z","message":{"role":"user","content":"X first child marker"}}`
	rowB := `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:02Z","message":{"role":"user","content":"X second child marker"}}`
	writeWorkFile(t, filepath.Join(childDirA, "agent-"+childID+".jsonl"), []string{rowA})
	writeWorkFile(t, filepath.Join(childDirB, "agent-"+childID+".jsonl"), []string{rowB})
	code, out, stderr := runTL(t, e, "--all", "--grep", "X", "--since", "2026-11-13T00:00:00Z")
	if code != 0 || stderr != "" {
		t.Fatalf("listing code %d, stderr %q, output:\n%s", code, stderr, out)
	}
	refs := reviewEventRefs(out)
	code, _, ambiguity := runTL(t, e, "a2222222")
	if code != exitUsage || !strings.Contains(ambiguity, "matches 3 transcripts") || strings.Count(ambiguity, "subagents/agent-") != 2 {
		t.Errorf("duplicate subagent candidates are not distinguishable: code %d, stderr %q", code, ambiguity)
	}
	for _, marker := range []string{"main marker", "first child marker", "second child marker"} {
		found := false
		for _, ref := range refs {
			code, opened, stderr := runTL(t, e, ref[0]+":"+ref[1])
			if strings.Contains(out, marker) && strings.Contains(opened, marker) {
				found = true
				if code != 0 || stderr != "" {
					t.Errorf("reference for %q: code %d, stderr %q", marker, code, stderr)
				}
				break
			}
		}
		if !found {
			t.Errorf("no unique reference opened %q; output:\n%s", marker, out)
		}
	}
	if !strings.Contains(out, "== "+mainID[:8]) {
		t.Fatalf("main source header missing:\n%s", out)
	}
	if got := strings.Count(out, " sub of "); got != 2 {
		t.Errorf("got %d subagent headers, want one per path:\n%s", got, out)
	}
	if got := strings.Count(out, "synthetic-parent"); got != 2 {
		t.Errorf("parent names were lost or shared across paths: got %d occurrences\n%s", got, out)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, " sub of ") {
			fields := strings.Fields(line)
			for i, field := range fields {
				if field == "of" && i+1 < len(fields) && len(fields[i+1]) <= 8 {
					t.Errorf("subagent parent header uses ambiguous id %q", fields[i+1])
				}
			}
		}
	}
}

func TestAmbiguousCandidatesAreCappedAndSurviveSourceMetadataErrors(t *testing.T) {
	e := reviewHome(t)
	for i := range 11 {
		id := fmt.Sprintf("deadcafe-%04x-4000-8000-%012x", i+1, i+1)
		writeReviewClaude(t, e, "synthetic", id, `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:00Z","message":{"role":"user","content":"synthetic"}}`)
	}
	code, _, stderr := runTL(t, e, "deadcafe")
	if code != exitUsage || !strings.Contains(stderr, "11") || !strings.Contains(stderr, "give more of the id") || !strings.Contains(stderr, "…") {
		t.Errorf("ambiguous error lacks count, capped list, or hint: code %d, stderr %q", code, stderr)
	}
	if got := strings.Count(stderr, "deadcafe-"); got != 10 {
		t.Errorf("ambiguous error lists %d candidates, want 10", got)
	}

	parent := "eeee0000-0000-4000-8000-000000000001"
	child := "agent-eeeeeeeeeeee000001.jsonl"
	path := filepath.Join(e.home, ".claude", "projects", "synthetic", parent, "subagents", child)
	writeWorkFile(t, path, []string{`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:00Z","message":{"role":"user","content":"synthetic"}}`})
	if err := os.Mkdir(path[:len(path)-len(".jsonl")]+".meta.json", 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(e.home, ".claude", "projects", "synthetic", "ffff0000-0000-4000-8000-000000000002", "subagents", "agent-eeeeeeeeeeee000002.jsonl")
	writeWorkFile(t, other, []string{`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:00Z","message":{"role":"user","content":"synthetic"}}`})
	code, _, stderr = runTL(t, e, "eeeeeeee")
	if code != exitUsage || !strings.Contains(stderr, "ambiguous") || !strings.Contains(stderr, "give more of the id") {
		t.Errorf("metadata error hid ambiguity: code %d, stderr %q", code, stderr)
	}
}

func TestUsageDeduplicatesCopiedMessagesAndKeepsSessionsDistinct(t *testing.T) {
	t.Run("copied subagent message", func(t *testing.T) {
		e := reviewHome(t)
		parents := []string{"11110000-0000-4000-8000-000000000001", "22220000-0000-4000-8000-000000000002"}
		row := strings.ReplaceAll(usageRecord("synthetic-message", "claude-opus-5-5", "synthetic reply", 10, 20, 30, 40), "2026-10-25T10:00:00Z", e.now.Format(time.RFC3339Nano))
		for _, parent := range parents {
			writeReviewClaude(t, e, "synthetic", parent, `{"type":"agent-name","agentName":"synthetic-parent"}`)
			writeWorkFile(t, filepath.Join(e.home, ".claude", "projects", "synthetic", parent, "subagents", "agent-abcdef123456000001.jsonl"), []string{row})
		}
		code, out, stderr := runTL(t, e, "--usage", "--all", "--since", "2026-11-13T00:00:00Z")
		if code != 0 || stderr != "" {
			t.Fatalf("usage code %d, stderr %q, output:\n%s", code, stderr, out)
		}
		rows := strings.Split(strings.TrimSpace(out), "\n")
		fields := strings.Split(rows[len(rows)-2], "\t")
		if fields[0] != "total" || fields[1] != "1" {
			t.Fatalf("copied reply counted more than once: %q\n%s", rows[len(rows)-2], out)
		}
		if len(rows) != 4 || strings.Split(rows[1], "\t")[0] == "abcdef12" {
			t.Fatalf("copied subagent needs one distinct session row:\n%s", out)
		}
	})

	t.Run("same message id with different request ids", func(t *testing.T) {
		e := reviewHome(t)
		parents := []string{"33331111-0000-4000-8000-000000000001", "44442222-0000-4000-8000-000000000002"}
		for i, parent := range parents {
			row := usageRecord("same-message", "claude-opus-5-5", "synthetic reply", 10, 0, 0, 20)
			row = strings.ReplaceAll(row, "2026-10-25T10:00:00Z", e.now.Format(time.RFC3339Nano))
			row = strings.ReplaceAll(row, "req-same-message", fmt.Sprintf("req-copy-%d", i))
			writeReviewClaude(t, e, "synthetic", parent, `{"type":"agent-name","agentName":"synthetic-parent"}`)
			writeWorkFile(t, filepath.Join(e.home, ".claude", "projects", "synthetic", parent, "subagents", "agent-abcdef123456000001.jsonl"), []string{row})
		}
		code, out, stderr := runTL(t, e, "--usage", "--all", "--since", "2026-11-13T00:00:00Z")
		if code != 0 || stderr != "" || !strings.Contains(out, "2 session(s), 2 reply(ies)") {
			t.Fatalf("different requests in copied agent files were merged: code %d, stderr %q, output:\n%s", code, stderr, out)
		}
	})

	t.Run("equal totals have stable unique session ids", func(t *testing.T) {
		e := reviewHome(t)
		ids := []string{"33330000-0001-4000-8000-000000000003", "33330000-0002-4000-8000-000000000004"}
		for i, id := range ids {
			row := strings.ReplaceAll(usageRecord(fmt.Sprintf("message-%d", i), "claude-opus-5-5", "synthetic reply", 10, 20, 30, 40), "2026-10-25T10:00:00Z", e.now.Format(time.RFC3339Nano))
			writeReviewClaude(t, e, "synthetic", id, row)
		}
		code, out, stderr := runTL(t, e, "--usage", "--all", "--since", "2026-11-13T00:00:00Z")
		if code != 0 || stderr != "" {
			t.Fatalf("usage code %d, stderr %q, output:\n%s", code, stderr, out)
		}
		rows := strings.Split(strings.TrimSpace(out), "\n")
		if len(rows) != 5 || !strings.HasPrefix(rows[1], ids[0][:13]+"\t") || !strings.HasPrefix(rows[2], ids[1][:13]+"\t") {
			t.Fatalf("equal-usage rows are unstable or session IDs are not unique:\n%s", out)
		}
	})

	t.Run("equal percentage totals have stable order", func(t *testing.T) {
		e := reviewHome(t)
		rates := filepath.Join(e.home, ".config", "akit", "rates.tsv")
		writeWorkFile(t, rates, []string{
			"valid_from\tprovider\tplan_usd_month\tapi_usd_week",
			"2026-01-01\tanthropic\t200\t1000",
		})
		ids := []string{"55550000-0001-4000-8000-000000000003", "55550000-0002-4000-8000-000000000004"}
		for i, id := range ids {
			row := strings.ReplaceAll(usageRecord(fmt.Sprintf("pct-message-%d", i), "claude-opus-5-5", "synthetic reply", 10, 20, 30, 40), "2026-10-25T10:00:00Z", e.now.Format(time.RFC3339Nano))
			writeReviewClaude(t, e, "synthetic", id, row)
		}
		code, out, stderr := runTL(t, e, "--usage", "--all", "--since", "2026-11-13T00:00:00Z")
		if code != 0 || stderr != "" {
			t.Fatalf("usage code %d, stderr %q, output:\n%s", code, stderr, out)
		}
		rows := strings.Split(strings.TrimSpace(out), "\n")
		if len(rows) != 5 || !strings.HasPrefix(rows[1], ids[0][:13]+"\t") || !strings.HasPrefix(rows[2], ids[1][:13]+"\t") {
			t.Fatalf("equal-percentage rows are not stable:\n%s", out)
		}
	})
}

func TestCodexFindMatchesPrefixOnlyAtSessionUUID(t *testing.T) {
	e := reviewHome(t)
	target := "deadbeef-0001-7000-8000-000000000001"
	writeReviewCodex(t, e, "target-worker", target, "X target marker", e.now)
	writeReviewCodex(t, e, "embedded-worker", "11111111-deadbeef-7000-8000-000000000002", "X embedded marker", e.now.Add(time.Minute))
	code, out, stderr := runTL(t, e, "deadbeef")
	if code != 0 || stderr != "" || !strings.Contains(out, "X target marker") || strings.Contains(out, "embedded marker") {
		t.Fatalf("prefix matched away from UUID start: code %d, stderr %q, output:\n%s", code, stderr, out)
	}
}

func TestBadLineDiagnosticUsesUniqueLinkID(t *testing.T) {
	e := reviewHome(t)
	ids := []string{"abababab-0001-7000-8000-000000000001", "abababab-0002-7000-8000-000000000002"}
	for i, id := range ids {
		path := writeReviewCodex(t, e, fmt.Sprintf("bad-worker-%d", i), id, "X marker", e.now.Add(time.Duration(i)*time.Minute))
		if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, out, stderr := runTL(t, e, "--all", "--codex", "--since", "2026-11-13T00:00:00Z")
	if code != exitNone || stderr != "" {
		t.Fatalf("bad transcript code %d, stderr %q, output:\n%s", code, stderr, out)
	}
	if !strings.Contains(out, "first: "+ids[0][:13]+" line") {
		t.Fatalf("bad-line diagnostic does not show selected unique id:\n%s", out)
	}
}

func TestSubagentIDsAndRolloutIDsShareOneCollisionDomain(t *testing.T) {
	e := reviewHome(t)
	ids := []string{"88888888-0001-7000-8000-000000000001", "88888888-0002-7000-8000-000000000002"}
	for i, id := range ids {
		writeReviewCodex(t, e, fmt.Sprintf("codex-%d", i), id, "X rollout marker", e.now.Add(time.Duration(i)*time.Minute))
	}
	parent := "99990000-0000-4000-8000-000000000001"
	writeReviewClaude(t, e, "synthetic", parent)
	childID := "88888888aaaa000001"
	writeWorkFile(t, filepath.Join(e.home, ".claude", "projects", "synthetic", parent, "subagents", "agent-"+childID+".jsonl"), []string{`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:01Z","message":{"role":"user","content":"X child marker"}}`})
	code, out, stderr := runTL(t, e, "--all", "--codex", "--grep", "X", "--since", "2026-11-13T00:00:00Z")
	if code != 0 || stderr != "" {
		t.Fatalf("listing code %d, stderr %q, output:\n%s", code, stderr, out)
	}
	for _, ref := range reviewEventRefs(out) {
		if len(ref[0]) <= 8 {
			t.Errorf("cross-kind collision kept short id %q", ref[0])
		}
		code, opened, stderr := runTL(t, e, ref[0]+":"+ref[1])
		if code != 0 || stderr != "" || !strings.Contains(opened, "marker") {
			t.Errorf("cross-kind reference %q failed: code %d, stderr %q, output %q", ref[0], code, stderr, opened)
		}
		if strings.Contains(opened, "child marker") && ref[0] != "88888888a" {
			t.Errorf("subagent link should use its filename ID prefix, got %q", ref[0])
		}
	}
	code, _, stderr = runTL(t, e, "88888888")
	if code != exitUsage || !strings.Contains(stderr, childID) || strings.Contains(stderr, childID+".jsonl") {
		t.Errorf("ambiguous candidates do not name the full agent ID: code %d, stderr %q", code, stderr)
	}
}

func TestRolloutIDsDifferingOnlyAtFinalCharacterGetFullLength(t *testing.T) {
	e := reviewHome(t)
	ids := []string{"fedcba98-1234-7000-8000-000000000001", "fedcba98-1234-7000-8000-000000000002"}
	for i, id := range ids {
		writeReviewCodex(t, e, fmt.Sprintf("worker-%d", i), id, "X marker", e.now.Add(time.Duration(i)*time.Minute))
	}
	code, out, stderr := runTL(t, e, "--all", "--codex", "--grep", "X", "--since", "2026-11-13T00:00:00Z")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d, stderr %q, output:\n%s", code, stderr, out)
	}
	refs := reviewEventRefs(out)
	if len(refs) != 2 || refs[0][0] != ids[0] || refs[1][0] != ids[1] {
		t.Fatalf("refs must extend through the final differing character: %v\n%s", refs, out)
	}
}

func TestLinkIDAvoidsLiveSessionName(t *testing.T) {
	e := reviewHome(t)
	rolloutID := "12345678-0001-7000-8000-000000000001"
	writeReviewCodex(t, e, "worker", rolloutID, "X rollout marker", e.now)
	otherID := "87654321-0000-4000-8000-000000000002"
	writeReviewClaude(t, e, "synthetic", otherID, `{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:00Z","message":{"role":"user","content":"live marker"}}`)
	stat, err := proc.ReadStat("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(e.home, ".claude", "sessions")
	writeWorkFile(t, filepath.Join(registry, fmt.Sprintf("%d.json", os.Getpid())), []string{
		fmt.Sprintf(`{"pid":%d,"procStart":%q,"name":"12345678","sessionId":%q}`, os.Getpid(), stat.Start, otherID),
	})
	code, out, stderr := runTL(t, e, "--all", "--codex", "--grep", "X", "--since", "2026-11-13T00:00:00Z")
	if code != 0 || stderr != "" {
		t.Fatalf("listing code %d, stderr %q, output:\n%s", code, stderr, out)
	}
	refs := reviewEventRefs(out)
	if len(refs) != 1 || refs[0][0] != rolloutID[:9] {
		t.Fatalf("link collides with live session name: %v\n%s", refs, out)
	}
	code, opened, stderr := runTL(t, e, refs[0][0]+":"+refs[0][1])
	if code != 0 || stderr != "" || !strings.Contains(opened, "rollout marker") || strings.Contains(opened, "live marker") {
		t.Fatalf("link opened wrong source: code %d, stderr %q, output:\n%s", code, stderr, opened)
	}
}

func TestUsageKeepsIndependentRepliesWithMatchingIDs(t *testing.T) {
	e := reviewHome(t)
	ids := []string{"abab1234-0001-4000-8000-000000000001", "abab1234-0002-4000-8000-000000000002"}
	for i, id := range ids {
		row := usageRecord("same-message", "claude-opus-5-5", "synthetic reply", 10, 0, 0, int64(i+1))
		row = strings.ReplaceAll(row, "2026-10-25T10:00:00Z", e.now.Format(time.RFC3339Nano))
		row = strings.ReplaceAll(row, "req-same-message", fmt.Sprintf("req-%d", i))
		writeReviewClaude(t, e, "synthetic", id, row)
	}
	code, out, stderr := runTL(t, e, "--usage", "--all", "--since", "2026-11-13T00:00:00Z")
	if code != 0 || stderr != "" {
		t.Fatalf("usage code %d, stderr %q, output:\n%s", code, stderr, out)
	}
	if !strings.Contains(out, "2 session(s), 2 reply(ies)") {
		t.Fatalf("same message id in independent sessions was merged:\n%s", out)
	}
}

func TestExternalTranscriptReferenceUsesPath(t *testing.T) {
	e := reviewHome(t)
	path := filepath.Join(t.TempDir(), "cccc0000-0000-4000-8000-000000000001.jsonl")
	writeWorkFile(t, path, []string{`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:00Z","message":{"role":"user","content":"X external marker"}}`})
	code, out, stderr := runTL(t, e, path)
	if code != 0 || stderr != "" {
		t.Fatalf("path listing code %d, stderr %q, output:\n%s", code, stderr, out)
	}
	refs := reviewEventRefs(out)
	if len(refs) != 1 || refs[0][0] != path {
		t.Fatalf("external transcript link is not a path: %v\n%s", refs, out)
	}
	code, opened, stderr := runTL(t, e, refs[0][0]+":"+refs[0][1])
	if code != 0 || stderr != "" || !strings.Contains(opened, "external marker") {
		t.Fatalf("external reference failed: code %d, stderr %q, output:\n%s", code, stderr, opened)
	}
}

func TestUsageDeduplicationUsesMessageIDBeforeRequestID(t *testing.T) {
	e := reviewHome(t)
	id := "abab5678-0000-4000-8000-000000000001"
	first := strings.ReplaceAll(usageRecord("same-message", "claude-opus-5-5", "first part", 10, 0, 0, 2), "2026-10-25T10:00:00Z", e.now.Format(time.RFC3339Nano))
	second := usageRecord("same-message", "claude-opus-5-5", "last part", 10, 0, 0, 20)
	second = strings.ReplaceAll(second, "2026-10-25T10:00:00Z", e.now.Format(time.RFC3339Nano))
	second = strings.ReplaceAll(second, "req-same-message", "req-other")
	writeReviewClaude(t, e, "synthetic", id, first, second)
	code, out, stderr := runTL(t, e, "--usage", id)
	if code != 0 || stderr != "" || !strings.Contains(out, "1 session(s), 1 reply(ies)") {
		t.Fatalf("reply parts with shared message ID were not deduplicated: code %d, stderr %q, output:\n%s", code, stderr, out)
	}
}

func TestUsageKeepsRepliesWithoutIDsInDistinctSessions(t *testing.T) {
	e := reviewHome(t)
	ids := []string{"abab9876-0001-4000-8000-000000000001", "abab9876-0002-4000-8000-000000000002"}
	for _, id := range ids {
		row := strings.ReplaceAll(usageRecord("", "claude-opus-5-5", "synthetic reply", 10, 0, 0, 2), "2026-10-25T10:00:00Z", e.now.Format(time.RFC3339Nano))
		row = strings.ReplaceAll(row, `"requestId":"req-"`, `"requestId":""`)
		writeReviewClaude(t, e, "synthetic", id, row)
	}
	code, out, stderr := runTL(t, e, "--usage", "--all", "--since", "2026-11-13T00:00:00Z")
	if code != 0 || stderr != "" || !strings.Contains(out, "2 session(s), 2 reply(ies)") {
		t.Fatalf("id-less replies in distinct sessions were merged: code %d, stderr %q, output:\n%s", code, stderr, out)
	}
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if strings.HasPrefix(rows[1], "abab9876\t") || strings.HasPrefix(rows[2], "abab9876\t") {
		t.Fatalf("session column does not use unique links:\n%s", out)
	}
}

func TestHeadersAppearOncePerSourceWithRepeatedHits(t *testing.T) {
	e := reviewHome(t)
	ids := []string{"cdcd0000-0001-4000-8000-000000000001", "cdcd0000-0002-4000-8000-000000000002"}
	for i, id := range ids {
		writeReviewClaude(t, e, "synthetic", id,
			fmt.Sprintf(`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:0%dZ","message":{"role":"user","content":"X first"}}`, i),
			fmt.Sprintf(`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:1%dZ","message":{"role":"user","content":"X second"}}`, i))
	}
	code, out, stderr := runTL(t, e, "--all", "--grep", "X", "--since", "2026-11-13T00:00:00Z")
	if code != 0 || stderr != "" || strings.Count(out, "== cdcd") != 2 {
		t.Fatalf("want one header per source with multiple hits: code %d, stderr %q, output:\n%s", code, stderr, out)
	}
}

func TestSubagentHeaderUsesUniqueParentIDOutsideSelection(t *testing.T) {
	e := reviewHome(t)
	parentIDs := []string{"dddd0000-0000-4000-8000-000000000001", "dddd0000-0000-4000-8000-000000000002"}
	for _, parent := range parentIDs {
		writeReviewClaude(t, e, "synthetic", parent, `{"type":"agent-name","agentName":"synthetic-parent"}`)
	}
	child := filepath.Join(e.home, ".claude", "projects", "synthetic", parentIDs[0], "subagents", "agent-eeeeeeee00000001.jsonl")
	writeWorkFile(t, child, []string{`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:00Z","message":{"role":"user","content":"X child"}}`})
	old := e.now.Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(e.home, ".claude", "projects", "synthetic", parentIDs[0]+".jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(e.home, ".claude", "projects", "synthetic", parentIDs[1]+".jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runTL(t, e, "--all", "--grep", "X", "--project", "synthetic", "--since", "7d")
	if code != 0 || stderr != "" || !strings.Contains(out, "sub of "+parentIDs[0][:13]) {
		t.Fatalf("parent display id not unique outside selection: code %d, stderr %q, output:\n%s", code, stderr, out)
	}
}

func TestSubagentSourceMetadataErrorIsReported(t *testing.T) {
	e := reviewHome(t)
	parent := "eeee1111-0000-4000-8000-000000000001"
	path := filepath.Join(e.home, ".claude", "projects", "synthetic", parent, "subagents", "agent-eeee2222.jsonl")
	writeWorkFile(t, path, []string{`{"parentUuid":null,"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-14T08:00:00Z","message":{"role":"user","content":"X child"}}`})
	if err := os.Mkdir(strings.TrimSuffix(path, ".jsonl")+".meta.json", 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runTL(t, e, "eeee2222:1")
	if code != exitUsage || !strings.Contains(stderr, "metadata") {
		t.Fatalf("subagent source metadata error was ignored: code %d, stderr %q", code, stderr)
	}
}
