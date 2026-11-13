package proc

import "syscall"

// ExitWithSignal terminates the current process and never returns. On Linux it
// resets HUP, INT and TERM and sends the signal to itself; KILL is sent directly.
// An inherited ignored disposition or blocked signal can prevent delivery, so
// it exits with 128+sig after a short wait if still alive. Other valid signals
// use that exit code immediately; invalid numbers use exit code 1. Outside
// Linux all valid signals use 128+sig.
//
// Go's Reset restores runtime signal behavior, not necessarily SIG_DFL: QUIT
// and ABRT dump stacks, a sent SEGV crashes, and sent PIPE, USR1 and ALRM
// normally do nothing. Changing process-wide handlers with raw sigaction would
// interfere with the runtime and requires architecture-specific kernel layouts.
func ExitWithSignal(sig syscall.Signal) {
	if sig <= 0 || sig > 64 {
		exitSignalCode(1)
	}
	exitWithSignal(sig)
}
