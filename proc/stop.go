package proc

// StopResult reports an already exited target and the number sent SIGKILL.
// Killed is informational: a process is identified by PID and start time,
// which has clock-tick resolution, so a PID reused within the same tick is
// counted once even if both processes were sent SIGKILL.
type StopResult struct {
	AlreadyExited bool
	Killed        int
}
