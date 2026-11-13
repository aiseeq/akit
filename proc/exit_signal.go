package proc

import "syscall"

// ExitWithSignal terminates the current process and never returns, so that a
// waiting parent sees the same death as the child had. On Linux:
//   - HUP, INT and TERM: signal.Reset and the signal to itself. An inherited
//     ignored disposition is kept, so the process then exits 128+sig after a
//     short wait.
//   - KILL: sent to itself.
//   - Signals whose default action stops the process or does nothing (STOP,
//     TSTP, TTIN, TTOU, CHLD, URG, WINCH, CONT): exit 128+sig at once.
//   - Every other signal (QUIT, ABRT, SEGV, PIPE, USR1, ALRM, real-time…), which
//     the Go runtime catches: the default action is restored by a raw
//     rt_sigaction past the runtime, the signal unblocked on this thread and
//     sent to itself. The process is made non-dumpable first, so a core-dumping
//     signal leaves no second core: the crash is the child's.
//
// Should the process survive delivery, it exits 128+sig. Another goroutine
// calling os/signal for the same signal meanwhile may put a Go handler back:
// the outcome is then the runtime's (QUIT, for one, dumps stacks and exits 2)
// or 128+sig, so do not race ExitWithSignal with os/signal. Numbers outside
// 1..64 are invalid and exit 1 (MIPS signals 65..128 included). Outside Linux
// all valid signals exit 128+sig. Signals go only to the current process.
func ExitWithSignal(sig syscall.Signal) {
	if sig <= 0 || sig > 64 {
		exitSignalCode(1)
	}
	exitWithSignal(sig)
}
