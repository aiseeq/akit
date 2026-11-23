//go:build !unix

package main

import "syscall"

// takeSlot: without flock no slot is ever free, so the shadow does not start.
func takeSlot(string) (func(), error) { return nil, nil }

func detached() *syscall.SysProcAttr { return nil }
