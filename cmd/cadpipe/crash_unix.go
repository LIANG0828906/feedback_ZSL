//go:build !windows

package main

import "syscall"

func forceKillSelf() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
}
