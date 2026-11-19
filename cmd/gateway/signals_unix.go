//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func setupReloadSignals(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGHUP)
}

func isReloadSignal(sig os.Signal) bool {
	return sig == syscall.SIGHUP
}
