//go:build !linux

package proc

import (
	"os"
	"syscall"
)

var exitSignalCode = os.Exit

func exitWithSignal(sig syscall.Signal) {
	exitSignalCode(128 + int(sig))
}
