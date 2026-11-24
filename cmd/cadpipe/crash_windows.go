//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
)

func forceKillSelf() {
	_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(os.Getpid())).Run()
	os.Exit(1)
}
