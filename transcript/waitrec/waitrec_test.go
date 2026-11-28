package waitrec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/aiseeq/akit/transcript"
)

type outputFixture struct {
	Events []eventFixture `json:"events"`
	Done   []doneFixture  `json:"done"`
}

type eventFixture struct {
	Role      transcript.Role `json:"role"`
	Who       string          `json:"who"`
	CallID    string          `json:"call_id"`
	Time      string          `json:"time"`
	MessageID string          `json:"message_id"`
	Sidechain bool            `json:"sidechain"`
	Text      string          `json:"text"`
	Raw       string          `json:"raw"`
	Error     bool            `json:"error"`
}

type doneFixture struct {
	Line      int    `json:"line"`
	Started   string `json:"started"`
	Completed string `json:"completed"`
	Command   string `json:"command"`
}

func TestVersionMatchesTranscriptOutput(t *testing.T) {
	data, err := os.ReadFile("testdata/fields.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture outputFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var output []byte
	appendOutput := func(value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, data...)
		output = append(output, '\n')
	}
	for i, item := range fixture.Events {
		at, err := time.Parse(time.RFC3339, item.Time)
		if err != nil {
			t.Fatalf("event %d time: %v", i, err)
		}
		event := transcript.Event{Line: i + 1, Time: at, Role: item.Role, Who: item.Who, CallID: item.CallID,
			MessageID: item.MessageID, Sidechain: item.Sidechain, Text: item.Text, Raw: json.RawMessage(item.Raw), Error: item.Error}
		record := FromEvent(event)
		appendOutput(struct {
			Record Record
			Keep   bool
		}{Record: record, Keep: Keep(record)})
	}
	for i, item := range fixture.Done {
		started, err := time.Parse(time.RFC3339, item.Started)
		if err != nil {
			t.Fatalf("completion %d start: %v", i, err)
		}
		completed, err := time.Parse(time.RFC3339, item.Completed)
		if err != nil {
			t.Fatalf("completion %d end: %v", i, err)
		}
		record, ok := Done(transcript.CodexCommandResult{Line: item.Line, Started: started, Completed: completed, Command: item.Command})
		appendOutput(struct {
			Record Record
			OK     bool
		}{Record: record, OK: ok})
	}
	got := sha256.Sum256(output)
	// Each output interpretation is bound to its version. Add a new history entry when bumping Version.
	golden := map[int]string{
		1: "a12a16d5a01e843335153302748a97ad85b28997b1c3ec57913a926bf4d8a739",
	}
	want, ok := golden[Version]
	if !ok {
		t.Fatalf("waitrec Version=%d has no output hash; add it to the version history", Version)
	}
	seen := map[string]int{}
	for version, hash := range golden {
		if previous, duplicate := seen[hash]; duplicate {
			t.Fatalf("versions %d and %d have the same output hash", previous, version)
		}
		seen[hash] = version
	}
	if hex.EncodeToString(got[:]) != want {
		t.Fatalf("waitrec output changed without updating Version: Version=%d sha256=%x; add a history entry and increment Version", Version, got)
	}
}

func TestDoneRejectsZeroCompletionTime(t *testing.T) {
	result := transcript.CodexCommandResult{Line: 7, Started: time.UnixMilli(1), Completed: time.UnixMilli(0), Command: "run"}
	if record, ok := Done(result); ok || record.Started != result.Started || record.Line != result.Line || record.Command != result.Command || !record.Time.IsZero() {
		t.Fatalf("Done = (%+v, %v), want identity with empty end and false", record, ok)
	}
}

func TestDoneAcceptsValidCompletionIncludingEmptyCommand(t *testing.T) {
	result := transcript.CodexCommandResult{Line: 7, Started: time.UnixMilli(1), Completed: time.UnixMilli(2)}
	record, ok := Done(result)
	if !ok || record.Time != result.Completed || record.Started != result.Started || record.Line != result.Line || record.Command != "" {
		t.Fatalf("Done = (%+v, %v), want valid result with empty command", record, ok)
	}
}

func TestIsRollout(t *testing.T) {
	if !IsRollout("/some/path/rollout-2026-11-27.jsonl") || IsRollout("session.jsonl") {
		t.Fatal("unexpected rollout classification")
	}
}
