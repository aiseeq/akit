package transcript

import (
	"bytes"
	"maps"
	"os"
	"testing"
)

// callsByDecode — what rememberTools on a fresh parser records for a line: the reference for toolCalls.
func callsByDecode(raw []byte) map[string]string {
	tmp := reader{claude: newClaudeParser(), toolNames: make(map[string]struct{})}
	tmp.rememberTools(raw)
	return tmp.claude.tools
}

// toolCalls gives the calls json.Unmarshal gives for a plain line and gives up on a line it cannot tell.
func TestToolCallsEqualDecode(t *testing.T) {
	data, err := os.ReadFile("testdata/claude.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	call := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"a\nb"},` +
		`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls","n":[1,-2.5e3,true,null]}}]}}`
	cases := map[string]bool{ // line → toolCalls is expected to answer itself
		call: true,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash"},{"type":"tool_use","id":"t2","name":"Read"}]}}`: true,
		`{"type":"user","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`:                                                  true,
		`{"type":"assistant","payload":{"x":1},"message":{"content":[{"type":"tool_use","id":"t1","name":"B"}]}}`:                              true,
		`{"type":"assistant","message":{"content":"text with \"tool_use\""}}`:                                                                  true,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","ID":"t1","name":"Bash"}]}}`:                                             false,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","id":"t2","name":"Bash"}]}}`:                                   false,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t\u0031","name":"Bash"}]}}`:                                        false,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","text":5}]}}`:                                    false,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash"}]}`:                                              false,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash"}]}} x`:                                           false,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":01}]}}`:                                  false,
		`{"type":"assistant","message":{"content":["tool_use"]}}`:                                                                              false,
		`{"type":"assistant","Message":{"content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`:                                             false,
	}
	for _, l := range lines {
		if bytes.Contains(l, []byte(`"tool_use"`)) {
			cases[string(l)] = true
		}
	}
	for line, plain := range cases {
		got, ok := toolCalls([]byte(line))
		if ok != plain {
			t.Errorf("ok %t, want %t: %s", ok, plain, line)
		}
		if !ok {
			continue
		}
		m := map[string]string{}
		for _, c := range got {
			m[c.id] = c.name
		}
		if want := callsByDecode([]byte(line)); !maps.Equal(m, want) {
			t.Errorf("calls %v, json.Unmarshal %v: %s", m, want, line)
		}
	}
}
