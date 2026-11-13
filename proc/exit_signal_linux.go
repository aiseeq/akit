//go:build linux

package proc

import (
	"os"
	"os/signal"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

var exitSignalCode = os.Exit

var sendExitSignal = func(sig syscall.Signal) error {
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}
	return process.Signal(sig)
}

func exitWithSignal(sig syscall.Signal) {
	switch sig {
	case syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM:
		signal.Reset(sig)
	case syscall.SIGKILL:
	case syscall.SIGSTOP, syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU,
		syscall.SIGCHLD, syscall.SIGURG, syscall.SIGWINCH, syscall.SIGCONT:
		// The default action stops the process or does nothing: no death to copy.
		exitSignalCode(128 + int(sig))
	default:
		restoreDefaultAction(sig)
	}
	if sendExitSignal(sig) == nil {
		time.Sleep(100 * time.Millisecond)
	}
	exitSignalCode(128 + int(sig))
}

// restoreDefaultAction puts sig back to SIG_DFL past the Go runtime, which
// otherwise catches QUIT, ABRT, SEGV, PIPE, USR1 and the rest, and unblocks it
// on this thread. The process dies on the very next delivery, so the runtime
// never sees the change. It turns non-dumpable first: the crash, if the
// signal dumps core, is the child's, and a second core record would be ours.
// Errors only leave the fallback exit code in place.
func restoreDefaultAction(sig syscall.Signal) {
	_ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
	// A zeroed kernel sigaction is SIG_DFL with no flags and an empty mask
	// whatever the architecture's field order; the buffer is larger than any
	// layout, and the kernel reads an 8-byte mask (sigsetsize 8).
	var dfl [64]byte
	_, _, _ = unix.RawSyscall6(unix.SYS_RT_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(&dfl)), 0, 8, 0, 0)
	var set unix.Sigset_t
	n := uint(sig - 1)
	bits := uint(unsafe.Sizeof(set.Val[0])) * 8
	set.Val[n/bits] |= 1 << (n % bits)
	_ = unix.PthreadSigmask(unix.SIG_UNBLOCK, &set, nil)
}
