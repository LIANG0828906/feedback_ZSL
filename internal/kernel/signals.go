package kernel

import (
	"os"
	"syscall"
)

type SignalDisposition string

const (
	SignalGraceful SignalDisposition = "graceful_finish_tick"
	SignalForced   SignalDisposition = "forced_unrecoverable_in_process"
	SignalUnknown  SignalDisposition = "unknown"
)

// ClassifyTermination distinguishes catchable graceful signals from forced
// termination. SIGKILL cannot be delivered to a Go process; callers simulate it
// by killing the process or truncating an unfinished temp file.
func ClassifyTermination(sig os.Signal) SignalDisposition {
	switch sig {
	case syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT:
		return SignalGraceful
	case syscall.SIGKILL, syscall.SIGSTOP:
		return SignalForced
	default:
		return SignalUnknown
	}
}

func GracefulSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}
}
