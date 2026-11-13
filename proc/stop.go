package proc

import "errors"

// StopResult reports an already exited target, the number of distinct
// processes sent the requested signal by StopDescendants or StopSession, the
// number sent SIGKILL, and whether StopCmd had to fall back to signaling only
// the direct child.
// Killed is informational: a process is identified by PID and start time,
// which has clock-tick resolution, so a PID reused within the same tick is
// counted once even if both processes were sent SIGKILL.
type StopResult struct {
	AlreadyExited bool
	// Signaled counts distinct processes that received the requested signal.
	Signaled int
	Killed   int
	// Degraded reports that StopCmd used direct-child os.Process signaling
	// because pidfd_open was unavailable. Descendants were not inspected.
	Degraded bool
	// SignalUnsupported reports that the platform rejected the requested
	// signal (Windows delivers only Kill), so StopCmd killed the process at
	// once without the grace period.
	SignalUnsupported bool
}

// SignalResult reports whether SignalCmd had to use direct-child fallback.
type SignalResult struct {
	// Degraded reports fallback to unverified direct-child signaling.
	Degraded bool
}

// ErrPidfdUnavailable reports that pidfd_open is unavailable in this process.
var ErrPidfdUnavailable = errors.New("pidfd_open unavailable")

// ErrNoStartRecord marks a session record without a usable /proc start time.
var ErrNoStartRecord = errors.New("session record has no start time")

// ErrSessionLeaderGone reports that a recorded session leader cannot be
// verified as the original live leader, so its remaining members are unsafe
// to identify by session ID alone.
var ErrSessionLeaderGone = errors.New("recorded session leader is gone or changed")

// SessionRecord identifies a detached session by its leader PID and /proc start time.
type SessionRecord struct {
	Pid   int
	Start string
}
