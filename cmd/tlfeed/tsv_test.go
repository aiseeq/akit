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
	}, 7)
	fields := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\t")
	if len(fields) != 26 {
		t.Fatalf("event row has %d columns, want 26: %q", len(fields), b.String())
	}
	if fields[6] != "7" {
		t.Fatalf("event sequence = %q, want 7", fields[6])
	}
	if fields[2] != "agent" {
		t.Fatalf("subagent event session = %q, want source session", fields[2])
	}
	if fields[25] != "session" {
		t.Fatalf("subagent usage session = %q, want parent session", fields[25])
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
	if fields[20] != "0" || fields[21] != "5000" || fields[22] != "true" || fields[23] != "us" {
		t.Fatalf("price fields were not written to event row: %q", b.String())
	}
}

func TestEventRowCodexUsesFullRolloutAndSkipsMissingUsage(t *testing.T) {
	var rows []string
	for _, name := range []string{"rollout-2026-11-25T10-00-00-0199abcd-1111-7000-8000-000000000001.jsonl", "rollout-2026-11-25T10-00-00-0199abcd-2222-7000-8000-000000000002.jsonl"} {
		var b bytes.Buffer
		eventRow(&b, &file{kind: "codex", src: transcript.Source{ID: "0199abcd", Path: "/synthetic/" + name}}, &transcript.Event{
			Line: 12, Tokens: &transcript.Tokens{Model: "gpt-5", Last: &transcript.TokenUsage{Input: 10, CachedInput: 4, Output: 3}},
		}, 0)
		fields := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\t")
		// model, reply key, request, input without the cached part, output, cache read, cache write
		if got, want := strings.Join(fields[13:20], ","), "gpt-5,codex:12,,6,3,4,0"; got != want {
			t.Errorf("codex usage fields %q, want %q", got, want)
		}
		want := strings.TrimSuffix(name, ".jsonl")
		if fields[25] != want {
			t.Errorf("usage session %q, want full rollout id %q", fields[25], want)
		}
		rows = append(rows, fields[25])
	}
	if rows[0] == rows[1] {
		t.Fatalf("rollouts sharing a prefix collapsed to %q", rows[0])
	}
	var b bytes.Buffer
	eventRow(&b, &file{src: transcript.Source{ID: "session"}}, &transcript.Event{Reply: &transcript.Reply{ID: "without-usage", Model: "claude-opus-5-5"}}, 0)
	fields := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\t")
	if fields[14] != "" || fields[15] != "" {
		t.Fatalf("reply without usage was included in usage keys: %q", fields[14:16])
	}
}

// A Codex report repeating the previous total is no call: the row has no usage key, and writing it does not panic.
func TestEventRowCodexRepeatHasNoUsageKey(t *testing.T) {
	var b bytes.Buffer
	eventRow(&b, &file{kind: "codex", src: transcript.Source{ID: "0199abcd", Path: "/synthetic/rollout-x.jsonl"}}, &transcript.Event{
		Line: 7, Tokens: &transcript.Tokens{Model: "gpt-5", Repeat: true, Last: &transcript.TokenUsage{Input: 10, Output: 3}},
	}, 0)
	fields := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\t")
	if got := strings.Join(fields[13:20], ","); got != ",,,0,0,0,0" {
		t.Fatalf("repeated report usage fields %q, want none", got)
	}
}
