package proc

// StopResult reports an already exited target and the number sent SIGKILL.
type StopResult struct {
	AlreadyExited bool
	Killed        int
}
