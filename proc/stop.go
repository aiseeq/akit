package proc

import "errors"

// StopResult reports an already exited target and the number sent SIGKILL.
// Killed is informational: a process is identified by PID and start time,
// which has clock-tick resolution, so a PID reused within the same tick is
// counted once even if both processes were sent SIGKILL.
type StopResult struct {
	AlreadyExited bool
	Killed        int
}

// ErrNoStartRecord marks a session record without a usable /proc start time.
var ErrNoStartRecord = errors.New("session record has no start time")

// SessionRecord identifies a detached session by its leader PID and /proc start time.
type SessionRecord struct {
	Pid   int
	Start string
}
