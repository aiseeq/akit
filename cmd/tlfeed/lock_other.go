//go:build !unix

package main

import (
	"errors"
	"os"
)

func tryLock(*os.File) (bool, error) {
	return false, errors.New("no flock on this system")
}
