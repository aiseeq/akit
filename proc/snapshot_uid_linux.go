//go:build linux

package proc

import (
	"errors"
	"os"
	"syscall"
)

func fileUID(fi os.FileInfo) (int, bool) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, false
	}
	return int(sys.Uid), true
}

func noSuchProcess(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}
