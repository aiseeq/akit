//go:build !linux

package main

import "errors"

func newWatcher(func(string, ...any)) (watcher, error) {
	return nil, errors.New("inotify: not on this system")
}
