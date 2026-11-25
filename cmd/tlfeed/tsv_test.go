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
