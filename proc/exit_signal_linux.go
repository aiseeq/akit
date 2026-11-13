//go:build linux

package proc

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

var exitSignalCode = os.Exit

// restoreSignalDefault is restoreDefaultAction; tests replace it to reach the
// fallback when the default action cannot be restored.
var restoreSignalDefault = restoreDefaultAction

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
		if restoreSignalDefault(sig) != nil {
			// The runtime still catches sig: sending it would dump stacks or
			// do nothing, not end the process the way the child ended.
			exitSignalCode(128 + int(sig))
		}
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
func restoreDefaultAction(sig syscall.Signal) error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("make process non-dumpable: %w", err)
	}
	// A zeroed kernel sigaction is SIG_DFL with no flags and an empty mask
	// whatever the architecture's field order; the buffer is larger than any
	// layout. The kernel accepts only its own mask size: 128 signals on MIPS,
	// 64 elsewhere.
	var dfl [64]byte
	sigsetSize := uintptr(8)
	if strings.HasPrefix(runtime.GOARCH, "mips") {
		sigsetSize = 16
	}
	if _, _, errno := unix.RawSyscall6(unix.SYS_RT_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(&dfl)), 0, sigsetSize, 0, 0); errno != 0 {
		return fmt.Errorf("default action of %v: %w", sig, errno)
	}
	var set unix.Sigset_t
	n := uint(sig - 1)
	bits := uint(unsafe.Sizeof(set.Val[0])) * 8
	set.Val[n/bits] |= 1 << (n % bits)
	if err := unix.PthreadSigmask(unix.SIG_UNBLOCK, &set, nil); err != nil {
		return fmt.Errorf("unblock %v: %w", sig, err)
	}
	return nil
}
