package main

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aiseeq/akit/transcript"
)

func TestProbeAlternateWithoutLiteral(t *testing.T) {
	cond, params, err := grepCondition("kk|needle")
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{"param_files": {"[1]"}, "param_kinds": {"['claude']"}, "param_roles": {"['user']"}}
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	ch := &fakeCH{t: t}
	if !ch.match(q, cond, &chFile{id: 1, kind: "claude"}, chEvent{ev: transcript.Event{Role: transcript.User, Text: "kk"}}) {
		t.Fatalf("branch without a literal lost: %q", cond)
	}
}

func TestProbeNoSubagentsDB(t *testing.T) {
	e, paths, unloaded := dbCorpus(t)
	ch := newFakeCH(t, paths, unloaded)
	srv := httptest.NewServer(ch)
	defer srv.Close()
	t.Setenv("TL_CLICKHOUSE_URL", srv.URL)
	probeCompare(t, e, "no subagents", "--all", "--no-subagents", "--role", "all", "--max", "0")
}
