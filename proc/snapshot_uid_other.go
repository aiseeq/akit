//go:build !linux

package proc

import "os"

func fileUID(os.FileInfo) (int, bool) { return -1, false }

func noSuchProcess(error) bool { return false }
