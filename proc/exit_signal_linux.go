//go:build linux

package proc

import (
	"os"
	"os/signal"
	"syscall"
	"time"
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
	default:
		exitSignalCode(128 + int(sig))
	}
	if sendExitSignal(sig) == nil {
		time.Sleep(100 * time.Millisecond)
	}
	exitSignalCode(128 + int(sig))
}
