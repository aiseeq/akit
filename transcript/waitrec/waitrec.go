// Package waitrec extracts the transcript fields used to track command waits.
package waitrec

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/aiseeq/akit/transcript"
	transcriptwaits "github.com/aiseeq/akit/transcript/waits"
)

// Version identifies the interpretation of transcript records in this package.
const Version = 1

// Record contains the transcript data needed by the wait tracker and its cache.
type Record struct {
	Role      transcript.Role
	CallID    string
	Time      time.Time
	Started   time.Time
	Line      int
	MessageID string
	Who       string
	Sidechain bool
	Command   string
	BG        bool
	Poll      bool
	Running   bool
	Session   string
	// ErrText stores the full raw JSON for an error output; it is unbounded so refusal parsing sees the source text.
	ErrText string
}

// FromEvent extracts all wait-related fields from one parsed transcript event.
func FromEvent(ev transcript.Event) Record {
	r := Record{Role: ev.Role, CallID: ev.CallID, Time: ev.Time, Line: ev.Line, MessageID: ev.MessageID,
		Who: ev.Who, Sidechain: ev.Sidechain}
	switch ev.Role {
	case transcript.Cmd:
		r.Command, r.BG = command(ev)
		r.Poll, r.Session = pollSession(ev.Who, r.Command, ev.Raw)
	case transcript.Out:
		if ev.Error {
			r.ErrText = string(ev.Raw)
		}
		r.Session, r.Running = transcriptwaits.Running(ev.Text)
	}
	return r
}

// Done converts a Codex command completion. A zero timestamp keeps its identity but has no usable end time.
func Done(result transcript.CodexCommandResult) (Record, bool) {
	record := Record{Started: result.Started, Line: result.Line, Command: result.Command}
	if result.Completed.IsZero() || result.Completed.UnixMilli() == 0 {
		return record, false
	}
	return Record{Time: result.Completed, Started: result.Started, Line: result.Line, Command: result.Command}, true
}

// Keep reports whether a record participates in wait tracking.
func Keep(r Record) bool {
	switch r.Role {
	case transcript.Cmd:
		if r.Poll {
			return true
		}
		return r.Command != "" || r.MessageID != "" && !r.Sidechain && isSerialTool(r.Who)
	case transcript.Out:
		return true
	default:
		return false
	}
}

// Serial reports whether a tool participates in Claude's sequential message chain.
func Serial(who string) bool { return isSerialTool(who) }

// IsRollout reports whether path names a Codex rollout transcript.
func IsRollout(path string) bool { return transcript.IsRollout(path) }

func command(ev transcript.Event) (string, bool) {
	if ev.Who == "Bash" {
		var input struct {
			Command         string `json:"command"`
			RunInBackground bool   `json:"run_in_background"`
		}
		if json.Unmarshal(ev.Raw, &input) == nil {
			if input.Command != "" {
				return input.Command, input.RunInBackground
			}
			return ev.Text, input.RunInBackground
		}
	}
	if ev.Who != "exec" && ev.Who != "exec_command" && ev.Who != "shell" {
		return "", false
	}
	if ev.Text != "" {
		return ev.Text, false
	}
	var input struct {
		Cmd     string `json:"cmd"`
		Command string `json:"command"`
	}
	if json.Unmarshal(ev.Raw, &input) == nil {
		if input.Cmd != "" {
			return input.Cmd, false
		}
		return input.Command, false
	}
	return "", false
}

func pollSession(who, command string, raw []byte) (bool, string) {
	if who != "write_stdin" && !(who == "exec" && strings.Contains(command, "write_stdin(") && !strings.Contains(command, "exec_command(")) {
		return false, ""
	}
	session := transcriptwaits.SessionOf(transcript.Event{Text: command, Raw: raw})
	return session != "", session
}

func isSerialTool(tool string) bool {
	switch tool {
	case "Read", "Grep", "Glob", "WebFetch", "WebSearch", "Agent", "Task", "TaskCreate", "TaskGet", "TaskList", "TaskOutput", "TaskStop":
		return false
	}
	return !strings.HasPrefix(tool, "Task")
}
