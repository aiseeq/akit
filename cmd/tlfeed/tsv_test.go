package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aiseeq/akit/price"
	"github.com/aiseeq/akit/transcript"
)

func TestEventRowKeepsClaudePriceFields(t *testing.T) {
	const raw = `{"input_tokens":7,"output_tokens":90,"cache_read_input_tokens":0,"cache_creation_input_tokens":5000,"speed":"fast","inference_geo":"us","cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":5000}}`
	u, err := transcript.ParseUsage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	eventRow(&b, &file{src: transcript.Source{ID: "agent", ParentID: "session"}, kind: "sub"}, &transcript.Event{
		Line: 1, Role: transcript.Agent, Reply: &transcript.Reply{ID: "reply", RequestID: "request", Model: "claude-opus-5-5", Usage: u},
	})
	fields := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\t")
	if len(fields) != 25 {
		t.Fatalf("event row has %d columns, want 25: %q", len(fields), b.String())
	}
	if fields[2] != "agent" {
		t.Fatalf("subagent event session = %q, want source session", fields[2])
	}
	if fields[24] != "session" {
		t.Fatalf("subagent usage session = %q, want parent session", fields[24])
	}
	fromUsage := price.Usage{Input: 7, Output: 90, CacheRead: 0, CacheWrite: 5000, Write5m: 0, Write1h: 5000, Fast: true, Geo: "us"}
	fromRaw, err := transcript.ParseUsage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	fromColumns := price.Usage{Input: 7, Output: 90, CacheWrite: 5000, Write1h: 5000, Fast: true, Geo: "us"}
	got, ok := price.Cost("claude-opus-5-5", fromColumns)
	want, rawOK := price.Cost("claude-opus-5-5", *fromRaw)
	if !ok || !rawOK || got != want || fromUsage != *fromRaw {
		t.Fatalf("usage row price %.9f (ok %v), raw price %.9f (ok %v), columns %+v raw %+v", got, ok, want, rawOK, fromColumns, fromRaw)
	}
	if fields[19] != "0" || fields[20] != "5000" || fields[21] != "true" || fields[22] != "us" {
		t.Fatalf("price fields were not written to event row: %q", b.String())
	}
}

func TestEventRowCodexUsesFullRolloutAndSkipsMissingUsage(t *testing.T) {
	var rows []string
	for _, name := range []string{"rollout-2026-11-25T10-00-00-0199abcd-1111-7000-8000-000000000001.jsonl", "rollout-2026-11-25T10-00-00-0199abcd-2222-7000-8000-000000000002.jsonl"} {
		var b bytes.Buffer
		eventRow(&b, &file{kind: "codex", src: transcript.Source{ID: "0199abcd", Path: "/synthetic/" + name}}, &transcript.Event{
			Line: 12, Tokens: &transcript.Tokens{Model: "gpt-5", Last: &transcript.TokenUsage{Input: 10, CachedInput: 4, Output: 3}},
		})
		fields := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\t")
		// model, reply key, request, input without the cached part, output, cache read, cache write
		if got, want := strings.Join(fields[12:19], ","), "gpt-5,codex:12,,6,3,4,0"; got != want {
			t.Errorf("codex usage fields %q, want %q", got, want)
		}
		want := strings.TrimSuffix(name, ".jsonl")
		if fields[24] != want {
			t.Errorf("usage session %q, want full rollout id %q", fields[24], want)
		}
		rows = append(rows, fields[24])
	}
	if rows[0] == rows[1] {
		t.Fatalf("rollouts sharing a prefix collapsed to %q", rows[0])
	}
	var b bytes.Buffer
	eventRow(&b, &file{src: transcript.Source{ID: "session"}}, &transcript.Event{Reply: &transcript.Reply{ID: "without-usage", Model: "claude-opus-5-5"}})
	fields := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\t")
	if fields[13] != "" || fields[14] != "" {
		t.Fatalf("reply without usage was included in usage keys: %q", fields[13:15])
	}
}
