//go:build windows

package main

import "os"

func setupReloadSignals(ch chan<- os.Signal) {}

func isReloadSignal(sig os.Signal) bool { return false }
